package web

import (
	"bytes"
	"embed"
	htmltemplate "html/template"
	texttemplate "text/template"

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
