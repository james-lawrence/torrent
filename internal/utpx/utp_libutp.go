//go:build cgo && !disable_libutp
// +build cgo,!disable_libutp

package utpx

import (
	"time"

	utp "github.com/anacrolix/go-libutp"
)

// grace is how long a socket stays open once asked to close. closing a connection only queues
// its FIN when the send window is full, and destroying the socket discards anything unsent,
// leaving the peer holding a dead connection until it times out.
const grace = 200 * time.Millisecond

// New ...
func New(network, addr string) (Socket, error) {
	s, err := utp.NewSocket(network, addr)
	if s == nil {
		return nil, err
	}

	return ungraceful{Socket: s}, err
}

// ungraceful gives the closing packets of its connections time to leave before the socket is destroyed.
type ungraceful struct {
	*utp.Socket
}

func (t ungraceful) Close() error {
	time.Sleep(grace)
	return t.Socket.Close()
}
