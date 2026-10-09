//go:build unix

package dhtrpc

import (
	"context"
	"encoding/binary"
	"net"
	"syscall"
	"testing"
	"time"
)

// punchLowTTL and punchDefaultTTL are the TTLs of a holepunch datagram (upstream holepuncher.js HOLEPUNCH_TTL and
// DEFAULT_TTL).
const (
	punchLowTTL     = 5
	punchDefaultTTL = 64
)

// ttlOf returns the IP TTL that the control messages oob carry, or -1 when they carry none. macOS sends the TTL of
// a received datagram as one byte, Linux as an int.
func ttlOf(oob []byte) int {
	cms, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return -1
	}
	for _, c := range cms {
		if c.Header.Level != syscall.IPPROTO_IP || (c.Header.Type != syscall.IP_RECVTTL && c.Header.Type != syscall.IP_TTL) {
			continue
		}
		switch len(c.Data) {
		case 1:
			return int(c.Data[0])
		case 4:
			return int(binary.NativeEndian.Uint32(c.Data))
		}
	}
	return -1
}

// Test case 1: a one-byte datagram that arrives on the node's socket reaches the punch handler, with the address it
// came from, and the node still answers requests afterwards. A dht-rpc packet is at least two bytes (Decode drops
// a shorter datagram), so the punch handler is the only place a one-byte datagram can go.
func TestPunchDatagramReachesPunchHandler(t *testing.T) {
	node := newNode(t, Config{})
	got := make(chan *net.UDPAddr, 4)
	stubCall(t, "OnPunch", func() { node.OnPunch(func(from *net.UDPAddr) { got <- from }) })

	sender := listen(t)
	if _, err := sender.WriteTo([]byte{0}, nodeAddr(t, node)); err != nil {
		t.Fatal(err)
	}
	select {
	case from := <-got:
		if from.Port != addrOf(sender).Port {
			t.Errorf("punch came from port %d, want the sender's port %d", from.Port, addrOf(sender).Port)
		}
	case <-time.After(punchWait):
		t.Fatal("the one-byte datagram did not reach the punch handler")
	}

	client, _ := newIO(t, noReply)
	ctx, cancel := context.WithTimeout(context.Background(), punchWait)
	defer cancel()
	resp, err := client.Request(ctx, nodeAddr(t, node), Request{Internal: true, Command: cmdFindNode, Target: make([]byte, 32)})
	checkImplemented(t, "Request", err)
	if err != nil {
		t.Fatalf("FIND_NODE after the punch got no reply: %v", err)
	}
	if resp.Error != 0 {
		t.Errorf("FIND_NODE error after the punch = %d, want 0", resp.Error)
	}
}

// Test case 2: each holepunch datagram leaves the node's socket with its own TTL. A send with the low TTL and one
// with the default TTL arrive in that order, each as one zero byte, from the node's address, with those TTLs. The
// receiver is a plain UDP socket that reports the IP TTL of what it gets (IP_RECVTTL). On the loopback interface the
// TTL is not lowered by a hop, so the value arrives as it was sent.
func TestSendPunchCarriesItsOwnTTL(t *testing.T) {
	node := newNode(t, Config{})
	recv := listen(t)
	raw, err := recv.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_RECVTTL, 1)
	}); err != nil || sockErr != nil {
		t.Fatalf("IP_RECVTTL on the receiver: %v, %v", err, sockErr)
	}

	to := addrOf(recv)
	for _, ttl := range []int{punchLowTTL, punchDefaultTTL} {
		var sendErr error
		stubCall(t, "SendPunch", func() { sendErr = node.SendPunch(to, ttl) })
		if sendErr != nil {
			checkImplemented(t, "SendPunch", sendErr)
			t.Fatalf("SendPunch with TTL %d: %v", ttl, sendErr)
		}
	}

	buf := make([]byte, 16)
	oob := make([]byte, 128)
	for _, want := range []int{punchLowTTL, punchDefaultTTL} {
		recv.SetReadDeadline(time.Now().Add(punchWait))
		n, oobn, _, from, err := recv.ReadMsgUDP(buf, oob)
		if err != nil {
			t.Fatalf("the datagram with TTL %d did not arrive: %v", want, err)
		}
		if n != 1 || buf[0] != 0 {
			t.Errorf("datagram = %v, want one zero byte", buf[:n])
		}
		if nodeUDP := nodeAddr(t, node); from.Port != nodeUDP.Port {
			t.Errorf("datagram came from port %d, want the node's port %d", from.Port, nodeUDP.Port)
		}
		if got := ttlOf(oob[:oobn]); got != want {
			t.Errorf("TTL of the datagram = %d, want %d", got, want)
		}
	}
}

// Test case 3: Observed pings a node and returns the address that node reports for the sending socket. On
// loopback that is the socket's own address: one NAT sample, which upstream's nat sampler takes from the 'to' field
// of a ping's reply.
func TestObservedIsTheAddressThePeerSees(t *testing.T) {
	tn := startTestnet(t, 3)
	node := tn.Nodes[0]
	ctx, cancel := context.WithTimeout(context.Background(), punchWait)
	defer cancel()
	var seen Addr
	var err error
	stubCall(t, "Observed", func() { seen, err = node.Observed(ctx, nodeAddr(t, tn.Nodes[1])) })
	checkImplemented(t, "Observed", err)
	if err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if want := dhtAddr(nodeAddr(t, node)); seen != want {
		t.Errorf("Observed = %v:%d, want the node's own address %v:%d", seen.Host, seen.Port, want.Host, want.Port)
	}
}
