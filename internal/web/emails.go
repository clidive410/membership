package web

import (
	"bytes"
	"embed"
	htmltemplate "html/template"
	"net/url"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/clidive410/membership/internal/auth"
	"github.com/clidive410/membership/internal/mail"
)

//go:embed templates/email/*
var emailFS embed.FS

// emailTemplate is one email: a plain-text body (text/template) and its
// HTML alternative (html/template, which escapes the data).
type emailTemplate struct {
	subject string
	text    *texttemplate.Template
	html    *htmltemplate.Template
}

func mustEmailTemplate(name, subject string) *emailTemplate {
	return &emailTemplate{
		subject: subject,
		text:    texttemplate.Must(texttemplate.ParseFS(emailFS, "templates/email/"+name+".txt")),
		html:    htmltemplate.Must(htmltemplate.ParseFS(emailFS, "templates/email/"+name+".html")),
	}
}

// loginLinkEmail is sent for a sign-in link request.
var loginLinkEmail = mustEmailTemplate("login_link", "Your Clidive sign-in link")

// loginLinkData is the data for loginLinkEmail.
type loginLinkData struct {
	Link    string // absolute /login/verify URL
	Minutes int    // validity
}

// passwordResetEmail is sent for a "Forgotten your password?" request.
var passwordResetEmail = mustEmailTemplate("password_reset", "Reset your Clidive membership password")

// inviteEmail is sent by `membership user invite` and the admin UI.
var inviteEmail = mustEmailTemplate("invite", "You've been given access to the Clidive members portal")

// resetData is the data for passwordResetEmail.
type resetData struct {
	Link    string // absolute /password/reset URL
	Minutes int
}

// inviteData is the data for inviteEmail.
type inviteData struct {
	Link     string // absolute /password/reset URL
	LoginURL string // absolute /login URL
	Email    string // the login
	Access   string // e.g. "admin"
	Days     int
}

// publicURL returns an absolute URL for an in-app path under BASE_URL.
func publicURL(base *url.URL, path string) string {
	return strings.TrimRight(base.String(), "/") + path
}

// PasswordResetURL is the emailed link for a password_reset token.
func PasswordResetURL(base *url.URL, token string) string {
	return publicURL(base, "/password/reset?"+url.Values{"token": {token}}.Encode())
}

// accessLabel describes a set of roles for emails and pages.
func accessLabel(roles []auth.Role) string {
	switch auth.AccessOf(roles, false) {
	case auth.AccessAdmin:
		return "admin"
	case auth.AccessMembershipManager:
		return "membership manager"
	case auth.AccessMember:
		return "member"
	}
	return "no access"
}

// InviteMessage renders the invitation email for an issued invite: a
// set-password link valid for ttl. The CLI and the admin UI both use it.
func InviteMessage(base *url.URL, inv *auth.Invite, ttl time.Duration) (mail.Message, error) {
	days := int((ttl + 12*time.Hour) / (24 * time.Hour))
	if days < 1 {
		days = 1
	}
	return inviteEmail.message(inv.Email, inviteData{
		Link:     PasswordResetURL(base, inv.Token),
		LoginURL: publicURL(base, "/login"),
		Email:    inv.Email,
		Access:   accessLabel(inv.Roles),
		Days:     days,
	})
}

// message renders the email for one recipient.
func (t *emailTemplate) message(to string, data any) (mail.Message, error) {
	var text, html bytes.Buffer
	if err := t.text.Execute(&text, data); err != nil {
		return mail.Message{}, err
	}
	if err := t.html.Execute(&html, data); err != nil {
		return mail.Message{}, err
	}
	return mail.Message{To: to, Subject: t.subject, Text: text.String(), HTML: html.String()}, nil
}
