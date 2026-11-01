package idp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/members"
)

// Scopes the provider knows. ScopeClidive adds the membership claims.
const ScopeClidive = "clidive"

// DefaultClientScopes are the scopes a client may request unless told
// otherwise.
var DefaultClientScopes = []string{oidc.ScopeOpenID, oidc.ScopeEmail, oidc.ScopeProfile, oidc.ScopeOfflineAccess, ScopeClidive}

// knownScopes are the scopes a client may be allowed.
var knownScopes = DefaultClientScopes

// MinSecretLength is the shortest client secret accepted from a file or
// the environment.
const MinSecretLength = 32

// Audit actions for client administration (actor: the CLI).
const (
	ActionClientCreate = "oidc.client_create"
	ActionClientUpdate = "oidc.client_update"
	ActionClientSecret = "oidc.client_secret"
	ActionClientRevoke = "oidc.client_disable"
)

// ErrClientNotFound is returned for an unknown client id.
var ErrClientNotFound = errors.New("no such OIDC client")

// ClientInfo is a registered relying party, as the CLI shows it.
type ClientInfo struct {
	ID                     string
	Name                   string
	RedirectURIs           []string
	PostLogoutRedirectURIs []string
	Scopes                 []string
	FirstParty             bool
	CreatedAt              time.Time
	DisabledAt             sql.NullTime
}

// Disabled reports whether the client may no longer be used.
func (c *ClientInfo) Disabled() bool { return c.DisabledAt.Valid }

// ClientSpec describes a client to create or update.
type ClientSpec struct {
	ID                     string
	Name                   string
	RedirectURIs           []string
	PostLogoutRedirectURIs []string // default: the origin ("https://host/") of each redirect URI
	Scopes                 []string // default: DefaultClientScopes
	FirstParty             bool
	// Secret is the client secret; empty in Create generates one.
	Secret string
}

func (sp *ClientSpec) normalize() error {
	sp.ID = strings.TrimSpace(sp.ID)
	sp.Name = strings.TrimSpace(sp.Name)
	if sp.ID == "" || len(sp.ID) > 100 || strings.ContainsAny(sp.ID, " \t\r\n:") {
		return errors.New("client id must be 1 to 100 characters without spaces or colons")
	}
	if sp.Name == "" {
		sp.Name = sp.ID
	}
	if len(sp.Name) > 100 {
		return errors.New("client name must be at most 100 characters")
	}
	if len(sp.RedirectURIs) == 0 {
		return errors.New("at least one redirect URI is required")
	}
	for _, u := range append(slices.Clone(sp.RedirectURIs), sp.PostLogoutRedirectURIs...) {
		if err := validateClientURI(u); err != nil {
			return err
		}
	}
	if len(sp.PostLogoutRedirectURIs) == 0 {
		for _, r := range sp.RedirectURIs {
			u, _ := url.Parse(r)
			o := u.Scheme + "://" + u.Host + "/"
			if !slices.Contains(sp.PostLogoutRedirectURIs, o) {
				sp.PostLogoutRedirectURIs = append(sp.PostLogoutRedirectURIs, o)
			}
		}
	}
	if len(sp.Scopes) == 0 {
		sp.Scopes = slices.Clone(DefaultClientScopes)
	}
	if !slices.Contains(sp.Scopes, oidc.ScopeOpenID) {
		sp.Scopes = append([]string{oidc.ScopeOpenID}, sp.Scopes...)
	}
	for _, s := range sp.Scopes {
		if !slices.Contains(knownScopes, s) {
			return fmt.Errorf("unknown scope %q (known: %s)", s, strings.Join(knownScopes, " "))
		}
	}
	if sp.Secret != "" && len(sp.Secret) < MinSecretLength {
		return fmt.Errorf("the client secret must be at least %d characters", MinSecretLength)
	}
	return nil
}

// validateClientURI accepts absolute https URLs, and http only on
// localhost or a loopback address (development). Fragments, user info and
// wildcards are refused: redirect URIs are matched exactly.
func validateClientURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" || u.User != nil || strings.Contains(raw, "*") || strings.ContainsAny(raw, " \t\r\n") {
		return fmt.Errorf("invalid redirect URI %q: it must be an absolute URL without fragment or wildcard", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if ip := net.ParseIP(h); h == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("invalid redirect URI %q: use https (http only for localhost)", raw)
}

// ClientStore manages oidc_clients.
type ClientStore struct{ DB *sql.DB }

const clientColumns = "client_id, name, redirect_uris, post_logout_redirect_uris, scopes, first_party, created_at, disabled_at"

type rowScanner interface{ Scan(dest ...any) error }

func scanClient(sc rowScanner) (*ClientInfo, error) {
	var (
		c             ClientInfo
		redir, logout string
		scopes        string
	)
	if err := sc.Scan(&c.ID, &c.Name, &redir, &logout, &scopes, &c.FirstParty, &c.CreatedAt, &c.DisabledAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(redir), &c.RedirectURIs); err != nil {
		return nil, fmt.Errorf("client %s redirect_uris: %w", c.ID, err)
	}
	if err := json.Unmarshal([]byte(logout), &c.PostLogoutRedirectURIs); err != nil {
		return nil, fmt.Errorf("client %s post_logout_redirect_uris: %w", c.ID, err)
	}
	c.Scopes = strings.Fields(scopes)
	return &c, nil
}

// Get returns a client (enabled or not).
func (cs *ClientStore) Get(ctx context.Context, id string) (*ClientInfo, error) {
	c, err := scanClient(cs.DB.QueryRowContext(ctx, "SELECT "+clientColumns+" FROM oidc_clients WHERE client_id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrClientNotFound
	}
	return c, err
}

// List returns all clients.
func (cs *ClientStore) List(ctx context.Context) ([]ClientInfo, error) {
	rows, err := cs.DB.QueryContext(ctx, "SELECT "+clientColumns+" FROM oidc_clients ORDER BY client_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClientInfo
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func jsonList(v []string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Create registers a client. When spec.Secret is empty a secret is
// generated; the returned secret is shown once and only its argon2id hash
// is stored.
func (cs *ClientStore) Create(ctx context.Context, actor members.Actor, spec ClientSpec) (string, error) {
	if err := spec.normalize(); err != nil {
		return "", err
	}
	secret := spec.Secret
	if secret == "" {
		secret = auth.NewToken()
	}
	hash, err := auth.HashPassword(secret)
	if err != nil {
		return "", err
	}
	err = inTx(ctx, cs.DB, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO oidc_clients (client_id, secret_hash, name, redirect_uris, post_logout_redirect_uris, scopes, first_party)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, spec.ID, hash, spec.Name, jsonList(spec.RedirectURIs), jsonList(spec.PostLogoutRedirectURIs),
			strings.Join(spec.Scopes, " "), spec.FirstParty)
		if isDuplicate(err) {
			return fmt.Errorf("client %q already exists (use `ensure` or `rotate-secret`)", spec.ID)
		} else if err != nil {
			return err
		}
		return members.WriteAudit(ctx, tx, actor, ActionClientCreate, 0, "oidc_client", 0, clientAudit(&spec))
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

func clientAudit(spec *ClientSpec) map[string]any {
	return map[string]any{"client_id": spec.ID, "name": spec.Name, "redirect_uris": spec.RedirectURIs,
		"post_logout_redirect_uris": spec.PostLogoutRedirectURIs, "scopes": spec.Scopes, "first_party": spec.FirstParty}
}

// RotateSecret replaces a client's secret (generated when secret is
// empty) and returns it. Outstanding tokens stay valid until they expire
// or are revoked; disable the client to cut them off.
func (cs *ClientStore) RotateSecret(ctx context.Context, actor members.Actor, id, secret string) (string, error) {
	if secret != "" && len(secret) < MinSecretLength {
		return "", fmt.Errorf("the client secret must be at least %d characters", MinSecretLength)
	}
	if secret == "" {
		secret = auth.NewToken()
	}
	hash, err := auth.HashPassword(secret)
	if err != nil {
		return "", err
	}
	err = inTx(ctx, cs.DB, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE oidc_clients SET secret_hash = ? WHERE client_id = ?", hash, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrClientNotFound
		}
		return members.WriteAudit(ctx, tx, actor, ActionClientSecret, 0, "oidc_client", 0, map[string]string{"client_id": id})
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

// Disable stops a client: it can no longer authorize, exchange codes or
// refresh, and all its tokens, codes and consents are deleted.
func (cs *ClientStore) Disable(ctx context.Context, actor members.Actor, id string) error {
	return inTx(ctx, cs.DB, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE oidc_clients SET disabled_at = COALESCE(disabled_at, UTC_TIMESTAMP()) WHERE client_id = ?", id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			var exists bool
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) > 0 FROM oidc_clients WHERE client_id = ?", id).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrClientNotFound
			}
		}
		for _, q := range []string{"DELETE FROM oidc_tokens WHERE client_id = ?", "DELETE FROM oidc_authorizations WHERE client_id = ?",
			"DELETE FROM oidc_consents WHERE client_id = ?"} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		return members.WriteAudit(ctx, tx, actor, ActionClientRevoke, 0, "oidc_client", 0, map[string]string{"client_id": id})
	})
}

// EnsureResult says what Ensure did.
type EnsureResult struct {
	Created       bool
	Updated       bool // name, URIs, scopes or first-party flag changed
	SecretChanged bool
	Disabled      bool // the client exists but is disabled; Ensure leaves it so
}

// Ensure creates the client or brings it in line with spec (idempotent):
// name, redirect and post-logout URIs, scopes, first-party flag and secret.
// The secret is re-hashed only when it does not match the stored hash. A
// disabled client is updated but not re-enabled. spec.Secret is required.
func (cs *ClientStore) Ensure(ctx context.Context, actor members.Actor, spec ClientSpec) (*EnsureResult, error) {
	if spec.Secret == "" {
		return nil, errors.New("ensure needs the client secret")
	}
	if err := spec.normalize(); err != nil {
		return nil, err
	}
	res := &EnsureResult{}
	err := inTx(ctx, cs.DB, func(tx *sql.Tx) error {
		var hash string
		cur, err := scanClientHash(tx.QueryRowContext(ctx, "SELECT "+clientColumns+", secret_hash FROM oidc_clients WHERE client_id = ? FOR UPDATE", spec.ID), &hash)
		if errors.Is(err, sql.ErrNoRows) {
			h, err := auth.HashPassword(spec.Secret)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO oidc_clients (client_id, secret_hash, name, redirect_uris, post_logout_redirect_uris, scopes, first_party)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, spec.ID, h, spec.Name, jsonList(spec.RedirectURIs), jsonList(spec.PostLogoutRedirectURIs),
				strings.Join(spec.Scopes, " "), spec.FirstParty); err != nil {
				return err
			}
			res.Created = true
			return members.WriteAudit(ctx, tx, actor, ActionClientCreate, 0, "oidc_client", 0, clientAudit(&spec))
		} else if err != nil {
			return err
		}
		res.Disabled = cur.Disabled()
		res.Updated = cur.Name != spec.Name || !slices.Equal(cur.RedirectURIs, spec.RedirectURIs) ||
			!slices.Equal(cur.PostLogoutRedirectURIs, spec.PostLogoutRedirectURIs) || !slices.Equal(cur.Scopes, spec.Scopes) ||
			cur.FirstParty != spec.FirstParty
		if ok, err := auth.VerifyPassword(hash, spec.Secret); err != nil || !ok {
			res.SecretChanged = true
			if hash, err = auth.HashPassword(spec.Secret); err != nil {
				return err
			}
		}
		if !res.Updated && !res.SecretChanged {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE oidc_clients SET secret_hash = ?, name = ?, redirect_uris = ?, post_logout_redirect_uris = ?,
			scopes = ?, first_party = ? WHERE client_id = ?`, hash, spec.Name, jsonList(spec.RedirectURIs), jsonList(spec.PostLogoutRedirectURIs),
			strings.Join(spec.Scopes, " "), spec.FirstParty, spec.ID); err != nil {
			return err
		}
		if res.Updated {
			if err := members.WriteAudit(ctx, tx, actor, ActionClientUpdate, 0, "oidc_client", 0, clientAudit(&spec)); err != nil {
				return err
			}
		}
		if res.SecretChanged {
			return members.WriteAudit(ctx, tx, actor, ActionClientSecret, 0, "oidc_client", 0, map[string]string{"client_id": spec.ID})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func scanClientHash(row *sql.Row, hash *string) (*ClientInfo, error) {
	return scanClient(scanFunc(func(dest ...any) error { return row.Scan(append(dest, hash)...) }))
}

type scanFunc func(dest ...any) error

func (f scanFunc) Scan(dest ...any) error { return f(dest...) }

// authenticate checks a client secret for an enabled client in constant
// time with respect to whether the client exists.
func (cs *ClientStore) authenticate(ctx context.Context, id, secret string) (*ClientInfo, error) {
	var hash string
	c, err := scanClientHash(cs.DB.QueryRowContext(ctx, "SELECT "+clientColumns+", secret_hash FROM oidc_clients WHERE client_id = ? AND disabled_at IS NULL", id), &hash)
	if errors.Is(err, sql.ErrNoRows) {
		auth.VerifyPassword(dummyClientHash, secret) // equalise timing
		return nil, ErrClientNotFound
	} else if err != nil {
		return nil, err
	}
	if secret == "" {
		return nil, ErrClientNotFound
	}
	ok, err := auth.VerifyPassword(hash, secret)
	if err != nil || !ok {
		return nil, ErrClientNotFound
	}
	return c, nil
}

var dummyClientHash, _ = auth.HashPassword("dummy-client-secret-for-timing")

// client adapts a ClientInfo to op.Client.
type client struct {
	info     *ClientInfo
	loginURL func(authRequestID string) string
}

var _ op.Client = (*client)(nil)

// Token lifetimes (docs/sso-plan.md).
const (
	CodeTTL         = 60 * time.Second
	AccessTokenTTL  = 10 * time.Minute
	IDTokenTTL      = 10 * time.Minute
	RefreshTokenTTL = 30 * 24 * time.Hour
	// AuthRequestTTL bounds the time between /oauth/authorize and the
	// consent decision.
	AuthRequestTTL = 10 * time.Minute
)

func (c *client) GetID() string                       { return c.info.ID }
func (c *client) RedirectURIs() []string              { return c.info.RedirectURIs }
func (c *client) PostLogoutRedirectURIs() []string    { return c.info.PostLogoutRedirectURIs }
func (c *client) ApplicationType() op.ApplicationType { return op.ApplicationTypeWeb }
func (c *client) AuthMethod() oidc.AuthMethod         { return oidc.AuthMethodBasic }
func (c *client) ResponseTypes() []oidc.ResponseType {
	return []oidc.ResponseType{oidc.ResponseTypeCode}
}
func (c *client) LoginURL(id string) string            { return c.loginURL(id) }
func (c *client) AccessTokenType() op.AccessTokenType  { return op.AccessTokenTypeBearer }
func (c *client) IDTokenLifetime() time.Duration       { return IDTokenTTL }
func (c *client) DevMode() bool                        { return false }
func (c *client) IsScopeAllowed(scope string) bool     { return slices.Contains(c.info.Scopes, scope) }
func (c *client) IDTokenUserinfoClaimsAssertion() bool { return true }
func (c *client) ClockSkew() time.Duration             { return 0 }

func (c *client) GrantTypes() []oidc.GrantType {
	return []oidc.GrantType{oidc.GrantTypeCode, oidc.GrantTypeRefreshToken}
}

func (c *client) RestrictAdditionalIdTokenScopes() func([]string) []string {
	return func(s []string) []string { return s }
}

func (c *client) RestrictAdditionalAccessTokenScopes() func([]string) []string {
	return func(s []string) []string { return s }
}

func inTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
