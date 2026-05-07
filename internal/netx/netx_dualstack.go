//go:build !linux

package netx

import "net"

func IsIPv6DualStack(pc net.PacketConn) bool {
	return false
}
