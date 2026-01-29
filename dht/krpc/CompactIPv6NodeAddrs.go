package krpc

import "encoding/binary"

type CompactIPv6NodeAddrs []NodeAddr

func (CompactIPv6NodeAddrs) ElemSize() int { return 18 }

func (me CompactIPv6NodeAddrs) MarshalBinary() ([]byte, error) {
	ret := make([]byte, 0, len(me)*me.ElemSize())
	for _, na := range me {
		ip := na.Addr().As16()
		ret = append(ret, ip[:]...)
		ret = binary.BigEndian.AppendUint16(ret, na.Port())
	}
	return ret, nil
}

func (me CompactIPv6NodeAddrs) MarshalBencode() ([]byte, error) {
	return bencodeBytesResult(me.MarshalBinary())
}

func (me *CompactIPv6NodeAddrs) UnmarshalBinary(b []byte) error {
	return unmarshalBinarySlice(me, b)
}

func (me *CompactIPv6NodeAddrs) UnmarshalBencode(b []byte) error {
	return unmarshalBencodedBinary(me, b)
}

func (me CompactIPv6NodeAddrs) NodeAddrs() []NodeAddr {
	return me
}

func (me CompactIPv6NodeAddrs) Index(x NodeAddr) int {
	return addrIndex(me, x)
}
