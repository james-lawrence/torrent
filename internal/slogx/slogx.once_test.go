package slogx_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/james-lawrence/torrent/internal/slogx"
	"github.com/stretchr/testify/require"
)

func TestOnce(t *testing.T) {
	t.Run("only logs the first record", func(t *testing.T) {
		var out bytes.Buffer
		l := slogx.With(slog.New(slog.NewTextHandler(&out, nil))).Once()
		l.Info("first")
		l.Info("second")
		require.Equal(t, 1, strings.Count(out.String(), "msg="))
		require.Contains(t, out.String(), "first")
	})

	t.Run("gate survives attrs", func(t *testing.T) {
		var out bytes.Buffer
		l := slogx.With(slog.New(slog.NewTextHandler(&out, nil))).Once().With("a", 1)
		l.Info("first")
		l.Info("second")
		require.Equal(t, 1, strings.Count(out.String(), "msg="))
	})
}
