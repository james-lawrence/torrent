package dht

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOptionPassive(t *testing.T) {
	t.Run("true marks the server passive", func(t *testing.T) {
		s := &Server{}
		OptionPassive(true)(s)
		require.True(t, s.passive)
	})

	t.Run("false leaves the server active", func(t *testing.T) {
		s := &Server{passive: true}
		OptionPassive(false)(s)
		require.False(t, s.passive)
	})
}
