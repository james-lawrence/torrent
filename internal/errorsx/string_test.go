package errorsx_test

import (
	"errors"
	"testing"

	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/stretchr/testify/assert"
)

func TestString(t *testing.T) {
	t.Run("stdlib interopt", func(t *testing.T) {
		var (
			local errorsx.String
			cause = errorsx.String("derp")
		)

		assert.True(t, errors.As(cause, &local))
		assert.Equal(t, cause, local)
		assert.True(t, errors.Is(cause, local))
		assert.True(t, errors.Is(cause, local))
	})

	t.Run("works with wrap", func(t *testing.T) {
		var (
			local   errorsx.String
			cause   = errorsx.String("derp")
			wrapped = errorsx.Wrap(cause, "wrapped error")
		)

		assert.True(t, errors.As(wrapped, &local))
		assert.True(t, errors.Is(wrapped, local))
		assert.True(t, errors.Is(wrapped, local))
	})
}
