package kinds

import (
	"context"
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
