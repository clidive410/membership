// Package mail sends the portal's transactional emails (sign-in links).
//
// SMTP sends over STARTTLS (port 587) or implicit TLS (port 465) with
// net/smtp from the standard library. Messages are multipart/alternative:
// plain text plus a simple HTML version.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Message is one email to one recipient.
type Message struct {
	To      string // bare address
	Subject string
	Text    string // plain-text body (required)
	HTML    string // optional HTML alternative
}

// Sender delivers messages.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// ErrNotConfigured is returned by Unconfigured.
var ErrNotConfigured = errors.New("email is not configured: set SMTP_HOST (and SMTP_PORT, SMTP_USERNAME, SMTP_PASSWORD)")

// Unconfigured is the Sender used in production when SMTP is not set up:
// every send fails with ErrNotConfigured.
type Unconfigured struct{}

// Send implements Sender.
func (Unconfigured) Send(context.Context, Message) error { return ErrNotConfigured }

// LogSender is for local development only: instead of sending, it logs the
// whole message (including any sign-in link) at INFO level.
type LogSender struct{ Log *slog.Logger }

// Send implements Sender.
func (l LogSender) Send(_ context.Context, m Message) error {
	l.Log.Info("email not sent (SMTP not configured; development only)", "to", m.To, "subject", m.Subject, "body", m.Text)
	return nil
}

// SMTP sends through an SMTP submission server.
type SMTP struct {
	Host     string
	Port     int // 587 (STARTTLS, default) or 465 (implicit TLS)
	Username string
	Password string
	From     *mail.Address
	Timeout  time.Duration // whole conversation; default 30s
	Log      *slog.Logger

	// tlsConfig overrides the TLS configuration (tests only).
	tlsConfig *tls.Config
}

// ParseFrom parses MAIL_FROM, either "addr@example.org" or "Name <addr@example.org>".
func ParseFrom(s string) (*mail.Address, error) {
	a, err := mail.ParseAddress(s)
	if err != nil {
		return nil, fmt.Errorf("MAIL_FROM %q: %w", s, err)
	}
	return a, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Send implements Sender. TLS is required (STARTTLS, or implicit TLS on
// port 465) unless the server is on the loopback interface, as a local
// test server would be.
func (s *SMTP) Send(ctx context.Context, m Message) (err error) {
	if s.From == nil {
		return errors.New("mail: no sender address")
	}
	if err := checkHeaderValue(m.To); err != nil {
		return err
	}
	if _, err := mail.ParseAddress(m.To); err != nil {
		return fmt.Errorf("mail: recipient: %w", err)
	}
	msg, err := Build(s.From, m, time.Now())
	if err != nil {
		return err
	}
	port := s.Port
	if port == 0 {
		port = 587
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	defer func() {
		if s.Log == nil {
			return
		}
		// Recipient domain only: the full address is personal data.
		domain := m.To[strings.LastIndexByte(m.To, '@')+1:]
		if err != nil {
			s.Log.Error("email send failed", "to_domain", domain, "subject", m.Subject, "err", err)
		} else {
			s.Log.Info("email sent", "to_domain", domain, "subject", m.Subject, "duration_ms", time.Since(start).Milliseconds())
		}
	}()

	tlsCfg := s.tlsConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(port))
	var conn net.Conn
	d := &net.Dialer{}
	if port == 465 {
		conn, err = (&tls.Dialer{NetDialer: d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mail: connecting to %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	// Abort the conversation if the context is cancelled early.
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mail: %w", err)
	}
	defer c.Close()
	if port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("mail: STARTTLS: %w", err)
			}
		} else if !isLoopback(s.Host) {
			return errors.New("mail: server does not offer STARTTLS; refusing to send credentials in clear text")
		}
	}
	if s.Username != "" {
		// PlainAuth itself refuses to run without TLS except on localhost.
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return fmt.Errorf("mail: AUTH: %w", err)
		}
	}
	if err := c.Mail(s.From.Address); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(m.To); err != nil {
		return fmt.Errorf("mail: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("mail: writing message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: sending message: %w", err)
	}
	return c.Quit()
}

func checkHeaderValue(v string) error {
	if strings.ContainsAny(v, "\r\n") {
		return errors.New("mail: header value contains a line break")
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Build renders m as an RFC 5322 message with CRLF line endings.
func Build(from *mail.Address, m Message, now time.Time) ([]byte, error) {
	for _, v := range []string{m.To, m.Subject} {
		if err := checkHeaderValue(v); err != nil {
			return nil, err
		}
	}
	if m.Text == "" {
		return nil, errors.New("mail: empty body")
	}
	domain := from.Address[strings.LastIndexByte(from.Address, '@')+1:]
	var b bytes.Buffer
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", from.String())
	h("To", (&mail.Address{Address: m.To}).String())
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", now.Format(time.RFC1123Z))
	h("Message-ID", "<"+randomHex(16)+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Auto-Submitted", "auto-generated")
	part := func(contentType, body string) error {
		b.WriteString("Content-Type: " + contentType + "; charset=utf-8\r\n")
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		qp := quotedprintable.NewWriter(&b)
		// Text mode: the writer emits every line break as CRLF.
		if _, err := qp.Write([]byte(body)); err != nil {
			return err
		}
		if err := qp.Close(); err != nil {
			return err
		}
		b.WriteString("\r\n")
		return nil
	}
	if m.HTML == "" {
		if err := part("text/plain", m.Text); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
	boundary := "clidive-" + randomHex(12)
	h("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	for _, p := range []struct{ ct, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		b.WriteString("--" + boundary + "\r\n")
		if err := part(p.ct, p.body); err != nil {
			return nil, err
		}
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), nil
}
