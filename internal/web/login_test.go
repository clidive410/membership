package web

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/mail"
)

// fakeSender records messages instead of sending them.
type fakeSender struct {
	mu   sync.Mutex
	msgs []mail.Message
	err  error
}

func (f *fakeSender) Send(_ context.Context, m mail.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, m)
	return nil
}

func (f *fakeSender) sent() []mail.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mail.Message(nil), f.msgs...)
}

func (e *env) wait(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.s.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

var linkRE = regexp.MustCompile(`https?://\S+/login/verify\?\S+`)

// requestLink asks for a sign-in link and returns the emailed URL ("" if
// no email was sent).
func (e *env) requestLink(t *testing.T, email string, extra url.Values) (*httptest.ResponseRecorder, string) {
	t.Helper()
	before := len(e.fs.sent())
	form := url.Values{"email": {email}}
	for k, v := range extra {
		form[k] = v
	}
	w := e.do(t, "POST", e.base+"/login/link", "", form)
	e.wait(t)
	msgs := e.fs.sent()
	if len(msgs) == before {
		return w, ""
	}
	return w, linkRE.FindString(msgs[len(msgs)-1].Text)
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// pathOf returns the path and query of an absolute URL.
func pathOf(t *testing.T, s string) string {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u.RequestURI()
}

func TestLoginPageOffersBothPaths(t *testing.T) {
	e := newEnv(t, "")
	body := e.do(t, "GET", "/login?next=/me", "", nil).Body.String()
	for _, want := range []string{`action="/login/link"`, "Email me a sign-in link", `action="/login"`, `type="password"`,
		"<details", `name="next" value="/me"`} {
		if !strings.Contains(body, want) {
			t.Errorf("login page lacks %q", want)
		}
	}
}

func TestLoginLinkResponseIsNeutral(t *testing.T) {
	e := newEnv(t, "")
	var bodies []string
	for _, email := range []string{"ada@example.org", "nobody@example.org", "cy@example.org" /* archived */} {
		w, link := e.requestLink(t, email, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", email, w.Code)
		}
		if (link != "") != (email == "ada@example.org") {
			t.Errorf("%s: link emailed = %v", email, link != "")
		}
		if cookieNamed(w, sessionCookie) != nil {
			t.Errorf("%s: requesting a link must not sign in", email)
		}
		bodies = append(bodies, w.Body.String())
	}
	if bodies[0] != bodies[1] || bodies[0] != bodies[2] {
		t.Error("responses differ between member, unknown and archived addresses")
	}
	if !strings.Contains(bodies[0], "If that address belongs to a current member, we've emailed a link") {
		t.Errorf("neutral message missing: %s", bodies[0])
	}
	msgs := e.fs.sent()
	if len(msgs) != 1 || msgs[0].To != "ada@example.org" || msgs[0].Subject == "" || !strings.Contains(msgs[0].HTML, "/login/verify?token=") {
		t.Errorf("messages: %+v", msgs)
	}
}

func TestLoginLinkInvalidEmail(t *testing.T) {
	e := newEnv(t, "")
	w := e.do(t, "POST", "/login/link", "", url.Values{"email": {"not an email"}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "valid email address") {
		t.Errorf("%d", w.Code)
	}
}

func TestLoginLinkFlow(t *testing.T) {
	e := newEnv(t, "", func(o *Options) {
		o.CookieDomain, o.CookieSecure = "clidive.org", true
		o.PublicURL, _ = url.Parse("https://members.clidive.org")
	})
	// Bob's membership has expired: he may still sign in.
	_, link := e.requestLink(t, " Bob@Example.org ", nil)
	if !strings.HasPrefix(link, "https://members.clidive.org/login/verify?token=") {
		t.Fatalf("link = %q", link)
	}
	w := e.do(t, "GET", pathOf(t, link), "", nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/me" {
		t.Fatalf("verify: %d %q", w.Code, w.Header().Get("Location"))
	}
	c := cookieNamed(w, sessionCookie)
	if c == nil || c.Domain != "clidive.org" || !c.Secure || !c.HttpOnly || c.MaxAge != 30*24*3600 {
		t.Fatalf("session cookie: %+v", c)
	}
	if e.fa.lastTTL != 30*24*time.Hour {
		t.Errorf("member session TTL = %v", e.fa.lastTTL)
	}

	body := e.do(t, "GET", "/me", c.Value, nil).Body.String()
	for _, want := range []string{"My membership", "Bob Buoy", "bob@example.org", `data-status="expired"`, "membership@clidive.org", "Sign out"} {
		if !strings.Contains(body, want) {
			t.Errorf("/me lacks %q", want)
		}
	}
	if strings.Contains(body, `href="/admin/members"`) {
		t.Error("members must not see admin navigation")
	}

	// Single use.
	w = e.do(t, "GET", pathOf(t, link), "", nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "expired or has already been used") || cookieNamed(w, sessionCookie) != nil {
		t.Errorf("reused link: %d", w.Code)
	}
	if w := e.do(t, "GET", "/login/verify?token=bogus", "", nil); w.Code != http.StatusBadRequest {
		t.Errorf("bogus token: %d", w.Code)
	}
	if w := e.do(t, "HEAD", pathOf(t, link), "", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("HEAD must not spend tokens: %d", w.Code)
	}

	// Members are refused the admin UI, with the styled page.
	for _, path := range []string{"/admin/members", "/admin/members/2", "/admin/payments", "/admin/export.csv", "/admin/emails"} {
		w := e.do(t, "GET", path, c.Value, nil)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Back to my membership") {
			t.Errorf("GET %s as member: %d", path, w.Code)
		}
	}
	if w := e.do(t, "GET", "/login", c.Value, nil); w.Header().Get("Location") != "/me" {
		t.Errorf("signed-in member on /login: %q", w.Header().Get("Location"))
	}
	if w := e.do(t, "GET", "/", c.Value, nil); w.Header().Get("Location") != "/me" {
		t.Errorf("member home: %q", w.Header().Get("Location"))
	}

	// Sign out.
	w = e.do(t, "POST", "/logout", c.Value, url.Values{})
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Errorf("logout: %d %q", w.Code, w.Header().Get("Set-Cookie"))
	}
	if w := e.do(t, "GET", "/me", c.Value, nil); w.Code != http.StatusSeeOther {
		t.Errorf("/me after logout: %d", w.Code)
	}
}

func TestLoginLinkArchivedBeforeUse(t *testing.T) {
	e := newEnv(t, "")
	_, link := e.requestLink(t, "ada@example.org", nil)
	e.fm.members[1].DeletedAt = day(2026, 10, 27)
	if w := e.do(t, "GET", pathOf(t, link), "", nil); w.Code != http.StatusBadRequest {
		t.Errorf("archived member signed in: %d", w.Code)
	}
}

func TestMagicLinkSessionIsMemberOnly(t *testing.T) {
	e := newEnv(t, "")
	e.fa.addUser("chair@example.org", "chair-password-12", 1, auth.RoleAdmin, auth.RoleMember)
	tok := e.fa.sessionVia("chair@example.org", auth.LoginMethodMagicLink)
	if w := e.do(t, "GET", "/admin/members", tok, nil); w.Code != http.StatusForbidden {
		t.Errorf("committee access via a link session: %d", w.Code)
	}
	if w := e.do(t, "GET", "/me", tok, nil); w.Code != 200 {
		t.Errorf("/me: %d", w.Code)
	}
}

func TestLoginNext(t *testing.T) {
	ret, _ := url.Parse("https://www.clidive.org/")
	e := newEnv(t, "", func(o *Options) { o.LoginReturnURL = ret })
	cases := []struct{ next, want string }{
		{"", "https://www.clidive.org/"},
		{"/me?tab=1", "/me?tab=1"},
		{"https://www.clidive.org/events/dive-trip/", "https://www.clidive.org/events/dive-trip/"},
		{"https://WWW.clidive.org/x", "https://WWW.clidive.org/x"},
		{"//evil.example/", "https://www.clidive.org/"},
		{"/\\evil.example/", "https://www.clidive.org/"},
		{"https://evil.example/", "https://www.clidive.org/"},
		{"http://www.clidive.org/", "https://www.clidive.org/"},
		{"https://user@www.clidive.org/", "https://www.clidive.org/"},
		{"javascript:alert(1)", "https://www.clidive.org/"},
		{"me", "https://www.clidive.org/"},
	}
	for _, c := range cases {
		var extra url.Values
		if c.next != "" {
			extra = url.Values{"next": {c.next}}
		}
		e.fs.msgs = nil
		e.s.linkByEmail.Reset("email:ada@example.org")
		_, link := e.requestLink(t, "ada@example.org", extra)
		if link == "" {
			t.Fatalf("%q: no link", c.next)
		}
		w := e.do(t, "GET", pathOf(t, link), "", nil)
		if got := w.Header().Get("Location"); got != c.want {
			t.Errorf("next=%q: redirect %q, want %q", c.next, got, c.want)
		}
	}

	// Without LOGIN_RETURN_URL members land on /me; with a base path the
	// in-app next is prefixed.
	e = newEnv(t, "/membership")
	_, link := e.requestLink(t, "ada@example.org", url.Values{"next": {"/me"}})
	if !strings.HasPrefix(link, "http://localhost:8080/membership/login/verify?") {
		t.Fatalf("link = %q", link)
	}
	w := e.do(t, "GET", pathOf(t, link), "", nil)
	if got := w.Header().Get("Location"); got != "/membership/me" {
		t.Errorf("prefixed redirect %q", got)
	}

	// The committee password form honours next too.
	e = newEnv(t, "")
	w = e.do(t, "POST", "/login", "", url.Values{"email": {"admin@example.org"}, "password": {"admin-password-123"}, "next": {"/admin/payments"}})
	if got := w.Header().Get("Location"); got != "/admin/payments" {
		t.Errorf("password login next: %q", got)
	}
}

func TestLoginLinkRateLimits(t *testing.T) {
	e := newEnv(t, "")
	for i := 0; i < linkRequestsPerEmail; i++ {
		if w, _ := e.requestLink(t, "nobody@example.org", nil); w.Code != 200 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	if w, _ := e.requestLink(t, "NOBODY@example.org", nil); w.Code != http.StatusTooManyRequests {
		t.Errorf("per-email limit: %d", w.Code)
	}
	for i := linkRequestsPerEmail; i < linkRequestsPerIP; i++ {
		e.requestLink(t, "other"+string(rune('a'+i))+"@example.org", nil)
	}
	w, link := e.requestLink(t, "ada@example.org", nil)
	if w.Code != http.StatusTooManyRequests || link != "" {
		t.Errorf("per-IP limit: %d %q", w.Code, link)
	}
}

func TestLoginLinkNeedsCSRF(t *testing.T) {
	e := newEnv(t, "")
	w := e.do(t, "POST", "/login/link", "", url.Values{"email": {"ada@example.org"}, "csrf_token": {"forged"}})
	e.wait(t)
	if w.Code != http.StatusForbidden || len(e.fs.sent()) != 0 {
		t.Errorf("forged CSRF: %d, %d emails", w.Code, len(e.fs.sent()))
	}
}

func TestLoginLinkMailFailureStaysNeutral(t *testing.T) {
	var logs bytes.Buffer
	e := newEnv(t, "", func(o *Options) {
		o.Mailer = mail.Unconfigured{}
		o.Log = slog.New(slog.NewTextHandler(&logs, nil))
	})
	w := e.do(t, "POST", "/login/link", "", url.Values{"email": {"ada@example.org"}})
	e.wait(t)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Check your email") {
		t.Errorf("%d", w.Code)
	}
	l := logs.String()
	if !strings.Contains(l, "emailing login link failed") || !strings.Contains(l, "SMTP_HOST") {
		t.Errorf("logs: %s", l)
	}
	if strings.Contains(l, "ada@example.org") || strings.Contains(l, "token=") {
		t.Errorf("logs leak the address or token: %s", l)
	}
}

func TestMeWithoutMemberRecord(t *testing.T) {
	e := newEnv(t, "")
	if w := e.do(t, "GET", "/me", "", nil); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login?next=%2Fme" {
		t.Errorf("anonymous /me: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := e.do(t, "GET", "/me", e.fa.session("admin@example.org"), nil); w.Header().Get("Location") != "/admin/members" {
		t.Errorf("admin without member row: %d %q", w.Code, w.Header().Get("Location"))
	}
	// Committee users linked to a member see their record inside the admin layout.
	e.fa.addUser("chair@example.org", "chair-password-12", 1, auth.RoleAdmin)
	body := e.do(t, "GET", "/me", e.fa.session("chair@example.org"), nil).Body.String()
	if !strings.Contains(body, "Ada Anchor") || !strings.Contains(body, `href="/admin/members"`) || !strings.Contains(body, `aria-current="page"`) {
		t.Error("committee /me page")
	}
	// An archived member's session shows a styled refusal.
	w := e.do(t, "GET", "/me", e.fa.sessionVia("cy@example.org", auth.LoginMethodMagicLink), nil)
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "Cy") {
		t.Errorf("archived member /me: %d", w.Code)
	}
}

func TestAuthVerify(t *testing.T) {
	e := newEnv(t, "")
	e.fa.addUser("chair@example.org", "chair-password-12", 2, auth.RoleAdmin) // committee, linked to expired Bob
	cases := []struct {
		name, session, member, status, id string
	}{
		{"no cookie", "", "", "", ""},
		{"bogus cookie", "nope", "", "", ""},
		{"admin without member row", e.fa.session("admin@example.org"), "1", "active", ""},
		{"manager without member row", e.fa.session("manager@example.org"), "1", "active", ""},
		{"committee with an expired member row", e.fa.session("chair@example.org"), "1", "expired", "2"},
		{"admin via link session, no member role", e.fa.sessionVia("admin@example.org", auth.LoginMethodMagicLink), "", "", ""},
		{"active member", e.fa.sessionVia("ada@example.org", auth.LoginMethodMagicLink), "1", "active", "1"},
		{"expired member", e.fa.sessionVia("bob@example.org", auth.LoginMethodMagicLink), "1", "expired", "2"},
		{"archived member", e.fa.sessionVia("cy@example.org", auth.LoginMethodMagicLink), "", "", ""},
	}
	for _, c := range cases {
		lookups := e.fa.lookups
		r := httptest.NewRequest("GET", "/auth/verify", nil)
		// A client trying to smuggle the headers in changes nothing.
		r.Header.Set("X-Clidive-Member", "1")
		if c.session != "" {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.session})
		}
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Errorf("%s: status %d, body %q", c.name, w.Code, w.Body)
		}
		h := w.Header()
		if h.Get(headerMember) != c.member || h.Get(headerMemberStatus) != c.status || h.Get(headerMemberID) != c.id {
			t.Errorf("%s: headers %q %q %q, want %q %q %q", c.name, h.Get(headerMember), h.Get(headerMemberStatus), h.Get(headerMemberID), c.member, c.status, c.id)
		}
		if h.Get("Cache-Control") != "no-store" || h.Get("Set-Cookie") != "" || h.Get("Location") != "" {
			t.Errorf("%s: must not be cached, set cookies or redirect: %v", c.name, h)
		}
		if e.fa.lookups != lookups {
			t.Errorf("%s: the full session lookup ran; verify must use VerifySession only", c.name)
		}
	}
	if w := e.do(t, "HEAD", "/auth/verify", e.fa.sessionVia("ada@example.org", auth.LoginMethodMagicLink), nil); w.Code != 200 || w.Header().Get(headerMember) != "1" {
		t.Errorf("HEAD verify: %d", w.Code)
	}
}

func TestLoginLinkEmailTemplate(t *testing.T) {
	link := `https://members.clidive.org/login/verify?token=abc&next=%2Fme"><script>`
	m, err := loginLinkEmail.message("ada@example.org", loginLinkData{Link: link, Minutes: 15})
	if err != nil {
		t.Fatal(err)
	}
	if m.To != "ada@example.org" || m.Subject != "Your Clidive sign-in link" {
		t.Errorf("message: %+v", m)
	}
	if !strings.Contains(m.Text, link) || !strings.Contains(m.Text, "expires in 15 minutes") {
		t.Errorf("text body: %q", m.Text)
	}
	if strings.Contains(m.HTML, "<script>") || !strings.Contains(m.HTML, "token=abc&amp;next=") {
		t.Errorf("HTML body must escape the link: %q", m.HTML)
	}
}
