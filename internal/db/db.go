// Package db opens the application database and applies embedded schema
// migrations.
package db

import (
	"context"
	"database/sql"
	"embed"
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

// Migrate applies all pending migrations. MariaDB DDL is not transactional,
// so each migration is recorded only after all its statements succeeded.
func Migrate(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version VARCHAR(100) NOT NULL PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	// Serialise concurrent starts (e.g. two replicas) with a named lock.
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK('membership_migrate', 60)").Scan(&got); err != nil || got.Int64 != 1 {
		return fmt.Errorf("acquiring migration lock: %v", err)
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "SELECT RELEASE_LOCK('membership_migrate')")

	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range ms {
		var n int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations WHERE version = ?", m.version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		log.Info("applying migration", "version", m.version)
		for _, stmt := range SplitStatements(m.sql) {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %s: %w", m.version, err)
			}
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES (?)", m.version); err != nil {
			return err
		}
	}
	return nil
}
