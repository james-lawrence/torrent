package dht

import (
	"context"
	"net"

	"github.com/james-lawrence/torrent/dht/int160"
)

func mustListen(addr string) net.PacketConn {
	ret, err := net.ListenPacket("udp", addr)
	if err != nil {
		panic(err)
	}
	return ret
}

func addrResolver(addr string) func(context.Context, dnscacher) ([]Addr, error) {
	return func(context.Context, dnscacher) ([]Addr, error) {
		ua, err := net.ResolveUDPAddr("udp", addr)
		return []Addr{NewAddr(ua.AddrPort())}, err
	}
}

func randomIdInBucket(rootId int160.T, bucketIndex int) int160.T {
	id := int160.Random()
	for i := range bucketIndex {
		id.SetBit(i, rootId.GetBit(i))
	}
	id.SetBit(bucketIndex, !rootId.GetBit(bucketIndex))
	return id
}
