// Package logging configures logrus for the portal and bridges log/slog
// (used by zitadel/oidc) to it, so every line comes out in one format.
package logging

import (
	"context"
	"io"
	"log/slog"

	"github.com/sirupsen/logrus"
)

// New returns a logger writing to w at the given level. format is "json"
// (the default) or "text".
func New(w io.Writer, level logrus.Level, format string) *logrus.Logger {
	l := logrus.New()
	l.SetOutput(w)
	l.SetLevel(level)
	if format == "text" {
		l.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	} else {
		l.SetFormatter(&logrus.JSONFormatter{})
	}
	return l
}

// Discard returns a logger that drops everything.
func Discard() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	l.SetLevel(logrus.PanicLevel)
	return l
}

// Slog returns a log/slog logger that forwards to l, for libraries that
// log through slog.
func Slog(l logrus.FieldLogger) *slog.Logger {
	return slog.New(&slogHandler{log: l})
}

type slogHandler struct {
	log   logrus.FieldLogger
	attrs []slog.Attr
	group string
}

func (h *slogHandler) Enabled(_ context.Context, level slog.Level) bool {
	e := h.log.WithFields(nil) // *logrus.Entry gives access to the level
	return e.Logger.IsLevelEnabled(toLogrusLevel(level))
}

func (h *slogHandler) Handle(_ context.Context, r slog.Record) error {
	fields := logrus.Fields{}
	for _, a := range h.attrs {
		h.add(fields, a)
	}
	r.Attrs(func(a slog.Attr) bool { h.add(fields, a); return true })
	h.log.WithFields(fields).Log(toLogrusLevel(r.Level), r.Message)
	return nil
}

func (h *slogHandler) add(fields logrus.Fields, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	key := a.Key
	if h.group != "" {
		key = h.group + "." + key
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, ga := range a.Value.Group() {
			ga.Key = key + "." + ga.Key
			h.add(fields, ga)
		}
		return
	}
	fields[key] = a.Value.Any()
}

func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &n
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	if h.group != "" {
		name = h.group + "." + name
	}
	n.group = name
	return &n
}

func toLogrusLevel(l slog.Level) logrus.Level {
	switch {
	case l >= slog.LevelError:
		return logrus.ErrorLevel
	case l >= slog.LevelWarn:
		return logrus.WarnLevel
	case l >= slog.LevelInfo:
		return logrus.InfoLevel
	}
	return logrus.DebugLevel
}
