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
	if c.ListenAddr != ":8080" || !c.CookieSecure || c.SessionTTL != 12*time.Hour || c.MigrateOnStart {
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

func TestTrustedProxies(t *testing.T) {
	base := map[string]string{"DB_DSN": "u:p@tcp(db:3306)/m", "SESSION_SECRET": "0123456789abcdef0123456789abcdef"}
	base["TRUSTED_PROXIES"] = "10.0.0.0/8, 192.168.1.7 ,fd00::/8"
	c, err := Load(env(base))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedProxies) != 3 || c.TrustedProxies[1].String() != "192.168.1.7/32" {
		t.Errorf("TrustedProxies = %v", c.TrustedProxies)
	}
	base["TRUSTED_PROXIES"] = "not-an-ip"
	if _, err := Load(env(base)); err == nil {
		t.Error("expected an error for an invalid proxy")
	}
}

func TestMigrateOnStart(t *testing.T) {
	base := map[string]string{
		"DB_DSN":         "u:p@tcp(db:3306)/membership",
		"SESSION_SECRET": "0123456789abcdef0123456789abcdef",
	}
	for _, tc := range []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{}, false},
		{map[string]string{"MIGRATE_ON_START": "true"}, true},
		{map[string]string{"AUTO_MIGRATE": "true"}, true},
		{map[string]string{"MIGRATE_ON_START": "false", "AUTO_MIGRATE": "true"}, false},
	} {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range tc.env {
			m[k] = v
		}
		c, err := Load(env(m))
		if err != nil {
			t.Fatal(err)
		}
		if c.MigrateOnStart != tc.want {
			t.Errorf("%v: MigrateOnStart = %v", tc.env, c.MigrateOnStart)
		}
	}
	if _, err := Load(env(map[string]string{"DB_DSN": "x", "SESSION_SECRET": base["SESSION_SECRET"], "MIGRATE_ON_START": "maybe"})); err == nil {
		t.Error("expected error for invalid MIGRATE_ON_START")
	}
}

func TestMailAndMemberLogin(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{"DB_DSN": "u:p@tcp(db:3306)/m", "SESSION_SECRET": "0123456789abcdef0123456789abcdef"}
	}
	c, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if c.SMTPHost != "" || c.SMTPPort != 587 || c.MemberSessionTTL != 30*24*time.Hour || c.LoginReturnURL != nil ||
		c.MailFrom != "membership@clidive.org" || c.Development() {
		t.Errorf("defaults: %+v", c)
	}

	pw := filepath.Join(t.TempDir(), "smtp_pw")
	if err := os.WriteFile(pw, []byte("smtp-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := base()
	m["SMTP_HOST"], m["SMTP_PORT"], m["SMTP_USERNAME"], m["SMTP_PASSWORD_FILE"] = "smtp.example.org", "465", "AKIAEXAMPLE", pw
	m["MAIL_FROM"], m["LOGIN_RETURN_URL"], m["LOG_FORMAT"] = "Clidive Membership <membership@clidive.org>", "https://www.clidive.org/", "text"
	c, err = Load(env(m))
	if err != nil {
		t.Fatal(err)
	}
	if c.SMTPHost != "smtp.example.org" || c.SMTPPort != 465 || c.SMTPPassword != "smtp-secret" ||
		c.LoginReturnURL.Host != "www.clidive.org" || !c.Development() {
		t.Errorf("config: %+v", c)
	}

	for k, v := range map[string]string{
		"SMTP_PORT": "smtp", "MAIL_FROM": "not an address", "LOGIN_RETURN_URL": "/relative",
		"MEMBER_SESSION_TTL": "-1h", "SMTP_USERNAME": "only-a-user",
	} {
		m := base()
		m[k] = v
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s=%q: expected an error", k, v)
		}
	}
}
