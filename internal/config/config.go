// Package config loads the service configuration from environment variables.
//
// Every variable can alternatively be supplied as a file by setting NAME_FILE
// to a path (for example a Docker Swarm secret mounted under /run/secrets).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the runtime configuration of the service.
type Config struct {
	ListenAddr    string        // LISTEN_ADDR, default ":8080"
	DBDSN         string        // DB_DSN, go-sql-driver/mysql DSN for the app database
	SessionSecret []byte        // SESSION_SECRET, at least 32 bytes
	BaseURL       *url.URL      // BASE_URL, public URL incl. optional path prefix
	CookieSecure  bool          // COOKIE_SECURE, defaults to true when BASE_URL is https
	SessionTTL    time.Duration // SESSION_TTL, default 12h
	LogLevel      slog.Level    // LOG_LEVEL: debug, info, warn, error
	LogFormat     string        // LOG_FORMAT: json (default) or text
	AutoMigrate   bool          // AUTO_MIGRATE, run schema migrations at startup (default true)
}

// BasePath returns the path prefix of BaseURL without a trailing slash
// ("" when the service is mounted at the root).
func (c *Config) BasePath() string {
	if c.BaseURL == nil {
		return ""
	}
	return strings.TrimRight(c.BaseURL.Path, "/")
}

// Getenv is the lookup function used by Load; tests may replace it.
type Getenv func(string) string

// lookup returns NAME, or the trimmed contents of the file named by NAME_FILE.
func lookup(getenv Getenv, name string) (string, error) {
	if v := getenv(name); v != "" {
		return v, nil
	}
	if p := getenv(name + "_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("reading %s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", nil
}

// LoadDB loads only what is needed to talk to the app database (used by CLI
// subcommands that do not serve HTTP).
func LoadDB(getenv Getenv) (string, error) {
	dsn, err := lookup(getenv, "DB_DSN")
	if err != nil {
		return "", err
	}
	if dsn == "" {
		return "", errors.New("DB_DSN is required")
	}
	return dsn, nil
}

// Load reads and validates the full configuration.
func Load(getenv Getenv) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	var errs []error
	get := func(name, def string) string {
		v, err := lookup(getenv, name)
		if err != nil {
			errs = append(errs, err)
		}
		if v == "" {
			return def
		}
		return v
	}

	c := &Config{
		ListenAddr: get("LISTEN_ADDR", ":8080"),
		DBDSN:      get("DB_DSN", ""),
		LogFormat:  strings.ToLower(get("LOG_FORMAT", "json")),
	}
	if c.DBDSN == "" {
		errs = append(errs, errors.New("DB_DSN is required"))
	}

	secret := get("SESSION_SECRET", "")
	if len(secret) < 32 {
		errs = append(errs, errors.New("SESSION_SECRET is required and must be at least 32 characters"))
	}
	c.SessionSecret = []byte(secret)

	base, err := url.Parse(get("BASE_URL", "http://localhost:8080"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		errs = append(errs, fmt.Errorf("BASE_URL must be an absolute URL, got %q", get("BASE_URL", "")))
	} else {
		c.BaseURL = base
	}

	secureDefault := c.BaseURL != nil && c.BaseURL.Scheme == "https"
	c.CookieSecure, err = parseBool(get("COOKIE_SECURE", ""), secureDefault)
	if err != nil {
		errs = append(errs, fmt.Errorf("COOKIE_SECURE: %w", err))
	}
	c.AutoMigrate, err = parseBool(get("AUTO_MIGRATE", ""), true)
	if err != nil {
		errs = append(errs, fmt.Errorf("AUTO_MIGRATE: %w", err))
	}

	c.SessionTTL, err = time.ParseDuration(get("SESSION_TTL", "12h"))
	if err != nil || c.SessionTTL <= 0 {
		errs = append(errs, fmt.Errorf("SESSION_TTL must be a positive duration"))
	}

	if err := c.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be json or text"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func parseBool(s string, def bool) (bool, error) {
	if s == "" {
		return def, nil
	}
	return strconv.ParseBool(s)
}
