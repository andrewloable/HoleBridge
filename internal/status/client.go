package status

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// ErrNotRunning is the error of a Query when no control socket answers in dir: no host or relay runs there.
var ErrNotRunning = errors.New("status: not running")

// maxAnswer bounds an answer line. A status answer is a few kilobytes at most.
const maxAnswer = 1 << 20

// Query sends cmd ("status" or "reload") to the control socket in dir and returns the JSON line the server
// answered. It fails with ErrNotRunning when nothing listens there.
func Query(dir string, cmd string) (json.RawMessage, error) {
	conn, err := dial(dir)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	req, err := json.Marshal(request{Cmd: cmd})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(io.LimitReader(conn, maxAnswer)).ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(line, "\r\n")), nil
}
