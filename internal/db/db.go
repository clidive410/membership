// Package db opens the application database and applies embedded schema
// migrations.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// NormalizeDSN parses a go-sql-driver/mysql DSN and forces the options the
// application relies on: parseTime, UTC, utf8mb4.
func NormalizeDSN(dsn string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("parsing DSN: %w", err)
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["charset"] = "utf8mb4"
	cfg.Collation = "utf8mb4_unicode_ci"
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	return cfg.FormatDSN(), nil
}

// Open connects to MariaDB and verifies the connection.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	norm, err := NormalizeDSN(dsn)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", norm)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	return db, nil
}

type migration struct {
	version string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: strings.TrimSuffix(e.Name(), ".sql"), sql: string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	return ms, nil
}

// SplitStatements splits a migration file into individual statements. It
// handles the simple SQL we write ourselves: statements end with ';' at the
// end of a line, and '--' line comments are ignored. Semicolons inside string
// literals at the end of a line are not supported.
func SplitStatements(src string) []string {
	var stmts []string
	var cur strings.Builder
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			s := strings.TrimSpace(cur.String())
			stmts = append(stmts, strings.TrimSuffix(s, ";"))
			cur.Reset()
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		stmts = append(stmts, s)
	}
	return stmts
}

// MigrationLock is the MariaDB named lock that serialises migration runs,
// so that replicas starting together (for example the old and new task of a
// start-first rolling update) never apply the same migration twice.
const MigrationLock = "membership_migrate"

// DefaultLockTimeout is how long Migrate waits for MigrationLock.
const DefaultLockTimeout = 60 * time.Second

// ErrLockTimeout is returned when the migration lock could not be obtained
// within the timeout, usually because another instance is still migrating.
var ErrLockTimeout = errors.New("timed out waiting for the migration lock")

// migrator is what runMigrations needs from the database. sqlMigrator
// implements it on a single connection (named locks belong to a session);
// tests use a fake.
type migrator interface {
	// AcquireLock waits up to timeout for the named lock and reports whether
	// it was obtained.
	AcquireLock(ctx context.Context, name string, timeout time.Duration) (bool, error)
	ReleaseLock(ctx context.Context, name string) error
	EnsureTable(ctx context.Context) error
	Applied(ctx context.Context, version string) (bool, error)
	Apply(ctx context.Context, m migration) error
}

// runMigrations takes the lock, applies every migration not yet recorded, in
// order, and releases the lock whatever happens.
func runMigrations(ctx context.Context, m migrator, ms []migration, timeout time.Duration, log *slog.Logger) (err error) {
	ok, err := m.AcquireLock(ctx, MigrationLock, timeout)
	if err != nil {
		return fmt.Errorf("acquiring migration lock: %w", err)
	}
	if !ok {
		return fmt.Errorf("%w %q after %s", ErrLockTimeout, MigrationLock, timeout)
	}
	defer func() {
		if rerr := m.ReleaseLock(context.WithoutCancel(ctx), MigrationLock); rerr != nil {
			log.Warn("releasing migration lock", "err", rerr)
		}
	}()

	if err := m.EnsureTable(ctx); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	applied := 0
	for _, mig := range ms {
		done, err := m.Applied(ctx, mig.version)
		if err != nil {
			return fmt.Errorf("checking migration %s: %w", mig.version, err)
		}
		if done {
			continue
		}
		log.Info("applying migration", "version", mig.version)
		if err := m.Apply(ctx, mig); err != nil {
			return err
		}
		applied++
	}
	log.Info("migrations up to date", "applied", applied, "total", len(ms))
	return nil
}

// Migrate applies all pending embedded migrations under the MigrationLock,
// waiting up to DefaultLockTimeout for it.
func Migrate(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	return MigrateWithTimeout(ctx, db, DefaultLockTimeout, log)
}

// MigrateWithTimeout is Migrate with an explicit lock timeout (whole seconds).
// MariaDB DDL is not transactional, so each migration is recorded only after
// all its statements succeeded.
func MigrateWithTimeout(ctx context.Context, db *sql.DB, timeout time.Duration, log *slog.Logger) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return runMigrations(ctx, sqlMigrator{conn}, ms, timeout, log)
}

type sqlMigrator struct{ conn *sql.Conn }

func (s sqlMigrator) AcquireLock(ctx context.Context, name string, timeout time.Duration) (bool, error) {
	// GET_LOCK returns 1 when obtained, 0 on timeout and NULL on error.
	var got sql.NullInt64
	secs := int64(timeout / time.Second)
	if err := s.conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", name, secs).Scan(&got); err != nil {
		return false, err
	}
	if !got.Valid {
		return false, errors.New("GET_LOCK returned NULL")
	}
	return got.Int64 == 1, nil
}

func (s sqlMigrator) ReleaseLock(ctx context.Context, name string) error {
	_, err := s.conn.ExecContext(ctx, "SELECT RELEASE_LOCK(?)", name)
	return err
}

func (s sqlMigrator) EnsureTable(ctx context.Context) error {
	_, err := s.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version VARCHAR(100) NOT NULL PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	return err
}

func (s sqlMigrator) Applied(ctx context.Context, version string) (bool, error) {
	var n int
	err := s.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = ?", version).Scan(&n)
	return n > 0, err
}

func (s sqlMigrator) Apply(ctx context.Context, m migration) error {
	for _, stmt := range SplitStatements(m.sql) {
		if _, err := s.conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration %s: %w", m.version, err)
		}
	}
	if _, err := s.conn.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES (?)", m.version); err != nil {
		return fmt.Errorf("recording migration %s: %w", m.version, err)
	}
	return nil
}
