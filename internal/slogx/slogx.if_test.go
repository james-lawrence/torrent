package slogx_test

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/james-lawrence/torrent/internal/slogx"
	"github.com/stretchr/testify/require"
)

func TestIf(t *testing.T) {
	t.Run("logs when enabled", func(t *testing.T) {
		var out bytes.Buffer
		slogx.With(slog.New(slog.NewTextHandler(&out, nil))).If(true).Info("hello")
		require.Contains(t, out.String(), "hello")
	})

	t.Run("drops the message when disabled", func(t *testing.T) {
		var out bytes.Buffer
		slogx.With(slog.New(slog.NewTextHandler(&out, nil))).If(false).Info("hello")
		require.Empty(t, out.String())
	})

	t.Run("preserves the gate through attrs and groups", func(t *testing.T) {
		var out bytes.Buffer
		slogx.With(slog.New(slog.NewTextHandler(&out, nil))).If(false).With("a", 1).WithGroup("g").Info("hello")
		require.Empty(t, out.String())
	})
}
