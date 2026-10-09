package kinds

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/protocol"
)

// testOptions are the design defaults: 3 s to connect and 3 s for a response.
var testOptions = Options{ConnectTimeout: 3 * time.Second, ResponseTimeout: 3 * time.Second}

// hello answers every HTTP request with 200 OK.
var hello = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	io.WriteString(w, "ok")
})

// tlsExpectedBody is the body nginx returns to a plain HTTP request sent to an HTTPS port.
const tlsExpectedBody = "<html>\r\n<head><title>400 The plain HTTP request was sent to HTTPS port</title></head>\r\n" +
	"<body>\r\n<center><h1>400 Bad Request</h1></center>\r\n" +
	"<center>The plain HTTP request was sent to HTTPS port</center>\r\n</body>\r\n</html>\r\n"

// serve listens on 127.0.0.1 and runs handle on each connection in its own goroutine. The test's end
// closes the listener and every connection, which ends the handlers. It returns the listen address.
func serve(t *testing.T, handle func(c net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		closed bool
		conns  []net.Conn
	)
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		closed = true
		for _, c := range conns {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				c.Close()
				return
			}
			conns = append(conns, c)
			mu.Unlock()
			go handle(c)
		}
	}()
	return ln.Addr().String()
}

// refusedAddr returns a 127.0.0.1 address with nothing listening on it.
func refusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// expectDetect runs Detect on target and fails the test unless it returns want and conclusive. The
// probe is bounded to 30 s so a hung probe fails the test instead of the whole run. A panic in Detect
// fails only this test, so every case reports on its own.
func expectDetect(t *testing.T, target string, opts Options, want protocol.Kind, conclusive bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Detect(%s) panicked: %v", target, r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	kind, ok := Detect(ctx, target, opts)
	if kind != want || ok != conclusive {
		t.Fatalf("Detect(%s) = kind %d, conclusive %t; want kind %d, conclusive %t",
			target, kind, ok, want, conclusive)
	}
}

// Case 1: an HTTPS server with a self-signed certificate is https.
func TestDetectTLSServerIsHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(hello)
	t.Cleanup(srv.Close)
	expectDetect(t, srv.Listener.Addr().String(), testOptions, protocol.KindHTTPS, true)
}

// Case 2: a plain HTTP server is http.
func TestDetectPlainServerIsHTTP(t *testing.T) {
	srv := httptest.NewServer(hello)
	t.Cleanup(srv.Close)
	expectDetect(t, srv.Listener.Addr().String(), testOptions, protocol.KindHTTP, true)
}

// Case 3: the TLS probe fails, and the plain HTTP probe gets a 400 that says the port expects TLS. That
// is https.
func TestDetectPlainReplySaysTLSIsHTTPS(t *testing.T) {
	addr := serve(t, func(c net.Conn) {
		var buf [1024]byte
		_, _ = c.Read(buf[:]) // the TLS hello or the plain request; the reply is the same for both
		fmt.Fprintf(c, "HTTP/1.1 400 Bad Request\r\nContent-Type: text/html\r\nContent-Length: %d\r\n\r\n%s",
			len(tlsExpectedBody), tlsExpectedBody)
		io.Copy(io.Discard, c)
	})
	expectDetect(t, addr, testOptions, protocol.KindHTTPS, true)
}

// Case 4: a target that sends a non-HTTP banner (an SSH server) is tcp.
func TestDetectNonHTTPBannerIsTCP(t *testing.T) {
	addr := serve(t, func(c net.Conn) {
		io.WriteString(c, "SSH-2.0-test\r\n")
		io.Copy(io.Discard, c)
	})
	expectDetect(t, addr, testOptions, protocol.KindTCP, true)
}

// Case 5: a target that accepts and never answers is tcp, conclusive, after two silent probes. The
// response timeout is 300 ms so the test is quick; the design value is 3 s.
func TestDetectSilentTargetIsTCP(t *testing.T) {
	addr := serve(t, func(c net.Conn) {
		io.Copy(io.Discard, c)
	})
	opts := Options{ConnectTimeout: 3 * time.Second, ResponseTimeout: 300 * time.Millisecond}
	expectDetect(t, addr, opts, protocol.KindTCP, true)
}

// Case 6: a refused port is inconclusive, with no kind.
func TestDetectRefusedPortIsInconclusive(t *testing.T) {
	expectDetect(t, refusedAddr(t), testOptions, protocol.KindUnknown, false)
}

// Case 7: a target that accepts and then closes is inconclusive, with no kind.
func TestDetectAcceptThenCloseIsInconclusive(t *testing.T) {
	addr := serve(t, func(c net.Conn) {
		c.Close()
	})
	expectDetect(t, addr, testOptions, protocol.KindUnknown, false)
}

// tlsServer runs an HTTPS server with cfg on 127.0.0.1 until the test ends, and returns its address. The
// certificate is the one net/http/httptest uses.
func tlsServer(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(hello)
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// requireRefusedByDefaultClient fails the test unless Go's default TLS client refuses addr. The old setups
// below only test the probe when the default client fails on them.
func requireRefusedByDefaultClient(t *testing.T, addr string) {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err == nil {
		conn.Close()
		t.Fatal("the default TLS client completes the handshake, so the test does not cover the old setup")
	}
}

// Case 8: an HTTPS server that speaks only TLS 1.0 is https. Go's default client refuses TLS 1.0, so the
// probe must accept it.
func TestDetectTLS10ServerIsHTTPS(t *testing.T) {
	addr := tlsServer(t, &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS10})
	requireRefusedByDefaultClient(t, addr)
	expectDetect(t, addr, testOptions, protocol.KindHTTPS, true)
}

// Case 9: an HTTPS server that offers only RSA key exchange (TLS 1.2) is https. Go's default client has no
// such suite, so the probe must accept it.
func TestDetectRSAKeyExchangeOnlyIsHTTPS(t *testing.T) {
	addr := tlsServer(t, &tls.Config{
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS12,
		CipherSuites: []uint16{tls.TLS_RSA_WITH_AES_128_CBC_SHA},
	})
	requireRefusedByDefaultClient(t, addr)
	expectDetect(t, addr, testOptions, protocol.KindHTTPS, true)
}

// Case 10: a target that answers the TLS hello with a TLS alert and closes speaks TLS, so it is https. The
// plain probe would read the alert as bytes that are not HTTP and say tcp.
func TestDetectTLSAlertIsHTTPS(t *testing.T) {
	addr := serve(t, func(c net.Conn) {
		var buf [1024]byte
		_, _ = c.Read(buf[:])                                     // the TLS hello
		c.Write([]byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x28}) // fatal handshake_failure
		c.Close()
	})
	expectDetect(t, addr, testOptions, protocol.KindHTTPS, true)
}

// Case 11: the plain-HTTP error pages of Go and Apache say the port expects TLS, as nginx's does, so both
// are https.
func TestDetectOtherTLSHintsAreHTTPS(t *testing.T) {
	replies := map[string]string{
		"Go": "HTTP/1.0 400 Bad Request\r\n\r\nClient sent an HTTP request to an HTTPS server.\n",
		"Apache": "HTTP/1.1 400 Bad Request\r\nContent-Type: text/html\r\nConnection: close\r\n\r\n" +
			"<html><body><h1>Bad Request</h1>\n<p>Reason: You're speaking plain HTTP to an SSL-enabled server port.</p>\n" +
			"</body></html>\n",
	}
	for name, reply := range replies {
		t.Run(name, func(t *testing.T) {
			addr := serve(t, func(c net.Conn) {
				var buf [1024]byte
				_, _ = c.Read(buf[:])
				io.WriteString(c, reply)
				c.Close()
			})
			expectDetect(t, addr, testOptions, protocol.KindHTTPS, true)
		})
	}
}
