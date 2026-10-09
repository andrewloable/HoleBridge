package hyperdht

import (
	"net"
	"testing"
)

// The birthday socket of a node binds to the address of the node's own socket, unmapped: a node on all interfaces
// gets a birthday socket on all interfaces, so it can send to any address the node can reach, not only loopback.
// Local reports the address a local peer reaches, so it maps an unspecified IP to 127.0.0.1 on the same port.

// loopbackIPv4 is 127.0.0.1, the address a local peer reaches an all-interfaces socket at.
var loopbackIPv4 = net.IPv4(127, 0, 0, 1)

// birthdayConnOf returns a birthday socket of d's punch pool and its conn. The socket is released when the test ends.
func birthdayConnOf(t *testing.T, d *DHT) (*dhtPunchSocket, *net.UDPConn) {
	t.Helper()
	s, ok := acquireBirthday(t, d).(*dhtPunchSocket)
	if !ok {
		t.Fatal("the birthday socket of a DHT is not a dhtPunchSocket")
	}
	if s.conn == nil {
		t.Fatal("the birthday socket of a DHT has no conn of its own")
	}
	return s, s.conn
}

// nonLoopbackIPv4 returns the first IPv4 address of this machine that is neither loopback, unspecified nor link-local,
// or nil when there is none.
func nonLoopbackIPv4(t *testing.T) net.IP {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	must(t, err)
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipn.IP.To4()
		if ip4 == nil || ip4.IsLoopback() || ip4.IsUnspecified() || ip4.IsLinkLocalUnicast() {
			continue
		}
		return ip4
	}
	return nil
}

// A node on all interfaces (hyperdht.New, as the host and the relay make it) gets a birthday socket bound to all
// interfaces: the unspecified IP, not 127.0.0.1. Its Local is 127.0.0.1 on the socket's port.
func TestBirthdaySocketBindsToAllInterfacesOnAllInterfacesNode(t *testing.T) {
	tn := startTestnet(t, 3)
	d, err := New(Config{Bootstrap: tn.Bootstrap})
	must(t, err)
	t.Cleanup(func() { d.Close() })

	s, conn := birthdayConnOf(t, d)
	bound := conn.LocalAddr().(*net.UDPAddr)
	if !bound.IP.IsUnspecified() {
		t.Fatalf("birthday socket of an all-interfaces node is bound to %v, want all interfaces (unspecified)", bound.IP)
	}
	local := s.Local()
	if local == nil || !local.IP.Equal(loopbackIPv4) || local.Port != bound.Port {
		t.Errorf("Local of the birthday socket is %v, want 127.0.0.1:%d, the address a local peer reaches", local, bound.Port)
	}
}

// A testnet node is bound to 127.0.0.1, so its birthday socket is bound to 127.0.0.1 too, as before.
func TestBirthdaySocketBindsToLoopbackOnTestnetNode(t *testing.T) {
	tn := startTestnet(t, 2)

	s, conn := birthdayConnOf(t, tn.Nodes[0])
	bound := conn.LocalAddr().(*net.UDPAddr)
	if !bound.IP.Equal(loopbackIPv4) {
		t.Fatalf("birthday socket of a testnet node is bound to %v, want 127.0.0.1", bound.IP)
	}
	local := s.Local()
	if local == nil || !local.IP.Equal(loopbackIPv4) || local.Port != bound.Port {
		t.Errorf("Local of the birthday socket is %v, want 127.0.0.1:%d", local, bound.Port)
	}
}

// Local maps an unspecified IP to 127.0.0.1 on the socket's port, and reports a loopback socket as it is.
func TestBirthdayLocalMapsUnspecifiedToLoopback(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	must(t, err)
	t.Cleanup(func() { conn.Close() })
	port := conn.LocalAddr().(*net.UDPAddr).Port

	s := &dhtPunchSocket{conn: conn}
	if local := s.Local(); local == nil || !local.IP.Equal(loopbackIPv4) || local.Port != port {
		t.Errorf("Local of a socket bound to all interfaces is %v, want 127.0.0.1:%d", local, port)
	}

	lo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: loopbackIPv4})
	must(t, err)
	t.Cleanup(func() { lo.Close() })
	loPort := lo.LocalAddr().(*net.UDPAddr).Port

	s = &dhtPunchSocket{conn: lo}
	if local := s.Local(); local == nil || !local.IP.Equal(loopbackIPv4) || local.Port != loPort {
		t.Errorf("Local of a socket bound to 127.0.0.1 is %v, want 127.0.0.1:%d", local, loPort)
	}
}

// A punch from a birthday socket of an all-interfaces node leaves from the machine's non-loopback address. The
// destination is one of this machine's own non-loopback addresses, so the kernel delivers it locally from either bind:
// a socket bound to 127.0.0.1 is still accepted and its punch arrives from 127.0.0.1. So the test asserts the source
// address the punch arrives from, which only the all-interfaces bind gives. A send to an off-host address is what fails
// with "can't assign requested address" from a 127.0.0.1 bind; a test cannot send off-host, so that was checked by hand.
// The test skips when the machine has no non-loopback IPv4 address.
func TestBirthdaySocketSendsToNonLoopbackAddress(t *testing.T) {
	ip := nonLoopbackIPv4(t)
	if ip == nil {
		t.Skip("no non-loopback IPv4 address on this machine")
	}
	tn := startTestnet(t, 2)
	d, err := New(Config{Bootstrap: tn.Bootstrap})
	must(t, err)
	t.Cleanup(func() { d.Close() })

	s, conn := birthdayConnOf(t, d)
	recv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	must(t, err)
	t.Cleanup(func() { recv.Close() })
	to := &net.UDPAddr{IP: ip, Port: recv.LocalAddr().(*net.UDPAddr).Port}

	must(t, s.SendPunch(to, 64))
	from, err := readPunch(recv, birthdayWait)
	if err != nil {
		t.Fatalf("the punch from the birthday socket to %v did not arrive: %v", to, err)
	}
	if want := conn.LocalAddr().(*net.UDPAddr).Port; from.Port != want {
		t.Errorf("the punch came from port %d, want the birthday socket's port %d", from.Port, want)
	}
	if from.IP.IsLoopback() {
		t.Errorf("the punch to %v came from %v, a loopback address: the birthday socket is bound to loopback", to, from.IP)
	}
}
