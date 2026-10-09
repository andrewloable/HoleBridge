// Interop tests for pears/dhtrpc: a Go dht-rpc node and dht-rpc 6.27.0 run as JS nodes on one testnet,
// in both directions. The JS side is interop/js/dhtrpc-node.js, which each test starts as a child
// process and drives over stdio. The tests skip when node is not installed, or when interop/js has no
// node_modules yet (run npm ci there).
package interop

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
)

// dht-rpc's built-in command numbers (lib/commands.js), which the Go side sends as Internal requests.
const (
	cmdPing     = 0
	cmdFindNode = 2
)

const (
	jsStartTimeout = 20 * time.Second // the JS node bootstraps before it prints its address
	jsReplyTimeout = 20 * time.Second
	queryTimeout   = 20 * time.Second
	jsStopTimeout  = 5 * time.Second // after stdin closes, before the child is killed
)

// jsReady is the first line the JS node writes: its address and its persistent id.
type jsReady struct {
	Ready bool   `json:"ready"`
	Host  string `json:"host"`
	Port  int    `json:"port"`
	ID    string `json:"id"`
}

// jsPeer is a node that the JS side heard from: its address and id.
type jsPeer struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	ID   string `json:"id"`
}

// jsReply is the JS node's answer to one command.
type jsReply struct {
	OK      bool     `json:"ok"`
	Error   string   `json:"error"`
	ID      string   `json:"id"`
	Replies []jsPeer `json:"replies"`
}

// jsNode is a dht-rpc node running as a child process of the test.
type jsNode struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string   // stdout lines, one per reply; closed when stdout ends
	quit   chan struct{} // closed when the node is stopping, so the reader stops handing out lines
	done   chan struct{} // closed when the reader has read stdout to its end
	stderr *syncBuffer
	id     []byte
	addr   *net.UDPAddr
}

// startJSNode starts interop/js/dhtrpc-node.js with args and waits for its ready line. The child is
// stopped when the test ends, whether it passed or failed: closing its stdin makes it exit, and it is
// killed if it does not exit in time.
func startJSNode(t *testing.T, args ...string) *jsNode {
	t.Helper()
	n := startJSProcess(t, "dhtrpc-node.js", args...)
	var ready jsReady
	n.receive(&ready, jsStartTimeout, "its ready line")
	if !ready.Ready {
		t.Fatalf("JS node sent a first line that is not ready: %+v", ready)
	}
	id, err := hex.DecodeString(ready.ID)
	if err != nil || len(id) != 32 {
		t.Fatalf("JS node id %q is not 32 bytes of hex", ready.ID)
	}
	n.id = id
	n.addr = &net.UDPAddr{IP: net.ParseIP(ready.Host), Port: ready.Port}
	return n
}

// startJSProcess starts interop/js/<script> with args as a child process and reads its stdout lines in the
// background. The caller reads the first line with receive. The child is stopped when the test ends.
func startJSProcess(t *testing.T, script string, args ...string) *jsNode {
	t.Helper()
	node := requireNodeJS(t)
	dir, err := filepath.Abs("js")
	if err != nil {
		t.Fatalf("interop/js: %v", err)
	}
	cmd := exec.Command(node, append([]string{script}, args...)...)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("JS node stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("JS node stdout: %v", err)
	}
	n := &jsNode{
		t:      t,
		cmd:    cmd,
		stdin:  stdin,
		lines:  make(chan string),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
		stderr: &syncBuffer{},
	}
	cmd.Stderr = n.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start JS node: %v", err)
	}
	t.Cleanup(n.stop)
	go n.readLines(stdout)
	return n
}

// requireNodeJS returns the node binary, or skips the test when node or the JS dependencies are missing. Under
// CI it fails instead (skipInterop).
func requireNodeJS(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		skipInterop(t, "interop: node is not installed, so the JS peers cannot run (install Node.js LTS)")
	}
	requireJSModule(t, "dht-rpc")
	return node
}

// readLines reads the node's stdout one line at a time. A line is handed to a waiting call, or dropped
// once the node is stopping. It runs until stdout ends, so the child never blocks on a full pipe.
func (n *jsNode) readLines(r io.Reader) {
	defer close(n.done)
	defer close(n.lines)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		select {
		case n.lines <- sc.Text():
		case <-n.quit:
		}
	}
}

// receive reads the next stdout line into v, failing the test when none comes in time.
func (n *jsNode) receive(v any, timeout time.Duration, what string) {
	n.t.Helper()
	select {
	case line, ok := <-n.lines:
		if !ok {
			n.t.Fatalf("JS node exited before %s: stderr: %s", what, n.stderr.String())
		}
		if err := json.Unmarshal([]byte(line), v); err != nil {
			n.t.Fatalf("JS node sent a line that is not JSON for %s: %q", what, line)
		}
	case <-time.After(timeout):
		n.t.Fatalf("no %s from the JS node within %v: stderr: %s", what, timeout, n.stderr.String())
	}
}

// call sends one command to the JS node and reads its reply.
func (n *jsNode) call(cmd any, timeout time.Duration) jsReply {
	n.t.Helper()
	n.send(cmd)
	var r jsReply
	n.receive(&r, timeout, "a reply")
	return r
}

// send writes one command to the JS node's stdin as a JSON line.
func (n *jsNode) send(cmd any) {
	n.t.Helper()
	b, err := json.Marshal(cmd)
	if err != nil {
		n.t.Fatalf("encode JS command: %v", err)
	}
	if _, err := n.stdin.Write(append(b, '\n')); err != nil {
		n.t.Fatalf("send JS command: %v: stderr: %s", err, n.stderr.String())
	}
}

// stop closes the node's stdin, waits for it to exit, and kills it if it does not.
func (n *jsNode) stop() {
	close(n.quit)
	n.stdin.Close()
	select {
	case <-n.done:
	case <-time.After(jsStopTimeout):
		n.cmd.Process.Kill()
		<-n.done
	}
	n.cmd.Wait()
}

// udpAddr returns the address the JS node listens on.
func (n *jsNode) udpAddr() *net.UDPAddr {
	return n.addr
}

// syncBuffer is a bytes.Buffer that the child's stderr copier and the test can use at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// freeUDPPort returns a UDP port on 127.0.0.1 that was free a moment ago.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("free UDP port: %v", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// hasReplyFrom reports whether one of the replies came from addr with the node id id.
func hasReplyFrom(replies []dhtrpc.Reply, addr *net.UDPAddr, id []byte) bool {
	for _, r := range replies {
		if r.From != nil && r.From.IP.Equal(addr.IP) && r.From.Port == addr.Port &&
			r.Response != nil && bytes.Equal(r.Response.ID, id) {
			return true
		}
	}
	return false
}

// TestDHTRPC_GoQueriesJSNode: a JS node joins a Go testnet. A Go query for the JS node's id finds it,
// and the JS node answers a PING from Go with its id.
func TestDHTRPC_GoQueriesJSNode(t *testing.T) {
	tn := dhtrpc.NewTestnet(t, 3)
	js := startJSNode(t, tn.Bootstrap...)
	jsAddr := js.udpAddr()

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	q := tn.Nodes[1].Query(ctx, dhtrpc.QueryOpts{Target: js.id, Command: cmdFindNode, Internal: true})
	closest := q.Closest()
	if err := q.Err(); err != nil {
		t.Fatalf("Go query for the JS node's id: %v", err)
	}
	if !hasReplyFrom(closest, jsAddr, js.id) {
		t.Fatalf("Go query for the JS node's id at %v: no reply from %v with its id among %d replies",
			js.id, jsAddr, len(closest))
	}

	resp, err := tn.Nodes[1].Request(ctx, jsAddr, dhtrpc.Request{Internal: true, Command: cmdPing})
	if err != nil {
		t.Fatalf("Go PING to the JS node: %v", err)
	}
	if resp.Error != 0 {
		t.Fatalf("JS node answered PING with error %d", resp.Error)
	}
	if !bytes.Equal(resp.ID, js.id) {
		t.Fatalf("JS node answered PING with id %x, want %x", resp.ID, js.id)
	}
}

// TestDHTRPC_JSQueriesGoNode: a Go node joins a JS testnet and becomes persistent there. A query from the
// JS node for the Go node's id finds it, and a PING from the JS node gets the Go node's id back.
func TestDHTRPC_JSQueriesGoNode(t *testing.T) {
	js := startJSNode(t, "--port", strconv.Itoa(freeUDPPort(t)))

	ephemeral := false
	goNode, err := dhtrpc.New(dhtrpc.Config{Bootstrap: []string{js.udpAddr().String()}, Ephemeral: &ephemeral})
	if err != nil {
		t.Fatalf("start Go node: %v", err)
	}
	t.Cleanup(func() { goNode.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	if err := goNode.Ready(ctx); err != nil {
		t.Fatalf("Go node did not bootstrap on the JS testnet: %v", err)
	}
	goID := goNode.ID()
	if goID == nil {
		nat := goNode.NAT()
		t.Fatalf("Go node is still ephemeral after bootstrap, so it has no id: NAT %+v", nat)
	}
	goAddr, err := goNode.Addr()
	if err != nil {
		t.Fatalf("Go node address: %v", err)
	}

	find := js.call(map[string]string{"cmd": "find", "target": hex.EncodeToString(goID)}, queryTimeout)
	if !find.OK {
		t.Fatalf("JS query for the Go node's id: %s", find.Error)
	}
	if !hasJSPeer(find.Replies, goAddr, goID) {
		t.Fatalf("JS query for the Go node's id at %x: no reply from %v with its id among %d replies",
			goID, goAddr, len(find.Replies))
	}

	ping := js.call(map[string]any{"cmd": "ping", "host": goAddr.IP.String(), "port": goAddr.Port}, jsReplyTimeout)
	if !ping.OK {
		t.Fatalf("JS PING to the Go node: %s", ping.Error)
	}
	if ping.ID != hex.EncodeToString(goID) {
		t.Fatalf("Go node answered the JS PING with id %s, want %x", ping.ID, goID)
	}
}

// hasJSPeer reports whether one of the JS node's replies came from addr with the node id id.
func hasJSPeer(peers []jsPeer, addr *net.UDPAddr, id []byte) bool {
	want := hex.EncodeToString(id)
	for _, p := range peers {
		if p.Host == addr.IP.String() && p.Port == addr.Port && p.ID == want {
			return true
		}
	}
	return false
}
