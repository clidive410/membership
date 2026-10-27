package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func env(m map[string]string) Getenv { return func(k string) string { return m[k] } }

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{
		"DB_DSN":         "u:p@tcp(db:3306)/membership",
		"SESSION_SECRET": "0123456789abcdef0123456789abcdef",
		"BASE_URL":       "https://clidive.org/membership/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || !c.CookieSecure || c.SessionTTL != 12*time.Hour || !c.AutoMigrate {
		t.Errorf("unexpected defaults: %+v", c)
	}
	if c.BasePath() != "/membership" {
		t.Errorf("BasePath = %q", c.BasePath())
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(env(map[string]string{"SESSION_SECRET": "short"})); err == nil {
		t.Fatal("expected error for missing DSN and short secret")
	}
}

func TestFileVariant(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte("abcdefghijklmnopqrstuvwxyz0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(env(map[string]string{
		"DB_DSN":              "u:p@tcp(db:3306)/membership",
		"SESSION_SECRET_FILE": p,
		"COOKIE_SECURE":       "false",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if string(c.SessionSecret) != "abcdefghijklmnopqrstuvwxyz0123456789" || c.CookieSecure {
		t.Errorf("unexpected config: %+v", c)
	}
}
