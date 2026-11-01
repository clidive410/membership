package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
)

func TestSlogBridge(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, logrus.InfoLevel, "text")
	s := Slog(l).With("app", "test").WithGroup("req")
	s.Debug("hidden")
	s.Warn("hello", "path", "/x", slog.Group("g", slog.Int("n", 1)))
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Errorf("debug line should be dropped at info level: %s", out)
	}
	for _, want := range []string{"level=warning", `msg=hello`, "app=test", "req.path=/x", "req.g.n=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
}

func TestDiscard(t *testing.T) {
	Discard().Error("nothing") // must not panic or print
}
