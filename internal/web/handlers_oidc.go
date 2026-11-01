package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/zitadel/oidc/v3/pkg/oidc"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/idp"
)

// Token and revocation endpoint limits: requests per client and address,
// and failed requests per address.
const (
	tokenRequestsPerClientIP = 120 // per minute
	tokenFailuresPerIP       = 30  // per 15 minutes
	maxTokenBodyBytes        = 64 << 10
)

// oidcCSRFExempt are provider endpoints exempt from the synchroniser-token
// and cross-origin checks: relying parties call them server to server
// (token, userinfo, revoke) or send the browser to them from their own
// site (authorize, end_session), protected by client authentication,
// state, nonce, PKCE and the id_token_hint. The consent step
// (/oauth/continue) is a portal form and keeps both checks.
var oidcCSRFExempt = []string{idp.PathAuthorize, idp.PathToken, idp.PathUserinfo, idp.PathRevoke, idp.PathEndSession}

// oidcNoSession are endpoints that never use the portal session.
var oidcNoSession = []string{idp.PathDiscovery, idp.PathJWKS, idp.PathToken, idp.PathUserinfo, idp.PathRevoke}

func (s *Server) oidcEnabled() bool { return s.opt.OIDC != nil }

func (s *Server) csrfExempt(path string) bool {
	return s.oidcEnabled() && slices.Contains(oidcCSRFExempt, path)
}

// oidcRoutes registers the provider endpoints (only when a provider is
// configured).
func (s *Server) oidcRoutes(r *mux.Router) {
	p := s.opt.OIDC
	r.HandleFunc(idp.PathDiscovery, p.Discovery).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(idp.PathJWKS, p.JWKS).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(idp.PathAuthorize, s.handleOIDCAuthorize).Methods(http.MethodGet, http.MethodPost)
	r.HandleFunc(idp.PathContinue, s.handleOIDCContinue).Methods(http.MethodGet, http.MethodPost)
	r.HandleFunc(idp.PathToken, s.limitToken(p.Token)).Methods(http.MethodPost)
	r.HandleFunc(idp.PathUserinfo, p.Userinfo).Methods(http.MethodGet, http.MethodPost)
	r.HandleFunc(idp.PathRevoke, s.limitToken(p.Revoke)).Methods(http.MethodPost)
	r.HandleFunc(idp.PathEndSession, s.handleOIDCEndSession).Methods(http.MethodGet, http.MethodPost)
}

// sessionHash returns the hash of the browser's session cookie when it
// identifies a signed-in principal, else "".
func (s *Server) sessionHash(r *http.Request) string {
	if principalFrom(r.Context()) == nil {
		return ""
	}
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		return auth.HashToken(c.Value)
	}
	return ""
}

// handleOIDCAuthorize is /oauth/authorize. Without a portal session the
// browser is sent to the login page, which brings it back to the same
// authorize URL; prompt=none instead lets the provider answer
// login_required. prompt=login, or a session older than max_age, signs
// the portal session out first and asks for a new sign-in.
func (s *Server) handleOIDCAuthorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	params := url.Values{}
	for k, v := range r.Form { // query and (for POST) body
		params[k] = v
	}
	prompts := strings.Fields(params.Get("prompt"))
	none := slices.Contains(prompts, oidc.PromptNone)
	hash := s.sessionHash(r)

	if hash != "" && (slices.Contains(prompts, oidc.PromptLogin) || s.sessionTooOld(r.Context(), hash, params.Get("max_age"))) {
		if none {
			hash = "" // cannot re-authenticate without interaction: login_required
		} else {
			if c, err := r.Cookie(sessionCookie); err == nil {
				if err := s.opt.Auth.DeleteSession(r.Context(), c.Value); err != nil {
					s.serverError(w, r, err)
					return
				}
			}
			s.setSessionCookie(w, "", 0)
			prompts = slices.DeleteFunc(prompts, func(p string) bool { return p == oidc.PromptLogin })
			if len(prompts) == 0 {
				params.Del("prompt")
			} else {
				params.Set("prompt", strings.Join(prompts, " "))
			}
			s.redirectToLogin(w, r, params)
			return
		}
	}
	if hash == "" && !none {
		s.redirectToLogin(w, r, params)
		return
	}
	s.opt.OIDC.Authorize(w, r.WithContext(idp.WithSession(r.Context(), hash, nil)))
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request, params url.Values) {
	w.Header().Set("Cache-Control", "no-store")
	next := idp.PathAuthorize + "?" + params.Encode()
	http.Redirect(w, r, s.url("/login?"+url.Values{"next": {next}}.Encode()), http.StatusSeeOther)
}

// sessionTooOld reports whether max_age (seconds) is set and the session
// signed in longer ago than that.
func (s *Server) sessionTooOld(ctx context.Context, hash, maxAge string) bool {
	if maxAge == "" {
		return false
	}
	secs, err := strconv.ParseUint(maxAge, 10, 32)
	if err != nil {
		return false // the provider rejects it
	}
	at, err := s.opt.OIDC.Storage().SessionAuthTime(ctx, hash)
	if err != nil {
		return true
	}
	return s.opt.Now().Sub(at) > time.Duration(secs)*time.Second
}

type consentData struct {
	ID         string
	ClientName string
	Scopes     []string // human descriptions
}

var scopeDescriptions = map[string]string{
	oidc.ScopeOpenID:        "Know that you are signed in to the Clidive portal (your portal user number)",
	oidc.ScopeEmail:         "Your sign-in email address",
	oidc.ScopeProfile:       "Your name",
	idp.ScopeClidive:        "Your membership number, type, status and expiry date, BSAC number and portal roles",
	oidc.ScopeOfflineAccess: "Keep you signed in for up to 30 days",
}

// handleOIDCContinue is the step between /oauth/authorize and the client:
// it checks that the request belongs to this browser's session and that
// the user may sign in, asks for consent when the client is not first
// party (remembered per user and client), then issues the code.
func (s *Server) handleOIDCContinue(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p, st := s.opt.OIDC, s.opt.OIDC.Storage()
	a, err := st.AuthRequest(r.Context(), r.FormValue("id"))
	if idp.IsNotFound(err) {
		s.renderError(w, r, http.StatusBadRequest, "This sign-in request has expired or is not valid. Please go back to the website and sign in again.")
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	pr := principalFrom(r.Context())
	hash := s.sessionHash(r)
	if a.UserID == 0 || pr == nil || hash == "" || a.SessionHash != hash || pr.UserID != a.UserID {
		if a.UserID == 0 && a.HasPrompt(oidc.PromptNone) {
			p.Fail(w, r, a, oidc.ErrLoginRequired())
			return
		}
		s.renderError(w, r, http.StatusBadRequest, "This sign-in request belongs to another browser session. Please go back to the website and sign in again.")
		return
	}
	client, err := p.Client(r.Context(), a.ClientID)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "This website is no longer allowed to use the Clidive sign-in.")
		return
	}
	if _, err := st.Allowed(r.Context(), a.UserID, a.AuthMethod); idp.IsNotAllowed(err) {
		s.opt.Log.Info("oidc: sign-in refused", "user_id", a.UserID, "client_id", a.ClientID)
		p.Fail(w, r, a, oidc.ErrAccessDenied().WithDescription("this account may not sign in to this site"))
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !client.FirstParty {
		ok, err := st.HasConsent(r.Context(), a.UserID, a.ClientID, a.Scopes)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		switch {
		case ok:
		case a.HasPrompt(oidc.PromptNone):
			p.Fail(w, r, a, idp.ErrConsentRequired())
			return
		case r.Method != http.MethodPost:
			d := consentData{ID: a.ID, ClientName: client.Name}
			for _, sc := range a.Scopes {
				if desc, ok := scopeDescriptions[sc]; ok {
					d.Scopes = append(d.Scopes, desc)
				}
			}
			s.allowFormRedirect(r.Context(), w, a.ClientID, a.RedirectURI)
			s.render(w, r, http.StatusOK, "consent", "Allow "+client.Name+"?", d)
			return
		case r.PostForm.Get("decision") == "allow":
			if err := st.SaveConsent(r.Context(), a.UserID, a.ClientID, a.Scopes); err != nil {
				s.serverError(w, r, err)
				return
			}
		default:
			p.Fail(w, r, a, oidc.ErrAccessDenied().WithDescription("the user declined"))
			return
		}
	}
	if err := st.Approve(r.Context(), a); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.opt.Log.Info("oidc: authorized", "user_id", a.UserID, "client_id", a.ClientID)
	p.Respond(w, r, a)
}

// allowFormRedirect widens form-action so that a form on this page may end
// in a redirect to the client's (registered) redirect URI: browsers apply
// form-action to the redirects that follow a form submission.
func (s *Server) allowFormRedirect(ctx context.Context, w http.ResponseWriter, clientID, redirectURI string) {
	origin, ok := s.opt.OIDC.RedirectOrigin(ctx, clientID, redirectURI)
	if !ok {
		return
	}
	w.Header().Set("Content-Security-Policy", strings.Replace(contentSecurityPolicy, "form-action 'self'", "form-action 'self' "+origin, 1))
}

// allowLoginRedirect applies allowFormRedirect for a login page whose
// next is an authorize URL.
func (s *Server) allowLoginRedirect(ctx context.Context, w http.ResponseWriter, next string) {
	if !s.oidcEnabled() || next == "" {
		return
	}
	u, err := url.Parse(next)
	if err != nil || u.Path != idp.PathAuthorize {
		return
	}
	q := u.Query()
	s.allowFormRedirect(ctx, w, q.Get("client_id"), q.Get("redirect_uri"))
}

// handleOIDCEndSession is /oauth/end_session (RP-initiated logout). With
// a valid id_token_hint for the signed-in user it ends the portal session
// and revokes the client's tokens, then redirects to the registered
// post_logout_redirect_uri. Without a hint, a signed-in user is asked to
// confirm (a cross-site link alone never signs anyone out).
func (s *Server) handleOIDCEndSession(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	hash := s.sessionHash(r)
	if r.Form.Get("id_token_hint") == "" && hash != "" {
		w.Header().Set("Cache-Control", "no-store")
		s.render(w, r, http.StatusOK, "endsession", "Sign out", nil)
		return
	}
	var end func(context.Context) error
	if c, err := r.Cookie(sessionCookie); err == nil && hash != "" {
		end = func(ctx context.Context) error {
			if err := s.opt.Auth.Logout(ctx, c.Value); err != nil {
				return err
			}
			s.setSessionCookie(w, "", 0)
			return nil
		}
	}
	s.opt.OIDC.EndSession(w, r.WithContext(idp.WithSession(r.Context(), hash, end)))
}

// statusWriter records the status code.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// limitToken rate-limits the token and revocation endpoints: every
// request counts against the client and address, failed ones against the
// address.
func (s *Server) limitToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxTokenBodyBytes)
		if err := r.ParseForm(); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "cannot parse the request body")
			return
		}
		clientID := r.PostForm.Get("client_id")
		if id, _, ok := r.BasicAuth(); ok {
			clientID, _ = url.QueryUnescape(id)
		}
		now := s.opt.Now()
		ipKey := "ip:" + clientIP(r)
		clientKey := "client:" + clientID + "|" + ipKey
		if s.tokenByClient.Blocked(now, clientKey) || s.tokenFailures.Blocked(now, ipKey) {
			s.opt.Log.Warn("oidc: token endpoint rate limited", "ip", clientIP(r), "path", r.URL.Path)
			w.Header().Set("Retry-After", "60")
			writeOAuthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many requests, try again later")
			return
		}
		s.tokenByClient.Fail(now, clientKey)
		sw := &statusWriter{ResponseWriter: w}
		next(sw, r)
		if sw.status >= 400 {
			s.tokenFailures.Fail(now, ipKey)
		}
	}
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write([]byte(`{"error":"` + code + `","error_description":"` + desc + `"}` + "\n"))
}
