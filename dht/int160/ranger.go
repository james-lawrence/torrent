package int160

import (
	"io"
	"net/netip"

	"github.com/james-lawrence/torrent/internal/cryptox"
	"github.com/james-lawrence/torrent/internal/rendezvous"
)

type dht interface {
	ID(addr netip.AddrPort) T
	DynamicAddrPort() netip.AddrPort
}

type Ranger interface {
	Generate() T
}

func NewRangeRandom() *RangerRandom {
	return &RangerRandom{}
}

type RangerRandom struct{}

func (t RangerRandom) Generate() T {
	return Random()
}

func NewRangeDynamic(s dht, n uint16) *RangerDynamic {
	stable := StableSuffix(s.ID(s.DynamicAddrPort()))
	src := cryptox.NewChaCha8(stable.String())
	return &RangerDynamic{
		stable: stable,
		ranges: rendezvous.SeededAddrN(n, src),
		src:    src,
	}
}

type RangerDynamic struct {
	stable T
	ranges []rendezvous.Paired[netip.Addr]
	src    io.Reader
}

func (t RangerDynamic) Ranges() []rendezvous.Paired[netip.Addr] {
	return t.ranges
}
func (t RangerDynamic) Stable() T {
	return t.stable
}

func (t RangerDynamic) Generate() T {
	var (
		buf [16]byte
	)

	_, _ = io.ReadFull(t.src, buf[:]) // ignore the error not the end of the world if we get one every now and then here.

	sampleip := rendezvous.Max(buf[:], func(n rendezvous.Paired[netip.Addr]) []byte { return n.Bi.Bytes() }, t.ranges...)
	target := t.stable.Secure(sampleip.N)

	return target
}

func NewRangeFixed(v T) *RangerFixed {
	return &RangerFixed{v: v}
}

type RangerFixed struct {
	v T
}

func (t RangerFixed) Generate() T {
	return t.v
}
