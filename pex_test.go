package torrent

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	pp "github.com/james-lawrence/torrent/btprotocol"
)

// TestPexSnapshotUsesAnnouncedPortForIncomingConnections proves pex.snapshot
// never reports a connection's raw remote address for an incoming
// connection. remoteAddr on an incoming connection is the peer's ephemeral,
// OS-assigned outgoing TCP source port for that one socket - not their real
// BitTorrent listening port. Only an outgoing connection's remoteAddr is
// trustworthy (we dialed that exact address ourselves); an incoming
// connection's real listening port can only come from what the peer told us
// in its BEP10 extended handshake (PeerListenPort). A peer that never
// announced a listening port must be omitted from PEX entirely rather than
// reported with a wrong address.
func TestPexSnapshotUsesAnnouncedPortForIncomingConnections(t *testing.T) {
	cfg := &ClientConfig{}
	var bits pp.ExtensionBits

	outgoing := newConnection(cfg, nil, true, netip.MustParseAddrPort("127.0.0.1:6001"), &bits, 0, netip.AddrPort{})

	incomingAnnounced := newConnection(cfg, nil, false, netip.MustParseAddrPort("127.0.0.1:55123"), &bits, 0, netip.AddrPort{})
	incomingAnnounced.PeerListenPort = 6002

	incomingUnannounced := newConnection(cfg, nil, false, netip.MustParseAddrPort("127.0.0.1:55124"), &bits, 0, netip.AddrPort{})

	px := newPex()
	px.added(outgoing)
	px.added(incomingAnnounced)
	px.added(incomingUnannounced)

	msg := px.snapshot(outgoing)
	require.NotNil(t, msg)

	var got []netip.AddrPort
	for _, na := range msg.Added {
		got = append(got, netip.AddrPortFrom(na.AddrPort.Addr().Unmap(), na.AddrPort.Port()))
	}

	require.Contains(t, got, netip.MustParseAddrPort("127.0.0.1:6002"), "an incoming connection must be reported using its BEP10-announced listening port")
	require.NotContains(t, got, netip.MustParseAddrPort("127.0.0.1:55123"), "an incoming connection's raw ephemeral remote address must never be reported via PEX")
	require.Len(t, got, 1, "an incoming connection that never announced a listening port must be omitted, not reported with a wrong address")
}
