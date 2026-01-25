package connections

import (
	"fmt"
	"net"
)

// NewBanned bans a connection.
func NewBanned(c net.Conn, silent bool, cause error) error {
	return bannedConnection{
		conn:  c,
		cause: cause,
	}
}

type bannedConnection struct {
	conn   net.Conn
	cause  error
	silent bool
}

func (t bannedConnection) Unwrap() error {
	return t.cause
}

func (t bannedConnection) Error() string {
	return fmt.Sprintf("banned connection %s - %s: %s", t.conn.LocalAddr().String(), t.conn.RemoteAddr().String(), t.cause)
}
