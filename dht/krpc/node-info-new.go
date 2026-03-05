package krpc

import (
	"bytes"
	crand "crypto/rand"
	"encoding"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"net"

	"github.com/james-lawrence/torrent/internal/errorsx"
)

// This is a comparable replacement for NodeInfo.
type NodeInfo struct {
	ID   ID
	Addr NodeAddr
}

func NewInfo(id ID, addr NodeAddr) NodeInfo {
	return NodeInfo{
		ID:   id,
		Addr: addr,
	}
}

func RandomNodeInfo(ipLen int) (ni NodeInfo) {
	tmp := make(net.IP, ipLen)
	_, err := crand.Read(ni.ID[:])
	errorsx.Panic(err)
	_, err = crand.Read(tmp)
	errorsx.Panic(err)
	ni.Addr = NewNodeAddrFromIPPort(tmp, uint16(rand.IntN(math.MaxUint16+1)))
	return ni
}

var _ interface {
	encoding.BinaryMarshaler
	encoding.BinaryUnmarshaler
} = (*NodeInfo)(nil)

func (ni NodeInfo) MarshalBinary() ([]byte, error) {
	var w bytes.Buffer
	w.Write(ni.ID[:])
	w.Write(ni.Addr.IP())
	err := binary.Write(&w, binary.BigEndian, ni.Addr.Port())
	return w.Bytes(), err
}

func (ni *NodeInfo) UnmarshalBinary(b []byte) error {
	copy(ni.ID[:], b)
	return ni.Addr.UnmarshalBinary(b[20:])
}
