package krpc

import "encoding/binary"

type (
	CompactIPv6NodeInfo []NodeInfo
)

func (CompactIPv6NodeInfo) ElemSize() int {
	return 38
}

func (me CompactIPv6NodeInfo) MarshalBinary() ([]byte, error) {
	ret := make([]byte, 0, len(me)*me.ElemSize())
	for _, ni := range me {
		ip := ni.Addr.Addr().As16()
		ret = append(ret, ni.ID[:]...)
		ret = append(ret, ip[:]...)
		ret = binary.BigEndian.AppendUint16(ret, ni.Addr.Port())
	}
	return ret, nil
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
