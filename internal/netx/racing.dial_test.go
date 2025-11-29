package netx

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/internal/errorsx"
	"github.com/stretchr/testify/require"
)

func TestRacingDialer_Dial(t *testing.T) {
	t.Run("immediate success from first network wins", func(t *testing.T) {
		dialer := NewRacing(10)
		timeout := 100 * time.Millisecond
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer l.Close()

		d := net.Dialer{
			Timeout: 5 * time.Second,
		}

		winnerNetwork := DialerFn(func(ctx context.Context, addr string) (net.Conn, error) {
			return d.DialContext(ctx, l.Addr().Network(), l.Addr().String())
		})

		loserNetwork := DialerFn(func(ctx context.Context, addr string) (net.Conn, error) {
			return nil, errorsx.Errorf("failed connection")
		})

		conn, err := dialer.Dial(t.Context(), timeout, l.Addr().String(), winnerNetwork, loserNetwork)
		require.NoError(t, err)
		require.NotNil(t, conn)

		require.Equal(t, l.Addr().String(), conn.RemoteAddr().String())

		require.NoError(t, conn.Close())
	})

	t.Run("all networks fail to dial", func(t *testing.T) {
		dialer := NewRacing(10)
		address := "127.0.0.1:8081"
		timeout := 100 * time.Millisecond
		expectedErr := errorsx.Errorf("dial error")

		network1 := DialerFn(func(ctx context.Context, addr string) (net.Conn, error) {
			return nil, expectedErr
		})
		network2 := DialerFn(func(ctx context.Context, addr string) (net.Conn, error) {
			return nil, expectedErr
		})

		conn, err := dialer.Dial(t.Context(), timeout, address, network1, network2)
		require.Nil(t, conn)
		require.Error(t, err)
		require.ErrorIs(t, err, expectedErr)
	})

	t.Run("dial timeout reached before any success", func(t *testing.T) {
		dialer := NewRacing(10)
		address := "127.0.0.1:8083"
		timeout := 10 * time.Millisecond

		slowNetwork := DialerFn(func(ctx context.Context, addr string) (net.Conn, error) {
			select {
			case <-ctx.Done():
				return nil, context.Cause(ctx)
			case <-time.After(50 * time.Millisecond):
				return nil, errorsx.Errorf("should not happen")
			}
		})

		conn, err := dialer.Dial(t.Context(), timeout, address, slowNetwork, slowNetwork)
		require.Nil(t, conn)
		require.Error(t, err)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("context cancelled before immediately", func(t *testing.T) {
		dialer := NewRacing(10)
		address := "127.0.0.1:8085"
		timeout := 100 * time.Millisecond

		stallingNetwork := DialerFn(func(ctx context.Context, addr string) (net.Conn, error) {
			<-ctx.Done()
			return nil, context.Cause(ctx)
		})

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		conn, err := dialer.Dial(ctx, timeout, address, stallingNetwork, stallingNetwork)
		require.Nil(t, conn)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("context cancelled before any success", func(t *testing.T) {
		dialer := NewRacing(10)
		address := "127.0.0.1:8085"
		timeout := 100 * time.Millisecond
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer l.Close()

		d := net.Dialer{
			Timeout: 5 * time.Second,
		}

		stallingNetwork := DialerFn(func(ctx context.Context, addr string) (net.Conn, error) {
			<-ctx.Done()
			return d.DialContext(ctx, l.Addr().Network(), l.Addr().String())
		})

		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)

		conn, err := dialer.Dial(ctx, timeout, address, stallingNetwork, stallingNetwork)
		require.Nil(t, conn)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("no networks provided", func(t *testing.T) {
		dialer := NewRacing(10)
		timeout := 100 * time.Millisecond

		conn, err := dialer.Dial(t.Context(), timeout, "127.0.0.1:8088")
		require.Nil(t, conn)
		require.ErrorIs(t, err, errorsx.String("no networks to dial"))
	})
}
