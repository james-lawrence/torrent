package iplist

import "net"

func Zero() *zero {
	return &zero{}
}

type zero struct{}

// Return a Range containing the IP.
func (t *zero) Lookup(net.IP) (r Range, ok bool) {
	return Range{}, false
}

func (t *zero) NumRanges() int {
	return 0
}
