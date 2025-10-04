package torrent

import (
	"net"
	"net/netip"

	"github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/internal/langx"
	"github.com/james-lawrence/torrent/internal/netx"
)

type PeerOption func(*Peer)

func PeerOptionSource(src peerSource) PeerOption {
	return func(p *Peer) {
		p.Source = src
	}
}

func PeerOptionEncrypted(b bool) PeerOption {
	return func(p *Peer) {
		p.SupportsEncryption = b
	}
}

func PeerOptionTrusted(b bool) PeerOption {
	return func(p *Peer) {
		p.Trusted = b
	}
}

func NewPeerDeprecated(id int160.T, ip net.IP, port int, options ...PeerOption) Peer {
	return NewPeer(id, netip.AddrPortFrom(netx.AddrFromIP(ip), uint16(port)), options...)
}

func NewPeer(id int160.T, addr netip.AddrPort, options ...PeerOption) Peer {
	return langx.Clone(Peer{
		ID:       id,
		AddrPort: addr,
	}, options...)
}

// Peer connection info, handed about publicly.
type Peer struct {
	ID int160.T
	netip.AddrPort
	btprotocol.PexPeerFlags
	Source             peerSource
	SupportsEncryption bool // Peer is known to support encryption.
	Trusted            bool // Whether we can ignore poor or bad behaviour from the peer.
}

// FromPex generate Peer from peer exchange
func (me *Peer) FromPex(na krpc.NodeAddr, fs btprotocol.PexPeerFlags) {
	me.AddrPort = na.AddrPort
	me.Source = peerSourcePex
	// If they prefer encryption, they must support it.
	if fs.Get(btprotocol.PexPrefersEncryption) {
		me.SupportsEncryption = true
	}
	me.PexPeerFlags = fs
}
