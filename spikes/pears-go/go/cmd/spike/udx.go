package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/blake2b"

	"holebridge-spike-pears-go/internal/udx"
)

const (
	goStreamID = 1001
	jsStreamID = 2002
	block      = 1 << 20
)

// runUDX is step 4: move a stream of the given size between the Go port and the pinned
// udx-native 1.21.3 peer (udx-peer.js), in one direction per run. The stream is the same
// 1 MiB random block repeated, so the BLAKE2b-256 digest of the sender is known before timing.
// Throughput is measured at the receiving end: first byte delivered to end of stream.
func runUDX(args []string) error {
	fs := flag.NewFlagSet("udx", flag.ContinueOnError)
	node := fs.String("node", "", "path to udx-peer.js")
	nodeBin := fs.String("nodebin", "node", "node binary")
	dir := fs.String("dir", "go2js", "go2js (Go sends, JS receives) or js2go (JS sends, Go receives)")
	total := fs.Int("bytes", 100*1024*1024, "stream size in bytes")
	runs := fs.Int("runs", 3, "runs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *node == "" {
		return errors.New("-node is required")
	}
	if *total%block != 0 {
		return fmt.Errorf("-bytes must be a multiple of %d", block)
	}
	var mbits []float64
	for i := 1; i <= *runs; i++ {
		var (
			m   runResult
			err error
		)
		switch *dir {
		case "go2js":
			m, err = udxGoToJS(*nodeBin, *node, *total)
		case "js2go":
			m, err = udxJSToGo(*nodeBin, *node, *total)
		default:
			return fmt.Errorf("bad -dir %q", *dir)
		}
		if err != nil {
			return fmt.Errorf("%s run %d: %w", *dir, i, err)
		}
		fmt.Printf("%s run=%d bytes=%d digest_match=%v go_retransmits=%d go_sack_packets=%d go_packets=%d\n",
			*dir, i, m.bytes, m.match, m.retrans, m.sacks, m.packets)
		fmt.Printf("%s run=%d recv_span_ms=%.1f recv_mbit_s=%.1f send_span_ms=%.1f send_mbit_s=%.1f\n",
			*dir, i, m.recvSpanMS, mbit(m.bytes, m.recvSpanMS), m.sendSpanMS, mbit(m.bytes, m.sendSpanMS))
		mbits = append(mbits, mbit(m.bytes, m.recvSpanMS))
		if !m.match {
			return errors.New("digest mismatch: stream was not intact")
		}
	}
	sort.Float64s(mbits)
	fmt.Printf("%s summary recv_mbit_s median=%.1f min=%.1f max=%.1f runs=%d\n",
		*dir, mbits[len(mbits)/2], mbits[0], mbits[len(mbits)-1], len(mbits))
	return nil
}

type runResult struct {
	bytes                   int
	match                   bool
	recvSpanMS, sendSpanMS  float64
	retrans, sacks, packets int
}

func mbit(bytes int, ms float64) float64 {
	if ms <= 0 {
		return 0
	}
	return float64(bytes) * 8 / (ms / 1000) / 1e6
}

// repeatReader yields the block over and over until total bytes.
type repeatReader struct {
	block []byte
	left  int
	off   int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && r.left > 0 {
		c := copy(p[n:], r.block[r.off:])
		if c > r.left {
			c = r.left
		}
		n += c
		r.left -= c
		r.off = (r.off + c) % len(r.block)
	}
	return n, nil
}

func expectedDigest(blk []byte, total int) string {
	h, _ := blake2b.New256(nil)
	for w := 0; w < total; w += len(blk) {
		h.Write(blk)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func newBlock() ([]byte, error) {
	blk := make([]byte, block)
	_, err := rand.Read(blk)
	return blk, err
}

func listen() (*net.UDPConn, error) {
	sock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	_ = sock.SetReadBuffer(4 << 20)
	_ = sock.SetWriteBuffer(4 << 20)
	return sock, nil
}

// readLineWithPrefix reads lines from the child until one starts with prefix.
func readLineWithPrefix(r *bufio.Reader, prefix string) (string, error) {
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("child output ended before %s: %w", prefix, err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return line, nil
		}
	}
}

func field(line, key string) string {
	for _, f := range strings.Fields(line) {
		if strings.HasPrefix(f, key+"=") {
			return strings.TrimPrefix(f, key+"=")
		}
	}
	return ""
}

func fieldFloat(line, key string) float64 {
	v, _ := strconv.ParseFloat(field(line, key), 64)
	return v
}

func udxGoToJS(nodeBin, peer string, total int) (runResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	blk, err := newBlock()
	if err != nil {
		return runResult{}, err
	}
	want := expectedDigest(blk, total)

	sock, err := listen()
	if err != nil {
		return runResult{}, err
	}
	defer sock.Close()
	goPort := sock.LocalAddr().(*net.UDPAddr).Port

	// The JS receiver binds a free port and reports it in READY.
	cmd := exec.CommandContext(ctx, nodeBin, peer, "recv", "0", strconv.Itoa(goPort),
		strconv.Itoa(jsStreamID), strconv.Itoa(goStreamID), strconv.Itoa(total))
	cmd.Stderr = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return runResult{}, err
	}
	defer func() { _ = cmd.Wait() }()
	rd := bufio.NewReader(stdout)
	ready, err := readLineWithPrefix(rd, "READY")
	if err != nil {
		return runResult{}, err
	}
	jsPort, err := strconv.Atoi(field(ready, "port"))
	if err != nil {
		return runResult{}, fmt.Errorf("READY line: %q", ready)
	}

	st := udx.New(sock, netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(jsPort)), goStreamID, jsStreamID)
	defer st.Close()
	start := time.Now()
	sent, err := st.Send(&repeatReader{block: blk, left: total}, time.Now().Add(120*time.Second))
	sendSpan := time.Since(start)
	if err != nil {
		return runResult{}, fmt.Errorf("go send: %w", err)
	}
	recvLine, err := readLineWithPrefix(rd, "RECV")
	if err != nil {
		return runResult{}, err
	}
	gotDigest := field(recvLine, "digest")
	return runResult{
		bytes:      int(fieldFloat(recvLine, "bytes")),
		match:      gotDigest == want && int(sent) == total,
		recvSpanMS: fieldFloat(recvLine, "span_ms"),
		sendSpanMS: float64(sendSpan.Microseconds()) / 1000,
		retrans:    st.Retransmits,
		sacks:      st.SackCount,
		packets:    st.Packets,
	}, nil
}

func udxJSToGo(nodeBin, peer string, total int) (runResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	sock, err := listen()
	if err != nil {
		return runResult{}, err
	}
	defer sock.Close()
	goPort := sock.LocalAddr().(*net.UDPAddr).Port

	cmd := exec.CommandContext(ctx, nodeBin, peer, "send", "0", strconv.Itoa(goPort),
		strconv.Itoa(jsStreamID), strconv.Itoa(goStreamID), strconv.Itoa(total))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return runResult{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return runResult{}, err
	}
	defer func() { _ = cmd.Wait() }()
	rd := bufio.NewReader(stdout)
	ready, err := readLineWithPrefix(rd, "READY")
	if err != nil {
		return runResult{}, err
	}
	jsPort, err := strconv.Atoi(field(ready, "port"))
	if err != nil {
		return runResult{}, fmt.Errorf("READY line: %q", ready)
	}

	st := udx.New(sock, netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(jsPort)), goStreamID, jsStreamID)
	defer st.Close()
	h, _ := blake2b.New256(nil)
	var t0, t1 time.Time
	sink := func(p []byte) {
		if t0.IsZero() {
			t0 = time.Now()
		}
		h.Write(p)
	}
	// Our stream is listening, so the sender may start.
	if _, err := io.WriteString(stdin, "GO\n"); err != nil {
		return runResult{}, err
	}
	got, err := st.Receive(sink, time.Now().Add(120*time.Second))
	t1 = time.Now()
	_ = stdin.Close()
	if err != nil {
		return runResult{}, fmt.Errorf("go receive: %w", err)
	}
	sentLine, err := readLineWithPrefix(rd, "SENT")
	if err != nil {
		return runResult{}, err
	}
	gotDigest := hex.EncodeToString(h.Sum(nil))
	return runResult{
		bytes:      int(got),
		match:      gotDigest == field(sentLine, "digest") && int(got) == total,
		recvSpanMS: float64(t1.Sub(t0).Microseconds()) / 1000,
		sendSpanMS: fieldFloat(sentLine, "span_ms"),
		retrans:    st.Retransmits,
		sacks:      st.SackCount,
		packets:    st.Packets,
	}, nil
}
