package connections

import (
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/james-lawrence/torrent/internal/errorsx"
)

// Firewall used to prevent connections.
type Firewall interface {
	Blocked(ip net.IP, port int) error
}

// FirewallStateful used when the firewall needs to be updated dynamically.
type FirewallStateful interface {
	Firewall
	Inhibit(ip net.IP, port int, cause error)
}

// NewBloomBanIP bans an IP address by adding to a bloom filter.
func NewBloomBanIP(d time.Duration) *BloomBanIP {
	return (&BloomBanIP{
		m:        &sync.Mutex{},
		duration: d,
		banned:   bloom.NewWithEstimates(10000, 0.5),
	}).reset()
}

// BloomBanIP bans an IP address by adding it to a bloom filter.
// BloomBanIP is stateful, and will track banned connections using a bloom filter.
type BloomBanIP struct {
	m           *sync.Mutex
	duration    time.Duration
	banned      *bloom.BloomFilter
	bannedReset time.Time
}

func (t *BloomBanIP) reset() *BloomBanIP {
	if time.Now().Before(t.bannedReset) {
		return t
	}

	t.m.Lock()
	defer t.m.Unlock()

	t.banned.ClearAll()
	t.bannedReset = time.Now().Add(t.duration)

	return t
}

// Blocked prevents banned connections from connecting for any reason until the timeout passes
func (t *BloomBanIP) Blocked(ip net.IP, p int) error {
	t.reset()

	if t.banned.Test(bloomkey(ip, p)) {
		return errorsx.Errorf("ip %s is banned", ip)
	}

	return nil
}

// Inhibit ban an IP address within the smallest 8 bit range.
func (t *BloomBanIP) Inhibit(ip net.IP, port int, cause error) {
	t.reset()

	t.banned.Add(bloomkey(ip, port))
}

// bloomkey derives the key an address is banned under. loopback addresses are
// keyed by address and port: every peer on a loopback network shares a block,
// and frequently the address itself, so banning the block would take out every
// other local peer along with the offender. private addresses are keyed by the
// exact address, anything else by the smallest 8 bit range.
//
// Blocked and Inhibit have to derive the key the same way, otherwise an
// address is banned under one key and looked up under another.
func bloomkey(ip net.IP, port int) []byte {
	v16 := ip.To16()
	if v16 == nil {
		return maskLower8Bits(ip)
	}

	// normalize before keying: the 4 byte and 4in6 forms of an address name
	// the same peer, but they mask to different byte slices and a 4in6
	// address reports neither loopback nor private.
	addr := netip.AddrFrom16([16]byte(v16)).Unmap()
	normalized := net.IP(addr.AsSlice())

	switch {
	case addr.IsLoopback():
		return binary.BigEndian.AppendUint16(normalized, uint16(port))
	case addr.IsPrivate():
		return normalized
	default:
		return maskLower8Bits(normalized)
	}
}

// BanIPv6 ban IPv6 addresses
type BanIPv6 struct{}

// Blocked prevents connections from IPv6 addresses.
func (BanIPv6) Blocked(ip net.IP, p int) error {
	if len(ip) == net.IPv6len && ip.To4() == nil {
		return errorsx.New("ipv6 disabled")
	}

	return nil
}

// BanIPv4 ban IPv4 addresses
type BanIPv4 struct{}

// Blocked prevents connections from IPv4 addresses.
func (BanIPv4) Blocked(ip net.IP, port int) error {
	if ip.To4() != nil {
		return errorsx.New("ipv4 peers disabled")
	}

	if len(ip) == net.IPv4len {
		return errorsx.New("ipv4 disabled")
	}

	return nil
}

// BanInvalidPort blocks connections with invalid port values.
type BanInvalidPort struct{}

func (BanInvalidPort) Blocked(ip net.IP, port int) error {
	if port <= 0 {
		return errorsx.New("invalid port")
	}

	return nil
}

type Private struct{}

func (t Private) Blocked(ip net.IP, port int) error {
	addr := netip.AddrFrom16([16]byte(ip.To16()))
	if !addr.IsPrivate() {
		return errorsx.Errorf("public network %s - %s", ip, addr)
	}

	return nil
}

type composedfirewall struct {
	firewalls []Firewall
}

func (t composedfirewall) Blocked(ip net.IP, port int) error {
	for _, fwall := range t.firewalls {
		if err := fwall.Blocked(ip, port); err != nil {
			return err
		}
	}

	return nil
}

func (t composedfirewall) Inhibit(ip net.IP, port int, cause error) {
	for _, fwall := range t.firewalls {
		if fwall, ok := fwall.(FirewallStateful); ok {
			fwall.Inhibit(ip, port, cause)
		}
	}
}

// NewFirewall compose multiple firewalls into a single firewall.
func NewFirewall(rules ...Firewall) FirewallStateful {
	return composedfirewall{firewalls: rules}
}

// AutoFirewall reasonable default firewall settings.
func AutoFirewall() FirewallStateful {
	return NewFirewall(
		BanInvalidPort{},
		NewBloomBanIP(10*time.Minute),
	)
}

// maskLower8Bits returns a new IP address with the lower 8 masked.
// this allows for banning ip's within a block safely.
func maskLower8Bits(ip net.IP) net.IP {
	bits := len(ip) * 8
	return ip.Mask(net.CIDRMask(bits-8, bits))
}
