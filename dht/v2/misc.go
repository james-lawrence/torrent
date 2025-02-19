package dht

import (
	"fmt"
	"hash/fnv"

	"github.com/anacrolix/missinggo/v2"
	"github.com/james-lawrence/torrent/internal/stmutil"

	"github.com/james-lawrence/torrent/dht/v2/krpc"
)

type addrMaybeId struct {
	Addr krpc.NodeAddr
	Id   *Int160
}

func (me addrMaybeId) String() string {
	if me.Id == nil {
		return fmt.Sprintf("unknown id at %s", me.Addr)
	} else {
		return fmt.Sprintf("%x at %v", *me.Id, me.Addr)
	}
}

func nodesByDistance(target Int160) stmutil.Settish[addrMaybeId] {
	return stmutil.NewSortedSet(func(l, r addrMaybeId) bool {
		var ml missinggo.MultiLess
		ml.NextBool(r.Id == nil, l.Id == nil)
		if l.Id != nil && r.Id != nil {
			d := distance(*l.Id, target).Cmp(distance(*r.Id, target))
			ml.StrictNext(d == 0, d < 0)
		}
		// TODO: Use bytes/hash when it's available (go1.14?), and have a unique seed for each
		// instance.
		hashString := func(s string) uint64 {
			h := fnv.New64a()
			h.Write([]byte(s))
			return h.Sum64()
		}
		lh := hashString(l.Addr.String())
		rh := hashString(r.Addr.String())
		ml.StrictNext(lh == rh, lh < rh)
		//ml.StrictNext(l.Addr.String() == r.Addr.String(), l.Addr.String() < r.Addr.String())
		return ml.Less()
	})
}
