package slogx_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/internal/slogx"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func TestLimit(t *testing.T) {
	t.Run("drops messages once the limiter denies", func(t *testing.T) {
		var out bytes.Buffer
		l := slogx.With(slog.New(slog.NewTextHandler(&out, nil))).Limit(rate.NewLimiter(rate.Every(time.Hour), 1))
		l.Info("first")
		l.Info("second")
		require.Equal(t, 1, strings.Count(out.String(), "msg="))
		require.Contains(t, out.String(), "first")
	})

	t.Run("chains with if", func(t *testing.T) {
		var out bytes.Buffer
		slogx.With(slog.New(slog.NewTextHandler(&out, nil))).If(false).Limit(rate.NewLimiter(rate.Inf, 1)).Info("hello")
		require.Empty(t, out.String())
	})
}
