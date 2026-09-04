package rendezvous

import (
	"crypto/md5"
	"io"
	"math/big"
	"net/netip"
	"sort"

	"github.com/gofrs/uuid/v5"
	"github.com/james-lawrence/torrent/internal/errorsx"
)

func Random[T any](v T) Paired[T] {
	bi := new(big.Int).Lsh(big.NewInt(1), 128)
	bi.SetBytes(uuid.Must(uuid.NewV4()).Bytes())
	return Paired[T]{N: v, Bi: bi}
}

func Seeded[T any](s io.Reader, v T) Paired[T] {
	var buf uuid.UUID
	bi := new(big.Int).Lsh(big.NewInt(1), uint(len(buf))*8)
	errorsx.Must(io.ReadFull(s, buf[:]))
	bi.SetBytes(buf[:])
	return Paired[T]{N: v, Bi: bi}
}

func SeededN(n uint16, s io.Reader) (res []Paired[uuid.UUID]) {
	res = make([]Paired[uuid.UUID], 0, n)
	for range n {
		var (
			buf uuid.UUID
		)

		bi := new(big.Int).Lsh(big.NewInt(1), uint(len(buf))*8)
		errorsx.Must(io.ReadFull(s, buf[:]))
		bi.SetBytes(buf[:])
		res = append(res, Paired[uuid.UUID]{N: buf, Bi: bi})
	}

	return res
}

func SeededAddr(s io.Reader) Paired[netip.Addr] {
	var buf [16]byte
	bi := new(big.Int).Lsh(big.NewInt(1), uint(len(buf))*8)
	errorsx.Must(io.ReadFull(s, buf[:]))
	bi.SetBytes(buf[:])
	return Paired[netip.Addr]{N: netip.AddrFrom16(buf), Bi: bi}
}

func SeededAddrN(n uint16, s io.Reader) (res []Paired[netip.Addr]) {
	res = make([]Paired[netip.Addr], 0, n)
	for range n {
		var buf [16]byte

		bi := new(big.Int).Lsh(big.NewInt(1), uint(len(buf))*8)
		errorsx.Must(io.ReadFull(s, buf[:]))
		bi.SetBytes(buf[:])
		res = append(res, Paired[netip.Addr]{N: netip.AddrFrom16(buf), Bi: bi})
	}

	return res
}

type Paired[T any] struct {
	N  T
	Bi *big.Int
}

// Compute computes the HRW for each node.
func Compute[T any](key []byte, d func(T) []byte, nodes ...T) []Paired[T] {
	results := make([]Paired[T], 0, len(nodes))
	for _, node := range nodes {
		h := md5.New()
		bi := big.NewInt(0)

		h.Write(d(node))
		h.Write(key)

		bi = bi.SetBytes(h.Sum(nil))
		results = append(results, Paired[T]{Bi: bi, N: node})
	}

	return results
}

// Max - finds the node with the highest hash for the given key.
func Max[T any](key []byte, d func(T) []byte, nodes ...T) (max T) {
	maxValue := big.NewInt(0)

	for _, p := range Compute(key, d, nodes...) {
		if p.Bi.Cmp(maxValue) == 1 {
			maxValue = p.Bi
			max = p.N
		}
	}

	return max
}

// MaxN - finds the node with the highest hash for the given key.
func MaxN[T any](n int, key []byte, d func(T) []byte, nodes ...T) []T {
	if n > len(nodes) {
		n = len(nodes)
	}

	results := make([]T, 0, n)
	peers := make([]Paired[T], 0, len(nodes))

	peers = append(peers, Compute(key, d, nodes...)...)

	sort.Slice(peers, func(i, j int) bool { return peers[i].Bi.Cmp(peers[j].Bi) == -1 })

	for _, p := range peers[:n] {
		results = append(results, p.N)
	}

	return results
}
