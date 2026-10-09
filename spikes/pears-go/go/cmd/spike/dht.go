package main

import (
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"time"

	"golang.org/x/crypto/blake2b"

	"holebridge-spike-pears-go/internal/dhtrpc"
)

// runDHT is step 2: PING and FIND_NODE against the testnet bootstrap node, replies decoded.
func runDHT(args []string) error {
	fs := flag.NewFlagSet("dht", flag.ContinueOnError)
	bootstrap := fs.String("bootstrap", "", "bootstrap host:port from the testnet")
	jsPing := fs.String("js-ping", "", "hex of the JS-encoded PING request for the fixed vector")
	jsFind := fs.String("js-find", "", "hex of the JS-encoded FIND_NODE request for the fixed vector")
	jsPeerID := fs.String("js-peerid", "", "hex of hyperdht's peer.id for the bootstrap address")
	rounds := fs.Int("rounds", 20, "requests of each kind after the warm-up")
	if err := fs.Parse(args); err != nil {
		return err
	}
	bs, err := netip.ParseAddrPort(*bootstrap)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	// Encoding check against hyperdht's own encoder, on fixed inputs.
	vecTarget := bytes.Repeat([]byte{7}, 32)
	vecTo := netip.MustParseAddrPort("127.0.0.1:4242")
	goPing := dhtrpc.EncodeRequest(0x1234, vecTo, dhtrpc.CmdPing, nil, nil, true)
	goFind := dhtrpc.EncodeRequest(0x1235, vecTo, dhtrpc.CmdFindNode, vecTarget, nil, true)
	fmt.Printf("encode ping matches js=%v bytes=%d\n", matchHex(goPing, *jsPing), len(goPing))
	fmt.Printf("encode find_node matches js=%v bytes=%d\n", matchHex(goFind, *jsFind), len(goFind))
	bsID := dhtrpc.PeerID(bs)
	fmt.Printf("peer.id matches js=%v\n", matchHex(bsID[:], *jsPeerID))

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return err
	}
	defer conn.Close()
	me := conn.LocalAddr().(*net.UDPAddr).AddrPort()
	fmt.Printf("client socket 127.0.0.1 port=%d bootstrap=%s peer id prefix=%x\n", me.Port(), bs, bsID[:4])

	pingLat, pingOK, err := roundTrips(conn, bs, *rounds, func(tid uint16) []byte {
		return dhtrpc.EncodeRequest(tid, bs, dhtrpc.CmdPing, nil, nil, true)
	}, func(res *dhtrpc.Response) (string, bool) {
		ok := res.Error == 0 && len(res.CloserNode) == 0 && res.Value == nil
		return fmt.Sprintf("error=%d closer=%d", res.Error, len(res.CloserNode)), ok
	})
	if err != nil {
		return err
	}
	fmt.Printf("ping replies ok=%d/%d rtt(ms) %s\n", pingOK, *rounds+1, stats(pingLat))

	findTarget := blake2b.Sum256([]byte("holebridge spike find_node target"))
	findLat, findOK, err := roundTrips(conn, bs, *rounds, func(tid uint16) []byte {
		return dhtrpc.EncodeRequest(tid, bs, dhtrpc.CmdFindNode, findTarget[:], nil, true)
	}, func(res *dhtrpc.Response) (string, bool) {
		ok := res.Error == 0 && len(res.CloserNode) > 0
		return fmt.Sprintf("error=%d closer=%d", res.Error, len(res.CloserNode)), ok
	})
	if err != nil {
		return err
	}
	fmt.Printf("find_node replies ok=%d/%d rtt(ms) %s\n", findOK, *rounds+1, stats(findLat))
	fmt.Printf("find_node last closer nodes: %v\n", lastClosers)
	return nil
}

var lastClosers []netip.AddrPort

// roundTrips sends one warm-up and rounds requests, one at a time, and checks each reply:
// tid echoed, sender id (when present) equal to the peer id, plus the kind-specific check.
func roundTrips(conn *net.UDPConn, bs netip.AddrPort, rounds int, build func(uint16) []byte,
	check func(*dhtrpc.Response) (string, bool)) ([]time.Duration, int, error) {
	var lat []time.Duration
	ok := 0
	idCount := 0
	to := &net.UDPAddr{IP: net.IP(bs.Addr().AsSlice()), Port: int(bs.Port())}
	buf := make([]byte, 64*1024)
	for i := 0; i <= rounds; i++ {
		tid := uint16(0x4000 + i)
		pkt := build(tid)
		start := time.Now()
		var res *dhtrpc.Response
		var detail string
		good := false
		for attempt := 0; attempt < 3 && !good; attempt++ {
			if _, err := conn.WriteToUDP(pkt, to); err != nil {
				return nil, ok, err
			}
			if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				return nil, ok, err
			}
			for {
				n, from, err := conn.ReadFromUDPAddrPort(buf)
				if err != nil {
					break // deadline: retry
				}
				if from.Addr() != bs.Addr() || from.Port() != bs.Port() {
					continue
				}
				r, err := dhtrpc.DecodeResponse(buf[:n])
				if err != nil || r.TID != tid {
					continue
				}
				res = r
				break
			}
			if res == nil {
				continue
			}
			// The sender id is optional: an ephemeral responder omits it. When present it must
			// equal the peer id of the address the reply came from.
			idPresent := res.ID != nil
			idOK := !idPresent || dhtrpc.CheckID(res, bs)
			if idPresent {
				idCount++
			}
			var kindOK bool
			detail, kindOK = check(res)
			good = idOK && kindOK
			if !good {
				detail += fmt.Sprintf(" idok=%v", idOK)
			}
		}
		if res != nil && i > 0 {
			lat = append(lat, time.Since(start))
		}
		if good {
			ok++
		}
		if res != nil && len(res.CloserNode) > 0 {
			lastClosers = res.CloserNode
		}
		if i == 0 {
			fmt.Printf("warm-up reply: %s good=%v\n", detail, good)
		}
	}
	fmt.Printf("replies carrying a sender id: %d of %d\n", idCount, rounds+1)
	return lat, ok, nil
}

func stats(lat []time.Duration) string {
	if len(lat) == 0 {
		return "none"
	}
	s := append([]time.Duration(nil), lat...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return fmt.Sprintf("min=%.2f p50=%.2f max=%.2f", ms(s[0]), ms(s[len(s)/2]), ms(s[len(s)-1]))
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func matchHex(b []byte, want string) bool {
	return want != "" && hex.EncodeToString(b) == want
}
