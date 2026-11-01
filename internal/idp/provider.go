// Package idp makes the portal an OpenID Connect provider (docs/sso-plan.md
// section 1) on top of github.com/zitadel/oidc/v3/pkg/op, with storage in
// MariaDB.
//
// Only the authorization code flow with PKCE (S256) is offered; implicit
// and hybrid flows, client credentials, JWT profile, token exchange,
// device authorization and introspection are not. Clients are registered
// with the CLI; until one is, the endpoints answer but no authorization can
// succeed and no signing key is generated.
//
// The web package owns the HTTP routes, the portal session and the consent
// page; this package owns everything the library needs (storage, keys,
// clients, claims).
package idp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"

	"github.com/clidive410/membership/internal/auth"
)

// Endpoint paths, relative to the issuer (BASE_URL).
const (
	PathDiscovery  = "/.well-known/openid-configuration"
	PathAuthorize  = "/oauth/authorize"
	PathContinue   = "/oauth/continue" // consent step (portal UI, not part of the protocol)
	PathToken      = "/oauth/token"
	PathUserinfo   = "/oauth/userinfo"
	PathJWKS       = "/oauth/jwks"
	PathEndSession = "/oauth/end_session"
	PathRevoke     = "/oauth/revoke"
)

// SupportedClaims are advertised in the discovery document.
var SupportedClaims = []string{"sub", "iss", "aud", "exp", "iat", "auth_time", "nonce", "amr", "azp", "at_hash", "c_hash",
	"email", "email_verified", "name", "given_name", "family_name",
	"member_id", "membership_status", "membership_type", "membership_expiry", "roles", "bsac_member_id"}

// Config configures a Provider.
type Config struct {
	DB            *sql.DB
	Auth          *auth.Store
	SessionSecret []byte
	// Issuer is BASE_URL without a trailing slash; it must be https
	// unless it is on localhost or a loopback address.
	Issuer   string
	BasePath string // path prefix of the issuer ("" at the root)
	Log      *slog.Logger
	Now      func() time.Time
}

// Provider is the OpenID Connect provider.
type Provider struct {
	op      *op.Provider
	storage *Storage
	keys    *KeyStore
	clients *ClientStore
	issuer  string
	log     *slog.Logger
}

// New builds the provider. It does not touch the database.
func New(cfg Config) (*Provider, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	issuer := strings.TrimRight(cfg.Issuer, "/")
	iu, err := url.Parse(issuer)
	if err != nil || iu.Host == "" {
		return nil, fmt.Errorf("idp: invalid issuer %q", cfg.Issuer)
	}
	keys, err := NewKeyStore(cfg.DB, cfg.SessionSecret, cfg.Now)
	if err != nil {
		return nil, err
	}
	tk, err := deriveKey(cfg.SessionSecret, infoTokenCrypto)
	if err != nil {
		return nil, err
	}
	ta, err := newAEAD(tk)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(cfg.BasePath, "/")
	clients := &ClientStore{DB: cfg.DB}
	st := &Storage{db: cfg.DB, clients: clients, keys: keys, auth: cfg.Auth, now: cfg.Now, log: cfg.Log,
		loginURL: func(id string) string { return base + PathContinue + "?" + url.Values{"id": {id}}.Encode() }}
	opts := []op.Option{
		op.WithCrypto(tokenCrypto{a: ta}),
		op.WithCustomEndpoints(op.NewEndpoint(PathAuthorize), op.NewEndpoint(PathToken), op.NewEndpoint(PathUserinfo),
			op.NewEndpoint(PathRevoke), op.NewEndpoint(PathEndSession), op.NewEndpoint(PathJWKS)),
	}
	if iu.Scheme == "http" {
		opts = append(opts, op.WithAllowInsecure()) // development only; production BASE_URL is https
	}
	o, err := op.NewProvider(&op.Config{
		CodeMethodS256:           true,
		AuthMethodPost:           true,
		GrantTypeRefreshToken:    true,
		DefaultLogoutRedirectURI: issuer + "/login?flash=loggedout",
		SupportedScopes:          DefaultClientScopes,
		SupportedClaims:          SupportedClaims,
	}, st, op.StaticIssuer(issuer), opts...)
	if err != nil {
		return nil, err
	}
	return &Provider{op: o, storage: st, keys: keys, clients: clients, issuer: issuer, log: cfg.Log}, nil
}

// Issuer returns the issuer identifier.
func (p *Provider) Issuer() string { return p.issuer }

// Keys returns the signing key store (for rotation).
func (p *Provider) Keys() *KeyStore { return p.keys }

// Clients returns the client store.
func (p *Provider) Clients() *ClientStore { return p.clients }

// Storage returns the library storage (consent step helpers live there).
func (p *Provider) Storage() *Storage { return p.storage }

func (p *Provider) withIssuer(r *http.Request) *http.Request {
	return r.WithContext(op.ContextWithIssuer(r.Context(), p.issuer))
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

// Discovery serves /.well-known/openid-configuration, advertising only
// what is supported.
func (p *Provider) Discovery(w http.ResponseWriter, r *http.Request) {
	basic := []oidc.AuthMethod{oidc.AuthMethodBasic, oidc.AuthMethodPost}
	cfg := &oidc.DiscoveryConfiguration{
		Issuer:                                 p.issuer,
		AuthorizationEndpoint:                  p.issuer + PathAuthorize,
		TokenEndpoint:                          p.issuer + PathToken,
		UserinfoEndpoint:                       p.issuer + PathUserinfo,
		RevocationEndpoint:                     p.issuer + PathRevoke,
		EndSessionEndpoint:                     p.issuer + PathEndSession,
		JwksURI:                                p.issuer + PathJWKS,
		ScopesSupported:                        DefaultClientScopes,
		ResponseTypesSupported:                 []string{string(oidc.ResponseTypeCode)},
		ResponseModesSupported:                 []string{string(oidc.ResponseModeQuery)},
		GrantTypesSupported:                    []oidc.GrantType{oidc.GrantTypeCode, oidc.GrantTypeRefreshToken},
		SubjectTypesSupported:                  []string{"public"},
		IDTokenSigningAlgValuesSupported:       []string{"RS256"},
		TokenEndpointAuthMethodsSupported:      basic,
		RevocationEndpointAuthMethodsSupported: basic,
		ClaimsSupported:                        SupportedClaims,
		CodeChallengeMethodsSupported:          []oidc.CodeChallengeMethod{oidc.CodeChallengeMethodS256},
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	op.Discover(w, cfg)
}

// JWKS serves the published signing keys.
func (p *Provider) JWKS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	op.Keys(w, p.withIssuer(r), p.storage)
}

// Authorize handles /oauth/authorize for a browser whose portal session
// (if any) the caller has put on the context with WithSession. A request
// that passes validation is stored and the browser sent to the consent
// step (PathContinue).
func (p *Provider) Authorize(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	op.Authorize(w, p.withIssuer(r), &authorizer{Provider: p.op, p: p})
}

// Token handles /oauth/token: authorization_code and refresh_token only.
func (p *Provider) Token(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	switch gt := r.PostFormValue("grant_type"); gt {
	case string(oidc.GrantTypeCode), string(oidc.GrantTypeRefreshToken):
	case "":
		op.RequestError(w, r, oidc.ErrInvalidRequest().WithDescription("grant_type missing"), nil)
		return
	default:
		op.RequestError(w, r, oidc.ErrUnsupportedGrantType().WithDescription("only authorization_code and refresh_token are supported"), nil)
		return
	}
	if r.PostFormValue("client_assertion") != "" {
		op.RequestError(w, r, oidc.ErrInvalidClient().WithDescription("client assertions are not supported"), nil)
		return
	}
	op.Exchange(w, p.withIssuer(r), p.op)
}

// Userinfo handles /oauth/userinfo.
func (p *Provider) Userinfo(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	op.Userinfo(w, p.withIssuer(r), p.op)
}

// Revoke handles /oauth/revoke (RFC 7009).
func (p *Provider) Revoke(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	op.Revoke(w, p.withIssuer(r), p.op)
}

// EndSession handles /oauth/end_session. The caller puts the browser's
// portal session on the context (WithSession) so that it can be ended.
func (p *Provider) EndSession(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	op.EndSession(w, p.withIssuer(r), p.op)
}

// Respond completes an approved request: it issues the code and redirects
// the browser to the client.
func (p *Provider) Respond(w http.ResponseWriter, r *http.Request, a *AuthRequest) {
	noStore(w)
	op.AuthResponse(a, &authorizer{Provider: p.op, p: p}, w, p.withIssuer(r))
}

// Fail sends an OAuth error (e.g. oidc.ErrAccessDenied()) to the client's
// redirect URI.
func (p *Provider) Fail(w http.ResponseWriter, r *http.Request, a *AuthRequest, err error) {
	noStore(w)
	op.AuthRequestError(w, p.withIssuer(r), a, err, &authorizer{Provider: p.op, p: p})
}

// ErrConsentRequired is the OIDC consent_required error (prompt=none).
func ErrConsentRequired() *oidc.Error {
	return &oidc.Error{ErrorType: "consent_required", Description: "the user has not consented to this client"}
}

// RedirectOrigin returns the origin ("https://host") of redirectURI when it
// is registered for the enabled client, for the form-action CSP of pages
// whose form submission ends in a redirect to the client.
func (p *Provider) RedirectOrigin(ctx context.Context, clientID, redirectURI string) (string, bool) {
	c, err := p.clients.Get(ctx, clientID)
	if err != nil || c.Disabled() || !slices.Contains(c.RedirectURIs, redirectURI) {
		return "", false
	}
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// Client returns an enabled client.
func (p *Provider) Client(ctx context.Context, id string) (*ClientInfo, error) {
	c, err := p.clients.Get(ctx, id)
	if err == nil && c.Disabled() {
		err = ErrClientNotFound
	}
	return c, err
}

// authorizer adds the provider's own checks to the library's validation
// of authorization requests.
type authorizer struct {
	*op.Provider
	p *Provider
}

var _ op.AuthorizeValidator = (*authorizer)(nil)

// Upper bounds for stored request values (column sizes).
const (
	maxStateLen = 1024
	maxNonceLen = 1024
)

// ValidateAuthRequest runs the library's checks (exact redirect URI,
// response type, scopes) and then requires: scope openid, PKCE with S256,
// a state and a nonce, and the query response mode. Scopes the client
// may not request are dropped.
func (a *authorizer) ValidateAuthRequest(ctx context.Context, req *oidc.AuthRequest, st op.Storage, v *op.IDTokenHintVerifier) (string, error) {
	c, err := st.GetClientByClientID(ctx, req.ClientID)
	if err != nil {
		return "", oidc.ErrInvalidRequestRedirectURI().WithDescription("unknown client").WithParent(err)
	}
	sub, err := op.ValidateAuthRequestClient(ctx, req, c, v)
	if err != nil {
		return "", err
	}
	req.Scopes = slices.DeleteFunc(req.Scopes, func(s string) bool { return !c.IsScopeAllowed(s) })
	switch {
	case !slices.Contains(req.Scopes, oidc.ScopeOpenID):
		return "", oidc.ErrInvalidScope().WithDescription("the openid scope is required")
	case req.ResponseMode != "" && req.ResponseMode != oidc.ResponseModeQuery:
		return "", oidc.ErrInvalidRequest().WithDescription("only response_mode=query is supported")
	case req.CodeChallenge == "" || req.CodeChallengeMethod != oidc.CodeChallengeMethodS256:
		return "", oidc.ErrInvalidRequest().WithDescription("PKCE with code_challenge_method=S256 is required")
	case len(req.CodeChallenge) < 43 || len(req.CodeChallenge) > 128:
		return "", oidc.ErrInvalidRequest().WithDescription("invalid code_challenge")
	case req.State == "" || len(req.State) > maxStateLen:
		return "", oidc.ErrInvalidRequest().WithDescription("a state of at most %d characters is required", maxStateLen)
	case req.Nonce == "" || len(req.Nonce) > maxNonceLen:
		return "", oidc.ErrInvalidRequest().WithDescription("a nonce of at most %d characters is required", maxNonceLen)
	}
	return sub, nil
}

// Purge deletes expired authorization requests and tokens.
func (p *Provider) Purge(ctx context.Context) error { return p.storage.Purge(ctx) }

// IsNotFound reports whether err means an unknown or expired request.
func IsNotFound(err error) bool { return errors.Is(err, errNotFound) }
