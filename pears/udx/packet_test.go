package udx

import (
	"bytes"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/testvec"
)

// udxVectors mirrors spec/vectors/udx-packets.json, written by spec/gen/udx-packets.js.
type udxVectors struct {
	Headers []headerVector `json:"udx_headers"`
	PingHex string         `json:"dht_rpc_ping_request"`
}

type headerVector struct {
	Name       string `json:"name"`
	Type       uint8  `json:"type"`
	DataOffset uint8  `json:"data_offset"`
	RemoteID   uint32 `json:"remote_id"`
	RecvWindow uint32 `json:"recv_window"`
	Seq        uint32 `json:"seq"`
	Ack        uint32 `json:"ack"`
	Payload    string `json:"payload"`
	Bytes      string `json:"bytes"`
}

func loadVectors(t *testing.T) udxVectors {
	t.Helper()
	var v udxVectors
	testvec.Load(t, "udx-packets.json", &v)
	return v
}

// packetNamed returns the wire bytes of the header vector with the given name.
func packetNamed(t *testing.T, v udxVectors, name string) []byte {
	t.Helper()
	for _, h := range v.Headers {
		if h.Name == name {
			return testvec.Hex(t, h.Bytes)
		}
	}
	t.Fatalf("no header vector named %q", name)
	return nil
}

// failOnPanic turns a panic from a stub into a test failure, so the other tests still run.
// Defer it first in a test that calls a stub which panics.
func failOnPanic(t *testing.T) {
	t.Helper()
	if r := recover(); r != nil {
		t.Fatalf("%v", r)
	}
}

// loopbackUDP opens a UDP socket on 127.0.0.1 with a free port.
func loopbackUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// sendUDP writes one datagram from one loopback socket to another.
func sendUDP(t *testing.T, from, to *net.UDPConn, b []byte) {
	t.Helper()
	if _, err := from.WriteToUDP(b, to.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("WriteToUDP: %v", err)
	}
}

// Every header vector encodes to its bytes and decodes back to the same header and payload.
func TestHeaderVectorsRoundTrip(t *testing.T) {
	v := loadVectors(t)
	if len(v.Headers) == 0 {
		t.Fatal("no header vectors in udx-packets.json")
	}
	for _, tc := range v.Headers {
		t.Run(tc.Name, func(t *testing.T) {
			defer failOnPanic(t)
			h := Header{Type: tc.Type, DataOffset: tc.DataOffset, RemoteID: tc.RemoteID, RecvWindow: tc.RecvWindow, Seq: tc.Seq, Ack: tc.Ack}
			payload := testvec.Hex(t, tc.Payload)
			want := testvec.Hex(t, tc.Bytes)

			if got := EncodeHeader(h, payload); !bytes.Equal(got, want) {
				t.Fatalf("EncodeHeader = %x, want %x", got, want)
			}
			gotH, gotPayload, err := DecodeHeader(want)
			if err != nil {
				t.Fatalf("DecodeHeader: %v", err)
			}
			if gotH != h {
				t.Errorf("DecodeHeader header = %+v, want %+v", gotH, h)
			}
			if !bytes.Equal(gotPayload, payload) {
				t.Errorf("DecodeHeader payload = %x, want %x", gotPayload, payload)
			}
		})
	}
}

// IsUDX is false for a dht-rpc request and for a 19-byte packet, and true for a UDX DATA packet.
func TestIsUDX(t *testing.T) {
	defer failOnPanic(t)
	v := loadVectors(t)
	if IsUDX(testvec.Hex(t, v.PingHex)) {
		t.Error("IsUDX(dht-rpc PING request) = true, want false")
	}
	data := packetNamed(t, v, "data")
	if !IsUDX(data) {
		t.Error("IsUDX(UDX DATA packet) = false, want true")
	}
	if IsUDX(data[:19]) {
		t.Error("IsUDX(19-byte packet) = true, want false: the UDX header is 20 bytes")
	}
}

// A socket hands a non-UDX datagram to Raw and a UDX packet to the stream registered under its id.
func TestSocketRoutesRawAndStreams(t *testing.T) {
	defer failOnPanic(t)
	v := loadVectors(t)
	conn := loopbackUDP(t)
	peer := loopbackUDP(t)
	sock, err := NewSocket(conn)
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	defer sock.Close()
	// 0x01020304 is the remote id of the data_end vector.
	streams, err := sock.Register(0x01020304)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	ping := testvec.Hex(t, v.PingHex)
	sendUDP(t, peer, conn, ping)
	raw := sock.Raw()
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := raw.ReadFrom(buf)
	if err != nil {
		t.Fatalf("Raw ReadFrom: %v", err)
	}
	if !bytes.Equal(buf[:n], ping) {
		t.Errorf("Raw got %x, want the PING request %x", buf[:n], ping)
	}

	sendUDP(t, peer, conn, packetNamed(t, v, "data_end"))
	select {
	case p := <-streams:
		if p.Header.Type != FlagData|FlagEnd || p.Header.Seq != 8 {
			t.Errorf("stream got header %+v, want the data_end header", p.Header)
		}
		if !bytes.Equal(p.Payload, []byte("!")) {
			t.Errorf("stream got payload %x, want 21", p.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("UDX packet did not reach the stream registered under its remote id")
	}
}

// A UDX packet for a stream id nobody registered is dropped and counted, and never reaches Raw.
func TestSocketDropsUnknownStream(t *testing.T) {
	defer failOnPanic(t)
	v := loadVectors(t)
	conn := loopbackUDP(t)
	peer := loopbackUDP(t)
	sock, err := NewSocket(conn)
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	defer sock.Close()
	streams, err := sock.Register(0x01020304)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// The data_end packet with its remote id changed to 99, which nothing registers.
	unknown := packetNamed(t, v, "data_end")
	copy(unknown[4:8], []byte{99, 0, 0, 0})
	sendUDP(t, peer, conn, unknown)
	// A packet for the registered stream, sent after it, shows the first one was already handled.
	sendUDP(t, peer, conn, packetNamed(t, v, "data_end"))
	select {
	case <-streams:
	case <-time.After(2 * time.Second):
		t.Fatal("the registered stream got no packet")
	}

	if got := sock.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d, want 1", got)
	}
	raw := sock.Raw()
	raw.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, _, err := raw.ReadFrom(make([]byte, 2048)); err == nil {
		t.Errorf("a dropped packet reached Raw (%d bytes)", n)
	}
}

// DecodeHeader rejects a short datagram, a bad magic or version, and a data offset past the end.
func TestDecodeHeaderRejectsBadPackets(t *testing.T) {
	v := loadVectors(t)
	data := packetNamed(t, v, "data")
	badMagic := append([]byte(nil), data...)
	badMagic[0] = 0xfe
	badVersion := append([]byte(nil), data...)
	badVersion[1] = 2
	pastEnd := append([]byte(nil), data...)
	pastEnd[3] = 0xff
	for name, b := range map[string][]byte{
		"short":           data[:19],
		"bad magic":       badMagic,
		"bad version":     badVersion,
		"offset past end": pastEnd,
	} {
		if _, _, err := DecodeHeader(b); err == nil {
			t.Errorf("DecodeHeader(%s) returned no error", name)
		}
	}
}

// Registering the same stream id twice is an error.
func TestRegisterTwiceFails(t *testing.T) {
	sock, err := NewSocket(loopbackUDP(t))
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	defer sock.Close()
	if _, err := sock.Register(5); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := sock.Register(5); err == nil {
		t.Error("second Register of the same id succeeded")
	}
}

// Unregister frees a stream id: packets for it are then dropped and counted, and the id can be
// registered again.
func TestUnregisterFreesID(t *testing.T) {
	defer failOnPanic(t)
	v := loadVectors(t)
	conn := loopbackUDP(t)
	peer := loopbackUDP(t)
	sock, err := NewSocket(conn)
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	defer sock.Close()
	// 0x01020304 is the remote id of the data_end vector.
	if _, err := sock.Register(0x01020304); err != nil {
		t.Fatalf("Register: %v", err)
	}
	sock.Unregister(0x01020304)

	sendUDP(t, peer, conn, packetNamed(t, v, "data_end"))
	// A non-UDX datagram sent after it reaches Raw only once the first one was handled.
	ping := testvec.Hex(t, v.PingHex)
	sendUDP(t, peer, conn, ping)
	raw := sock.Raw()
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := raw.ReadFrom(make([]byte, 2048)); err != nil {
		t.Fatalf("Raw ReadFrom: %v", err)
	}
	if got := sock.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d after a packet for an unregistered id, want 1", got)
	}

	streams, err := sock.Register(0x01020304)
	if err != nil {
		t.Fatalf("Register after Unregister: %v", err)
	}
	sendUDP(t, peer, conn, packetNamed(t, v, "data_end"))
	select {
	case <-streams:
	case <-time.After(2 * time.Second):
		t.Fatal("the re-registered id got no packet")
	}
}

// Setting a read deadline wakes a read that is already waiting, and that read times out.
func TestRawReadDeadlineWakesPendingRead(t *testing.T) {
	sock, err := NewSocket(loopbackUDP(t))
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	defer sock.Close()
	raw := sock.Raw()
	errc := make(chan error, 1)
	go func() {
		_, _, err := raw.ReadFrom(make([]byte, 2048))
		errc <- err
	}()
	raw.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	select {
	case err := <-errc:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("ReadFrom error = %v, want a deadline error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the pending read did not wake when the deadline was set")
	}
}
