package main

import (
	"log/slog"
	"testing"

	"github.com/clidive410/membership/internal/config"
	"github.com/clidive410/membership/internal/mail"
)

func TestNewMailer(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	for _, c := range []struct {
		cfg  config.Config
		want string
	}{
		{config.Config{MailFrom: "Clidive <membership@clidive.org>", SMTPHost: "smtp.example.org", SMTPPort: 587}, "smtp"},
		{config.Config{MailFrom: "membership@clidive.org", LogFormat: "text"}, "log"},
		{config.Config{MailFrom: "membership@clidive.org", LogFormat: "json"}, "unconfigured"},
	} {
		m, err := newMailer(&c.cfg, log)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		switch s := m.(type) {
		case *mail.SMTP:
			got = "smtp"
			if s.From.Name != "Clidive" || s.Host != "smtp.example.org" {
				t.Errorf("smtp sender: %+v", s)
			}
		case mail.LogSender:
			got = "log"
		case mail.Unconfigured:
			got = "unconfigured"
		}
		if got != c.want {
			t.Errorf("%+v: got %s, want %s", c.cfg, got, c.want)
		}
	}
}
