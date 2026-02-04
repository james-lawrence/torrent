package dht

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/upnp"
)

type mockDevice struct {
	externalIP  net.IP
	externalErr error
	mappedPort  int
	mapErr      error
}

func (m *mockDevice) ID() string                            { return "mock" }
func (m *mockDevice) GetLocalIPv4Address() net.IP           { return net.IPv4(192, 168, 1, 1) }
func (m *mockDevice) SupportsIPVersion(upnp.IPVersion) bool { return true }
func (m *mockDevice) AddPinhole(context.Context, upnp.Protocol, netip.AddrPort, time.Duration) ([]net.IP, error) {
	return nil, nil
}

func (m *mockDevice) GetExternalIPv4Address(context.Context) (net.IP, error) {
	return m.externalIP, m.externalErr
}

func (m *mockDevice) AddPortMapping(context.Context, upnp.Protocol, int, int, string, time.Duration) (int, error) {
	return m.mappedPort, m.mapErr
}

func TestUpnpPortForward(t *testing.T) {
	fallback := netip.MustParseAddrPort("192.168.1.100:6881")
	lease := 50 * time.Millisecond

	t.Run("yields fallback when no devices", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		discover := func(context.Context) []upnp.Device { return nil }
		seq, err := upnpPortForward(ctx, "test", 6881, fallback, discover, lease)
		require.NoError(t, err)

		for addr := range seq {
			require.Equal(t, fallback, addr)
			return
		}
		t.Fatal("iterator yielded nothing")
	})

	t.Run("yields fallback when mapping fails", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		discover := func(context.Context) []upnp.Device {
			return []upnp.Device{&mockDevice{
				externalIP: net.IPv4(1, 2, 3, 4),
				mappedPort: 0,
				mapErr:     errors.New("mapping failed"),
			}}
		}
		seq, err := upnpPortForward(ctx, "test", 6881, fallback, discover, lease)
		require.NoError(t, err)

		for addr := range seq {
			require.Equal(t, fallback, addr)
			return
		}
		t.Fatal("iterator yielded nothing")
	})

	t.Run("yields mapped address on success", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		discover := func(context.Context) []upnp.Device {
			return []upnp.Device{&mockDevice{
				externalIP: net.IPv4(1, 2, 3, 4),
				mappedPort: 6881,
			}}
		}
		seq, err := upnpPortForward(ctx, "test", 6881, fallback, discover, lease)
		require.NoError(t, err)

		for addr := range seq {
			require.Equal(t, uint16(6881), addr.Port())
			require.Equal(t, "1.2.3.4", addr.Addr().Unmap().String())
			return
		}
		t.Fatal("iterator yielded nothing")
	})

	t.Run("context cancellation stops iterator", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		discover := func(context.Context) []upnp.Device { return nil }
		seq, err := upnpPortForward(ctx, "test", 6881, fallback, discover, lease)
		require.NoError(t, err)

		done := make(chan struct{})
		go func() {
			defer close(done)
			for range seq {
				cancel()
			}
		}()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("iterator did not stop")
		}
	})
}
