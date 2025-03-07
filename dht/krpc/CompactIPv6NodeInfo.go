package krpc

import (
	"net/netip"

	"github.com/anacrolix/missinggo/slices"
)

type (
	CompactIPv6NodeInfo []NodeInfo
)

func (CompactIPv6NodeInfo) ElemSize() int {
	return 38
}

func (me CompactIPv6NodeInfo) MarshalBinary() ([]byte, error) {
	return marshalBinarySlice(slices.Map(func(ni NodeInfo) NodeInfo {
		ni.Addr = NewNodeAddrFromAddrPort(netip.AddrPortFrom(netip.AddrFrom16(ni.Addr.Addr().As16()), ni.Addr.Port()))
		return ni
	}, me).(CompactIPv6NodeInfo))
}

func (me CompactIPv6NodeInfo) MarshalBencode() ([]byte, error) {
	return bencodeBytesResult(me.MarshalBinary())
}

func (me *CompactIPv6NodeInfo) UnmarshalBinary(b []byte) error {
	return unmarshalBinarySlice(me, b)
}

func (me *CompactIPv6NodeInfo) UnmarshalBencode(b []byte) error {
	return unmarshalBencodedBinary(me, b)
}
