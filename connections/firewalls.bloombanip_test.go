package connections

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// loopback addresses are keyed by address and port: every peer on a loopback
// network shares a block, and frequently the address itself, so banning the
// block takes out every other local peer along with the offender.
func TestBloomBanIPLoopback(t *testing.T) {
	cause := errors.New("cuz")

	t.Run("bans the address and port it was given", func(t *testing.T) {
		b := NewBloomBanIP(time.Minute)
		b.Inhibit(net.ParseIP("127.0.0.1"), 5000, cause)
		require.Error(t, b.Blocked(net.ParseIP("127.0.0.1"), 5000))
	})

	t.Run("leaves other ports on the same address reachable", func(t *testing.T) {
		b := NewBloomBanIP(time.Minute)
		b.Inhibit(net.ParseIP("127.0.0.1"), 5000, cause)
		require.NoError(t, b.Blocked(net.ParseIP("127.0.0.1"), 5001))
	})

	t.Run("leaves the rest of the loopback block reachable", func(t *testing.T) {
		b := NewBloomBanIP(time.Minute)
		b.Inhibit(net.ParseIP("127.0.0.1"), 5000, cause)
		require.NoError(t, b.Blocked(net.ParseIP("127.0.0.2"), 5000))
		require.NoError(t, b.Blocked(net.ParseIP("127.0.0.5"), 5000))
		require.NoError(t, b.Blocked(net.ParseIP("127.0.0.255"), 5000))
	})

	t.Run("ipv6 loopback is keyed by port as well", func(t *testing.T) {
		b := NewBloomBanIP(time.Minute)
		b.Inhibit(net.ParseIP("::1"), 5000, cause)
		require.Error(t, b.Blocked(net.ParseIP("::1"), 5000))
		require.NoError(t, b.Blocked(net.ParseIP("::1"), 5001))
	})

	// net.ParseIP yields the 16 byte 4in6 form while an address off a
	// netip.Addr is 4 bytes. both name the same peer, so they have to be
	// banned and looked up under the same key.
	t.Run("4 byte and 4in6 forms of an address agree", func(t *testing.T) {
		b := NewBloomBanIP(time.Minute)
		b.Inhibit(net.IP{127, 0, 0, 1}, 5000, cause)
		require.Error(t, b.Blocked(net.ParseIP("127.0.0.1"), 5000))

		b = NewBloomBanIP(time.Minute)
		b.Inhibit(net.ParseIP("127.0.0.1"), 5000, cause)
		require.Error(t, b.Blocked(net.IP{127, 0, 0, 1}, 5000))
	})
}

// private addresses are keyed by the exact address: hosts on a LAN are peers
// of each other, not of the offender.
func TestBloomBanIPPrivate(t *testing.T) {
	cause := errors.New("cuz")

	t.Run("bans the address on every port", func(t *testing.T) {
		b := NewBloomBanIP(time.Minute)
		b.Inhibit(net.ParseIP("192.168.1.5"), 5000, cause)
		require.Error(t, b.Blocked(net.ParseIP("192.168.1.5"), 5000))
		require.Error(t, b.Blocked(net.ParseIP("192.168.1.5"), 5001))
	})

	t.Run("leaves the rest of the block reachable", func(t *testing.T) {
		b := NewBloomBanIP(time.Minute)
		b.Inhibit(net.ParseIP("192.168.1.5"), 5000, cause)
		require.NoError(t, b.Blocked(net.ParseIP("192.168.1.6"), 5000))
	})

	t.Run("4 byte and 4in6 forms of an address agree", func(t *testing.T) {
		b := NewBloomBanIP(time.Minute)
		b.Inhibit(net.IP{192, 168, 1, 5}, 5000, cause)
		require.Error(t, b.Blocked(net.ParseIP("192.168.1.5"), 5000))
	})
}

// everything else keeps the original behaviour: banned within the smallest 8
// bit range, on every port.
func TestBloomBanIPPublic(t *testing.T) {
	cause := errors.New("cuz")

	t.Run("bans the surrounding 8 bit range", func(t *testing.T) {
		b := NewBloomBanIP(time.Minute)
		b.Inhibit(net.ParseIP("185.90.60.219"), 1, cause)
		require.Error(t, b.Blocked(net.ParseIP("185.90.60.219"), 1))
		require.Error(t, b.Blocked(net.ParseIP("185.90.60.219"), 2))
		require.Error(t, b.Blocked(net.ParseIP("185.90.60.7"), 1))
	})

	t.Run("leaves neighbouring ranges reachable", func(t *testing.T) {
		b := NewBloomBanIP(time.Minute)
		b.Inhibit(net.ParseIP("185.90.60.219"), 1, cause)
		require.NoError(t, b.Blocked(net.ParseIP("185.90.61.219"), 1))
	})

	t.Run("4 byte and 4in6 forms of an address agree", func(t *testing.T) {
		b := NewBloomBanIP(time.Minute)
		b.Inhibit(net.IP{185, 90, 60, 219}, 1, cause)
		require.Error(t, b.Blocked(net.ParseIP("185.90.60.219"), 1))
	})
}

// the ban is lifted once the configured window passes, and holds until then.
func TestBloomBanIPExpiry(t *testing.T) {
	cause := errors.New("cuz")

	t.Run("holds within the window", func(t *testing.T) {
		b := NewBloomBanIP(time.Minute)
		b.Inhibit(net.ParseIP("127.0.0.1"), 5000, cause)
		require.Error(t, b.Blocked(net.ParseIP("127.0.0.1"), 5000))
		require.Error(t, b.Blocked(net.ParseIP("127.0.0.1"), 5000))
	})

	t.Run("lifts once the window passes", func(t *testing.T) {
		b := NewBloomBanIP(time.Millisecond)
		b.Inhibit(net.ParseIP("127.0.0.1"), 5000, cause)
		require.Error(t, b.Blocked(net.ParseIP("127.0.0.1"), 5000))

		require.Eventually(t, func() bool {
			return b.Blocked(net.ParseIP("127.0.0.1"), 5000) == nil
		}, time.Second, 10*time.Millisecond)
	})
}
