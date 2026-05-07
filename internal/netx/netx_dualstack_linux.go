//go:build linux

package netx

import (
	"net"
	"syscall"
)

// IsIPv6DualStack checks whether the given packet conn is an IPv6 socket
// that also accepts IPv4 traffic (dual-stack / IPV6_V6ONLY=0).
// Returns false for non-UDP connections or if the check fails.
func IsIPv6DualStack(pc net.PacketConn) bool {
	if pc == nil {
		return false
	}
	udp, ok := pc.(*net.UDPConn)
	if !ok {
		return false
	}
	conn, err := udp.SyscallConn()
	if err != nil {
		return false
	}
	var v6Only int
	ctrlErr := conn.Control(func(fd uintptr) {
		v6Only, err = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY)
	})
	if ctrlErr != nil {
		return false
	}
	if err != nil {
		// ENOPROTOOPT or EINVAL means the option is not supported.
		return false
	}
	// v6Only == 0 means dual-stack is enabled.
	return v6Only == 0
}
