package auth

import (
	"strings"
	"testing"

	"github.com/clidive410/membership/internal/members"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Errorf("unexpected hash format %q", h)
	}
	if ok, err := VerifyPassword(h, "correct horse battery staple"); !ok || err != nil {
		t.Errorf("verify correct: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword(h, "wrong horse battery staple"); ok {
		t.Error("wrong password verified")
	}
	h2, _ := HashPassword("correct horse battery staple")
	if h == h2 {
		t.Error("hashes must be salted")
	}
}

func TestWeakPassword(t *testing.T) {
	if _, err := HashPassword("short"); err != ErrWeakPassword {
		t.Errorf("err = %v", err)
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	for _, h := range []string{"", "plaintext", "$2a$10$abc", "$argon2id$v=19$m=x$a$b"} {
		if ok, err := VerifyPassword(h, "whatever-password"); ok || err == nil {
			t.Errorf("VerifyPassword(%q) = %v, %v", h, ok, err)
		}
	}
}

func TestTokens(t *testing.T) {
	a, b := NewToken(), NewToken()
	if a == b || len(a) < 40 {
		t.Errorf("tokens not random/long enough: %q %q", a, b)
	}
	if HashToken(a) == a || len(HashToken(a)) != 64 {
		t.Error("HashToken should return hex sha256")
	}
}

func TestCSRF(t *testing.T) {
	c := CSRF{Secret: []byte("0123456789abcdef0123456789abcdef")}
	tok := c.Token("browser-1")
	if !c.Valid("browser-1", tok) {
		t.Error("valid token rejected")
	}
	if c.Valid("browser-2", tok) || c.Valid("browser-1", "") || c.Valid("", tok) {
		t.Error("invalid token accepted")
	}
	other := CSRF{Secret: []byte("another-secret-another-secret-xx")}
	if other.Valid("browser-1", tok) {
		t.Error("token valid under a different secret")
	}
}

func TestRolePermissions(t *testing.T) {
	admin := &Principal{Roles: []Role{RoleAdmin}}
	mgr := &Principal{Roles: []Role{RoleMembershipManager}}
	member := &Principal{Roles: []Role{RoleMember}}
	var nobody *Principal

	if !admin.Can(PermExport) || !admin.Can(PermEditPersonal) {
		t.Error("admin should have full access")
	}
	if !mgr.Can(PermEditMembership) || !mgr.Can(PermRecordPayments) || !mgr.Can(PermViewMembers) {
		t.Error("membership manager lacks membership permissions")
	}
	if mgr.Can(PermEditPersonal) || mgr.Can(PermExport) || mgr.Can(PermArchiveMembers) {
		t.Error("membership manager has too many permissions")
	}
	if member.Can(PermViewMembers) || member.Can(PermEditMembership) {
		t.Error("member must not access the admin UI")
	}
	if nobody.Can(PermViewMembers) {
		t.Error("nil principal must have no permissions")
	}
	if g := mgr.EditableGroups(); len(g) != 1 || g[0] != members.GroupMembership {
		t.Errorf("manager groups = %v", g)
	}
	if g := member.SelfEditableGroups(); len(g) != 1 || g[0] != members.GroupPersonal {
		t.Errorf("member self groups = %v", g)
	}
	if len(admin.EditableGroups()) != 3 {
		t.Error("admin should edit all groups")
	}
}
