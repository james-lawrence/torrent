// Package bep0051 implements DHT infohash indexing https://www.bittorrent.org/beps/bep_0051.html
package bep0051

import (
	"context"
	"time"

	"github.com/james-lawrence/torrent/bencode"
	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/internal/errorsx"
)

const (
	Query  = "sample_infohashes"
	TTLMin = 0
	TTLMax = 21600
)

type Args struct {
	ID     krpc.ID `bencode:"id"`               // ID of the querying Node
	Target krpc.ID `bencode:"target,omitempty"` // ID of the node sought
}

type Request struct {
	Q string `bencode:"q,omitempty"` // sample_infohashes
	A Args   `bencode:"a,omitempty"`
	T string `bencode:"t"` // required: transaction ID
	Y string `bencode:"y"` // required: type of the message: q for QUERY, r for RESPONSE, e for ERROR
}

type Sample struct {
	ID        krpc.ID                  `bencode:"id"`               // ID of the sending node.
	Interval  uint                     `bencode:"interval"`         // ttl for the current sample to refresh.
	Available uint                     `bencode:"num"`              // total number of info hashes available for this node.
	Nodes     krpc.CompactIPv4NodeInfo `bencode:"nodes,omitempty"`  // K closest nodes to the requested target
	Nodes6    krpc.CompactIPv6NodeInfo `bencode:"nodes6,omitempty"` // K closest nodes to the requested target
	Sample    []byte                   `bencode:"samples"`          // sample infohashes
}

type Response struct {
	R Sample `bencode:"r"` // required sample info hashes
	T string `bencode:"t"` // required: transaction ID
	Y string `bencode:"y"` // required: type of the message: r for RESPONSE, e for ERROR
}

func NewRequest(from int160.T, to krpc.ID) (qi dht.QueryInput, err error) {
	req := Request{
		Y: krpc.YQuery,
		T: krpc.TimestampTransactionID(),
		Q: Query,
		A: Args{
			ID:     from.AsByteArray(),
			Target: to,
		},
	}

	encoded, err := bencode.Marshal(req)
	return dht.NewEncodedRequest(req.Q, req.T, encoded), err
}

type Sampler interface {
	Snapshot(max int) (ttl uint, total uint, sample []byte)
}

// provide a noop implementation that returns no hashes.
type EmptySampler struct{}

func (t EmptySampler) Snapshot(max int) (ttl uint, total uint, sample []byte) {
	return TTLMax, 0, []byte{}
}

func NewEndpoint(s Sampler) Endpoint {
	return Endpoint{s: s}
}

type Endpoint struct {
	s Sampler
}

func (t Endpoint) Handle(ctx context.Context, source dht.Addr, s *dht.Server, raw []byte, _ *krpc.Msg) error {
	var (
		m Request
	)

	if err := bencode.Unmarshal(raw, &m); err != nil {
		return err
	}

	ttl, total, sampled := t.s.Snapshot(128)

	msg := Response{
		T: m.T,
		R: Sample{
			ID:        s.ID().AsByteArray(),
			Interval:  ttl,
			Available: total,
			Sample:    sampled,
			Nodes:     s.MakeReturnNodes(int160.FromByteArray(m.A.Target), func(na krpc.NodeAddr) bool { return na.Addr().Is4() }),
			Nodes6:    s.MakeReturnNodes(int160.FromByteArray(m.A.Target), func(krpc.NodeAddr) bool { return true }),
		},
	}

	b, err := bencode.Marshal(msg)
	if err != nil {
		return err
	}

	_, err = s.SendToNode(ctx, b, source, 1)
	return err
}

func LatestSampleForNodeInfo(ctx context.Context, s *dht.Server, n krpc.NodeInfo) (*Sample, error) {
	var (
		resp Response
	)

	qi, err := NewRequest(s.ID(), n.ID)
	if err != nil {
		return nil, errorsx.Wrapf(err, "unable to generate sample request: %s", n.ID)
	}
	dst := dht.NewAddr(n.Addr.AddrPort)

	dctx, done := context.WithTimeout(ctx, 30*time.Second)
	defer done()

	ret := s.Query(dctx, dst, qi)
	if err := ret.Err; err != nil {
		return nil, errorsx.Wrap(err, "sample query failed")
	}

	if err := bencode.Unmarshal(ret.Raw, &resp); err != nil {
		if _, ok := err.(bencode.ErrUnusedTrailingBytes); !ok {
			return nil, errorsx.Wrapf(err, "unable to deserialize sample response: %T %s", err, n.ID)
		}
	}

	return &resp.R, nil
}
