package dht

import (
	"time"

	"github.com/anacrolix/generics"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/dht/types"
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

func (n *node) hasAddrAndID(addr Addr, id int160.T) bool {
	return id == n.Id && n.Addr.AddrPort() == addr.AddrPort()
}

func (n *node) IsSecure() bool {
	return n.Id.IsSecure(n.Addr.AddrPort().Addr())
}

func (n *node) idString() string {
	return n.Id.ByteString()
}

func (n *node) NodeInfo() (ret krpc.NodeInfo) {
	return krpc.NodeInfo{
		Addr: n.Addr.KRPC(),
		ID:   n.Id.AsByteArray(),
	}
}

func (n *node) MaybeId() types.AddrMaybeId {
	return types.AddrMaybeId{
		Addr: n.Addr.KRPC(),
		Id:   generics.OptionFromTuple(n.Id, n.Id.IsZero()),
	}
}

func nodeclosest(target int160.T) func(a, b *node) int {
	return func(a, b *node) int {
		return int160.CmpTo(target, a.Id, b.Id)
	}
}
