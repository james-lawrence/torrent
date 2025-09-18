package torrent

import (
	"net/netip"
	"testing"

	"github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/dht/krpc"
)

// BenchmarkPeersAppendFromPex benchmarks the original AppendFromPex implementation
func BenchmarkPeersAppendFromPex(b *testing.B) {
	const numPeers = 1000

	nas := make([]krpc.NodeAddr, numPeers)
	fs := make([]btprotocol.PexPeerFlags, numPeers)

	for i := 0; i < numPeers; i++ {
		addr := netip.AddrPortFrom(netip.AddrFrom4([4]byte{192, 168, 1, byte(i % 255)}), 6881)
		nas[i] = krpc.NewNodeAddrFromAddrPort(addr)
		fs[i] = btprotocol.PexPeerFlags(i % 8)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		var peers Peers
		peers.AppendFromPex(nas, fs)
	}
}

