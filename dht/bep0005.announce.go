package dht

import (
	"context"
	"errors"

	"github.com/james-lawrence/torrent/dht/krpc"
)

func NewAnnouncePeerRequest(from krpc.ID, id krpc.ID, port uint16, token string, impliedPort bool) (qi QueryInput, err error) {
	if port == 0 && !impliedPort {
		return qi, errors.New("no port specified")
	}

	return NewMessageRequest(
		"announce_peer",
		&krpc.MsgArgs{
			ID:          from,
			ImpliedPort: impliedPort,
			InfoHash:    id,
			Port:        &port,
			Token:       token,
		},
	)
}

func AnnouncePeerQuery(ctx context.Context, q Queryer, to Addr, from krpc.ID, id krpc.ID, port uint16, token string, impliedPort bool) QueryResult {
	qi, err := NewAnnouncePeerRequest(from, id, port, token, impliedPort)
	if err != nil {
		return NewQueryResultErr(err)
	}

	return q.Query(ctx, to, qi)
}
