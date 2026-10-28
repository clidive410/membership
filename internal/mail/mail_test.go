package mail

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseFrom(t *testing.T) {
	a, err := ParseFrom("Clidive Membership <membership@clidive.org>")
	if err != nil || a.Name != "Clidive Membership" || a.Address != "membership@clidive.org" {
		t.Fatalf("%+v %v", a, err)
	}
	if a, err := ParseFrom("membership@clidive.org"); err != nil || a.Address != "membership@clidive.org" {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := ParseFrom("not an address"); err == nil {
		t.Error("expected an error")
	}
}

func TestBuildMultipart(t *testing.T) {
	from, _ := ParseFrom("Clidive Membership <membership@clidive.org>")
	link := "https://members.clidive.org/login/verify?token=" + strings.Repeat("A", 43)
	raw, err := Build(from, Message{To: "diver@example.org", Subject: "Your sign-in link – Clidive",
		Text: "Hello\n\n" + link + "\n", HTML: `<p><a href="` + link + `">Sign in</a></p>`}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ReplaceAll(string(raw), "\r\n", ""), "\n") {
		t.Error("bare LF in message")
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	subj, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if subj != "Your sign-in link – Clidive" || msg.Header.Get("To") != "<diver@example.org>" ||
		!strings.Contains(msg.Header.Get("From"), "membership@clidive.org") || !strings.HasSuffix(msg.Header.Get("Message-Id"), "@clidive.org>") {
		t.Errorf("headers: %v (subject %q)", msg.Header, subj)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content type %q", msg.Header.Get("Content-Type"))
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var types []string
	for {
		p, err := mr.NextPart() // decodes quoted-printable
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(p)
		types = append(types, p.Header.Get("Content-Type"))
		if !strings.Contains(string(body), link) {
			t.Errorf("part %s lacks the link: %q", p.Header.Get("Content-Type"), body)
		}
	}
	if strings.Join(types, ",") != "text/plain; charset=utf-8,text/html; charset=utf-8" {
		t.Errorf("parts = %v", types)
	}
}

func TestBuildRejectsHeaderInjection(t *testing.T) {
	from, _ := ParseFrom("membership@clidive.org")
	if _, err := Build(from, Message{To: "a@example.org", Subject: "x\r\nBcc: evil@example.org", Text: "t"}, time.Now()); err == nil {
		t.Error("CRLF in subject accepted")
	}
	s := &SMTP{Host: "127.0.0.1", From: from}
	if err := s.Send(context.Background(), Message{To: "a@example.org\r\nRCPT TO:<b@example.org>", Subject: "x", Text: "t"}); err == nil {
		t.Error("CRLF in recipient accepted")
	}
}

func TestUnconfigured(t *testing.T) {
	if err := (Unconfigured{}).Send(context.Background(), Message{}); err != ErrNotConfigured {
		t.Errorf("err = %v", err)
	}
}

// fakeServer is a minimal SMTP server on loopback (no TLS) that records one
// conversation.
type fakeServer struct {
	ln       net.Listener
	starttls bool
	mu       sync.Mutex
	cmds     []string
	data     string
	done     chan struct{}
}

func newFakeServer(t *testing.T, starttls bool) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{ln: ln, starttls: starttls, done: make(chan struct{})}
	t.Cleanup(func() { ln.Close() })
	go f.serve()
	return f
}

func (f *fakeServer) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeServer) serve() {
	defer close(f.done)
	c, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	say := func(s string) { io.WriteString(c, s+"\r\n") }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.mu.Lock()
		f.cmds = append(f.cmds, line)
		f.mu.Unlock()
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch verb {
		case "EHLO":
			if f.starttls {
				say("250-fake")
				say("250-STARTTLS")
			} else {
				say("250-fake")
			}
			say("250 AUTH PLAIN")
		case "AUTH":
			say("235 ok")
		case "MAIL", "RCPT":
			say("250 ok")
		case "DATA":
			say("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		case "STARTTLS":
			say("454 TLS not available")
		default:
			say("502 unknown")
		}
	}
}

func TestSMTPSend(t *testing.T) {
	f := newFakeServer(t, false)
	from, _ := ParseFrom("Clidive <membership@clidive.org>")
	var logs strings.Builder
	s := &SMTP{Host: "127.0.0.1", Port: f.port(), Username: "user", Password: "secret-pw", From: from,
		Log: slog.New(slog.NewTextHandler(&logs, nil))}
	err := s.Send(context.Background(), Message{To: "diver@example.org", Subject: "Hi", Text: "Body text"})
	if err != nil {
		t.Fatal(err)
	}
	<-f.done
	want := "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00user\x00secret-pw"))
	cmds := strings.Join(f.cmds, "\n")
	for _, c := range []string{want, "MAIL FROM:<membership@clidive.org>", "RCPT TO:<diver@example.org>", "QUIT"} {
		if !strings.Contains(cmds, c) {
			t.Errorf("missing %q in conversation:\n%s", c, cmds)
		}
	}
	if !strings.Contains(f.data, "Body text") || !strings.Contains(f.data, "Subject: Hi") {
		t.Errorf("data = %q", f.data)
	}
	if strings.Contains(logs.String(), "secret-pw") || strings.Contains(logs.String(), "diver@") || !strings.Contains(logs.String(), "to_domain=example.org") {
		t.Errorf("log = %q", logs.String())
	}
}

func TestSMTPNeverFallsBackToClearText(t *testing.T) {
	// The server offers STARTTLS but the handshake fails: never fall back
	// to clear text.
	f := newFakeServer(t, true)
	from, _ := ParseFrom("membership@clidive.org")
	s := &SMTP{Host: "127.0.0.1", Port: f.port(), From: from}
	err := s.Send(context.Background(), Message{To: "diver@example.org", Subject: "Hi", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(strings.Join(f.cmds, "\n"), "MAIL FROM") {
		t.Error("sent without TLS")
	}
}

func TestSMTPTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // accept and say nothing
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(2 * time.Second)
		}
	}()
	from, _ := ParseFrom("membership@clidive.org")
	s := &SMTP{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, From: from, Timeout: 200 * time.Millisecond}
	start := time.Now()
	if err := s.Send(context.Background(), Message{To: "a@example.org", Subject: "x", Text: "x"}); err == nil {
		t.Error("expected a timeout")
	}
	if time.Since(start) > time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}
