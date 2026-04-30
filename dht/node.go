package dht

import (
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/internal/langx"
)

type nodeKey struct {
	Addr Addr
	Id   int160.T
}

type node struct {
	nodeKey

	lastGotQuery    time.Time // From the remote node
	lastGotResponse time.Time // From the remote node

	numReceivesFrom            int
	failedLastQuestionablePing bool
}

func (s *Server) IsQuestionable(n *node) bool {
	return !s.IsGood(n) && !nodeIsBad(langx.Zero(s.id.Load()), n)
}

func (n *node) hasAddrAndID(addr Addr, id int160.T) bool {
	return id == n.Id && n.Addr.AddrPort() == addr.AddrPort()
}

func (n *node) IsSecure() bool {
	return NodeIdSecure(n.Id.AsByteArray(), n.Addr.IP())
}

func (n *node) idString() string {
	return n.Id.ByteString()
}

func (n *node) NodeInfo() (ret krpc.NodeInfo) {
	ret.Addr = n.Addr.KRPC()
	if n := copy(ret.ID[:], n.idString()); n != 20 {
		panic(n)
	}
	return
}

func nodeclosest(target int160.T) func(a, b *node) int {
	return func(a, b *node) int {
		return int160.CmpTo(target, a.Id, b.Id)
	}
}

// Per the spec in BEP 5.
func (s *Server) IsGood(n *node) bool {
	if nodeIsBad(langx.Zero(s.id.Load()), n) {
		return false
	}
	return time.Since(n.lastGotResponse) < 15*time.Minute ||
		!n.lastGotResponse.IsZero() && time.Since(n.lastGotQuery) < 15*time.Minute
}
