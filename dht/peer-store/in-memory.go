package peer_store

import (
	"cmp"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/internal/multiless"
)

type InMemory struct {
	// This is used for sorting infohashes by distance in WriteDebug.
	RootId int160.T
	mu     sync.RWMutex
	index  map[InfoHash]indexValue
}

type indexValue = map[netip.Addr]NodeAndTime

type debugWriterInterface interface {
	WriteDebug(w io.Writer)
}

var _ interface {
	debugWriterInterface
} = (*InMemory)(nil)

func (me *InMemory) GetPeers(ih InfoHash) (ret []krpc.NodeAddr) {
	me.mu.RLock()
	defer me.mu.RUnlock()
	for _, v := range me.index[ih] {
		ret = append(ret, v.NodeAddr)
	}
	return
}

func (me *InMemory) AddPeer(ih InfoHash, na krpc.NodeAddr) {
	key := na.Addr()
	me.mu.Lock()
	defer me.mu.Unlock()
	if me.index == nil {
		me.index = make(map[InfoHash]indexValue)
	}
	nodes := me.index[ih]
	if nodes == nil {
		nodes = make(indexValue)
		me.index[ih] = nodes
	}
	nodes[key] = NodeAndTime{na, time.Now()}
}

type NodeAndTime struct {
	krpc.NodeAddr
	time.Time
}

func (me *InMemory) GetAll() (ret map[InfoHash][]NodeAndTime) {
	me.mu.RLock()
	defer me.mu.RUnlock()
	ret = make(map[InfoHash][]NodeAndTime, len(me.index))
	for ih, nodes := range me.index {
		for _, v := range nodes {
			ret[ih] = append(ret[ih], v)
		}
	}
	return
}

func (me *InMemory) WriteDebug(w io.Writer) {
	all := me.GetAll()
	var totalCount int
	type sliceElem struct {
		InfoHash
		addrs []NodeAndTime
	}
	var allSlice []sliceElem
	for ih, addrs := range all {
		totalCount += len(addrs)
		allSlice = append(allSlice, sliceElem{ih, addrs})
	}
	fmt.Fprintf(w, "total count: %v\n\n", totalCount)
	slices.SortStableFunc(allSlice, func(a, b sliceElem) int {
		da := int160.FromByteArray(a.InfoHash).Distance(me.RootId)
		db := int160.FromByteArray(b.InfoHash).Distance(me.RootId)
		return da.Cmp(db)
	})

	for _, elem := range allSlice {
		addrs := elem.addrs
		fmt.Fprintf(w, "%v (count %v):\n", elem.InfoHash, len(addrs))
		sort.Slice(addrs, func(i, j int) bool {
			var ml multiless.T
			ml.Compare(addrs[i].Addr().Compare(addrs[j].Addr()))
			ml.StrictNext(addrs[j].Time.UnixNano() == addrs[i].Time.UnixNano(),
				addrs[j].Time.UnixNano() < addrs[i].Time.UnixNano())
			ml.Compare(cmp.Compare(addrs[i].Port(), addrs[j].Port()))
			return ml.Final()
		})
		for _, na := range addrs {
			fmt.Fprintf(w, "\t%v (age: %v)\n", na.NodeAddr, time.Since(na.Time))
		}
	}
	fmt.Fprintln(w)
}
