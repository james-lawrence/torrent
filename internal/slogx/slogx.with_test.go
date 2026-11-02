package slogx_test

import (
	"bytes"
	"log"
	"log/slog"
	"testing"

	"github.com/james-lawrence/torrent/internal/slogx"
	"github.com/stretchr/testify/require"
)

func TestWith(t *testing.T) {
	t.Run("slog logger", func(t *testing.T) {
		var out bytes.Buffer
		slogx.With(slog.New(slog.NewTextHandler(&out, nil))).Info("hello")
		require.Contains(t, out.String(), "hello")
	})

	t.Run("log logger", func(t *testing.T) {
		var out bytes.Buffer
		slogx.With(log.New(&out, "", 0)).Info("hello")
		require.Contains(t, out.String(), "hello")
	})

	t.Run("nil discards", func(t *testing.T) {
		require.NotPanics(t, func() {
			slogx.With(nil).Info("hello")
			slogx.With((*log.Logger)(nil)).Info("hello")
		})
	})
}
