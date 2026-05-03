package dht

import (
	"context"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/dht/traversal2"
)

func NewTraversalQuerier(id int160.T, s Queryer) traversal2.Querier {
	return querier{id: id, s: s}
}

type querier struct {
	id int160.T
	s  Queryer
}

func (q querier) Query(ctx context.Context, addr krpc.NodeAddr, target int160.T, scrape bool) traversal2.QueryResult {
	res := FindPeers(ctx, q.s, NewAddr(addr.AddrPort), q.id.AsByteArray(), target.AsByteArray(), scrape)
	r := res.Reply.R

	if r == nil {
		return traversal2.QueryResult{}
	}

	return traversal2.QueryResult{
		IP:           res.Reply.IP.AddrPort,
		ResponseFrom: &krpc.NodeInfo{Addr: addr, ID: r.ID},
		Nodes:        append(r.Nodes6, r.Nodes...),
		Peers:        r.Values,
	}
}
