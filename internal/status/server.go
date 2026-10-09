// Package status is the local control socket of a running host or relay: "holebridge status" and reload talk
// to it. It runs on a Unix socket in the config directory (a named pipe on Windows) and answers JSON lines. It
// never carries a key or a target address (docs/architecture.md, State, status and logs).
package status

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/andrewloable/HoleBridge/pears/dhtrpc"
)

// socketName is the control socket's file name in the config directory.
const socketName = "control.sock"

// maxRequest bounds a request line. A request is a few bytes long.
const maxRequest = 4096

// errAlreadyServing is returned by Serve when another Serve already listens in the directory.
var errAlreadyServing = errors.New("status: a control socket already serves this directory")

// listener is the server side of the control socket: Accept hands out one connection at a time.
type listener interface {
	Accept() (io.ReadWriteCloser, error)
	Close() error
}

// deadlines bound one connection: the request line must arrive within request of the connection's start, and the
// answer must be taken within answer of the start of the write.
type deadlines struct {
	request time.Duration
	answer  time.Duration
}

// defaultDeadlines are the deadlines Serve uses. A request takes milliseconds, so a few seconds is generous.
var defaultDeadlines = deadlines{request: 5 * time.Second, answer: 5 * time.Second}

// deadliner is a connection that takes deadlines: Unix socket connections do. On Windows a named pipe handle
// returns an error from these calls, so its reads and writes have no deadline there. Serve still stops such a
// connection's handler calls, and it closes the handle, but a read that is already blocked may stay blocked.
type deadliner interface {
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// Handler answers the control socket. Status returns the snapshot that a status request reports. Reload is run
// by a reload request. Each request runs on its own goroutine, so the handler must be safe for concurrent use.
type Handler interface {
	Status() Status
	Reload() error
}

// Status is the snapshot a status request returns: the services, the sessions with their routes and counts, the
// NAT state and whether a relay is set. It has no field for a key or a target address.
type Status struct {
	Services []Service      `json:"services"`
	Sessions []Session      `json:"sessions"`
	NAT      dhtrpc.NATInfo `json:"nat"`
	Relay    bool           `json:"relay"`
}

// Service is one configured service, by name and kind.
type Service struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Session is one connection: its route, its streams and flows, and the bytes it carried.
type Session struct {
	Route    string `json:"route"`
	Streams  int    `json:"streams"`
	Flows    int    `json:"flows"`
	BytesIn  uint64 `json:"bytesIn"`
	BytesOut uint64 `json:"bytesOut"`
}

// request is one request line: {"cmd": "status"} or {"cmd": "reload"}.
type request struct {
	Cmd string `json:"cmd"`
}

// reloadAnswer is the answer to a reload request: {"ok": true}, or {"ok": false, "error": "..."}.
type reloadAnswer struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// errorAnswer is the answer to a request the server does not understand.
type errorAnswer struct {
	Error string `json:"error"`
}

// Serve listens on the control socket in dir and answers requests with h until ctx is done. It returns nil when
// ctx ends, and an error when it cannot listen, for example because another Serve already listens in dir. When it
// returns, its connections have been closed and no handler call is running or will start.
func Serve(ctx context.Context, dir string, h Handler) error {
	return serve(ctx, dir, h, defaultDeadlines)
}

// serve is Serve with the deadlines given, so the tests can use short ones.
func serve(ctx context.Context, dir string, h Handler, d deadlines) error {
	ln, err := listen(dir)
	if err != nil {
		return err
	}
	s := &server{h: h, deadlines: d, conns: map[io.ReadWriteCloser]struct{}{}}
	defer s.shutdown()
	defer ln.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			ln.Close()
		case <-stop:
		}
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.track(conn)
		go s.serveConn(conn)
	}
}

// server is the state of one Serve: its live connections, so that Serve can close them, and the handler calls in
// progress, so that Serve can wait for them.
type server struct {
	h         Handler
	deadlines deadlines
	mu        sync.Mutex
	conns     map[io.ReadWriteCloser]struct{}
	stopped   bool
	calls     sync.WaitGroup
}

func (s *server) track(conn io.ReadWriteCloser) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns[conn] = struct{}{}
}

func (s *server) untrack(conn io.ReadWriteCloser) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
}

// startCall reports whether a handler call may start. Once shutdown has begun it returns false, so a request read
// just before the stop is not run. A true return must be followed by calls.Done.
func (s *server) startCall() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	s.calls.Add(1)
	return true
}

// shutdown stops new handler calls, closes the live connections, and waits for the calls in progress.
func (s *server) shutdown() {
	s.mu.Lock()
	s.stopped = true
	for conn := range s.conns {
		conn.Close()
	}
	s.mu.Unlock()
	s.calls.Wait()
}

// serveConn reads one request line from conn, writes one answer line and closes conn. The request line and the
// answer each have a deadline, so a client that goes quiet does not hold the connection.
func (s *server) serveConn(conn io.ReadWriteCloser) {
	defer s.untrack(conn)
	defer conn.Close()
	if d, ok := conn.(deadliner); ok {
		_ = d.SetReadDeadline(time.Now().Add(s.deadlines.request))
	}
	line, _ := bufio.NewReader(io.LimitReader(conn, maxRequest)).ReadBytes('\n')
	if len(line) == 0 {
		return
	}
	ans, ok := s.answer(line)
	if !ok {
		return
	}
	out, err := json.Marshal(ans)
	if err != nil {
		return
	}
	if d, ok := conn.(deadliner); ok {
		_ = d.SetWriteDeadline(time.Now().Add(s.deadlines.answer))
	}
	_, _ = conn.Write(append(out, '\n'))
}

// answer returns the answer to the request in line. It reports false, and runs nothing, once shutdown has begun.
func (s *server) answer(line []byte) (any, bool) {
	var req request
	if json.Unmarshal(line, &req) != nil {
		return errorAnswer{Error: "bad request"}, true
	}
	if req.Cmd != "status" && req.Cmd != "reload" {
		return errorAnswer{Error: "unknown command"}, true
	}
	if !s.startCall() {
		return nil, false
	}
	defer s.calls.Done()
	if req.Cmd == "status" {
		return s.h.Status(), true
	}
	if err := s.h.Reload(); err != nil {
		return reloadAnswer{Error: err.Error()}, true
	}
	return reloadAnswer{OK: true}, true
}
