package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/mail"
	"github.com/clidive410/membership/internal/members"
)

func (e *env) uid(email string) string {
	e.fa.mu.Lock()
	defer e.fa.mu.Unlock()
	return strconv.FormatInt(e.fa.users[email].p.UserID, 10)
}

func (e *env) roles(email string) []auth.Role {
	e.fa.mu.Lock()
	defer e.fa.mu.Unlock()
	return e.fa.users[email].p.Roles
}

func TestUsersPagePermissions(t *testing.T) {
	e := newEnv(t, "")
	admin := e.fa.session("admin@example.org")
	w := e.do(t, "GET", "/admin/users", admin, nil)
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("admin: %d", w.Code)
	}
	for _, want := range []string{"Invite by email", "manager@example.org", `action="/admin/users/` + e.uid("manager@example.org") + `/access"`,
		`<option value="membership_manager" selected>`, "Anchor", `href="/admin/users" aria-current="page"`, "you cannot remove your own access"} {
		if !strings.Contains(body, want) {
			t.Errorf("users page lacks %q", want)
		}
	}
	if w := e.do(t, "GET", "/admin/members", e.fa.session("manager@example.org"), nil); strings.Contains(w.Body.String(), `href="/admin/users"`) {
		t.Error("managers must not see the Portal access nav entry")
	}

	posts := []struct{ path string }{
		{"/admin/users/" + e.uid("bob@example.org") + "/access"},
		{"/admin/members/2/access"},
		{"/admin/users"},
	}
	for _, who := range []string{"manager@example.org", "ada@example.org"} {
		sess := e.fa.session(who)
		if w := e.do(t, "GET", "/admin/users", sess, nil); w.Code != http.StatusForbidden {
			t.Errorf("GET users as %s: %d", who, w.Code)
		}
		for _, p := range posts {
			form := url.Values{"access": {"admin"}, "action": {"save"}, "email": {"bob@example.org"}}
			if w := e.do(t, "POST", p.path, sess, form); w.Code != http.StatusForbidden {
				t.Errorf("POST %s as %s: %d", p.path, who, w.Code)
			}
		}
	}
	if e.fa.accessCalls != 0 || len(e.fs.sent()) != 0 {
		t.Errorf("forbidden requests changed access: %d calls, %d emails", e.fa.accessCalls, len(e.fs.sent()))
	}
	if w := e.do(t, "POST", "/admin/users/1/access", "", url.Values{"access": {"admin"}}); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", w.Code)
	}
	if w := e.do(t, "POST", "/admin/users/"+e.uid("bob@example.org")+"/access", admin, url.Values{"access": {"admin"}, "csrf_token": {"forged"}}); w.Code != http.StatusForbidden {
		t.Errorf("forged CSRF: %d", w.Code)
	}
}

func TestAdminChangesRole(t *testing.T) {
	e := newEnv(t, "")
	admin := e.fa.session("admin@example.org")
	w := e.do(t, "POST", "/admin/users/"+e.uid("bob@example.org")+"/access", admin, url.Values{"access": {"membership_manager"}, "action": {"save"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/users?flash=access-saved#user-"+e.uid("bob@example.org") {
		t.Fatalf("save: %d %q", w.Code, w.Header().Get("Location"))
	}
	if r := e.roles("bob@example.org"); len(r) != 2 || !(&auth.Principal{Roles: r}).HasRole(auth.RoleMembershipManager) {
		t.Errorf("bob roles: %v", r)
	}
	if !strings.Contains(strings.Join(e.fa.audit, "\n"), "admin@example.org auth.access membership_manager") {
		t.Errorf("audit: %v", e.fa.audit)
	}
	if w := e.do(t, "POST", "/admin/users/1/access", admin, url.Values{"access": {"superuser"}}); w.Code != http.StatusBadRequest {
		t.Errorf("unknown level: %d", w.Code)
	}
	if w := e.do(t, "POST", "/admin/users/999/access", admin, url.Values{"access": {"admin"}}); w.Code != http.StatusNotFound {
		t.Errorf("unknown user: %d", w.Code)
	}
}

func TestAdminCannotLockThemselvesOut(t *testing.T) {
	e := newEnv(t, "")
	admin := e.fa.session("admin@example.org")
	for _, a := range []string{"none", "member", "membership_manager"} {
		w := e.do(t, "POST", "/admin/users/"+e.uid("admin@example.org")+"/access", admin, url.Values{"access": {a}, "action": {"save"}})
		if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "flash=self-lockout") {
			t.Errorf("self %s: %d %q", a, w.Code, w.Header().Get("Location"))
		}
	}
	if e.fa.accessCalls != 0 || len(e.roles("admin@example.org")) != 1 {
		t.Errorf("own access changed: %d calls, roles %v", e.fa.accessCalls, e.roles("admin@example.org"))
	}
	if w := e.do(t, "GET", "/admin/members", admin, nil); w.Code != 200 {
		t.Errorf("admin locked out: %d", w.Code)
	}
	body := e.do(t, "GET", "/admin/users?flash=self-lockout", admin, nil).Body.String()
	if !strings.Contains(body, "You cannot remove your own admin access") {
		t.Error("lock-out message not shown")
	}
	// The member page of an admin's own linked record offers no role select.
	e.fm.add(membersNew(4, "chair@example.org"))
	e.fa.addUser("chair@example.org", "chair-password-12", 4, auth.RoleAdmin, auth.RoleMember)
	chair := e.fa.session("chair@example.org")
	body = e.do(t, "GET", "/admin/members/4", chair, nil).Body.String()
	if strings.Contains(body, `id="access-level"`) || !strings.Contains(body, "you cannot remove your own access") {
		t.Error("own member page must not offer a role select")
	}
}

func TestRevokeDisablesAndEndsSessions(t *testing.T) {
	e := newEnv(t, "")
	admin := e.fa.session("admin@example.org")
	mgr := e.fa.session("manager@example.org")
	uid := e.uid("manager@example.org")
	w := e.do(t, "POST", "/admin/users/"+uid+"/access", admin, url.Values{"access": {"none"}, "action": {"save"}})
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "flash=access-revoked") {
		t.Fatalf("revoke: %d %q", w.Code, w.Header().Get("Location"))
	}
	e.fa.mu.Lock()
	u := e.fa.users["manager@example.org"]
	e.fa.mu.Unlock()
	if !u.disabled || len(u.p.Roles) != 0 {
		t.Errorf("manager after revoke: %+v", u)
	}
	n, _ := strconv.ParseInt(uid, 10, 64)
	if e.fa.sessionsOf(n) != 0 {
		t.Error("sessions not revoked")
	}
	if w := e.do(t, "GET", "/admin/members", mgr, nil); w.Code != http.StatusSeeOther {
		t.Errorf("revoked session still works: %d", w.Code)
	}
	if _, err := e.fa.Authenticate(t.Context(), "manager@example.org", "manager-password-1"); err == nil {
		t.Error("disabled login can still sign in")
	}
	body := e.do(t, "GET", "/admin/users", admin, nil).Body.String()
	if !strings.Contains(body, "disabled") {
		t.Error("users page must show the login as disabled")
	}
}

func TestMemberPageAccessCard(t *testing.T) {
	e := newEnv(t, "")
	admin := e.fa.session("admin@example.org")
	body := e.do(t, "GET", "/admin/members/1", admin, nil).Body.String()
	for _, want := range []string{"Portal access", `action="/admin/members/1/access"`, `id="access-level"`, "ada@example.org", "Send invite / reset email", "never"} {
		if !strings.Contains(body, want) {
			t.Errorf("admin member page lacks %q", want)
		}
	}
	body = e.do(t, "GET", "/admin/members/1", e.fa.session("manager@example.org"), nil).Body.String()
	if !strings.Contains(body, "Portal access") || strings.Contains(body, `action="/admin/members/1/access"`) {
		t.Error("managers see the card read-only")
	}

	// Invite from the member page: grants the role and emails a set-password link.
	w := e.do(t, "POST", "/admin/members/2/access", admin, url.Values{"access": {"membership_manager"}, "action": {"invite"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/members/2?flash=invite-sent#access" {
		t.Fatalf("invite: %d %q", w.Code, w.Header().Get("Location"))
	}
	msgs := e.fs.sent()
	if len(msgs) != 1 || msgs[0].To != "bob@example.org" || !strings.Contains(msgs[0].Subject, "given access") {
		t.Fatalf("emails: %+v", msgs)
	}
	link := resetLinkRE.FindString(msgs[0].Text)
	if w := e.do(t, "GET", pathOf(t, link), "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Set your password") {
		t.Errorf("invite link: %d", w.Code)
	}
	if !(&auth.Principal{Roles: e.roles("bob@example.org")}).HasRole(auth.RoleMembershipManager) {
		t.Errorf("role not granted: %v", e.roles("bob@example.org"))
	}

	// A member without a login gets one created.
	e.fm.add(membersNew(4, "dee@example.org"))
	w = e.do(t, "POST", "/admin/members/4/access", admin, url.Values{"access": {"member"}, "action": {"save"}})
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "access-saved") {
		t.Errorf("create: %d %q", w.Code, w.Header().Get("Location"))
	}
	if u, _ := e.fa.UserForMember(t.Context(), 4, "dee@example.org"); u == nil || u.Access() != auth.AccessMember || u.MemberID.Int64 != 4 {
		t.Errorf("created login: %+v", u)
	}
	if w := e.do(t, "POST", "/admin/members/4/access", admin, url.Values{"access": {"none"}, "action": {"invite"}}); !strings.Contains(w.Header().Get("Location"), "choose-role") {
		t.Errorf("invite without a role: %q", w.Header().Get("Location"))
	}
}

func TestInviteByEmail(t *testing.T) {
	e := newEnv(t, "")
	admin := e.fa.session("admin@example.org")
	w := e.do(t, "POST", "/admin/users", admin, url.Values{"email": {" Chair@Example.org "}, "access": {"admin"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/admin/users?flash=invite-sent" {
		t.Fatalf("invite: %d %q", w.Code, w.Header().Get("Location"))
	}
	if r := e.roles("chair@example.org"); len(r) != 1 || r[0] != auth.RoleAdmin {
		t.Errorf("roles: %v", r)
	}
	if msgs := e.fs.sent(); len(msgs) != 1 || msgs[0].To != "chair@example.org" || !strings.Contains(msgs[0].Text, "(admin)") {
		t.Errorf("emails: %+v", msgs)
	}
	if w := e.do(t, "POST", "/admin/users", admin, url.Values{"email": {"nope"}, "access": {"admin"}}); !strings.Contains(w.Header().Get("Location"), "bad-email") {
		t.Errorf("bad email: %q", w.Header().Get("Location"))
	}

	// A mail failure is reported, the access change is kept.
	e = newEnv(t, "", func(o *Options) { o.Mailer = mail.Unconfigured{} })
	w = e.do(t, "POST", "/admin/users", e.fa.session("admin@example.org"), url.Values{"email": {"sec@example.org"}, "access": {"membership_manager"}})
	if !strings.Contains(w.Header().Get("Location"), "invite-failed") || len(e.roles("sec@example.org")) != 1 {
		t.Errorf("mail failure: %q %v", w.Header().Get("Location"), e.roles("sec@example.org"))
	}
}

func membersNew(id int64, email string) members.Member {
	return members.Member{ID: id, FirstName: "Dee", LastName: "Diver", Email1: email, Email1Use: true, TypeID: typ(1), MembershipExpiry: day(2027, 3, 31)}
}
