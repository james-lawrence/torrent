package krpc

import (
	"crypto/rand"
	"fmt"

	"github.com/james-lawrence/torrent/bencode"
)

type ID [20]byte

var (
	_ interface {
		bencode.Unmarshaler
	} = (*ID)(nil)
	_ bencode.Marshaler = ID{}
)

func IdFromString(s string) (id ID) {
	if n := copy(id[:], s); n != 20 {
		panic(n)
	}
	return
}

// generate a random id.
func RandomID() (id ID) {
	_, err := rand.Read(id[:])
	if err != nil {
		panic(err)
	}
	return id
}

func (id ID) MarshalBencode() ([]byte, error) {
	return []byte("20:" + string(id[:])), nil
}

func (id *ID) UnmarshalBencode(b []byte) error {
	var s string
	if err := bencode.Unmarshal(b, &s); err != nil {
		return err
	}
	if n := copy(id[:], s); n != 20 {
		return fmt.Errorf("string has wrong length: %d", n)
	}
	return nil
}
