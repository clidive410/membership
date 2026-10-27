package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/members"
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

func TestPaginate(t *testing.T) {
	q := url.Values{"q": {"x"}, "page": {"3"}}
	lo, hi, p := paginate("/admin/members", q, 3, 230, 50)
	if lo != 100 || hi != 150 || p.Pages != 5 || p.From != 101 || p.To != 150 {
		t.Errorf("page 3: %d %d %+v", lo, hi, p)
	}
	if string(p.Prev) != "/admin/members?page=2&q=x" || string(p.Next) != "/admin/members?page=4&q=x" {
		t.Errorf("prev/next: %q %q", p.Prev, p.Next)
	}
	if _, _, p := paginate("/a", nil, 99, 10, 50); p.Page != 1 || p.Prev != "" || p.Next != "" || p.To != 10 {
		t.Errorf("clamped: %+v", p)
	}
	if lo, hi, p := paginate("/a", nil, 1, 0, 50); lo != 0 || hi != 0 || p.From != 0 {
		t.Errorf("empty: %+v", p)
	}
	_, _, p = paginate("/a", nil, 10, 1000, 50)
	gaps := 0
	for _, l := range p.Links {
		if l.Gap {
			gaps++
		}
	}
	if gaps != 2 || len(p.Links) != 9 {
		t.Errorf("windowed links: %+v", p.Links)
	}
}

func TestMemberListPaginates(t *testing.T) {
	e := newEnv(t, "")
	for i := range 120 {
		e.fm.add(members.Member{FirstName: "Pat", LastName: fmt.Sprintf("Page%03d", i), Email1: fmt.Sprintf("p%d@example.org", i)})
	}
	tok := e.fa.session("admin@example.org")
	body := e.do(t, "GET", "/admin/members?sort=last_name&page=2", tok, nil).Body.String()
	if !strings.Contains(body, "Showing <strong>51–100</strong> of <strong>122</strong>") || !strings.Contains(body, `aria-current="page">2</a>`) {
		t.Error("page 2 not rendered")
	}
}

// TestPagesRender renders every page for each role so that template errors
// (e.g. an unknown icon) fail a test rather than production.
func TestPagesRender(t *testing.T) {
	e := newEnv(t, "")
	for _, who := range []string{"admin@example.org", "manager@example.org"} {
		tok := e.fa.session(who)
		for _, p := range []string{"/admin/members", "/admin/members?status=all", "/admin/members/1", "/admin/members/3",
			"/admin/members/new", "/admin/payments", "/admin/emails", "/admin/members/999", "/login"} {
			w := e.do(t, "GET", p, tok, nil)
			if w.Code >= 500 {
				t.Errorf("%s %s: %d", who, p, w.Code)
			}
		}
	}
	if w := e.do(t, "GET", "/admin/members", e.fa.session("ada@example.org"), nil); w.Code != 403 || !strings.Contains(w.Body.String(), "empty-title") {
		t.Errorf("member 403 page: %d", w.Code)
	}
}
