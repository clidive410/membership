package web

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/members"
)

var secret = []byte("test-secret-test-secret-test-secret!")

const browser = "test-browser-value"

var fixedNow = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)

type env struct {
	h  http.Handler
	fm *fakeMembers
	fa *fakeAuth
}

func day(y, m, d int) sql.NullTime {
	return sql.NullTime{Time: time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC), Valid: true}
}

func typ(id int64) sql.NullInt64 { return sql.NullInt64{Int64: id, Valid: true} }

// newEnv builds a server with invented members and users.
func newEnv(t *testing.T, base string) *env {
	t.Helper()
	fm, fa := newFakeMembers(), newFakeAuth()
	fm.add(members.Member{ID: 1, FirstName: "Ada", LastName: "Anchor", Email1: "ada@example.org", Email1Use: true,
		TypeID: typ(2), MembershipExpiry: day(2027, 3, 31), CommitteeMailing: true})
	fm.add(members.Member{ID: 2, FirstName: "Bob", LastName: "Buoy", Email1: "bob@example.org", Email1Use: true,
		TypeID: typ(2), MembershipExpiry: day(2025, 3, 31)})
	fm.add(members.Member{ID: 3, FirstName: "Cy", LastName: "Current", Email1: "cy@example.org", Email1Use: true,
		TypeID: typ(6), DeletedAt: day(2024, 1, 1)})
	fa.addUser("admin@example.org", "admin-password-123", 0, auth.RoleAdmin)
	fa.addUser("manager@example.org", "manager-password-1", 0, auth.RoleMembershipManager)
	fa.addUser("ada@example.org", "member-password-12", 1, auth.RoleMember)
	fa.addUser("bob@example.org", "member-password-12", 2, auth.RoleMember)
	fa.addUser("cy@example.org", "member-password-12", 3, auth.RoleMember)
	s := New(Options{Members: fm, Auth: fa, SessionSecret: secret, BasePath: base, Now: func() time.Time { return fixedNow }})
	return &env{h: s.Handler(), fm: fm, fa: fa}
}

func (e *env) do(t *testing.T, method, path, session string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		if _, ok := form["csrf_token"]; !ok {
			form.Set("csrf_token", auth.CSRF{Secret: secret}.Token(browser))
		}
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: browser})
	if session != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func TestUnauthenticatedRedirectsToLogin(t *testing.T) {
	e := newEnv(t, "")
	w := e.do(t, "GET", "/admin/members", "", nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("got %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := e.do(t, "POST", "/admin/members/1/archive", "", url.Values{}); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST: %d", w.Code)
	}
}

func TestLoginFlow(t *testing.T) {
	e := newEnv(t, "")
	w := e.do(t, "GET", "/login", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `name="csrf_token"`) {
		t.Fatalf("login form: %d", w.Code)
	}
	w = e.do(t, "POST", "/login", "", url.Values{"email": {"Admin@Example.org "}, "password": {"admin-password-123"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/members" {
		t.Fatalf("login: %d %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	var c *http.Cookie
	for _, k := range w.Result().Cookies() {
		if k.Name == sessionCookie {
			c = k
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Value == "" {
		t.Fatalf("bad session cookie: %+v", c)
	}
	if w := e.do(t, "GET", "/admin/members", c.Value, nil); w.Code != 200 {
		t.Errorf("after login: %d", w.Code)
	}
	w = e.do(t, "POST", "/logout", c.Value, url.Values{})
	if w.Code != http.StatusSeeOther {
		t.Errorf("logout: %d", w.Code)
	}
	if w := e.do(t, "GET", "/admin/members", c.Value, nil); w.Code != http.StatusSeeOther {
		t.Errorf("session still valid after logout: %d", w.Code)
	}
}

func TestLoginFailureAndRateLimit(t *testing.T) {
	e := newEnv(t, "")
	for i := 0; i < 10; i++ {
		w := e.do(t, "POST", "/login", "", url.Values{"email": {"admin@example.org"}, "password": {"wrong"}})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := e.do(t, "POST", "/login", "", url.Values{"email": {"admin@example.org"}, "password": {"admin-password-123"}})
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("expected rate limit, got %d", w.Code)
	}
}

func TestSessionCookieDomain(t *testing.T) {
	fm, fa := newFakeMembers(), newFakeAuth()
	fa.addUser("admin@example.org", "admin-password-123", 0, auth.RoleAdmin)
	s := New(Options{Members: fm, Auth: fa, SessionSecret: secret, CookieDomain: "clidive.org", CookieSecure: true})
	e := &env{h: s.Handler(), fm: fm, fa: fa}
	w := e.do(t, "POST", "/login", "", url.Values{"email": {"admin@example.org"}, "password": {"admin-password-123"}})
	sc := w.Header().Get("Set-Cookie")
	if !strings.Contains(sc, "Domain=clidive.org") || !strings.Contains(sc, "Secure") || !strings.Contains(sc, "Path=/") {
		t.Errorf("Set-Cookie = %q", sc)
	}
}

func TestCSRFRequired(t *testing.T) {
	e := newEnv(t, "")
	tok := e.fa.session("admin@example.org")
	w := e.do(t, "POST", "/admin/members/1/archive", tok, url.Values{"csrf_token": {"forged"}})
	if w.Code != http.StatusForbidden {
		t.Errorf("forged token: %d", w.Code)
	}
	if e.fm.members[1].Archived() {
		t.Error("member archived despite bad CSRF token")
	}
	// Cross-site browser request rejected by CrossOriginProtection.
	r := httptest.NewRequest("POST", "/admin/members/1/archive", strings.NewReader("csrf_token="+auth.CSRF{Secret: secret}.Token(browser)))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: browser})
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	rw := httptest.NewRecorder()
	e.h.ServeHTTP(rw, r)
	if rw.Code != http.StatusForbidden {
		t.Errorf("cross-site POST: %d", rw.Code)
	}
}

func TestMemberListFiltersAndEscaping(t *testing.T) {
	e := newEnv(t, "")
	e.fm.add(members.Member{ID: 9, FirstName: "<script>x</script>", LastName: "Zed", Email1: "z@example.org"})
	tok := e.fa.session("admin@example.org")
	body := e.do(t, "GET", "/admin/members", tok, nil).Body.String()
	if !strings.Contains(body, "Anchor") || !strings.Contains(body, "Buoy") || strings.Contains(body, "Current</a>") {
		t.Error("default list should show current, non-archived members only")
	}
	if strings.Contains(body, "<script>x") || !strings.Contains(body, "&lt;script&gt;") {
		t.Error("member names must be HTML-escaped")
	}
	if !strings.Contains(body, `data-status="expired"`) {
		t.Error("expired tag missing for a member whose expiry is in the past")
	}
	body = e.do(t, "GET", "/admin/members?status=all&q=current", tok, nil).Body.String()
	if !strings.Contains(body, "Current") || strings.Contains(body, "Anchor") {
		t.Error("search across all statuses failed")
	}
}

func TestPermissions(t *testing.T) {
	e := newEnv(t, "")
	mgr := e.fa.session("manager@example.org")
	member := e.fa.session("ada@example.org")
	cases := []struct {
		session, method, path string
		want                  int
	}{
		{mgr, "GET", "/admin/members", 200},
		{mgr, "GET", "/admin/members/1", 200},
		{mgr, "GET", "/admin/payments", 200},
		{mgr, "GET", "/admin/export.csv", 403},
		{mgr, "GET", "/admin/emails", 403},
		{mgr, "GET", "/admin/members/new", 403},
		{mgr, "POST", "/admin/members/1/archive", 403},
		{member, "GET", "/admin/members", 403},
		{member, "GET", "/admin/members/1", 403},
		{member, "POST", "/admin/payments", 403},
	}
	for _, c := range cases {
		var form url.Values
		if c.method == "POST" {
			form = url.Values{}
		}
		if w := e.do(t, c.method, c.path, c.session, form); w.Code != c.want {
			t.Errorf("%s %s as %s: got %d, want %d", c.method, c.path, c.session[:4], w.Code, c.want)
		}
	}
}

func fullForm(m map[string]string) url.Values {
	v := url.Values{}
	for k, val := range m {
		v.Set(k, val)
	}
	return v
}

func TestManagerCanOnlyEditMembershipFields(t *testing.T) {
	e := newEnv(t, "")
	mgr := e.fa.session("manager@example.org")
	w := e.do(t, "POST", "/admin/members/1", mgr, fullForm(map[string]string{
		"FirstName": "Hacked", "LastName": "Name", "Email1": "evil@example.org", "Driver": "on",
		"TypeID": "6", "MembershipExpiry": "2026-12-31", "Notes": "Renewal chased",
	}))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	m := e.fm.members[1]
	if m.FirstName != "Ada" || m.Email1 != "ada@example.org" || m.Driver {
		t.Errorf("manager changed non-membership fields: %+v", m)
	}
	if m.TypeID.Int64 != 6 || members.FormatDate(m.MembershipExpiry) != "2026-12-31" || m.Notes != "Renewal chased" {
		t.Errorf("membership fields not updated: %+v", m)
	}
	if got := e.fm.audit[len(e.fm.audit)-1]; got != "manager@example.org member.update" {
		t.Errorf("audit = %q", got)
	}
	// The form renders the personal fieldset disabled for the manager.
	body := e.do(t, "GET", "/admin/members/1", mgr, nil).Body.String()
	if !strings.Contains(body, "<fieldset disabled>") {
		t.Error("expected disabled fieldsets for the manager")
	}
}

func TestAdminCreateAndValidate(t *testing.T) {
	e := newEnv(t, "")
	tok := e.fa.session("admin@example.org")
	w := e.do(t, "POST", "/admin/members", tok, fullForm(map[string]string{"FirstName": "", "Email1": "not-an-email", "DateOfBirth": "31/12/1990"}))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid create: %d", w.Code)
	}
	for _, msg := range []string{"First name is required", "does not look like an email", "Enter a valid date"} {
		if !strings.Contains(w.Body.String(), msg) {
			t.Errorf("missing error %q", msg)
		}
	}
	w = e.do(t, "POST", "/admin/members", tok, fullForm(map[string]string{"FirstName": "Dee", "LastName": "Diver", "Email1": "ADA@example.org"}))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "already uses this primary email") {
		t.Errorf("duplicate primary email accepted: %d", w.Code)
	}
	w = e.do(t, "POST", "/admin/members", tok, fullForm(map[string]string{
		"FirstName": "Dee", "LastName": "Diver", "Email1": " Dee@Example.org", "Email1Use": "on",
		"Gender": "F", "DateOfBirth": "1990-12-31", "BSACAnnualFee": "71.50", "TypeID": "1", "Driver": "on",
	}))
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/admin/members/") {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var created *members.Member
	for _, m := range e.fm.members {
		if m.LastName == "Diver" {
			created = m
		}
	}
	if created == nil || created.Email1 != "dee@example.org" || created.BSACAnnualFee.String() != "71.50" || !created.Driver {
		t.Errorf("created member wrong: %+v", created)
	}
}

func TestArchiveRestore(t *testing.T) {
	e := newEnv(t, "")
	tok := e.fa.session("admin@example.org")
	if w := e.do(t, "POST", "/admin/members/2/archive", tok, url.Values{}); w.Code != http.StatusSeeOther {
		t.Fatalf("archive: %d", w.Code)
	}
	if !e.fm.members[2].Archived() {
		t.Error("not archived")
	}
	if w := e.do(t, "POST", "/admin/members/2/restore", tok, url.Values{}); w.Code != http.StatusSeeOther || e.fm.members[2].Archived() {
		t.Errorf("restore failed: %d", w.Code)
	}
	if w := e.do(t, "POST", "/admin/members/999/archive", tok, url.Values{}); w.Code != http.StatusNotFound {
		t.Errorf("archive unknown: %d", w.Code)
	}
}

func TestBulkPayment(t *testing.T) {
	e := newEnv(t, "")
	mgr := e.fa.session("manager@example.org")
	body := e.do(t, "GET", "/admin/payments", mgr, nil).Body.String()
	if !strings.Contains(body, `value="1" id="m1" checked`) || !strings.Contains(body, "Buoy") {
		t.Error("standing order members should be listed and pre-selected")
	}
	w := e.do(t, "POST", "/admin/payments", mgr, url.Values{
		"type": {"2"}, "PaidOn": {"2026-10-01"}, "NewExpiry": {"2027-09-30"}, "member": {"1", "2", "3", "x"},
	})
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "n=2") {
		t.Fatalf("bulk: %d %q", w.Code, w.Header().Get("Location"))
	}
	if len(e.fm.payments) != 2 || members.FormatDate(e.fm.members[2].MembershipExpiry) != "2027-09-30" {
		t.Errorf("payments %+v", e.fm.payments)
	}
	w = e.do(t, "POST", "/admin/payments", mgr, url.Values{"type": {"2"}, "PaidOn": {""}, "member": {"1"}})
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("missing date accepted: %d", w.Code)
	}
}

func TestSinglePaymentAddDelete(t *testing.T) {
	e := newEnv(t, "")
	tok := e.fa.session("admin@example.org")
	w := e.do(t, "POST", "/admin/members/1/payments", tok, url.Values{"PaidOn": {"2026-09-01"}, "Amount": {""}, "Method": {""}, "Note": {"cash at pool"}})
	if w.Code != http.StatusSeeOther || len(e.fm.payments) != 1 || e.fm.payments[0].Amount.Valid {
		t.Fatalf("add payment: %d %+v", w.Code, e.fm.payments)
	}
	if w := e.do(t, "POST", "/admin/members/1/payments", tok, url.Values{"PaidOn": {"2026-09-01"}, "Method": {"Bitcoin"}}); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("unknown method accepted: %d", w.Code)
	}
	if w := e.do(t, "POST", "/admin/members/1/payments/1/delete", tok, url.Values{}); w.Code != http.StatusSeeOther || len(e.fm.payments) != 0 {
		t.Errorf("delete payment: %d", w.Code)
	}
}

func TestExportCSV(t *testing.T) {
	e := newEnv(t, "")
	tok := e.fa.session("admin@example.org")
	w := e.do(t, "GET", "/admin/export.csv", tok, nil)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/csv") ||
		!strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("export: %d %v", w.Code, w.Header())
	}
	body := w.Body.String()
	if !strings.Contains(body, "MemberID,Firstname,Lastname") || !strings.Contains(body, "Anchor") || strings.Contains(body, "Current") {
		t.Error("current export content wrong")
	}
	if body := e.do(t, "GET", "/admin/export.csv?status=all", tok, nil).Body.String(); !strings.Contains(body, "Current") {
		t.Error("all export should include archived members")
	}
}

func TestEmailLists(t *testing.T) {
	e := newEnv(t, "")
	tok := e.fa.session("admin@example.org")
	body := e.do(t, "GET", "/admin/emails?list=committee", tok, nil).Body.String()
	if !strings.Contains(body, "ada@example.org") || strings.Contains(body, "bob@example.org") {
		t.Error("committee list wrong")
	}
	w := e.do(t, "GET", "/admin/emails.txt?list=all", tok, nil)
	if w.Body.String() != "ada@example.org\nbob@example.org\n" {
		t.Errorf("all list = %q", w.Body.String())
	}
	if w := e.do(t, "GET", "/admin/emails?list=bogus", tok, nil); w.Code != http.StatusNotFound {
		t.Errorf("bogus list: %d", w.Code)
	}
}

func TestAuthVerify(t *testing.T) {
	e := newEnv(t, "")
	cases := []struct {
		name, session, member, status string
	}{
		{"no cookie", "", "", ""},
		{"bogus cookie", "nope", "", ""},
		{"admin without member row", e.fa.session("admin@example.org"), "", ""},
		{"active member", e.fa.session("ada@example.org"), "1", "active"},
		{"expired member", e.fa.session("bob@example.org"), "1", "expired"},
		{"archived member", e.fa.session("cy@example.org"), "", ""},
	}
	for _, c := range cases {
		w := e.do(t, "GET", "/auth/verify", c.session, nil)
		if w.Code != 200 {
			t.Errorf("%s: status %d", c.name, w.Code)
		}
		if got := w.Header().Get("X-Clidive-Member"); got != c.member {
			t.Errorf("%s: X-Clidive-Member = %q, want %q", c.name, got, c.member)
		}
		if got := w.Header().Get("X-Clidive-Member-Status"); got != c.status {
			t.Errorf("%s: status header = %q, want %q", c.name, got, c.status)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: verify must not be cached", c.name)
		}
	}
}

func TestHealth(t *testing.T) {
	e := newEnv(t, "")
	if w := e.do(t, "GET", "/healthz", "", nil); w.Code != 200 {
		t.Errorf("healthz: %d", w.Code)
	}
	e.fm.pingErr = errors.New("down")
	if w := e.do(t, "GET", "/healthz", "", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("healthz when DB down: %d", w.Code)
	}
}

func TestBasePathAndHeaders(t *testing.T) {
	e := newEnv(t, "/membership")
	w := e.do(t, "GET", "/membership/admin/members", "", nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/membership/login" {
		t.Errorf("prefixed redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
	w = e.do(t, "GET", "/membership/login", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `href="/membership/static/style.css?v=`) {
		t.Errorf("prefixed login page: %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("security headers missing")
	}
	if w := e.do(t, "GET", "/membership/static/style.css", "", nil); w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "text/css") {
		t.Errorf("static css: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if w := e.do(t, "GET", "/admin/members", "", nil); w.Code != http.StatusNotFound {
		t.Errorf("unprefixed path should 404, got %d", w.Code)
	}
}
