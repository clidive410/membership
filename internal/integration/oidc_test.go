package integration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/idp"
	"github.com/clidive410/membership/internal/members"
	"github.com/clidive410/membership/internal/web"
)

const (
	rpRedirect = "https://rp.example.org/wp-admin/admin-ajax.php?action=openid-connect-authorize"
	rpHost     = "rp.example.org"
	testSecret = "integration-test-session-secret-0123456789"
)

var cliActor = members.Actor{Label: "cli"}

type oidcEnv struct {
	f        *fixture
	ctx      context.Context
	ts       *httptest.Server
	base     string
	prov     *idp.Provider
	as       *auth.Store
	ms       *members.Store
	clientID string
	secret   string
}

func newOIDCEnv(t *testing.T, firstParty bool) *oidcEnv {
	t.Helper()
	f := setup(t)
	f.importOnce(t)
	e := &oidcEnv{f: f, ctx: f.ctx, as: auth.NewStore(f.app), ms: members.NewStore(f.app), clientID: "wordpress"}
	e.ts = httptest.NewUnstartedServer(nil)
	e.base = "http://" + e.ts.Listener.Addr().String()
	var err error
	e.prov, err = idp.New(idp.Config{DB: f.app, Auth: e.as, SessionSecret: []byte(testSecret), Issuer: e.base})
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := url.Parse(e.base)
	srv := web.New(web.Options{Members: e.ms, Auth: e.as, SessionSecret: []byte(testSecret), PublicURL: pub, OIDC: e.prov})
	e.ts.Config.Handler = srv.Handler()
	e.ts.Start()
	t.Cleanup(e.ts.Close)
	e.secret, err = e.prov.Clients().Create(e.ctx, cliActor, idp.ClientSpec{ID: e.clientID, Name: "Clidive website",
		RedirectURIs: []string{rpRedirect}, FirstParty: firstParty})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// browser is a cookie-keeping client that does not follow redirects.
type browser struct {
	c *http.Client
	e *oidcEnv
}

func (e *oidcEnv) browser(t *testing.T) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{e: e, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// signIn gives the browser a portal session of userID started with method.
func (b *browser) signIn(t *testing.T, userID int64, method string) string {
	t.Helper()
	tok, err := b.e.as.CreateSession(b.e.ctx, userID, method, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(b.e.base)
	b.c.Jar.SetCookies(u, []*http.Cookie{{Name: "clidive_session", Value: tok, Path: "/"}})
	return tok
}

func (b *browser) get(t *testing.T, u string) *http.Response {
	t.Helper()
	if strings.HasPrefix(u, "/") {
		u = b.e.base + u
	}
	resp, err := b.c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (b *browser) post(t *testing.T, path string, form url.Values) *http.Response {
	t.Helper()
	resp, err := b.c.PostForm(b.e.base+path, form)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

var csrfRE = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b)
}

// pkce returns a verifier and its S256 challenge.
func pkce() (string, string) {
	v := auth.NewToken() + auth.NewToken()
	sum := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(sum[:])
}

type authz struct {
	verifier, state, nonce string
	params                 url.Values
}

func (e *oidcEnv) newAuthz(scope string) *authz {
	v, c := pkce()
	a := &authz{verifier: v, state: "st-" + auth.NewToken()[:10], nonce: "n-" + auth.NewToken()[:10]}
	a.params = url.Values{"client_id": {e.clientID}, "redirect_uri": {rpRedirect}, "response_type": {"code"}, "scope": {scope},
		"state": {a.state}, "nonce": {a.nonce}, "code_challenge": {c}, "code_challenge_method": {"S256"}}
	return a
}

const fullScope = "openid email profile clidive offline_access"

// follow follows portal redirects until the browser is sent to the
// relying party (returned) or gets a non-redirect answer (returned as resp).
func (b *browser) follow(t *testing.T, resp *http.Response) (*url.URL, *http.Response) {
	t.Helper()
	for range 10 {
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return nil, resp
		}
		loc, err := resp.Location()
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if loc.Host == rpHost {
			return loc, nil
		}
		resp = b.get(t, loc.String())
	}
	t.Fatal("too many redirects")
	return nil, nil
}

// code runs /oauth/authorize for a signed-in browser and returns the code.
func (b *browser) code(t *testing.T, a *authz) string {
	t.Helper()
	cb, resp := b.follow(t, b.get(t, idp.PathAuthorize+"?"+a.params.Encode()))
	if cb == nil {
		t.Fatalf("authorize ended at %d: %s", resp.StatusCode, body(t, resp))
	}
	q := cb.Query()
	if q.Get("state") != a.state || q.Get("code") == "" || q.Get("error") != "" {
		t.Fatalf("callback %v", q)
	}
	return q.Get("code")
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
}

func (e *oidcEnv) token(t *testing.T, form url.Values) (int, tokenResp) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.base+idp.PathToken, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(e.clientID, e.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var tr tokenResp
	json.Unmarshal([]byte(body(t, resp)), &tr)
	return resp.StatusCode, tr
}

func (e *oidcEnv) exchange(t *testing.T, code, verifier string) (int, tokenResp) {
	return e.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {rpRedirect}, "code_verifier": {verifier}})
}

func (e *oidcEnv) refresh(t *testing.T, rt string) (int, tokenResp) {
	return e.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}})
}

func (e *oidcEnv) userinfo(t *testing.T, at string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.base+idp.PathUserinfo, nil)
	req.Header.Set("Authorization", "Bearer "+at)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	json.Unmarshal([]byte(body(t, resp)), &m)
	return resp.StatusCode, m
}

// idClaims verifies an ID token against the JWKS and returns its claims and kid.
func (e *oidcEnv) idClaims(t *testing.T, raw string) (map[string]any, string) {
	t.Helper()
	resp, err := http.Get(e.base + idp.PathJWKS)
	if err != nil {
		t.Fatal(err)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal([]byte(body(t, resp)), &set); err != nil {
		t.Fatal(err)
	}
	jws, err := jose.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatal(err)
	}
	kid := jws.Signatures[0].Header.KeyID
	keys := set.Key(kid)
	if len(keys) != 1 {
		t.Fatalf("kid %s not in JWKS (%d keys)", kid, len(set.Keys))
	}
	payload, err := jws.Verify(keys[0])
	if err != nil {
		t.Fatalf("ID token signature: %v", err)
	}
	m := map[string]any{}
	json.Unmarshal(payload, &m)
	return m, kid
}

func (e *oidcEnv) passwordUser(t *testing.T, email, password string, memberID int64, roles ...auth.Role) int64 {
	t.Helper()
	uid, err := e.as.EnsureUser(e.ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	if password != "" {
		if err := e.as.SetPassword(e.ctx, uid, password); err != nil {
			t.Fatal(err)
		}
	}
	if memberID != 0 {
		if err := e.as.LinkMember(e.ctx, uid, memberID); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range roles {
		if err := e.as.GrantRole(e.ctx, uid, r); err != nil {
			t.Fatal(err)
		}
	}
	return uid
}

func (e *oidcEnv) count(t *testing.T, q string, args ...any) (n int) {
	t.Helper()
	if err := e.f.app.QueryRowContext(e.ctx, q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestOIDCCodeFlow: authorize -> login -> code -> token -> userinfo, then
// the failure and revocation cases.
func TestOIDCCodeFlow(t *testing.T) {
	e := newOIDCEnv(t, true)
	const pw = "a-long-test-password"
	alice := e.passwordUser(t, "alice@example.org", pw, 101, auth.RoleAdmin, auth.RoleMember)

	// Discovery advertises only what is supported.
	resp, _ := http.Get(e.base + idp.PathDiscovery)
	var disc map[string]any
	json.Unmarshal([]byte(body(t, resp)), &disc)
	if disc["issuer"] != e.base || disc["token_endpoint"] != e.base+"/oauth/token" || disc["jwks_uri"] != e.base+"/oauth/jwks" ||
		disc["end_session_endpoint"] != e.base+"/oauth/end_session" || disc["revocation_endpoint"] != e.base+"/oauth/revoke" {
		t.Errorf("discovery endpoints: %v", disc)
	}
	if rt, _ := json.Marshal(disc["response_types_supported"]); string(rt) != `["code"]` {
		t.Errorf("response types: %s", rt)
	}
	if cm, _ := json.Marshal(disc["code_challenge_methods_supported"]); string(cm) != `["S256"]` {
		t.Errorf("PKCE methods: %s", cm)
	}
	if _, ok := disc["introspection_endpoint"]; ok {
		t.Error("introspection must not be advertised")
	}
	if n := e.count(t, "SELECT COUNT(*) FROM oidc_keys"); n != 0 {
		t.Errorf("no key may be generated before the first token: %d", n)
	}

	b := e.browser(t)
	a := e.newAuthz(fullScope)
	resp = b.get(t, idp.PathAuthorize+"?"+a.params.Encode())
	loc, _ := resp.Location()
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || loc.Path != "/login" || !strings.HasPrefix(loc.Query().Get("next"), "/oauth/authorize?") {
		t.Fatalf("unsigned authorize: %d %v", resp.StatusCode, loc)
	}
	next := loc.Query().Get("next")
	login := b.get(t, loc.String())
	page := body(t, login)
	if !strings.Contains(login.Header.Get("Content-Security-Policy"), "form-action 'self' https://rp.example.org") {
		t.Errorf("login page CSP must allow the redirect to the client: %q", login.Header.Get("Content-Security-Policy"))
	}
	m := csrfRE.FindStringSubmatch(page)
	if m == nil || !strings.Contains(page, `name="next" value="/oauth/authorize?`) {
		t.Fatal("login page lacks the CSRF token or next")
	}
	resp = b.post(t, "/login", url.Values{"email": {"alice@example.org"}, "password": {pw}, "next": {next}, "csrf_token": {m[1]}})
	cb, end := b.follow(t, resp)
	if cb == nil {
		t.Fatalf("after login: %d %s", end.StatusCode, body(t, end))
	}
	if cb.Query().Get("state") != a.state || cb.Query().Get("code") == "" {
		t.Fatalf("callback: %v", cb)
	}
	code := cb.Query().Get("code")
	if n := e.count(t, "SELECT COUNT(*) FROM oidc_authorizations WHERE code_hash = ?", auth.HashToken(code)); n != 1 {
		t.Error("only the hash of the code may be stored")
	}

	// Wrong verifier, then the right one: the code was spent by the first try.
	if st, tr := e.exchange(t, code, "wrong-"+a.verifier); st != 400 || tr.Error != "invalid_grant" {
		t.Errorf("wrong verifier: %d %+v", st, tr)
	}
	if st, _ := e.exchange(t, code, a.verifier); st != 400 {
		t.Errorf("a code used with a wrong verifier must be spent: %d", st)
	}

	// A fresh code: the browser is signed in now, no login.
	a = e.newAuthz(fullScope)
	code = b.code(t, a)
	st, tr := e.exchange(t, code, a.verifier)
	if st != 200 || tr.AccessToken == "" || tr.IDToken == "" || tr.RefreshToken == "" || tr.ExpiresIn < 590 || tr.ExpiresIn > 600 {
		t.Fatalf("exchange: %d %+v", st, tr.Error)
	}
	for _, h := range []string{auth.HashToken(tr.RefreshToken)} {
		if n := e.count(t, "SELECT COUNT(*) FROM oidc_tokens WHERE token_hash = ? AND kind = 'refresh'", h); n != 1 {
			t.Error("refresh tokens must be stored hashed")
		}
	}
	if n := e.count(t, "SELECT COUNT(*) FROM oidc_tokens WHERE token_hash IN (?, ?)", tr.RefreshToken, tr.AccessToken); n != 0 {
		t.Error("a raw token value is stored")
	}
	claims, _ := e.idClaims(t, tr.IDToken)
	sub := idp.Subject(alice)
	if claims["iss"] != e.base || claims["sub"] != sub || claims["nonce"] != a.nonce || claims["aud"] == nil ||
		claims["email"] != "alice@example.org" || claims["email_verified"] != true || claims["name"] != "Alice Example" ||
		claims["given_name"] != "Alice" || claims["family_name"] != "Example" || claims["member_id"] != float64(101) {
		t.Errorf("ID token claims: %v", claims)
	}
	if exp, iat := claims["exp"].(float64), claims["iat"].(float64); exp-iat > 600 || exp-iat < 590 {
		t.Errorf("ID token lifetime %v", exp-iat)
	}
	if roles, _ := json.Marshal(claims["roles"]); string(roles) != `["admin","member"]` {
		t.Errorf("password session roles: %s", roles)
	}
	st, info := e.userinfo(t, tr.AccessToken)
	if st != 200 || info["sub"] != sub || info["membership_status"] == nil || info["email"] != "alice@example.org" || info["bsac_member_id"] != "X0001" {
		t.Errorf("userinfo: %d %v", st, info)
	}
	if n := e.count(t, "SELECT COUNT(*) FROM audit_log WHERE action = ? AND actor_user_id = ? AND changes LIKE '%wordpress%'", idp.ActionAuthorize, alice); n != 2 {
		t.Errorf("authorize audit entries: %d", n)
	}
	if n := e.count(t, "SELECT COUNT(*) FROM audit_log WHERE action = ? AND actor_user_id = ?", idp.ActionToken, alice); n != 1 {
		t.Errorf("token audit entries: %d", n)
	}

	// Code reuse is refused and revokes what the code produced.
	if st, tr2 := e.exchange(t, code, a.verifier); st != 400 || tr2.Error != "invalid_grant" {
		t.Errorf("code reuse: %d %+v", st, tr2)
	}
	if st, _ := e.userinfo(t, tr.AccessToken); st == 200 {
		t.Error("tokens issued from a reused code must be revoked")
	}
	if st, _ := e.refresh(t, tr.RefreshToken); st == 200 {
		t.Error("refresh token from a reused code must be revoked")
	}

	// Refresh rotation.
	a = e.newAuthz(fullScope)
	_, tr = e.exchange(t, b.code(t, a), a.verifier)
	st, tr2 := e.refresh(t, tr.RefreshToken)
	if st != 200 || tr2.RefreshToken == "" || tr2.RefreshToken == tr.RefreshToken || tr2.IDToken == "" {
		t.Fatalf("refresh: %d %+v", st, tr2.Error)
	}
	if c2, _ := e.idClaims(t, tr2.IDToken); c2["sub"] != sub || c2["membership_status"] == nil {
		t.Errorf("refreshed ID token: %v", c2)
	}
	if st, _ := e.refresh(t, tr.RefreshToken); st != 400 {
		t.Errorf("a rotated refresh token must be refused: %d", st)
	}
	if st, _ := e.refresh(t, tr2.RefreshToken); st != 400 {
		t.Errorf("reusing a rotated refresh token must revoke the grant: %d", st)
	}

	// Revocation endpoint.
	a = e.newAuthz(fullScope)
	_, tr = e.exchange(t, b.code(t, a), a.verifier)
	req, _ := http.NewRequest("POST", e.base+idp.PathRevoke, strings.NewReader(url.Values{"token": {tr.RefreshToken}, "token_type_hint": {"refresh_token"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(e.clientID, e.secret)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("revoke: %v %v", resp, err)
	}
	if st, _ := e.refresh(t, tr.RefreshToken); st != 400 {
		t.Error("revoked refresh token still works")
	}
	if st, _ := e.userinfo(t, tr.AccessToken); st == 200 {
		t.Error("revoking the refresh token must revoke the grant's access token")
	}

	// Portal logout revokes the tokens issued during the session.
	a = e.newAuthz(fullScope)
	_, tr = e.exchange(t, b.code(t, a), a.verifier)
	page = body(t, b.get(t, "/me"))
	m = csrfRE.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no CSRF token on /me")
	}
	if resp := b.post(t, "/logout", url.Values{"csrf_token": {m[1]}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if st, _ := e.refresh(t, tr.RefreshToken); st != 400 {
		t.Error("refresh token survived portal logout")
	}
	if st, _ := e.userinfo(t, tr.AccessToken); st == 200 {
		t.Error("access token survived portal logout")
	}

	// Disabling the user (access none) revokes everything.
	b.signIn(t, alice, auth.LoginMethodPassword)
	a = e.newAuthz(fullScope)
	_, tr = e.exchange(t, b.code(t, a), a.verifier)
	if _, err := e.as.SetAccess(e.ctx, cliActor, auth.AccessRequest{Email: "alice@example.org", Access: auth.AccessNone}); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.refresh(t, tr.RefreshToken); st != 400 {
		t.Error("refresh token survived access none")
	}
	if n := e.count(t, "SELECT COUNT(*) FROM oidc_tokens WHERE user_id = ?", alice); n != 0 {
		t.Errorf("tokens left after access none: %d", n)
	}
}

// TestOIDCRefusalsAndChecks covers the request checks, refused users and
// end_session.
func TestOIDCRefusalsAndChecks(t *testing.T) {
	e := newOIDCEnv(t, true)
	b := e.browser(t)
	cara := e.passwordUser(t, "cara@example.org", "", 103, auth.RoleMember)
	b.signIn(t, cara, auth.LoginMethodMagicLink)

	cases := map[string]func(v url.Values){
		"no PKCE":        func(v url.Values) { v.Del("code_challenge"); v.Del("code_challenge_method") },
		"plain PKCE":     func(v url.Values) { v.Set("code_challenge_method", "plain") },
		"no state":       func(v url.Values) { v.Del("state") },
		"no nonce":       func(v url.Values) { v.Del("nonce") },
		"implicit":       func(v url.Values) { v.Set("response_type", "id_token token") },
		"hybrid":         func(v url.Values) { v.Set("response_type", "code id_token") },
		"form_post":      func(v url.Values) { v.Set("response_mode", "form_post") },
		"no openid":      func(v url.Values) { v.Set("scope", "email profile") },
		"other redirect": func(v url.Values) { v.Set("redirect_uri", "https://rp.example.org/elsewhere") },
		"unknown client": func(v url.Values) { v.Set("client_id", "nobody") },
	}
	for name, mod := range cases {
		a := e.newAuthz(fullScope)
		mod(a.params)
		resp := b.get(t, idp.PathAuthorize+"?"+a.params.Encode())
		cb, end := b.follow(t, resp)
		if cb != nil && cb.Fragment != "" { // errors for implicit-style response types go in the fragment
			cb.RawQuery = cb.Fragment
		}
		switch {
		case cb != nil && cb.Query().Get("code") != "":
			t.Errorf("%s: a code was issued", name)
		case cb != nil && cb.Query().Get("error") == "":
			t.Errorf("%s: redirect without error: %v", name, cb)
		case cb == nil && end.StatusCode != 400:
			t.Errorf("%s: %d", name, end.StatusCode)
		}
		if end != nil {
			end.Body.Close()
		}
		if (name == "other redirect" || name == "unknown client") && cb != nil {
			t.Errorf("%s: must not redirect to an unregistered URI", name)
		}
	}

	// prompt=none without a session: login_required at the client.
	anon := e.browser(t)
	a := e.newAuthz(fullScope)
	a.params.Set("prompt", "none")
	if cb, _ := anon.follow(t, anon.get(t, idp.PathAuthorize+"?"+a.params.Encode())); cb == nil || cb.Query().Get("error") != "login_required" {
		t.Errorf("prompt=none: %v", cb)
	}

	// A member linked to an archived record is refused.
	barry := e.passwordUser(t, "barry@example.org", "", 102, auth.RoleMember)
	bb := e.browser(t)
	bb.signIn(t, barry, auth.LoginMethodMagicLink)
	a = e.newAuthz(fullScope)
	if cb, _ := bb.follow(t, bb.get(t, idp.PathAuthorize+"?"+a.params.Encode())); cb == nil || cb.Query().Get("error") != "access_denied" || cb.Query().Get("state") != a.state {
		t.Errorf("archived member: %v", cb)
	}

	// A request started in one session cannot be completed in another.
	a = e.newAuthz(fullScope)
	resp := b.get(t, idp.PathAuthorize+"?"+a.params.Encode())
	cont, _ := resp.Location()
	resp.Body.Close()
	other := e.browser(t)
	other.signIn(t, cara, auth.LoginMethodMagicLink)
	if resp := other.get(t, cont.String()); resp.StatusCode != 400 {
		t.Errorf("continue from another session: %d", resp.StatusCode)
	}

	// end_session with an id_token_hint signs the portal out and revokes.
	a = e.newAuthz(fullScope)
	_, tr := e.exchange(t, b.code(t, a), a.verifier)
	q := url.Values{"id_token_hint": {tr.IDToken}, "post_logout_redirect_uri": {"https://rp.example.org/"}, "state": {"bye"}}
	resp = b.get(t, idp.PathEndSession+"?"+q.Encode())
	loc, _ := resp.Location()
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || loc == nil || loc.Host != rpHost || loc.Query().Get("state") != "bye" {
		t.Fatalf("end_session: %d %v", resp.StatusCode, loc)
	}
	if st, _ := e.refresh(t, tr.RefreshToken); st != 400 {
		t.Error("refresh token survived end_session")
	}
	if resp := b.get(t, "/me"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("portal session survived end_session: %d", resp.StatusCode)
	}
	// An unregistered post-logout URI is refused.
	b.signIn(t, cara, auth.LoginMethodMagicLink)
	a = e.newAuthz(fullScope)
	_, tr = e.exchange(t, b.code(t, a), a.verifier)
	q.Set("id_token_hint", tr.IDToken)
	q.Set("post_logout_redirect_uri", "https://evil.example.net/")
	if resp := b.get(t, idp.PathEndSession+"?"+q.Encode()); resp.StatusCode == http.StatusFound {
		t.Error("end_session redirected to an unregistered URI")
	}
	// Without a hint a signed-in user is asked to confirm.
	if resp := b.get(t, idp.PathEndSession); resp.StatusCode != 200 || !strings.Contains(body(t, resp), "Sign out of the Clidive portal?") {
		t.Error("end_session without hint should ask for confirmation")
	}

	// Token endpoint: bad client secret and unsupported grants.
	req, _ := http.NewRequest("POST", e.base+idp.PathToken, strings.NewReader("grant_type=refresh_token&refresh_token="+tr.RefreshToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(e.clientID, "wrong-secret-wrong-secret-wrong-secret")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 && resp.StatusCode != 400 {
		t.Errorf("wrong client secret: %d", resp.StatusCode)
	}
	for _, g := range []string{"client_credentials", "password", "urn:ietf:params:oauth:grant-type:jwt-bearer", "urn:ietf:params:oauth:grant-type:token-exchange"} {
		if st, tr := e.token(t, url.Values{"grant_type": {g}}); st != 400 || tr.Error != "unsupported_grant_type" {
			t.Errorf("grant %s: %d %s", g, st, tr.Error)
		}
	}

	// Rate limit: failures per address.
	limited := false
	for range 40 {
		if st, _ := e.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {"nope"}}); st == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("token endpoint failures are not rate limited")
	}
}

// TestOIDCClaimsByStatus checks the clidive claims for active, expired and
// no-member users, and that link and password sessions carry the same roles.
func TestOIDCClaimsByStatus(t *testing.T) {
	e := newOIDCEnv(t, true)
	now := time.Now().UTC()
	mk := func(first, email string, expiry time.Time, typeID int64) int64 {
		m := &members.Member{FirstName: first, LastName: "Tester", Email1: email, Email1Use: true,
			TypeID: sql.NullInt64{Int64: typeID, Valid: true}, MembershipExpiry: sql.NullTime{Time: expiry, Valid: true}}
		id, err := e.ms.CreateMember(e.ctx, cliActor, m)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	activeID := mk("Ann", "ann@example.org", now.AddDate(1, 0, 0), 1)
	expiredID := mk("Exe", "exe@example.org", now.AddDate(0, -2, 0), 6)
	ann := e.passwordUser(t, "ann@example.org", "", activeID, auth.RoleMember)
	exe := e.passwordUser(t, "exe@example.org", "", expiredID, auth.RoleMember)
	ed := e.passwordUser(t, "editor@example.org", "editor-password-123", 0, auth.RoleEditor)
	alice := e.passwordUser(t, "alice@example.org", "a-long-test-password", 101, auth.RoleAdmin, auth.RoleMember, auth.RoleEditor)

	run := func(uid int64, method string) (map[string]any, map[string]any) {
		t.Helper()
		b := e.browser(t)
		b.signIn(t, uid, method)
		a := e.newAuthz(fullScope)
		st, tr := e.exchange(t, b.code(t, a), a.verifier)
		if st != 200 {
			t.Fatalf("user %d: %d %s", uid, st, tr.Error)
		}
		idc, _ := e.idClaims(t, tr.IDToken)
		_, ui := e.userinfo(t, tr.AccessToken)
		return idc, ui
	}
	check := func(name string, c map[string]any, want map[string]any) {
		t.Helper()
		for k, v := range want {
			got, _ := json.Marshal(c[k])
			exp, _ := json.Marshal(v)
			if _, present := c[k]; !present || string(got) != string(exp) {
				t.Errorf("%s: %s = %s, want %s", name, k, got, exp)
			}
		}
	}
	for _, tc := range []struct {
		name   string
		uid    int64
		method string
		want   map[string]any
		absent []string
	}{
		{"active", ann, auth.LoginMethodMagicLink, map[string]any{"sub": idp.Subject(ann), "member_id": activeID, "membership_status": "active",
			"membership_type": "Annual", "membership_expiry": now.AddDate(1, 0, 0).Format("2006-01-02"), "roles": []string{"member"},
			"name": "Ann Tester", "email_verified": true}, []string{"bsac_member_id"}},
		{"expired", exe, auth.LoginMethodMagicLink, map[string]any{"membership_status": "expired", "membership_type": "Lapsed",
			"membership_expiry": now.AddDate(0, -2, 0).Format("2006-01-02")}, nil},
		{"no member", ed, auth.LoginMethodPassword, map[string]any{"membership_status": "none", "member_id": nil, "membership_type": nil,
			"membership_expiry": nil, "roles": []string{"editor"}, "name": "editor@example.org", "email": "editor@example.org"}, []string{"bsac_member_id", "given_name"}},
		{"committee via link", alice, auth.LoginMethodMagicLink, map[string]any{"roles": []string{"admin", "editor", "member"}, "bsac_member_id": "X0001"}, nil},
		{"committee via password", alice, auth.LoginMethodPassword, map[string]any{"roles": []string{"admin", "editor", "member"}}, nil},
	} {
		idc, ui := run(tc.uid, tc.method)
		check(tc.name+" (ID token)", idc, tc.want)
		check(tc.name+" (userinfo)", ui, tc.want)
		for _, k := range tc.absent {
			if _, ok := ui[k]; ok {
				t.Errorf("%s: %s must be absent", tc.name, k)
			}
		}
	}

	// Without the clidive scope, no membership claims.
	b := e.browser(t)
	b.signIn(t, ann, auth.LoginMethodMagicLink)
	a := e.newAuthz("openid email")
	_, tr := e.exchange(t, b.code(t, a), a.verifier)
	idc, _ := e.idClaims(t, tr.IDToken)
	if _, ok := idc["membership_status"]; ok || tr.RefreshToken != "" || idc["email"] != "ann@example.org" {
		t.Errorf("openid email only: %v (refresh token issued: %v)", idc, tr.RefreshToken != "")
	}
}

// TestOIDCStaleSessionCookieCopy: a browser that still carries a session
// cookie from an earlier cookie scope (sent ahead of the live one) completes
// the code flow, and the stale copy is expired on the way.
func TestOIDCStaleSessionCookieCopy(t *testing.T) {
	e := newOIDCEnv(t, true)
	dan := e.passwordUser(t, "dan@example.org", "", 0, auth.RoleMember)
	b := e.browser(t)
	b.signIn(t, dan, auth.LoginMethodMagicLink)
	// A longer path sorts first in the Cookie header, like a stale
	// parent-domain cookie does in real browsers.
	u, _ := url.Parse(e.base)
	b.c.Jar.SetCookies(u, []*http.Cookie{{Name: "clidive_session", Value: "stale-from-old-scope", Path: "/oauth"}})
	a := e.newAuthz(fullScope)
	_, tr := e.exchange(t, b.code(t, a), a.verifier)
	if idc, _ := e.idClaims(t, tr.IDToken); idc["email"] != "dan@example.org" {
		t.Errorf("id token claims: %v", idc)
	}
}

// TestOIDCConsent: a client that is not first party asks once per user.
func TestOIDCConsent(t *testing.T) {
	e := newOIDCEnv(t, false)
	cara := e.passwordUser(t, "cara@example.org", "", 103, auth.RoleMember)
	b := e.browser(t)
	b.signIn(t, cara, auth.LoginMethodMagicLink)

	a := e.newAuthz(fullScope)
	_, resp := b.follow(t, b.get(t, idp.PathAuthorize+"?"+a.params.Encode()))
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("expected the consent page")
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "form-action 'self' https://rp.example.org") {
		t.Error("consent page CSP must allow the redirect to the client")
	}
	page := body(t, resp)
	m := csrfRE.FindStringSubmatch(page)
	idm := regexp.MustCompile(`name="id" value="([^"]+)"`).FindStringSubmatch(page)
	if m == nil || idm == nil || !strings.Contains(page, "Allow Clidive website?") {
		t.Fatal("consent page incomplete")
	}
	// The consent POST needs the CSRF token.
	if resp := b.post(t, idp.PathContinue, url.Values{"id": {idm[1]}, "decision": {"allow"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("consent without CSRF token: %d", resp.StatusCode)
	}
	cb, _ := b.follow(t, b.post(t, idp.PathContinue, url.Values{"id": {idm[1]}, "decision": {"allow"}, "csrf_token": {m[1]}}))
	if cb == nil || cb.Query().Get("code") == "" {
		t.Fatalf("after consent: %v", cb)
	}
	// Remembered.
	a = e.newAuthz(fullScope)
	if code := b.code(t, a); code == "" {
		t.Error("consent not remembered")
	}
	// Declining yields access_denied (another user).
	ann := e.passwordUser(t, "alice@example.org", "", 101, auth.RoleMember)
	b2 := e.browser(t)
	b2.signIn(t, ann, auth.LoginMethodMagicLink)
	a = e.newAuthz(fullScope)
	_, resp = b2.follow(t, b2.get(t, idp.PathAuthorize+"?"+a.params.Encode()))
	page = body(t, resp)
	m = csrfRE.FindStringSubmatch(page)
	idm = regexp.MustCompile(`name="id" value="([^"]+)"`).FindStringSubmatch(page)
	cb, _ = b2.follow(t, b2.post(t, idp.PathContinue, url.Values{"id": {idm[1]}, "decision": {"deny"}, "csrf_token": {m[1]}}))
	if cb == nil || cb.Query().Get("error") != "access_denied" {
		t.Errorf("deny: %v", cb)
	}
}

// TestOIDCKeyRotation: the old kid stays published (and verifiable) for
// 24 hours after a rotation.
func TestOIDCKeyRotation(t *testing.T) {
	e := newOIDCEnv(t, true)
	cara := e.passwordUser(t, "cara@example.org", "", 103, auth.RoleMember)
	b := e.browser(t)
	b.signIn(t, cara, auth.LoginMethodMagicLink)
	a := e.newAuthz(fullScope)
	_, tr := e.exchange(t, b.code(t, a), a.verifier)
	_, kid1 := e.idClaims(t, tr.IDToken)

	var sealed []byte
	e.f.app.QueryRowContext(e.ctx, "SELECT private_key_enc FROM oidc_keys WHERE kid = ?", kid1).Scan(&sealed)
	if len(sealed) == 0 || strings.Contains(string(sealed), "PRIVATE KEY") {
		t.Error("the private key must be stored encrypted")
	}

	// Rotate (as the CLI does) on a separate key store; the provider's
	// cache picks the new key up after a minute, so use a fresh provider.
	ks, err := idp.NewKeyStore(e.f.app, []byte(testSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	kid2, err := ks.Rotate(e.ctx)
	if err != nil || kid2 == kid1 {
		t.Fatalf("rotate: %v", err)
	}
	keys, err := ks.Published(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var kids []string
	for _, k := range keys {
		kids = append(kids, k.ID())
	}
	if !slices.Contains(kids, kid1) || !slices.Contains(kids, kid2) || len(kids) != 2 {
		t.Errorf("JWKS after rotation: %v", kids)
	}
	// The ID token signed with the old key still verifies against the JWKS.
	if c, kid := e.idClaims(t, tr.IDToken); kid != kid1 || c["sub"] != idp.Subject(cara) {
		t.Error("old ID token no longer verifies")
	}
	active, err := ks.Active(e.ctx)
	if err != nil || active.ID() != kid2 {
		t.Errorf("active key: %v", err)
	}
	// After the grace period the old key is withdrawn.
	e.f.app.ExecContext(e.ctx, "UPDATE oidc_keys SET retired_at = ? WHERE kid = ?", time.Now().UTC().Add(-25*time.Hour), kid1)
	keys, _ = ks.Published(e.ctx)
	if len(keys) != 1 || keys[0].ID() != kid2 {
		t.Errorf("JWKS after the grace period: %d keys", len(keys))
	}
	// A wrong SESSION_SECRET cannot use the key.
	bad, _ := idp.NewKeyStore(e.f.app, []byte(strings.Repeat("x", 40)), nil)
	if _, err := bad.Active(e.ctx); err == nil {
		t.Error("a different secret must not decrypt the signing key")
	}
}

// TestOIDCEnsureIdempotent covers `membership oidc client ensure`.
func TestOIDCEnsureIdempotent(t *testing.T) {
	f := setup(t)
	cs := &idp.ClientStore{DB: f.app}
	ctx := f.ctx
	secret := strings.Repeat("s", 40)
	spec := idp.ClientSpec{ID: "wordpress", Name: "Clidive website", RedirectURIs: []string{rpRedirect}, FirstParty: true, Secret: secret}
	res, err := cs.Ensure(ctx, cliActor, spec)
	if err != nil || !res.Created {
		t.Fatalf("first ensure: %+v %v", res, err)
	}
	var hash string
	f.app.QueryRowContext(ctx, "SELECT secret_hash FROM oidc_clients WHERE client_id = 'wordpress'").Scan(&hash)
	if !strings.HasPrefix(hash, "$argon2id$") || strings.Contains(hash, secret) {
		t.Error("the client secret must be stored as an argon2id hash")
	}
	res, err = cs.Ensure(ctx, cliActor, spec)
	if err != nil || res.Created || res.Updated || res.SecretChanged {
		t.Fatalf("second ensure must change nothing: %+v %v", res, err)
	}
	var h2 string
	f.app.QueryRowContext(ctx, "SELECT secret_hash FROM oidc_clients WHERE client_id = 'wordpress'").Scan(&h2)
	if h2 != hash {
		t.Error("an unchanged secret must not be re-hashed")
	}
	spec.RedirectURIs = []string{"https://www.example.org/wp-admin/admin-ajax.php?action=openid-connect-authorize"}
	if res, _ := cs.Ensure(ctx, cliActor, spec); !res.Updated || res.SecretChanged {
		t.Errorf("redirect change: %+v", res)
	}
	spec.Secret = strings.Repeat("t", 40)
	if res, _ := cs.Ensure(ctx, cliActor, spec); res.Updated || !res.SecretChanged {
		t.Errorf("secret change: %+v", res)
	}
	c, err := cs.Get(ctx, "wordpress")
	if err != nil || c.RedirectURIs[0] != spec.RedirectURIs[0] || c.PostLogoutRedirectURIs[0] != "https://www.example.org/" || !c.FirstParty {
		t.Errorf("stored client: %+v %v", c, err)
	}
	if err := cs.Disable(ctx, cliActor, "wordpress"); err != nil {
		t.Fatal(err)
	}
	if res, _ := cs.Ensure(ctx, cliActor, spec); !res.Disabled {
		t.Error("ensure must report a disabled client")
	}
	if c, _ := cs.Get(ctx, "wordpress"); !c.Disabled() {
		t.Error("ensure must not re-enable a disabled client")
	}
	var n int
	f.app.QueryRowContext(ctx, "SELECT COUNT(*) FROM oidc_clients").Scan(&n)
	if n != 1 {
		t.Errorf("clients: %d", n)
	}
	for _, bad := range []idp.ClientSpec{
		{ID: "x", RedirectURIs: []string{"http://example.org/cb"}, Secret: secret},
		{ID: "x", RedirectURIs: []string{"https://example.org/*"}, Secret: secret},
		{ID: "x", RedirectURIs: []string{"https://example.org/cb"}, Secret: "short"},
		{ID: "x", RedirectURIs: []string{"https://example.org/cb"}, Secret: secret, Scopes: []string{"admin"}},
	} {
		if _, err := cs.Ensure(ctx, cliActor, bad); err == nil {
			t.Errorf("accepted %+v", bad.RedirectURIs)
		}
	}
}

// TestEditorRoleAccess: the editor role is set by SetAccess, survives level
// changes, and is carried only by password sessions.
func TestEditorRoleAccess(t *testing.T) {
	f := setup(t)
	f.importOnce(t)
	as := auth.NewStore(f.app)
	yes, no := true, false
	res, err := as.SetAccess(f.ctx, cliActor, auth.AccessRequest{Email: "cara@example.org", Access: auth.AccessMember, Editor: &yes})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := as.GetUser(f.ctx, res.UserID)
	if !u.Editor() || u.Access() != auth.AccessMember {
		t.Fatalf("editor not granted: %+v", u.Roles)
	}
	as.SetAccess(f.ctx, cliActor, auth.AccessRequest{Email: "cara@example.org", Access: auth.AccessMembershipManager})
	if u, _ = as.GetUser(f.ctx, res.UserID); !u.Editor() || u.Access() != auth.AccessMembershipManager {
		t.Errorf("editor must survive a level change without Editor: %+v", u.Roles)
	}
	if p, _ := as.PrincipalFor(f.ctx, res.UserID, auth.LoginMethodMagicLink); !p.HasRole(auth.RoleEditor) || !p.HasRole(auth.RoleMembershipManager) {
		t.Error("link sessions carry the editor and committee roles too")
	}
	if p, _ := as.PrincipalFor(f.ctx, res.UserID, auth.LoginMethodPassword); !p.HasRole(auth.RoleEditor) || p.Can(auth.PermExport) {
		t.Error("password sessions carry editor, which grants nothing")
	}
	as.SetAccess(f.ctx, cliActor, auth.AccessRequest{Email: "cara@example.org", Access: auth.AccessMember, Editor: &no})
	if u, _ = as.GetUser(f.ctx, res.UserID); u.Editor() {
		t.Error("editor not revoked")
	}
	var changes string
	f.app.QueryRowContext(f.ctx, "SELECT changes FROM audit_log WHERE action = ? ORDER BY id DESC LIMIT 1", auth.ActionAccess).Scan(&changes)
	if !strings.Contains(changes, `"editor":["true","false"]`) {
		t.Errorf("audit: %s", changes)
	}
	if _, err := as.RequestPasswordReset(f.ctx, "editor-only@example.org", time.Hour); err == nil {
		t.Error("unknown user got a reset link")
	}
}
