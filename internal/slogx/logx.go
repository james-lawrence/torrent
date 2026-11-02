package slogx

import (
	"context"
	"io"
	"log"
	"log/slog"
	"sync"

	"golang.org/x/time/rate"
)

// Logger is a slog compatible logger that can be conditionally gated.
type Logger struct {
	*slog.Logger
}

// With wraps either a *log.Logger or *slog.Logger into a slog compatible logger.
// a nil logger discards all records.
func With(l any) Logger {
	switch v := l.(type) {
	case *slog.Logger:
		if v != nil {
			return Logger{Logger: v}
		}
	case *log.Logger:
		if v != nil {
			return Logger{Logger: slog.New(slog.NewTextHandler(v.Writer(), nil))}
		}
	default:
		log.Printf("slogx.With: unsupported logger type %T, discarding\n", l)
	}

	return Logger{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// If creates a logger that only outputs when enabled is true, e.g.)
// slogx.With(l).If(envx.Boolean(false, "LOGS_INFO")).Info("...").
func (t Logger) If(enabled bool) Logger {
	return t.gate(func() bool { return enabled })
}

// Limit creates a logger whose output is restricted by the limiter.
func (t Logger) Limit(l *rate.Limiter) Logger {
	return t.gate(l.Allow)
}

// Once creates a logger that only outputs the first record, subsequent records are dropped.
func (t Logger) Once() Logger {
	var once sync.Once
	return t.gate(func() (allowed bool) {
		once.Do(func() { allowed = true })
		return allowed
	})
}

func (t Logger) gate(allow func() bool) Logger {
	return Logger{Logger: slog.New(gated{Handler: t.Handler(), allow: allow})}
}

type gated struct {
	slog.Handler
	allow func() bool
}

func (t gated) Enabled(ctx context.Context, level slog.Level) bool {
	return t.allow() && t.Handler.Enabled(ctx, level)
}

func (t gated) WithAttrs(attrs []slog.Attr) slog.Handler {
	return gated{Handler: t.Handler.WithAttrs(attrs), allow: t.allow}
}

func (t gated) WithGroup(name string) slog.Handler {
	return gated{Handler: t.Handler.WithGroup(name), allow: t.allow}
}
