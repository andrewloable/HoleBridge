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
// ctx ends, and an error when it cannot listen, for example because another Serve already listens in dir.
func Serve(ctx context.Context, dir string, h Handler) error {
	ln, err := listen(dir)
	if err != nil {
		return err
	}
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
		go serveConn(conn, h)
	}
}

// serveConn reads one request line from conn, writes one answer line and closes conn.
func serveConn(conn io.ReadWriteCloser, h Handler) {
	defer conn.Close()
	line, _ := bufio.NewReader(io.LimitReader(conn, maxRequest)).ReadBytes('\n')
	if len(line) == 0 {
		return
	}
	var ans any = errorAnswer{Error: "bad request"}
	var req request
	if json.Unmarshal(line, &req) == nil {
		switch req.Cmd {
		case "status":
			ans = h.Status()
		case "reload":
			ans = reloadAnswer{OK: true}
			if err := h.Reload(); err != nil {
				ans = reloadAnswer{Error: err.Error()}
			}
		default:
			ans = errorAnswer{Error: "unknown command"}
		}
	}
	out, err := json.Marshal(ans)
	if err != nil {
		return
	}
	_, _ = conn.Write(append(out, '\n'))
}
