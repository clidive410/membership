package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/clidive410/membership/internal/auth"
)

var resetLinkRE = regexp.MustCompile(`https?://\S+/password/reset\?token=[A-Za-z0-9_-]+`)

// requestReset posts the forgotten-password form and returns the emailed
// link ("" if no email was sent).
func (e *env) requestReset(t *testing.T, email string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	before := len(e.fs.sent())
	w := e.do(t, "POST", e.base+"/password/forgot", "", url.Values{"email": {email}})
	e.wait(t)
	msgs := e.fs.sent()
	if len(msgs) == before {
		return w, ""
	}
	return w, resetLinkRE.FindString(msgs[len(msgs)-1].Text)
}

func TestLoginPageLinksToForgotPassword(t *testing.T) {
	e := newEnv(t, "")
	body := e.do(t, "GET", "/login", "", nil).Body.String()
	if !strings.Contains(body, `href="/password/forgot"`) || !strings.Contains(body, "Forgotten your password?") {
		t.Error("login page lacks the forgotten-password link")
	}
	w := e.do(t, "GET", "/password/forgot", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `action="/password/forgot"`) || !strings.Contains(w.Body.String(), `name="csrf_token"`) {
		t.Errorf("forgot form: %d", w.Code)
	}
}

func TestForgotPasswordIsNeutral(t *testing.T) {
	e := newEnv(t, "")
	e.fa.addUser("nopw@example.org", "", 0, auth.RoleMember) // member, no password: not eligible
	e.fa.addUser("chair@example.org", "", 0, auth.RoleAdmin) // committee, no password yet: eligible
	var bodies []string
	for _, c := range []struct {
		email string
		sent  bool
	}{
		{"admin@example.org", true},
		{"nobody@example.org", false},
		{"nopw@example.org", false},
		{"Chair@Example.org ", true},
	} {
		w, link := e.requestReset(t, c.email)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", c.email, w.Code)
		}
		if (link != "") != c.sent {
			t.Errorf("%s: link emailed = %v", c.email, link != "")
		}
		if cookieNamed(w, sessionCookie) != nil {
			t.Errorf("%s: must not sign in", c.email)
		}
		bodies = append(bodies, w.Body.String())
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Error("responses differ between eligible and other addresses")
		}
	}
	if !strings.Contains(bodies[0], "we've emailed a link to set a new password") {
		t.Errorf("neutral message missing: %s", bodies[0])
	}
	msgs := e.fs.sent()
	if len(msgs) != 2 || msgs[0].To != "admin@example.org" || msgs[0].Subject != "Reset your Clidive membership password" ||
		!strings.Contains(msgs[0].HTML, "/password/reset?token=") || !strings.Contains(msgs[0].Text, "expires in 30 minutes") {
		t.Errorf("messages: %+v", msgs)
	}
	if w := e.do(t, "POST", "/password/forgot", "", url.Values{"email": {"nope"}}); w.Code != http.StatusBadRequest {
		t.Errorf("invalid email: %d", w.Code)
	}
}

func TestForgotPasswordRateLimitsAndCSRF(t *testing.T) {
	e := newEnv(t, "")
	for i := 0; i < linkRequestsPerEmail; i++ {
		if w, _ := e.requestReset(t, "admin@example.org"); w.Code != 200 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	if w, link := e.requestReset(t, "ADMIN@example.org"); w.Code != http.StatusTooManyRequests || link != "" {
		t.Errorf("per-email limit: %d", w.Code)
	}
	for i := linkRequestsPerEmail; i < linkRequestsPerIP; i++ {
		e.requestReset(t, "other"+string(rune('a'+i))+"@example.org")
	}
	if w, link := e.requestReset(t, "manager@example.org"); w.Code != http.StatusTooManyRequests || link != "" {
		t.Errorf("per-IP limit: %d %q", w.Code, link)
	}
	// Sign-in link requests have their own buckets.
	if w, _ := e.requestLink(t, "ada@example.org", nil); w.Code != 200 {
		t.Errorf("sign-in link after reset limit: %d", w.Code)
	}

	e = newEnv(t, "")
	w := e.do(t, "POST", "/password/forgot", "", url.Values{"email": {"admin@example.org"}, "csrf_token": {"forged"}})
	e.wait(t)
	if w.Code != http.StatusForbidden || len(e.fs.sent()) != 0 {
		t.Errorf("forged CSRF: %d, %d emails", w.Code, len(e.fs.sent()))
	}
}

func TestResetRejectsBadTokens(t *testing.T) {
	e := newEnv(t, "")
	cases := map[string]string{
		"unknown":       "bogus",
		"empty":         "",
		"expired":       e.fa.token("admin@example.org", auth.PurposePasswordReset, -time.Minute, false),
		"used":          e.fa.token("admin@example.org", auth.PurposePasswordReset, time.Hour, true),
		"wrong purpose": e.fa.token("admin@example.org", auth.PurposeMagicLink, time.Hour, false),
	}
	for name, tok := range cases {
		w := e.do(t, "GET", "/password/reset?token="+url.QueryEscape(tok), "", nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "expired or has already been used") || strings.Contains(w.Body.String(), `name="password"`) {
			t.Errorf("GET %s: %d", name, w.Code)
		}
		w = e.do(t, "POST", "/password/reset", "", url.Values{"token": {tok}, "password": {"a-long-new-password"}, "password2": {"a-long-new-password"}})
		if w.Code != http.StatusBadRequest || cookieNamed(w, sessionCookie) != nil {
			t.Errorf("POST %s: %d", name, w.Code)
		}
	}
	if _, err := e.fa.Authenticate(t.Context(), "admin@example.org", "admin-password-123"); err != nil {
		t.Error("password changed by a bad token")
	}
	// A reset token is not a sign-in link either.
	tok := e.fa.token("ada@example.org", auth.PurposePasswordReset, time.Hour, false)
	if w := e.do(t, "GET", "/login/verify?token="+tok, "", nil); w.Code != http.StatusBadRequest {
		t.Errorf("reset token used as a sign-in link: %d", w.Code)
	}
}

func TestResetSetsPasswordRevokesSessionsAndSignsIn(t *testing.T) {
	e := newEnv(t, "", func(o *Options) { o.PublicURL, _ = url.Parse("https://members.clidive.org") })
	old := e.fa.session("admin@example.org")
	_, link := e.requestReset(t, "admin@example.org")
	if !strings.HasPrefix(link, "https://members.clidive.org/password/reset?token=") {
		t.Fatalf("link = %q", link)
	}
	path := pathOf(t, link)
	tok := strings.TrimPrefix(path, "/password/reset?token=")

	// The form does not spend the token.
	for i := 0; i < 2; i++ {
		w := e.do(t, "GET", path, "", nil)
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, "Set your password") || !strings.Contains(body, `value="admin@example.org"`) ||
			!strings.Contains(body, `name="token" value="`+tok+`"`) || !strings.Contains(body, `autocomplete="new-password"`) {
			t.Fatalf("form %d: %d", i, w.Code)
		}
	}
	// Mismatch and short passwords leave the token valid.
	w := e.do(t, "POST", "/password/reset", "", url.Values{"token": {tok}, "password": {"a-long-new-password"}, "password2": {"another-password"}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "do not match") {
		t.Errorf("mismatch: %d", w.Code)
	}
	w = e.do(t, "POST", "/password/reset", "", url.Values{"token": {tok}, "password": {"short"}, "password2": {"short"}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "at least 12 characters") {
		t.Errorf("short: %d", w.Code)
	}

	w = e.do(t, "POST", "/password/reset", "", url.Values{"token": {tok}, "password": {"a-long-new-password"}, "password2": {"a-long-new-password"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/members?flash=password-set" {
		t.Fatalf("reset: %d %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	c := cookieNamed(w, sessionCookie)
	if c == nil || c.MaxAge != 12*3600 {
		t.Fatalf("session cookie: %+v", c)
	}
	if w := e.do(t, "GET", "/admin/members?flash=password-set", c.Value, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Your password has been set.") {
		t.Errorf("signed in with full roles: %d", w.Code)
	}
	if p, _ := e.fa.LookupSession(t.Context(), old); p != nil {
		t.Error("existing sessions must be revoked")
	}
	if _, err := e.fa.Authenticate(t.Context(), "admin@example.org", "a-long-new-password"); err != nil {
		t.Error("new password not set")
	}
	if w := e.do(t, "POST", "/password/reset", "", url.Values{"token": {tok}, "password": {"x-long-new-password"}, "password2": {"x-long-new-password"}}); w.Code != http.StatusBadRequest {
		t.Errorf("token reused: %d", w.Code)
	}
	if w := e.do(t, "HEAD", path, "", nil); w.Code != http.StatusBadRequest {
		t.Errorf("HEAD after use: %d", w.Code)
	}
}

func TestResetGrantsNoRoles(t *testing.T) {
	e := newEnv(t, "")
	_, link := e.requestReset(t, "ada@example.org") // a member with a password
	tok := strings.TrimPrefix(pathOf(t, link), "/password/reset?token=")
	w := e.do(t, "POST", "/password/reset", "", url.Values{"token": {tok}, "password": {"a-long-new-password"}, "password2": {"a-long-new-password"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/me?flash=password-set" {
		t.Fatalf("member reset: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := e.do(t, "GET", "/admin/members", cookieNamed(w, sessionCookie).Value, nil); w.Code != http.StatusForbidden {
		t.Errorf("a reset must not grant committee access: %d", w.Code)
	}
}

func TestPasswordEmailTemplates(t *testing.T) {
	base, _ := url.Parse("https://members.clidive.org")
	inv := &auth.Invite{Token: "abc", Email: "chair@example.org", Roles: []auth.Role{auth.RoleAdmin, auth.RoleMember}}
	m, err := InviteMessage(base, inv, InviteTTL)
	if err != nil {
		t.Fatal(err)
	}
	if m.To != "chair@example.org" || m.Subject != "You've been given access to the Clidive members portal" ||
		!strings.Contains(m.Text, "https://members.clidive.org/password/reset?token=abc") || !strings.Contains(m.Text, "expires in 7 days") ||
		!strings.Contains(m.Text, "(admin)") || !strings.Contains(m.HTML, "Set your password") {
		t.Errorf("invite: %+v", m)
	}
}
