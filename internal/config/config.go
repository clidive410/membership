// Package config loads the service configuration from environment variables.
//
// Every variable can alternatively be supplied as a file by setting NAME_FILE
// to a path (for example a Docker Swarm secret mounted under /run/secrets).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/netip"
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
	CookieDomain  string        // COOKIE_DOMAIN, e.g. "clidive.org" to share the session with the WordPress site
	SessionTTL    time.Duration // SESSION_TTL, default 12h (password sign-in)
	// MEMBER_SESSION_TTL: lifetime of sessions started with an emailed
	// sign-in link, default 720h (30 days).
	MemberSessionTTL time.Duration
	// LOGIN_RETURN_URL: optional default destination after a member signs
	// in (e.g. https://www.clidive.org/). Absolute next= URLs are accepted
	// only on this URL's scheme and host.
	LoginReturnURL *url.URL
	LogLevel       slog.Level // LOG_LEVEL: debug, info, warn, error
	LogFormat      string     // LOG_FORMAT: json (default) or text
	// MIGRATE_ON_START: apply pending migrations before serving (default
	// false; `serve --migrate` forces it). AUTO_MIGRATE is a deprecated alias.
	MigrateOnStart bool
	MailFrom       string // MAIL_FROM, sender address, "addr" or "Name <addr>", default membership@clidive.org
	SMTPHost       string // SMTP_HOST; empty: emails are not sent (logged in development)
	SMTPPort       int    // SMTP_PORT, default 587 (STARTTLS); 465 for implicit TLS
	SMTPUsername   string // SMTP_USERNAME
	SMTPPassword   string // SMTP_PASSWORD
	// TRUSTED_PROXIES: comma-separated CIDRs of reverse proxies whose
	// X-Forwarded-For is believed (e.g. the Swarm overlay network).
	TrustedProxies []netip.Prefix
}

// Development reports whether the service runs in local development mode
// (LOG_FORMAT=text). Only then may sign-in links be logged instead of
// emailed when SMTP is not configured.
func (c *Config) Development() bool { return c.LogFormat == "text" }

// BasePath returns the path prefix of BaseURL without a trailing slash
// ("" when the service is mounted at the root).
func (c *Config) BasePath() string {
	if c.BaseURL == nil {
		return ""
	}
	return strings.TrimRight(c.BaseURL.Path, "/")
}

// OIDCClient is a relying party registered at start-up by `serve` and
// `membership oidc client ensure` (the WordPress site).
type OIDCClient struct {
	ID          string // OIDC_WORDPRESS_CLIENT_ID
	Secret      string // OIDC_WORDPRESS_CLIENT_SECRET (or _FILE); never log it
	RedirectURI string // OIDC_WORDPRESS_REDIRECT_URI
	// PostLogoutRedirectURI is OIDC_WORDPRESS_POST_LOGOUT_URI (optional;
	// default: the origin of RedirectURI followed by "/").
	PostLogoutRedirectURI string
}

// LoadOIDCClient reads the WordPress client registration. It returns
// (nil, nil) when OIDC_WORDPRESS_CLIENT_ID is not set. Errors never fail
// the server: the caller logs them.
func LoadOIDCClient(getenv Getenv) (*OIDCClient, error) {
	id, err := lookup(getenv, "OIDC_WORDPRESS_CLIENT_ID")
	if err != nil || id == "" {
		return nil, err
	}
	c := &OIDCClient{ID: id}
	var errs []error
	for _, f := range []struct {
		name string
		dst  *string
	}{{"OIDC_WORDPRESS_CLIENT_SECRET", &c.Secret}, {"OIDC_WORDPRESS_REDIRECT_URI", &c.RedirectURI}, {"OIDC_WORDPRESS_POST_LOGOUT_URI", &c.PostLogoutRedirectURI}} {
		v, err := lookup(getenv, f.name)
		if err != nil {
			errs = append(errs, err)
		}
		*f.dst = v
	}
	if c.Secret == "" {
		errs = append(errs, errors.New("OIDC_WORDPRESS_CLIENT_SECRET (or _FILE) is required with OIDC_WORDPRESS_CLIENT_ID"))
	}
	if c.RedirectURI == "" {
		errs = append(errs, errors.New("OIDC_WORDPRESS_REDIRECT_URI is required with OIDC_WORDPRESS_CLIENT_ID"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// OIDCIssuer returns the OpenID Connect issuer: BASE_URL without a trailing
// slash. OIDC_ISSUER, if set, must name the same URL; a different value is
// reported (and ignored) so that a typo cannot take the portal down.
func (c *Config) OIDCIssuer(getenv Getenv) (string, error) {
	issuer := strings.TrimRight(c.BaseURL.String(), "/")
	v, err := lookup(getenv, "OIDC_ISSUER")
	if err != nil {
		return issuer, err
	}
	if v != "" && strings.TrimRight(v, "/") != issuer {
		return issuer, fmt.Errorf("OIDC_ISSUER %q differs from BASE_URL %q; the issuer is BASE_URL", v, issuer)
	}
	return issuer, nil
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

// LoadSessionSecret reads only SESSION_SECRET (for CLI subcommands that
// handle OIDC signing keys, which are sealed with a key derived from it).
func LoadSessionSecret(getenv Getenv) ([]byte, error) {
	v, err := lookup(getenv, "SESSION_SECRET")
	if err != nil {
		return nil, err
	}
	if len(v) < 32 {
		return nil, errors.New("SESSION_SECRET is required and must be at least 32 characters")
	}
	return []byte(v), nil
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
		ListenAddr:   get("LISTEN_ADDR", ":8080"),
		DBDSN:        get("DB_DSN", ""),
		LogFormat:    strings.ToLower(get("LOG_FORMAT", "json")),
		CookieDomain: strings.TrimPrefix(get("COOKIE_DOMAIN", ""), "."),
		MailFrom:     get("MAIL_FROM", "membership@clidive.org"),
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
	migrateName := "MIGRATE_ON_START"
	migrate := get(migrateName, "")
	if migrate == "" {
		migrateName, migrate = "AUTO_MIGRATE", get("AUTO_MIGRATE", "")
	}
	c.MigrateOnStart, err = parseBool(migrate, false)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", migrateName, err))
	}

	c.SessionTTL, err = time.ParseDuration(get("SESSION_TTL", "12h"))
	if err != nil || c.SessionTTL <= 0 {
		errs = append(errs, fmt.Errorf("SESSION_TTL must be a positive duration"))
	}
	c.MemberSessionTTL, err = time.ParseDuration(get("MEMBER_SESSION_TTL", "720h"))
	if err != nil || c.MemberSessionTTL <= 0 {
		errs = append(errs, fmt.Errorf("MEMBER_SESSION_TTL must be a positive duration"))
	}
	if v := get("LOGIN_RETURN_URL", ""); v != "" {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			errs = append(errs, fmt.Errorf("LOGIN_RETURN_URL must be an absolute http(s) URL, got %q", v))
		} else {
			c.LoginReturnURL = u
		}
	}

	c.SMTPHost = get("SMTP_HOST", "")
	c.SMTPUsername = get("SMTP_USERNAME", "")
	c.SMTPPassword = get("SMTP_PASSWORD", "")
	c.SMTPPort, err = strconv.Atoi(get("SMTP_PORT", "587"))
	if err != nil || c.SMTPPort <= 0 || c.SMTPPort > 65535 {
		errs = append(errs, fmt.Errorf("SMTP_PORT must be a port number"))
	}
	if (c.SMTPUsername == "") != (c.SMTPPassword == "") {
		errs = append(errs, errors.New("SMTP_USERNAME and SMTP_PASSWORD must be set together"))
	}
	if _, err := mail.ParseAddress(c.MailFrom); err != nil {
		errs = append(errs, fmt.Errorf("MAIL_FROM must be an address or \"Name <address>\": %w", err))
	}

	if err := c.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}
	for _, v := range strings.Split(get("TRUSTED_PROXIES", ""), ",") {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		pfx, err := netip.ParsePrefix(v)
		if a, aerr := netip.ParseAddr(v); aerr == nil { // a bare address
			pfx, err = a.Prefix(a.BitLen())
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("TRUSTED_PROXIES: %w", err))
			continue
		}
		c.TrustedProxies = append(c.TrustedProxies, pfx.Masked())
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
