package status

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// ErrNotRunning is the error of a Query when no control socket answers in dir: no host or relay runs there.
var ErrNotRunning = errors.New("status: not running")

// ErrTimeout is the error of a Query when the host accepts the request but does not answer in time, for example
// because a reload is blocked.
var ErrTimeout = errors.New("status: the host did not answer in time")

// maxAnswer bounds an answer line. A status answer is a few kilobytes at most.
const maxAnswer = 1 << 20

// Waits a Query allows for its answer. A status answer is quick; a reload may take a while.
const (
	statusWait = 10 * time.Second
	reloadWait = 30 * time.Second
)

// Query sends cmd ("status" or "reload") to the control socket in dir and returns the JSON line the server
// answered. It fails with ErrNotRunning when nothing listens there, and with ErrTimeout when no answer comes in
// time.
func Query(dir string, cmd string) (json.RawMessage, error) {
	wait := statusWait
	if cmd == "reload" {
		wait = reloadWait
	}
	return query(dir, cmd, wait)
}

// query is Query with the wait given, so the tests can use a short one.
func query(dir string, cmd string, wait time.Duration) (json.RawMessage, error) {
	conn, err := dial(dir)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	req, err := json.Marshal(request{Cmd: cmd})
	if err != nil {
		return nil, err
	}
	// The exchange runs on its own goroutine, and the timer waits on it. A named pipe on Windows takes no
	// deadline, so this is the only bound there. Closing conn on return ends the goroutine on Unix.
	type result struct {
		line []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		if _, err := conn.Write(append(req, '\n')); err != nil {
			done <- result{err: err}
			return
		}
		line, err := bufio.NewReader(io.LimitReader(conn, maxAnswer)).ReadBytes('\n')
		done <- result{line: line, err: err}
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		return json.RawMessage(bytes.TrimRight(r.line, "\r\n")), nil
	case <-timer.C:
		return nil, ErrTimeout
	}
}
