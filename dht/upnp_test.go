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

	collect := func(ds []upnp.Device) []netip.AddrPort {
		var addrs []netip.AddrPort
		upnpPortForward("test", 6881, fallback, ds, time.Millisecond, func(addr netip.AddrPort) bool {
			addrs = append(addrs, addr)
			return true
		})
		return addrs
	}

	t.Run("yields fallback when no devices", func(t *testing.T) {
		addrs := collect(nil)
		require.Equal(t, []netip.AddrPort{fallback}, addrs)
	})

	t.Run("yields fallback when mapping fails", func(t *testing.T) {
		ds := []upnp.Device{&mockDevice{
			externalIP: net.IPv4(1, 2, 3, 4),
			mapErr:     errors.New("mapping failed"),
		}}
		addrs := collect(ds)
		require.Equal(t, []netip.AddrPort{fallback}, addrs)
	})

	t.Run("yields mapped address on success", func(t *testing.T) {
		ds := []upnp.Device{&mockDevice{
			externalIP: net.IPv4(1, 2, 3, 4),
			mappedPort: 6881,
		}}
		addrs := collect(ds)
		require.Len(t, addrs, 2) // TCP and UDP
		require.Equal(t, uint16(6881), addrs[0].Port())
		require.Equal(t, "1.2.3.4", addrs[0].Addr().Unmap().String())
	})

	t.Run("context cancellation stops iterator", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		seq, err := UPnPPortForward(ctx, "test", 6881, fallback)
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
		case <-time.After(5 * time.Second):
			t.Fatal("iterator did not stop")
		}
	})
}
