package main

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/mail"
	"github.com/clidive410/membership/internal/members"
)

type fakeInviter struct {
	got   auth.InviteRequest
	actor members.Actor
	inv   *auth.Invite
	err   error
}

func (f *fakeInviter) InviteUser(_ context.Context, a members.Actor, req auth.InviteRequest) (*auth.Invite, error) {
	f.got, f.actor = req, a
	return f.inv, f.err
}

type recordSender struct {
	msgs []mail.Message
	err  error
}

func (r *recordSender) Send(_ context.Context, m mail.Message) error {
	if r.err != nil {
		return r.err
	}
	r.msgs = append(r.msgs, m)
	return nil
}

func TestParseInvite(t *testing.T) {
	req, err := parseInvite([]string{"-email", "chair@example.org", "-role", "admin", "-member-id", "12"})
	if err != nil || req.Email != "chair@example.org" || req.Role != auth.RoleAdmin || req.MemberID != 12 || req.TTL.Hours() != 7*24 {
		t.Errorf("%+v %v", req, err)
	}
	for _, args := range [][]string{
		{"-role", "admin"},
		{"-email", "a@example.org"},
		{"-email", "a@example.org", "-role", "member"},
		{"-email", "a@example.org", "-role", "root"},
		{"-email", "a@example.org", "-role", "admin", "-member-id", "-1"},
	} {
		if _, err := parseInvite(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestRunInvite(t *testing.T) {
	base, _ := url.Parse("https://members.clidive.org")
	req, _ := parseInvite([]string{"-email", "Chair@Example.org", "-role", "membership_manager"})
	store := &fakeInviter{inv: &auth.Invite{Token: "secret-token-value", UserID: 7, Email: "chair@example.org", Created: true,
		MemberID: 12, Roles: []auth.Role{auth.RoleMember, auth.RoleMembershipManager}}}
	sender := &recordSender{}
	var out bytes.Buffer
	if err := runInvite(context.Background(), store, sender, base, req, &out); err != nil {
		t.Fatal(err)
	}
	if store.got.Role != auth.RoleMembershipManager || store.actor.Label != "cli" {
		t.Errorf("request %+v by %+v", store.got, store.actor)
	}
	o := out.String()
	for _, want := range []string{"created login chair@example.org", "roles: member, membership_manager", "linked to member 12", "invitation emailed to chair@example.org", "7 days"} {
		if !strings.Contains(o, want) {
			t.Errorf("output lacks %q:\n%s", want, o)
		}
	}
	if strings.Contains(o, "secret-token-value") || strings.Contains(o, "/password/reset") {
		t.Errorf("the link must never be printed:\n%s", o)
	}
	if len(sender.msgs) != 1 || sender.msgs[0].To != "chair@example.org" ||
		!strings.Contains(sender.msgs[0].Text, "https://members.clidive.org/password/reset?token=secret-token-value") ||
		!strings.Contains(sender.msgs[0].Text, "(membership manager)") {
		t.Errorf("email: %+v", sender.msgs)
	}

	// Re-running for an existing login, without a member record.
	store.inv = &auth.Invite{Token: "t2", Email: "chair@example.org", Roles: []auth.Role{auth.RoleAdmin}, Note: "no current member has this primary email: not linked to a member record"}
	out.Reset()
	if err := runInvite(context.Background(), store, sender, base, req, &out); err != nil || !strings.Contains(out.String(), "already exists") || !strings.Contains(out.String(), "not linked") {
		t.Errorf("re-run: %v\n%s", err, out.String())
	}

	// A send failure is an error; store errors pass through.
	sender.err = mail.ErrNotConfigured
	if err := runInvite(context.Background(), store, sender, base, req, &out); err == nil || !strings.Contains(err.Error(), "re-run") {
		t.Errorf("send failure: %v", err)
	}
	store.err = auth.ErrUserDisabled
	if err := runInvite(context.Background(), store, sender, base, req, &out); !errors.Is(err, auth.ErrUserDisabled) {
		t.Errorf("store error: %v", err)
	}
}
