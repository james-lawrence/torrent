package int160

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"hash/crc32"
	"io"
	"math"
	"math/big"
	"net/netip"

	"github.com/james-lawrence/torrent/internal/errorsx"
)

func Clone(b T) (ret T) {
	copy(ret.bits[:], b.bits[:])
	return
}

func New[Y string | []byte](b Y) (ret T) {
	v := sha1.Sum([]byte(b))
	copy(ret.bits[:], v[:])
	return
}

func RandomPrefixed[X ~[]byte | ~string](b X) (ret T, err error) {
	var buf [20]byte
	o := copy(buf[:], b)
	if _, err = rand.Read(buf[o:]); err != nil {
		return ret, errorsx.Wrap(err, "error generating int160")
	}

	return FromByteArray(buf), nil
}

func Random() (id T) {
	n, err := rand.Read(id.bits[:])
	if err != nil {
		panic(err)
	}
	if n < len(id.bits[:]) {
		panic(io.ErrShortWrite)
	}

	return id
}

func Zero() (id T) {
	id.bits = [20]byte{}
	return id
}

func Max() (id T) {
	id.bits = [20]byte{
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
		math.MaxUint8,
	}
	return id
}

func Not(a T) (ret T) {
	const mask byte = 0xFF
	for i := range len(ret.bits) {
		ret.bits[i] = a.bits[i] ^ mask
	}

	return ret
}

func Closest(target T, values ...T) (ret T) {
	farthest := Not(target)
	closest := Not(target)
	for _, v := range values {
		var (
			a, b T
		)
		a.Xor(&target, &v)
		b.Xor(&target, &closest)

		if a.Cmp(b) < 0 {
			closest = v
		}
	}

	if closest.Equal(farthest) {
		return Zero()
	}

	return closest
}

// compare a and b using the target.
// returns -1 is a is closer to target.
// return 0 if they are equal distance.
// return 1 if b is closer to target.
func CmpTo(target T, a T, b T) int {
	return target.Distance(a).Cmp(target.Distance(b))
}

type T struct {
	bits [20]uint8
}

func (me T) String() string {
	return hex.EncodeToString(me.bits[:])
}

func (me T) AsByteArray() [20]byte {
	return me.bits
}

func (me T) ByteString() string {
	return string(me.bits[:])
}

func (me T) BitLen() int {
	var a big.Int
	a.SetBytes(me.bits[:])
	return a.BitLen()
}

func (me *T) SetBytes(b []byte) {
	n := copy(me.bits[:], b)
	if n != 20 {
		panic(n)
	}
}

func (me *T) SetBit(index int, val bool) {
	var orVal uint8
	if val {
		orVal = 1 << (7 - index%8)
	}
	var mask uint8 = ^(1 << (7 - index%8))
	me.bits[index/8] = me.bits[index/8]&mask | orVal
}

func (me *T) GetBit(index int) bool {
	return me.bits[index/8]>>(7-index%8)&1 == 1
}

func (me T) Bytes() []byte {
	return me.bits[:]
}

func (l T) Cmp(r T) int {
	return bytes.Compare(l.bits[:], r.bits[:])
}

func (l T) Equal(r T) bool {
	return l.Cmp(r) == 0
}

func (me T) IsZero() bool {
	for _, b := range me.bits {
		if b != 0 {
			return false
		}
	}
	return true
}

func (me *T) Xor(a, b *T) *T {
	for i := range me.bits {
		me.bits[i] = a.bits[i] ^ b.bits[i]
	}

	return me
}

func (me T) Prefix(b []byte) T {
	var buf [20]byte
	o := copy(buf[:], []byte(b))
	copy(buf[o:], me.bits[o:])
	return FromByteArray(buf)
}

func (a T) Distance(b T) (ret T) {
	ret.Xor(&a, &b)
	return
}

func FromHashedBytes(b []byte) (ret T) {
	hasher := sha1.New()
	hasher.Write(b)
	copy(ret.bits[:], hasher.Sum(nil))
	return ret
}

func ByteArray(id T) [20]byte {
	return id.bits
}

func FromBytes(b []byte) (ret T) {
	ret.SetBytes(b)
	return
}

func FromByteArray(b [20]byte) (ret T) {
	ret.SetBytes(b[:])
	return
}

func FromBytesOrZero(b []byte) T {
	if len(b) == 20 {
		return FromBytes(b)
	}

	return Zero()
}

func FromByteString(s string) (ret T) {
	ret.SetBytes([]byte(s))
	return
}

func FromHexEncodedString(s string) (ret T, err error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return ret, err
	}
	return FromBytes(b), nil
}

// Secure returns a new T with the first 3 bytes re-derived from addr per BEP42.
// The stable portion (id[3..19]) is preserved.
func (me T) Secure(addr netip.Addr) T {
	id := me.AsByteArray()
	crc := crcAddr(addr, id[19])
	id[0] = byte(crc >> 24 & 0xff)
	id[1] = byte(crc >> 16 & 0xff)
	id[2] = byte(crc>>8&0xf8) | id[2]&7
	return FromByteArray(id)
}

// IsSecure returns whether the ID is valid for addr per BEP42.
// Local-network addresses are always considered secure.
func (me T) IsSecure(addr netip.Addr) bool {
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return true
	}
	crc := crcAddr(addr, me.bits[19])
	return me.bits[0] == byte(crc>>24&0xff) &&
		me.bits[1] == byte(crc>>16&0xff) &&
		me.bits[2]&0xf8 == byte(crc>>8&0xf8)
}

// StableSuffix returns an int160 whose first 3 bytes are zeroed and whose
// remaining 17 bytes (id[3..19]) are the stable portion preserved by Secure.
func StableSuffix(id T) T {
	var s T
	copy(s.bits[3:], id.bits[3:])
	return s
}

func crcAddr(addr netip.Addr, rand uint8) uint32 {
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	var ip []byte
	var mask []byte
	if addr.Is4() {
		a := addr.As4()
		ip = []byte{a[0], a[1], a[2], a[3]}
		mask = []byte{0x03, 0x0f, 0x3f, 0xff}
	} else {
		a := addr.As16()
		ip = []byte{a[0], a[1], a[2], a[3], a[4], a[5], a[6], a[7]}
		mask = []byte{0x01, 0x03, 0x07, 0x0f, 0x1f, 0x3f, 0x7f, 0xff}
	}
	for i := range mask {
		ip[i] &= mask[i]
	}
	r := rand & 7
	ip[0] |= r << 5
	return crc32.Checksum(ip, crc32.MakeTable(crc32.Castagnoli))
}
