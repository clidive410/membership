package web

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/clidive410/membership/internal/auth"
)

const (
	sessionCookie = "clidive_session"
	csrfCookie    = "clidive_csrf"
	csrfField     = "csrf_token"
	maxFormBytes  = 1 << 20
)

type ctxKey int

const principalKey ctxKey = iota

func principalFrom(ctx context.Context) *auth.Principal {
	p, _ := ctx.Value(principalKey).(*auth.Principal)
	return p
}

// loadSession resolves the session cookie into a Principal on the context.
func (s *Server) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err == nil && c.Value != "" {
			p, err := s.opt.Auth.LookupSession(r.Context(), c.Value)
			if err != nil {
				s.opt.Log.Error("looking up session", "err", err)
			} else if p != nil {
				r = r.WithContext(context.WithValue(r.Context(), principalKey, p))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// setSessionCookie writes (or, with an empty token, clears) the session
// cookie. With COOKIE_DOMAIN set the cookie is shared with sibling hosts so
// that Caddy can forward it to /auth/verify.
func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	c := &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Domain:   s.opt.CookieDomain,
		HttpOnly: true,
		Secure:   s.opt.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.opt.SessionTTL.Seconds()),
	}
	if token == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// csrfToken returns the form token for this browser, issuing the per-browser
// CSRF cookie if needed.
func (s *Server) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && c.Value != "" {
		return s.csrf.Token(c.Value)
	}
	v := auth.NewToken()
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    v,
		Path:     s.cookiePath(),
		HttpOnly: true,
		Secure:   s.opt.CookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	// Make the new value visible to the rest of this request.
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: v})
	return s.csrf.Token(v)
}

func (s *Server) cookiePath() string {
	if s.opt.BasePath == "" {
		return "/"
	}
	return s.opt.BasePath + "/"
}

// checkCSRF rejects unsafe requests without a valid synchroniser token. It
// complements http.CrossOriginProtection (Sec-Fetch-Site / Origin checks).
func (s *Server) checkCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		c, err := r.Cookie(csrfCookie)
		if err != nil || !s.csrf.Valid(c.Value, r.PostForm.Get(csrfField)) {
			s.opt.Log.Warn("csrf check failed", "path", r.URL.Path)
			http.Error(w, "Invalid or missing form token. Please go back, reload the page and try again.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// require wraps a handler with an authorization check.
func (s *Server) require(perm auth.Permission, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		if p == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, s.url("/login"), http.StatusSeeOther)
				return
			}
			http.Error(w, "Please sign in.", http.StatusUnauthorized)
			return
		}
		if !p.Can(perm) {
			s.opt.Log.Warn("permission denied", "user_id", p.UserID, "perm", perm, "path", r.URL.Path)
			s.renderError(w, r, http.StatusForbidden, "You do not have permission to do this.")
			return
		}
		h(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logRequests logs method, path (never the query string, which may contain
// member names), status and duration.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == s.opt.BasePath+"/healthz" {
			return
		}
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		s.opt.Log.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.opt.Log.Error("panic", "value", v, "stack", string(debug.Stack()))
				http.Error(w, "Internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// limiter counts failed logins per key within a window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, fails: map[string][]time.Time{}}
}

func (l *limiter) prune(key string, now time.Time) []time.Time {
	ts := l.fails[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > l.window {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.fails, key)
	} else {
		l.fails[key] = ts
	}
	return ts
}

// Blocked reports whether any key has reached the failure limit.
func (l *limiter) Blocked(now time.Time, keys ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		if len(l.prune(k, now)) >= l.max {
			return true
		}
	}
	return false
}

// Fail records a failure for each key.
func (l *limiter) Fail(now time.Time, keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.fails) > 10000 { // bound memory under a spray of distinct keys
		l.fails = map[string][]time.Time{}
	}
	for _, k := range keys {
		l.fails[k] = append(l.prune(k, now), now)
	}
}

// Reset forgets failures for the keys (after a successful login).
func (l *limiter) Reset(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.fails, k)
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
