package torrent

import (
	"context"
	"iter"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime"
	"time"

	pp "github.com/james-lawrence/torrent/btprotocol"
	"github.com/james-lawrence/torrent/connections"
	"github.com/james-lawrence/torrent/dht"
	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/internal/bytesx"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/james-lawrence/torrent/internal/userx"
	"github.com/james-lawrence/torrent/metainfo"
	"github.com/james-lawrence/torrent/storage"
	"github.com/james-lawrence/torrent/tracker"
	"github.com/james-lawrence/torrent/x/conntrack"
	"golang.org/x/exp/constraints"
	"golang.org/x/time/rate"

	"github.com/james-lawrence/torrent/mse"
)

// DefaultHTTPUserAgent ...
const DefaultHTTPUserAgent = "Ghost-Torrent/1.0"

// ClientConfig not safe to modify this after it's given to a Client.
type ClientConfig struct {
	defaultCacheDirectory string
	defaultStorage        storage.ClientImpl
	defaultMetadata       MetadataStore
	extensionbits         pp.ExtensionBits // Our BitTorrent protocol extension bytes, sent in the BT handshakes.

	// defaultPortForwarding bool
	dynamicip func(ctx context.Context, c *Client) (iter.Seq[netip.AddrPort], error)

	UpnpID string

	dhtStartingNodes []func(network string) dht.StartingNodesGetter

	// Upload even after there's nothing in it for us. By default uploading is
	// not altruistic, we'll only upload to encourage the peer to reciprocate.
	Seed bool `long:"seed"`

	// Only applies to chunks uploaded to peers, to maintain responsiveness
	// communicating local Client state to peers. Each limiter token
	// represents one byte. The Limiter's burst must be large enough to fit a
	// whole chunk, which is usually 16 KiB (see TorrentSpec.ChunkSize).
	UploadRateLimiter *rate.Limiter
	// Rate limits all reads from connections to peers. Each limiter token
	// represents one byte. The Limiter's burst must be bigger than the
	// largest Read performed on a the underlying rate-limiting io.Reader
	// minus one. This is likely to be the larger of the main read loop buffer
	// (~4096), and the requested chunk size (~16KiB, see
	// TorrentSpec.ChunkSize).
	DownloadRateLimiter *rate.Limiter

	maximumOutstandingRequests int

	// Rate limit connection dialing
	dialRateLimiter *rate.Limiter
	// Number of dialing routines to run.
	dialPoolSize uint16

	// rate limit for accepting connections
	acceptRateLimiter *rate.Limiter

	bucketLimit int // maximum number of peers per bucket in the DHT.

	// User-provided Client peer ID. If not present, one is generated automatically.
	PeerID string

	HeaderObfuscationPolicy HeaderObfuscationPolicy
	// The crypto methods to offer when initiating connections with header obfuscation.
	CryptoProvides mse.CryptoMethod
	// Chooses the crypto method to use when receiving connections with header obfuscation.
	CryptoSelector mse.CryptoSelector

	DisableIPv4Peers bool
	Logger           logging // standard logging for errors, defaults to stderr
	Warn             logging // warn logging
	Debug            logging // debug logging, defaults to discard

	// HTTPProxy defines proxy for HTTP requests.
	// Format: func(*Request) (*url.URL, error),
	// or result of http.ProxyURL(HTTPProxy).
	// By default, it is composed from ClientConfig.ProxyURL,
	// if not set explicitly in ClientConfig struct
	HTTPProxy func(*http.Request) (*url.URL, error)
	// HTTPUserAgent changes default UserAgent for HTTP requests
	HTTPUserAgent string
	// Updated occasionally to when there's been some changes to client
	// behaviour in case other clients are assuming anything of us. See also
	// `bep20`.
	ExtendedHandshakeClientVersion string
	// Peer ID client identifier prefix. We'll update this occasionally to
	// reflect changes to client behaviour that other clients may depend on.
	// Also see `extendedHandshakeClientVersion`.
	Bep20 string

	// Peer dial timeout to use when there are limited peers.
	NominalDialTimeout time.Duration
	// Minimum peer dial timeout to use (even if we have lots of peers).
	MinDialTimeout          time.Duration
	HalfOpenConnsPerTorrent int

	// Maximum number of peer addresses in reserve.
	TorrentPeersHighWater int
	// Minumum number of peers before effort is made to obtain more peers.
	TorrentPeersLowWater int

	// Limit how long handshake can take. This is to reduce the lingering
	// impact of a few bad apples. 4s loses 1% of successful handshakes that
	// are obtained with 60s timeout, and 5% of unsuccessful handshakes.
	HandshakesTimeout time.Duration

	dialer netx.Dialer
	// The IP addresses as our peers should see them. May differ from the
	// local interfaces due to NAT or other network configurations.
	publicIP4 net.IP
	publicIP6 net.IP

	// Don't add connections that have the same peer ID as an existing
	// connection for a given Torrent.
	dropDuplicatePeerIds bool

	ConnTracker *conntrack.Instance

	connections.Handshaker

	// OnQuery hook func
	DHTOnQuery      func(query *krpc.Msg, source net.Addr) (propagate bool)
	DHTAnnouncePeer func(ih metainfo.Hash, ip net.IP, port int, portOk bool)
	DHTMuxer        dht.Muxer

	ConnectionClosed func(ih metainfo.Hash, stats ConnStats, remaining int)
	extensions       map[pp.ExtensionName]pp.ExtensionNumber
	localID          int160.T
}

func (cfg *ClientConfig) extension(id pp.ExtensionName) pp.ExtensionNumber {
	return cfg.extensions[id]
}

func (cfg *ClientConfig) Storage() storage.ClientImpl {
	return cfg.defaultStorage
}

func (cfg *ClientConfig) PublicIP4() net.IP {
	return cfg.publicIP4
}

func (cfg *ClientConfig) PublicIP6() net.IP {
	return cfg.publicIP6
}

func (cfg *ClientConfig) AnnounceRequest() tracker.Announce {
	return tracker.Announce{
		UserAgent: cfg.HTTPUserAgent,
		ClientIp4: krpc.NewNodeAddrFromIPPort(cfg.publicIP4, 0),
		ClientIp6: krpc.NewNodeAddrFromIPPort(cfg.publicIP6, 0),
		Dialer:    cfg.dialer,
	}
}

func (cfg *ClientConfig) errors() logging {
	return cfg.Logger
}

func (cfg *ClientConfig) warn() logging {
	return cfg.Warn
}

func (cfg *ClientConfig) info() logging {
	return cfg.Logger
}

func (cfg *ClientConfig) debug() logging {
	return cfg.Debug
}

// ClientConfigOption options for the client configuration
type ClientConfigOption func(*ClientConfig)

// useful for default noop configurations.
func ClientConfigNoop(c *ClientConfig) {}

func ClientConfigCompose(options ...ClientConfigOption) ClientConfigOption {
	return func(cc *ClientConfig) {
		for _, opt := range options {
			opt(cc)
		}
	}
}

func ClientConfigDialer(d netx.Dialer) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.dialer = d
	}
}

func ClientConfigPortForward(b bool) ClientConfigOption {
	if b {
		return ClientConfigDynamicIP(UPnPPortForward)
	} else {
		return ClientConfigDisableDynamicIP
	}
}

func ClientConfigDisableDynamicIP(cc *ClientConfig) {
	cc.dynamicip = nil
}

func ClientConfigDynamicIP(fn func(ctx context.Context, c *Client) (iter.Seq[netip.AddrPort], error)) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.dynamicip = fn
	}
}

func ClientConfigIPv4(ip string) ClientConfigOption {
	return func(cc *ClientConfig) {
		if len(ip) == 0 {
			return
		}

		cc.publicIP4 = net.ParseIP(ip)
	}
}

func ClientConfigIPv6(ip string) ClientConfigOption {
	return func(cc *ClientConfig) {
		if len(ip) == 0 {
			return
		}

		cc.publicIP6 = net.ParseIP(ip)
	}
}

func ClientConfigDialRateLimit(l *rate.Limiter) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.dialRateLimiter = l
	}
}

func ClientConfigBucketLimit(i int) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.bucketLimit = i
	}
}

// ClientConfigInfoLogger set the info logger
func ClientConfigInfoLogger(l logging) ClientConfigOption {
	return func(c *ClientConfig) {
		c.Logger = l
	}
}

func ClientConfigDebugLogger(l logging) ClientConfigOption {
	return func(c *ClientConfig) {
		c.Debug = l
	}
}

// ClientConfigSeed enable/disable seeding
func ClientConfigSeed(b bool) ClientConfigOption {
	return func(c *ClientConfig) {
		c.Seed = b
	}
}

func ClientConfigStorage(s storage.ClientImpl) ClientConfigOption {
	return func(c *ClientConfig) {
		c.defaultStorage = s
	}
}

func ClientConfigStorageDir(dir string) ClientConfigOption {
	return func(c *ClientConfig) {
		c.defaultStorage = storage.NewFile(dir)
		c.defaultMetadata = NewMetadataCache(dir)
	}
}

// configure what endpoints the dht's will support.
func ClientConfigMuxer(m dht.Muxer) ClientConfigOption {
	return func(c *ClientConfig) {
		c.DHTMuxer = m
	}
}

func ClientConfigPeerID(s string) ClientConfigOption {
	return func(c *ClientConfig) {
		c.PeerID = s
	}
}

// resets the set of bootstrap functions to an empty set.
func ClientConfigBootstrapNone(c *ClientConfig) {
	c.dhtStartingNodes = nil
}

func ClientConfigBootstrapFn(fn func(n string) dht.StartingNodesGetter) ClientConfigOption {
	return func(c *ClientConfig) {
		c.dhtStartingNodes = append(c.dhtStartingNodes, fn)
	}
}

func ClientConfigBootstrapGlobal(c *ClientConfig) {
	c.dhtStartingNodes = append(c.dhtStartingNodes, func(network string) dht.StartingNodesGetter {
		return func() ([]dht.Addr, error) { return dht.GlobalBootstrapAddrs(network) }
	})
}

// Bootstrap from a file written by dht.WriteNodesToFile
func ClientConfigBootstrapPeerFile(path string) ClientConfigOption {
	return ClientConfigBootstrapFn(func(n string) dht.StartingNodesGetter {
		return func() (res []dht.Addr, err error) {
			ps, err := dht.ReadNodesFromFile(path)
			if err != nil {
				return nil, err
			}

			for _, p := range ps {
				res = append(res, dht.NewAddr(p.Addr.UDP()))
			}

			return res, nil
		}
	})
}

func ClientConfigHTTPUserAgent(s string) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.HTTPUserAgent = s
	}
}

func ClientConfigConnectionClosed(fn func(ih metainfo.Hash, stats ConnStats, remaining int)) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.ConnectionClosed = fn
	}
}

func ClientConfigEnableEncryption(cc *ClientConfig) {
	cc.HeaderObfuscationPolicy = HeaderObfuscationPolicy{
		Preferred:        true,
		RequirePreferred: false,
	}
}

func ClientConfigFirewall(fw connections.FirewallStateful) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.Handshaker = connections.NewHandshaker(fw)
	}
}

func ClientConfigPEX(b bool) ClientConfigOption {
	return func(cc *ClientConfig) {
		if b {
			cc.extensions[pp.ExtensionNamePex] = pp.PEXExtendedID
		} else {
			delete(cc.extensions, pp.ExtensionNamePex)
		}
	}
}

func ClientConfigMetadata(b bool) ClientConfigOption {
	return func(cc *ClientConfig) {
		if b {
			cc.extensions[pp.ExtensionNameMetadata] = pp.PEXExtendedID
		} else {
			delete(cc.extensions, pp.ExtensionNameMetadata)
		}
	}
}

// change what extension bits are set.
func ClientConfigExtensionBits(bits ...uint) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.extensionbits = pp.NewExtensionBits(bits...)
	}
}
func ClientConfigMaxOutstandingRequests(n int) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.maximumOutstandingRequests = n
	}
}

func ClientConfigCacheDirectory(s string) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.defaultCacheDirectory = s
	}
}

// specify dialing timeouts healthy timeout for when the the torrent has sufficient peers connected.
// and starving when it lacks peers. defaults are 4 and 16 respectively. generally, when starving
// you'll want to be more forgiving and set a larger timeout.
func ClientConfigDialTimeouts(healthy, starving time.Duration) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.NominalDialTimeout = starving
		cc.MinDialTimeout = healthy
	}
}

// specify the number of routines for dialing peers
func ClientConfigDialPoolSize[T constraints.Integer](n T) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.dialPoolSize = max(uint16(n), 1)
	}
}

// specify the low and high watermarks for torrent peering
func ClientConfigPeerLimits[T constraints.Integer](low, high T) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.TorrentPeersLowWater = int(low)
		cc.TorrentPeersHighWater = int(high)
	}
}

// specify the global capacity for uploading pieces to peers.
func ClientConfigUploadLimit(l *rate.Limiter) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.UploadRateLimiter = l
	}
}

// specify the global capacity for downloading pieces from peers.
func ClientConfigDownloadLimit(l *rate.Limiter) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.DownloadRateLimiter = l
	}
}

// specify the global capacity for accepting inbound connections
func ClientConfigAcceptLimit(l *rate.Limiter) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.acceptRateLimiter = l
	}
}

// peer id prefix for BEP20 implementation.
func ClientConfigIDPrefix(prefix string) ClientConfigOption {
	return func(cc *ClientConfig) {
		cc.Bep20 = prefix
	}
}

// NewDefaultClientConfig default client configuration.
func NewDefaultClientConfig(mdstore MetadataStore, store storage.ClientImpl, options ...ClientConfigOption) *ClientConfig {
	cc := &ClientConfig{
		defaultCacheDirectory:          userx.DefaultCacheDirectory("torrents"),
		defaultMetadata:                mdstore,
		defaultStorage:                 store,
		extensionbits:                  defaultPeerExtensionBytes(),
		HTTPUserAgent:                  DefaultHTTPUserAgent,
		ExtendedHandshakeClientVersion: "ghost.torrent dev 2025",
		Bep20:                          "-GT0002-",
		UpnpID:                         "ghost.torrent",
		maximumOutstandingRequests:     64,
		NominalDialTimeout:             16 * time.Second,
		MinDialTimeout:                 4 * time.Second,
		HalfOpenConnsPerTorrent:        32,
		TorrentPeersHighWater:          128,
		TorrentPeersLowWater:           48,
		HandshakesTimeout:              4 * time.Second,
		dynamicip:                      UPnPPortForward,
		dhtStartingNodes:               nil,
		UploadRateLimiter:              rate.NewLimiter(rate.Limit(128*bytesx.MiB), bytesx.MiB),
		DownloadRateLimiter:            rate.NewLimiter(rate.Limit(256*bytesx.MiB), bytesx.MiB),
		dialRateLimiter:                rate.NewLimiter(rate.Limit(32), 128),
		acceptRateLimiter:              rate.NewLimiter(rate.Limit(runtime.NumCPU()), runtime.NumCPU()),
		dialPoolSize:                   uint16(runtime.NumCPU()),
		ConnTracker:                    conntrack.NewInstance(),
		HeaderObfuscationPolicy: HeaderObfuscationPolicy{
			Preferred:        false,
			RequirePreferred: false,
		},
		CryptoSelector:   mse.DefaultCryptoSelector,
		CryptoProvides:   mse.AllSupportedCrypto,
		Logger:           discard{},
		Warn:             discard{},
		Debug:            discard{},
		DHTAnnouncePeer:  func(ih metainfo.Hash, ip net.IP, port int, portOk bool) {},
		DHTMuxer:         dht.DefaultMuxer(),
		ConnectionClosed: func(t metainfo.Hash, stats ConnStats, remaining int) {},
		Handshaker: connections.NewHandshaker(
			connections.AutoFirewall(),
		),
		extensions: map[pp.ExtensionName]pp.ExtensionNumber{
			pp.ExtensionNameMetadata: pp.MetadataExtendedID,
			pp.ExtensionNamePex:      pp.PEXExtendedID,
		},
	}

	for _, opt := range options {
		opt(cc)
	}

	return cc
}

// HeaderObfuscationPolicy ...
type HeaderObfuscationPolicy struct {
	RequirePreferred bool // Whether the value of Preferred is a strict requirement.
	Preferred        bool // Whether header obfuscation is preferred.
}
