package dht

import (
	"net"
	"testing"

	"github.com/james-lawrence/torrent/dht/int160"
)


// BenchmarkBucketNodeIter benchmarks the NodeIter implementation
func BenchmarkBucketNodeIter(b *testing.B) {
	bucket := &bucket{}

	for i := 0; i < 100; i++ {
		n := &node{
			nodeKey: nodeKey{
				Id:   int160.FromByteArray([20]byte{byte(i), byte(i >> 8), byte(i >> 16)}),
				Addr: NewAddr(&net.UDPAddr{IP: net.IPv4(192, 168, 1, byte(i)), Port: 6881}),
			},
		}
		bucket.AddNode(n, 100)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		count := 0
		for n := range bucket.NodeIter() {
			count++
			if n == nil {
				break
			}
		}
	}
}