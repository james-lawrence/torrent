package transactions

import "net/netip"

// Matches both ID and addr to transactions.
type Key struct {
	// The KRPC transaction ID.
	T Id
	// The remote address of the transaction.
	RemoteAddr netip.AddrPort
}

// Transaction key type, probably should match whatever is used in KRPC messages for the `t` field.
type Id = string
