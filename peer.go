package torrent

import (
	"fmt"
	"log"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

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

func NewPeerDeprecated(id int160.T, ip net.IP, port uint16, options ...PeerOption) Peer {
	return NewPeer(id, netip.AddrPortFrom(netx.AddrFromIP(ip), port), options...)
}

func NewPeer(id int160.T, addr netip.AddrPort, options ...PeerOption) Peer {
	return langx.Clone(Peer{
		ID:          id,
		AddrPort:    addr,
		LastAttempt: time.Now(),
	}, options...)
}

// generate a peer from a uri in the form of:
// p0000000000000000000000000000000000000000://127.0.0.1:3196
// aka: p{nodeID}://ip:port
func NewPeersFromURI(uris ...url.URL) (peers []Peer) {
	peers = make([]Peer, 0, len(uris))
	for _, uri := range uris {
		id, err := int160.FromHexEncodedString(strings.TrimPrefix(uri.Scheme, "p"))
		if err != nil {
			log.Println("failed to create peer from", uri, "invalid id, expected hex encoding similar to", fmt.Sprintf("p%s", int160.Zero()))
			continue
		}

		addrport, err := netip.ParseAddrPort(uri.Host)
		if err != nil {
			log.Println("failed to create peer from", uri, "invalid host/port must be in the form ip:port", uri.Host)
			continue
		}

		peers = append(peers, NewPeer(id, addrport))
	}

	return peers
}

// Peer connection info, handed about publicly.
type Peer struct {
	ID int160.T
	netip.AddrPort
	btprotocol.PexPeerFlags
	LastAttempt        time.Time
	Attempts           uint64
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
