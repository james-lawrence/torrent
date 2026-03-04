package traversal2

import (
	"context"
	"net/netip"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
)

// Querier executes a query against a DHT node.
type Querier interface {
	Query(ctx context.Context, addr krpc.NodeAddr, target int160.T, scrape bool) QueryResult
}

// QueryResult contains the response from a DHT query.
type QueryResult struct {
	IP           netip.AddrPort
	ResponseFrom *krpc.NodeInfo
	Nodes        []krpc.NodeInfo
	Peers        []krpc.NodeAddr
}
