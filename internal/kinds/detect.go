// Package kinds works out the kind of a service by probing its target from the host. The rules are
// docs/architecture.md, "Service kinds".
package kinds

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/andrewloable/HoleBridge/internal/protocol"
)

// Options sets the probe. The design defaults are 3 s for ConnectTimeout and 3 s for
// ResponseTimeout. Dial connects to the target, or nil for a net.Dialer.
type Options struct {
	ConnectTimeout  time.Duration
	ResponseTimeout time.Duration
	Dial            func(ctx context.Context, network, addr string) (net.Conn, error)
}

// result is what one probe saw on its connection.
type result int

const (
	gotHTTP    result = iota // an HTTP response
	gotTLSHint               // a plain HTTP reply that says the port expects TLS
	gotNonHTTP               // bytes that are not HTTP
	silent                   // no reply before the response timeout
	failed                   // closed, reset, or the TLS handshake failed
	gotTLS                   // a TLS server hello or alert, with no HTTP response over TLS
)

// maxHintBody bounds how much of a plain HTTP reply is read when looking for the TLS hint.
const maxHintBody = 4 << 10

// tlsHints are the phrases that plain-HTTP error pages use when the port expects TLS: nginx, Go's
// net/http and Apache. They are matched in lower case.
var tlsHints = []string{
	"sent to https port",
	"http request to an https server",
	"speaking plain http to an ssl-enabled server port",
}

// Detect probes target ("host:port") and returns its kind and whether the probe was conclusive. An
// inconclusive probe returns protocol.KindUnknown; the caller keeps the kind it had before.
func Detect(ctx context.Context, target string, opts Options) (protocol.Kind, bool /*conclusive*/) {
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = 3 * time.Second
	}
	if opts.ResponseTimeout <= 0 {
		opts.ResponseTimeout = 3 * time.Second
	}
	if opts.Dial == nil {
		var nd net.Dialer
		opts.Dial = nd.DialContext
	}

	conn, err := dial(ctx, target, opts)
	if err != nil {
		return protocol.KindUnknown, false
	}
	tlsResult := probe(ctx, conn, target, opts, true)
	if tlsResult == gotHTTP || tlsResult == gotTLS {
		// A TLS answer makes the target https even when the handshake was refused: a plain probe would
		// only read the server's alert as bytes that are not HTTP, and say tcp.
		return protocol.KindHTTPS, true
	}

	conn, err = dial(ctx, target, opts)
	if err != nil {
		return protocol.KindUnknown, false
	}
	switch probe(ctx, conn, target, opts, false) {
	case gotHTTP:
		return protocol.KindHTTP, true
	case gotTLSHint:
		return protocol.KindHTTPS, true
	case gotNonHTTP:
		return protocol.KindTCP, true
	case silent:
		// Two silent probes make the target a plain TCP service.
		if tlsResult == silent {
			return protocol.KindTCP, true
		}
	}
	return protocol.KindUnknown, false
}

// dial connects to target within the connect timeout.
func dial(ctx context.Context, target string, opts Options) (net.Conn, error) {
	cctx, cancel := context.WithTimeout(ctx, opts.ConnectTimeout)
	defer cancel()
	return opts.Dial(cctx, "tcp", target)
}

// probe sends one request over conn, with TLS first when useTLS is set, and reports what came back.
// It closes conn. The whole probe, handshake included, must finish within the response timeout.
func probe(ctx context.Context, conn net.Conn, target string, opts Options, useTLS bool) result {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := conn.SetDeadline(time.Now().Add(opts.ResponseTimeout)); err != nil {
		return failed
	}

	var rw io.ReadWriter = conn
	if useTLS {
		// The check only learns the protocol, so a self-signed certificate is accepted here. Nothing
		// is sent over this connection except the request below.
		host, _, _ := net.SplitHostPort(target)
		sc := &sniffConn{Conn: conn}
		tc := tls.Client(sc, probeTLSConfig(host))
		if err := tc.Handshake(); err != nil {
			if sc.speaksTLS() {
				return gotTLS
			}
			return classify(err)
		}
		rw = tc
	}

	io.WriteString(rw, "GET / HTTP/1.1\r\nHost: "+target+"\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(rw), nil)
	if err != nil {
		return classify(err)
	}
	defer resp.Body.Close()
	if useTLS {
		return gotHTTP
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxHintBody))
	lower := strings.ToLower(string(body))
	for _, hint := range tlsHints {
		if strings.Contains(lower, hint) {
			return gotTLSHint
		}
	}
	return gotHTTP
}

// probeTLSConfig accepts every TLS version and cipher suite that Go can speak, not only the defaults,
// so the probe learns the protocol of an old target (TLS 1.0 or 1.1, RSA key exchange only) that the
// default client refuses. It is used for detection only, never to send data.
func probeTLSConfig(host string) *tls.Config {
	var suites []uint16
	for _, cs := range tls.CipherSuites() {
		suites = append(suites, cs.ID)
	}
	for _, cs := range tls.InsecureCipherSuites() {
		suites = append(suites, cs.ID)
	}
	return &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS10,
		CipherSuites:       suites,
	}
}

// sniffConn remembers the first two bytes the target sends, so a failed handshake can tell a TLS
// reply from anything else.
type sniffConn struct {
	net.Conn
	head []byte
}

func (c *sniffConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if need := 2 - len(c.head); need > 0 {
		c.head = append(c.head, p[:min(n, need)]...)
	}
	return n, err
}

// speaksTLS reports whether the target's first bytes are a TLS record header: a handshake (0x16) or
// alert (0x15) record with major version 3. A server hello or a TLS alert both start that way.
func (c *sniffConn) speaksTLS() bool {
	return len(c.head) == 2 && (c.head[0] == 0x16 || c.head[0] == 0x15) && c.head[1] == 0x03
}

// classify maps an error from a probe to its result. A timeout is silence. A closed or reset
// connection is failed. Anything else is bytes that are not HTTP.
func classify(err error) result {
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return silent
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed),
		errors.Is(err, syscall.ECONNRESET):
		return failed
	}
	return gotNonHTTP
}
