package db

import (
	"strings"
	"testing"
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
