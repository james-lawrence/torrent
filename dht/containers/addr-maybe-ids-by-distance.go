package containers

import (
	"github.com/anacrolix/missinggo/v2/iter"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/types"
	"github.com/james-lawrence/torrent/internal/stmutil"
)

type addrMaybeId = types.AddrMaybeId

type AddrMaybeIdsByDistance interface {
	Add(addrMaybeId) AddrMaybeIdsByDistance
	Next() addrMaybeId
	Delete(addrMaybeId) AddrMaybeIdsByDistance
	Len() int
}

type stmSettishWrapper struct {
	set stmutil.Settish[addrMaybeId]
}

func (me stmSettishWrapper) Next() addrMaybeId {
	first, _ := iter.First(me.set.Iter)
	return first.(addrMaybeId)
}

func (me stmSettishWrapper) Delete(x addrMaybeId) AddrMaybeIdsByDistance {
	return stmSettishWrapper{me.set.Delete(x)}
}

func (me stmSettishWrapper) Len() int {
	return me.set.Len()
}

func (me stmSettishWrapper) Add(x addrMaybeId) AddrMaybeIdsByDistance {
	return stmSettishWrapper{me.set.Add(x)}
}

func NewImmutableAddrMaybeIdsByDistance(target int160.T) AddrMaybeIdsByDistance {
	return stmSettishWrapper{stmutil.NewSortedSet[addrMaybeId](func(l, r addrMaybeId) bool {
		return l.CloserThan(r, target)
	})}
}
