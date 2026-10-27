package web

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/clidive410/membership/internal/auth"
)

func TestHeadAnswersLikeGet(t *testing.T) {
	e := newEnv(t, "")
	if w := e.do(t, "HEAD", "/healthz", "", nil); w.Code != 200 {
		t.Errorf("HEAD /healthz: %d", w.Code)
	}
	if w := e.do(t, "HEAD", "/admin/members", "", nil); w.Code != http.StatusSeeOther {
		t.Errorf("HEAD /admin/members unauthenticated: %d", w.Code)
	}
}

func TestRoutingEdgeCases(t *testing.T) {
	e := newEnv(t, "")
	tok := e.fa.session("admin@example.org")
	cases := []struct {
		method, path string
		want         int
	}{
		{"GET", "/", http.StatusSeeOther},
		{"GET", "/nope", http.StatusNotFound},
		{"GET", "/logout", http.StatusMethodNotAllowed},
		{"GET", "/admin/export.csv/", http.StatusNotFound},
		{"GET", "/admin/members/new", 200},
		{"GET", "/admin/members/abc", http.StatusNotFound},
		{"GET", "/admin/members/999", http.StatusNotFound},
		{"GET", "/admin/members/1", 200},
		{"GET", "/static/style.css", 200},
	}
	for _, c := range cases {
		if w := e.do(t, c.method, c.path, tok, nil); w.Code != c.want {
			t.Errorf("%s %s: got %d, want %d", c.method, c.path, w.Code, c.want)
		}
	}
	w := e.do(t, "GET", "/nope", "", nil)
	if w.Header().Get("X-Frame-Options") != "DENY" || len(w.Header().Get("X-Request-Id")) != 16 {
		t.Errorf("404s must carry security headers and a request id: %v", w.Header())
	}
}

func TestBareBasePathRedirects(t *testing.T) {
	e := newEnv(t, "/membership")
	w := e.do(t, "GET", "/membership", "", nil)
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/membership/" {
		t.Errorf("bare base: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestUndeclaredAdminRouteFailsClosed(t *testing.T) {
	s := New(Options{Members: newFakeMembers(), Auth: newFakeAuth(), SessionSecret: secret})
	r := mux.NewRouter()
	r.HandleFunc("/x", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	r.Use(s.requirePermission(map[*mux.Route]auth.Permission{}))
	req := httptest.NewRequest("GET", "/x", nil)
	req = req.WithContext(withPrincipal(req.Context(), &auth.Principal{UserID: 1, Roles: []auth.Role{auth.RoleAdmin}}))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("route without a declared permission: %d", w.Code)
	}
}

func TestClientIPTrustsOnlyConfiguredProxies(t *testing.T) {
	proxy := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	cases := []struct {
		name    string
		trusted []netip.Prefix
		remote  string
		xff     string
		want    string
	}{
		{"no proxies: header ignored", nil, "203.0.113.5:1234", "198.51.100.1", "203.0.113.5"},
		{"untrusted peer: header ignored", proxy, "203.0.113.5:1234", "198.51.100.1", "203.0.113.5"},
		{"trusted peer", proxy, "10.0.0.2:1234", "198.51.100.1", "198.51.100.1"},
		{"spoofed left entry ignored", proxy, "10.0.0.2:1234", "1.2.3.4, 198.51.100.1", "198.51.100.1"},
		{"several trusted hops", proxy, "10.0.0.2:1234", "198.51.100.1, 10.0.0.9", "198.51.100.1"},
		{"garbage stops the walk", proxy, "10.0.0.2:1234", "198.51.100.1, junk", "10.0.0.2"},
	}
	for _, c := range cases {
		s := &Server{opt: Options{TrustedProxies: c.trusted}}
		var got string
		h := s.clientIP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = clientIP(r) }))
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		r.Header.Set("X-Forwarded-For", c.xff)
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPanicIsRecovered(t *testing.T) {
	e := newEnv(t, "")
	e.fm.panicOnPing = true
	if w := e.do(t, "GET", "/healthz", "", nil); w.Code != http.StatusInternalServerError {
		t.Errorf("panic: %d", w.Code)
	}
}

func TestTimeout(t *testing.T) {
	h := timeout(10 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("timeout: %d", w.Code)
	}
}
