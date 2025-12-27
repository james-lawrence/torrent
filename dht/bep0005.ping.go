package dht

import (
	"context"
	"time"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
)

func NewPingRequest(from int160.T) (qi QueryInput, err error) {
	return NewMessageRequest(
		"ping",
		&krpc.MsgArgs{
			ID: from.AsByteArray(),
		},
	)
}

func Ping(ctx context.Context, q Queryer, to Addr, from int160.T) QueryResult {
	qi, err := NewPingRequest(from)
	if err != nil {
		return NewQueryResultErr(err)
	}

	return q.Query(ctx, to, qi)
}

func Ping3S(ctx context.Context, q Queryer, to Addr, from int160.T) QueryResult {
	return PingDuration(ctx, 3*time.Second, q, to, from)
}

func PingDuration(ctx context.Context, d time.Duration, q Queryer, to Addr, from int160.T) QueryResult {
	ctx, done := context.WithTimeout(ctx, d)
	defer done()
	return Ping(ctx, q, to, from)
}
