package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/config"
	"github.com/clidive410/membership/internal/mail"
	"github.com/clidive410/membership/internal/members"
	"github.com/clidive410/membership/internal/web"
)

// inviter is the part of *auth.Store that `user invite` needs.
type inviter interface {
	InviteUser(ctx context.Context, a members.Actor, req auth.InviteRequest) (*auth.Invite, error)
}

// cliActor labels audit entries written by CLI commands.
var cliActor = members.Actor{Label: "cli"}

// parseInvite reads the flags of `user invite`.
func parseInvite(args []string) (auth.InviteRequest, error) {
	fs := flag.NewFlagSet("user invite", flag.ContinueOnError)
	email := fs.String("email", "", "email of the committee member (login identity)")
	role := fs.String("role", "", "admin or membership_manager")
	memberID := fs.Int64("member-id", 0, "member record to link (default: the one current member with this primary email, if any)")
	if err := fs.Parse(args); err != nil {
		return auth.InviteRequest{}, err
	}
	if *email == "" {
		return auth.InviteRequest{}, errors.New("-email is required")
	}
	r := auth.Role(*role)
	if r != auth.RoleAdmin && r != auth.RoleMembershipManager {
		return auth.InviteRequest{}, errors.New("-role must be admin or membership_manager")
	}
	if *memberID < 0 {
		return auth.InviteRequest{}, errors.New("-member-id must be positive")
	}
	return auth.InviteRequest{Email: *email, Role: r, MemberID: *memberID, TTL: web.InviteTTL}, nil
}

// inviteCmd is `membership user invite`: it gives a committee member access
// without anyone sharing a password, by emailing a set-password link.
func inviteCmd(ctx context.Context, args []string, out io.Writer) error {
	req, err := parseInvite(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := cliLogger()
	mailer, err := newMailer(cfg, log)
	if err != nil {
		return err
	}
	if _, ok := mailer.(mail.Unconfigured); ok {
		return errors.New("SMTP_HOST is not set: the invitation cannot be emailed (nothing was changed)")
	}
	_, store, closeDB, err := openAppDB(ctx, log)
	if err != nil {
		return err
	}
	defer closeDB()
	return runInvite(ctx, store, mailer, cfg.BaseURL, req, out)
}

// runInvite creates or updates the login and emails the invitation. It
// prints what was done but never the link: with LOG_FORMAT=text and no
// SMTP the development mail sender logs the whole email instead.
func runInvite(ctx context.Context, store inviter, sender mail.Sender, base *url.URL, req auth.InviteRequest, out io.Writer) error {
	inv, err := store.InviteUser(ctx, cliActor, req)
	if err != nil {
		return err
	}
	if inv.Created {
		fmt.Fprintf(out, "created login %s (no password)\n", inv.Email)
	} else {
		fmt.Fprintf(out, "login %s already exists\n", inv.Email)
	}
	roles := make([]string, len(inv.Roles))
	for i, r := range inv.Roles {
		roles[i] = string(r)
	}
	fmt.Fprintf(out, "roles: %s\n", strings.Join(roles, ", "))
	switch {
	case inv.MemberID != 0:
		fmt.Fprintf(out, "linked to member %d\n", inv.MemberID)
	case inv.Note != "":
		fmt.Fprintln(out, inv.Note)
	}
	msg, err := web.InviteMessage(base, inv, req.TTL)
	if err != nil {
		return err
	}
	if err := sender.Send(ctx, msg); err != nil {
		return fmt.Errorf("the login and role are set up, but the invitation email was not sent (re-run to try again): %w", err)
	}
	fmt.Fprintf(out, "invitation emailed to %s: a link to set a password, valid for %d days\n", inv.Email, int(req.TTL.Hours()/24))
	return nil
}
