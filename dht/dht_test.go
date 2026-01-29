package dht

import (
	"context"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/james-lawrence/torrent/bencode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/james-lawrence/torrent/dht/int160"
	"github.com/james-lawrence/torrent/dht/krpc"
	"github.com/james-lawrence/torrent/internal/netx"
	"github.com/james-lawrence/torrent/internal/testx"
)

func TestSetNilBigInt(t *testing.T) {
	i := new(big.Int)
	i.SetBytes(make([]byte, 2))
}

func TestMarshalCompactNodeInfo(t *testing.T) {
	cni := krpc.CompactIPv4NodeInfo{krpc.NodeInfo{
		ID: [20]byte{'a', 'b', 'c'},
	}}
	addr, err := net.ResolveUDPAddr("udp4", "1.2.3.4:5")
	require.NoError(t, err)
	ip, err := netx.NetIP(addr)
	require.NoError(t, err)
	port, err := netx.NetPort(addr)
	require.NoError(t, err)

	cni[0].Addr = krpc.NewNodeAddrFromIPPort(ip.To4(), uint16(port))
	b, err := cni.MarshalBinary()
	require.NoError(t, err)
	var bb [26]byte
	copy(bb[:], []byte("abc"))
	copy(bb[20:], []byte("\x01\x02\x03\x04\x00\x05"))
	assert.EqualValues(t, string(bb[:]), string(b))
}

const zeroID = "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"

var testIDs []int160.T

func init() {
	for _, s := range []string{
		zeroID,
		"\x03" + zeroID[1:],
		"\x03" + zeroID[1:18] + "\x55\xf0",
		"\x55" + zeroID[1:17] + "\xff\x55\x0f",
		"\x54" + zeroID[1:18] + "\x50\x0f",
	} {
		testIDs = append(testIDs, int160.FromByteString(s))
	}
	testIDs = append(testIDs, int160.T{})
}

func TestMaxDistanceString(t *testing.T) {
	require.EqualValues(t, "\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff", int160.Max().Bytes())
}

func TestDHTDefaultConfig(t *testing.T) {
	s, err := NewServer(32)
	assert.NoError(t, err)
	s.Close()
}

func TestPing(t *testing.T) {
	recvConn := mustListen("127.0.0.1:0")
	srv, err := NewServer(32)
	require.NoError(t, err)
	backgroundServe(t, srv, recvConn)
	srvUdpAddr := func(s *Server) *net.UDPAddr {
		return &net.UDPAddr{
			IP:   s.Addr().(*net.UDPAddr).IP,
			Port: s.Addr().(*net.UDPAddr).Port,
		}
	}
	defer srv.Close()

	srv0, err := NewServer(
		32,
		OptionBootstrapNodesFn(addrResolver(srvUdpAddr(srv).String())),
	)
	require.NoError(t, err)
	backgroundServe(t, srv0, mustListen("127.0.0.1:0"))
	defer srv0.Close()
	res := srv.Ping(srv0.AddrPort())
	require.NoError(t, res.Err)
	require.EqualValues(t, srv0.ID(), res.Reply.SenderID().Int160())
}

func TestServerCustomNodeId(t *testing.T) {
	idHex := "5a3ce1c14e7a08645677bbd1cfe7d8f956d53256"
	id, err := int160.FromHexEncodedString(idHex)
	require.NoError(t, err)
	// How to test custom *secure* ID when tester computers will have
	// different IDs? Generate custom ids for local IPs and use mini-ID?
	s, err := NewServer(
		32,
		OptionNodeID(id),
	)
	require.NoError(t, err)
	require.Equal(t, id, s.ID(), s.ID().String())
	backgroundServe(t, s, mustListen(":0"))
	defer s.Close()

	assert.Equal(t, id.Prefix([]byte{0x0, 0x0, 0x0}), s.ID().Prefix([]byte{0x0, 0x0, 0x0}), s.ID().String())
}

func TestAnnounceTimeout(t *testing.T) {
	ctx, done0 := testx.Context(t)
	defer done0()

	s, err := NewServer(
		32,
		OptionBootstrapNodesFn(addrResolver("1.2.3.4:5")),
		OptionQueryResendDelay(0),
	)
	require.NoError(t, err)
	backgroundServe(t, s, mustListen(":0"))

	ctx, done := context.WithTimeout(ctx, 300*time.Millisecond)
	defer done()
	a, err := s.AnnounceTraversal(ctx, int160.FromByteString("12341234123412341234"), AnnouncePeer(s, true))
	assert.NoError(t, err)
	<-a.Peers
	a.Close()
	s.Close()
}

func TestEqualPointers(t *testing.T) {
	assert.EqualValues(t, &krpc.Msg{R: &krpc.Return{}}, &krpc.Msg{R: &krpc.Return{}})
}

func TestHook(t *testing.T) {
	rconn := mustListen("127.0.0.1:0")
	pconn := mustListen("127.0.0.1:0")
	pinger, err := NewServer(32)
	require.NoError(t, err)
	backgroundServe(t, pinger, pconn)
	defer pinger.Close()
	// Establish server with a hook attached to "ping"
	hookCalled := make(chan struct{}, 1)
	receiver, err := NewServer(
		32,
		OptionBootstrapFixedAddrs(
			NewAddr(pconn.LocalAddr().(*net.UDPAddr).AddrPort()),
		),
		OptionOnQuery(func(m *krpc.Msg, addr net.Addr) bool {
			t.Logf("receiver got msg: %v", m)
			if m.Q == "ping" {
				select {
				case hookCalled <- struct{}{}:
				default:
				}
			}
			return true
		}),
	)
	require.NoError(t, err)
	backgroundServe(t, receiver, rconn)
	defer receiver.Close()
	// Ping receiver from pinger to trigger hook. Should also receive a response.
	t.Log("TestHook: Servers created, hook for ping established. Calling Ping.")
	res := pinger.Ping(receiver.AddrPort())
	assert.NoError(t, res.Err)

	// Await signal that hook has been called.
	select {
	case <-hookCalled:
		// Success, hook was triggered. TODO: Ensure that "ok" channel
		// receives, also, indicating normal handling proceeded also.
		t.Log("TestHook: Received ping, hook called and returned to normal execution!")
		t.Log("TestHook: Sender received response from pinged hook server, so normal execution resumed.")
	case <-time.After(time.Second * 1):
		t.Error("Failed to see evidence of ping hook being called after 2 seconds.")
	}
}

// Check that address resolution doesn't rat out invalid SendTo addr
// arguments.
func TestResolveBadAddr(t *testing.T) {
	ua, err := net.ResolveUDPAddr("udp", "0.131.255.145:33085")
	require.NoError(t, err)
	assert.False(t, validNodeAddr(ua))
}

func TestGlobalBootstrapAddrs(t *testing.T) {
	addrs, err := GlobalBootstrapAddrs("udp")
	if err != nil {
		t.Skip(err)
	}
	for _, a := range addrs {
		t.Log(a)
	}
}

// https://github.com/anacrolix/dht/pull/19
func TestBadGetPeersResponse(t *testing.T) {
	ctx, done := testx.Context(t)
	defer done()

	pc, err := net.ListenPacket("udp", "localhost:0")
	require.NoError(t, err)
	defer pc.Close()
	s, err := NewServer(
		32,
		OptionBootstrapFixedAddrs(
			NewAddr(pc.LocalAddr().(*net.UDPAddr).AddrPort()),
		),
	)
	require.NoError(t, err)
	backgroundServe(t, s, mustListen("localhost:0"))
	defer s.Close()
	go func() {
		b := make([]byte, 1024)
		n, addr, err := pc.ReadFrom(b)
		require.NoError(t, err)
		var rm krpc.Msg
		require.NoError(t, bencode.Unmarshal(b[:n], &rm))
		m := krpc.Msg{
			R: &krpc.Return{},
			T: rm.T,
		}
		b, err = bencode.Marshal(m)
		require.NoError(t, err)
		_, err = pc.WriteTo(b, addr)
		require.NoError(t, err)
	}()
	a, err := s.AnnounceTraversal(ctx, int160.Zero(), AnnouncePeer(s, true))
	require.NoError(t, err)
	// Drain the Announce until it closes.
	for range a.Peers {
	}
}

// func TestBootstrapRace(t *testing.T) {
// 	ctx, done := testx.Context(t)
// 	defer done()

// 	remotePc, err := net.ListenPacket("udp", "localhost:0")
// 	require.NoError(t, err)
// 	defer remotePc.Close()
// 	serverPc := bootstrapRacePacketConn{
// 		read:     make(chan read),
// 		maxWrite: defaultAttempts,
// 	}
// 	t.Logf("remote addr: %s", remotePc.LocalAddr())
// 	s, err := NewServer(&ServerConfig{
// 		StartingNodes:    addrResolver(remotePc.LocalAddr().String()),
// 		QueryResendDelay: func() time.Duration { return 20 * time.Millisecond },
// 	})
// 	require.NoError(t, err)
// 	backgroundServeNoWait(t, s, &serverPc)
// 	log.Println("WAAAAT 0")
// 	go func() {
// 		for i := 0; i < serverPc.maxWrite-1; i++ {
// 			remotePc.ReadFrom(nil)
// 		}

// 		var b [1024]byte
// 		_, addr, _ := remotePc.ReadFrom(b[:])
// 		var m krpc.Msg
// 		bencode.Unmarshal(b[:], &m)
// 		m.Y = "r"
// 		rb, err := bencode.Marshal(m)
// 		if err != nil {
// 			panic(err)
// 		}
// 		remotePc.WriteTo(rb, addr)
// 	}()
// 	log.Println("WAAAAT 1")
// 	defer s.Close()

// 	ts, err := s.Bootstrap(ctx)
// 	t.Logf("%#v", ts)
// 	require.NoError(t, err)
// }

// type emptyNetAddr struct{}

// func (emptyNetAddr) Network() string { return "" }
// func (emptyNetAddr) String() string  { return "" }

// type read struct {
// 	b    []byte
// 	addr net.Addr
// }

// type bootstrapRacePacketConn struct {
// 	mu       sync.Mutex
// 	writes   int
// 	maxWrite int
// 	read     chan read
// }

// func (me *bootstrapRacePacketConn) Close() error {
// 	close(me.read)
// 	return nil
// }
// func (me *bootstrapRacePacketConn) LocalAddr() net.Addr { return emptyNetAddr{} }
// func (me *bootstrapRacePacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
// 	r, ok := <-me.read
// 	if !ok {
// 		return 0, nil, io.EOF
// 	}
// 	copy(b, r.b)
// 	log.Printf("reading %q from %s", r.b, r.addr)
// 	return len(r.b), r.addr, nil
// }
// func (me *bootstrapRacePacketConn) SetDeadline(time.Time) error      { return nil }
// func (me *bootstrapRacePacketConn) SetReadDeadline(time.Time) error  { return nil }
// func (me *bootstrapRacePacketConn) SetWriteDeadline(time.Time) error { return nil }

// func (me *bootstrapRacePacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
// 	me.mu.Lock()
// 	defer me.mu.Unlock()
// 	me.writes++
// 	if me.writes == me.maxWrite {
// 		var m krpc.Msg
// 		bencode.Unmarshal(b[:], &m)
// 		m.Y = "r"
// 		rb, err := bencode.Marshal(m)
// 		if err != nil {
// 			panic(err)
// 		}
// 		me.read <- read{rb, addr}
// 		return 0, errors.New("write error")
// 	}
// 	return len(b), nil
// }
