package btprotocol

import (
	"bufio"
	"encoding/binary"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func BenchmarkDecodePieces(t *testing.B) {
	r, w := io.Pipe()
	const pieceLen = 1 << 14
	msg := Message{
		Type:  Piece,
		Index: 0,
		Begin: 1,
		Piece: make([]byte, pieceLen),
	}
	b, err := msg.MarshalBinary()
	require.NoError(t, err)
	t.SetBytes(int64(len(b)))
	defer r.Close()
	go func() {
		defer w.Close()
		for {
			n, err := w.Write(b)
			if err == io.ErrClosedPipe {
				return
			}
			require.NoError(t, err)
			require.Equal(t, len(b), n)
		}
	}()
	d := Decoder{
		// Emulate what package torrent's client would do.
		R:         bufio.NewReader(r),
		MaxLength: 1 << 18,
		Pool: &sync.Pool{
			New: func() interface{} {
				b := make([]byte, pieceLen)
				return &b
			},
		},
	}
	for range t.N {
		var msg Message
		require.NoError(t, d.Decode(&msg))
		// WWJD
		d.Pool.Put(&msg.Piece)
	}
}

func TestDecodeShortPieceEOF(t *testing.T) {
	r, w := io.Pipe()
	go func() {
		w.Write(Message{Type: Piece, Piece: make([]byte, 1)}.MustMarshalBinary())
		w.Close()
	}()
	d := Decoder{
		R:         bufio.NewReader(r),
		MaxLength: 1 << 15,
		Pool: &sync.Pool{New: func() interface{} {
			b := make([]byte, 2)
			return &b
		}},
	}
	var m Message
	require.NoError(t, d.Decode(&m))
	assert.Len(t, m.Piece, 1)
	assert.Equal(t, io.EOF, d.Decode(&m))
}

func TestDecodeOverlongPiece(t *testing.T) {
	r, w := io.Pipe()
	go func() {
		w.Write(Message{Type: Piece, Piece: make([]byte, 3)}.MustMarshalBinary())
		w.Close()
	}()
	d := Decoder{
		R:         bufio.NewReader(r),
		MaxLength: 1 << 15,
		Pool: &sync.Pool{New: func() interface{} {
			b := make([]byte, 2)
			return &b
		}},
	}
	var m Message
	require.Error(t, d.Decode(&m))
}
func BenchmarkDecodeBitfield(b *testing.B) {
	r, w := io.Pipe()
	const bitfieldLen = 128 * 1024
	bitfieldData := make([]byte, bitfieldLen)
	msgLen := uint32(1 + bitfieldLen)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], msgLen)
	data := append(lenBuf[:], byte(Bitfield))
	data = append(data, bitfieldData...)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	defer r.Close()
	go func() {
		defer w.Close()
		for {
			n, err := w.Write(data)
			if err == io.ErrClosedPipe {
				return
			}
			require.NoError(b, err)
			require.Equal(b, len(data), n)
		}
	}()
	d := Decoder{
		R:         bufio.NewReader(r),
		MaxLength: 1 << 18,
	}
	for i := 0; i < b.N; i++ {
		var msg Message
		require.NoError(b, d.Decode(&msg))
	}
}

func BenchmarkDecodeExtended(b *testing.B) {
	r, w := io.Pipe()
	const payloadLen = 128 * 1024
	payloadData := make([]byte, payloadLen)
	msgLen := uint32(1 + 1 + payloadLen)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], msgLen)
	data := append(lenBuf[:], byte(Extended))
	data = append(data, byte(0))
	data = append(data, payloadData...)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	defer r.Close()
	go func() {
		defer w.Close()
		for {
			n, err := w.Write(data)
			if err == io.ErrClosedPipe {
				return
			}
			require.NoError(b, err)
			require.Equal(b, len(data), n)
		}
	}()
	d := Decoder{
		R:         bufio.NewReader(r),
		MaxLength: 1 << 18,
	}
	for i := 0; i < b.N; i++ {
		var msg Message
		require.NoError(b, d.Decode(&msg))
	}
}
