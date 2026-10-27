package db

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSplitStatements(t *testing.T) {
	src := "-- comment\nCREATE TABLE a (\n  x INT\n);\n\nINSERT INTO a VALUES\n  (1),\n  (2);\n"
	got := SplitStatements(src)
	if len(got) != 2 {
		t.Fatalf("got %d statements: %q", len(got), got)
	}
	if !strings.HasPrefix(got[0], "CREATE TABLE a") || strings.HasSuffix(got[1], ";") {
		t.Errorf("unexpected statements: %q", got)
	}
}

func TestEmbeddedMigrationsParse(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil || len(ms) == 0 {
		t.Fatalf("loadMigrations: %v (%d)", err, len(ms))
	}
	for _, m := range ms {
		if len(SplitStatements(m.sql)) == 0 {
			t.Errorf("migration %s has no statements", m.version)
		}
	}
}

func TestNormalizeDSN(t *testing.T) {
	got, err := NormalizeDSN("user:pw@tcp(db:3306)/membership")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"parseTime=true", "charset=utf8mb4"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing %q", got, want)
		}
	}
}

type fakeMigrator struct {
	lockOK   bool
	lockErr  error
	applyErr error
	done     map[string]bool
	calls    []string
}

func (f *fakeMigrator) AcquireLock(_ context.Context, name string, timeout time.Duration) (bool, error) {
	f.calls = append(f.calls, "lock "+name+" "+timeout.String())
	return f.lockOK, f.lockErr
}
func (f *fakeMigrator) ReleaseLock(_ context.Context, name string) error {
	f.calls = append(f.calls, "release "+name)
	return nil
}
func (f *fakeMigrator) EnsureTable(context.Context) error {
	f.calls = append(f.calls, "table")
	return nil
}
func (f *fakeMigrator) Applied(_ context.Context, v string) (bool, error) { return f.done[v], nil }
func (f *fakeMigrator) Apply(_ context.Context, m migration) error {
	f.calls = append(f.calls, "apply "+m.version)
	if f.applyErr != nil {
		return f.applyErr
	}
	f.done[m.version] = true
	return nil
}

var testMigrations = []migration{{version: "0001_a"}, {version: "0002_b"}, {version: "0003_c"}}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRunMigrationsAppliesPendingUnderLock(t *testing.T) {
	f := &fakeMigrator{lockOK: true, done: map[string]bool{"0001_a": true}}
	if err := runMigrations(context.Background(), f, testMigrations, 60*time.Second, discard()); err != nil {
		t.Fatal(err)
	}
	want := []string{"lock membership_migrate 1m0s", "table", "apply 0002_b", "apply 0003_c", "release membership_migrate"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
}

func TestRunMigrationsLockTimeout(t *testing.T) {
	f := &fakeMigrator{lockOK: false, done: map[string]bool{}}
	err := runMigrations(context.Background(), f, testMigrations, time.Second, discard())
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	if !slices.Equal(f.calls, []string{"lock membership_migrate 1s"}) {
		t.Errorf("must not migrate or release a lock it does not hold: %q", f.calls)
	}
}

func TestRunMigrationsLockError(t *testing.T) {
	f := &fakeMigrator{lockErr: errors.New("conn refused"), done: map[string]bool{}}
	err := runMigrations(context.Background(), f, testMigrations, time.Second, discard())
	if err == nil || errors.Is(err, ErrLockTimeout) || len(f.calls) != 1 {
		t.Fatalf("err = %v, calls = %q", err, f.calls)
	}
}

func TestRunMigrationsReleasesLockOnFailure(t *testing.T) {
	f := &fakeMigrator{lockOK: true, applyErr: errors.New("boom"), done: map[string]bool{}}
	if err := runMigrations(context.Background(), f, testMigrations, time.Second, discard()); err == nil {
		t.Fatal("expected error")
	}
	want := []string{"lock membership_migrate 1s", "table", "apply 0001_a", "release membership_migrate"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %q, want %q", f.calls, want)
	}
}
