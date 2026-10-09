//go:build unix

package status

import (
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// unixListener is a net.Listener that hands out io.ReadWriteCloser connections.
type unixListener struct {
	net.Listener
}

func (l unixListener) Accept() (io.ReadWriteCloser, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return c, nil
}

// listen binds the control socket in dir and makes it owner-only (mode 0600). A socket file left by a server
// that stopped without cleaning up is replaced. A socket that still answers belongs to a live server, and
// listen fails with errAlreadyServing. The config directory is 0700, so no other user can reach the file
// between the bind and the chmod.
func listen(dir string) (listener, error) {
	path := filepath.Join(dir, socketName)
	ln, err := net.Listen("unix", path)
	if err != nil {
		if c, derr := net.Dial("unix", path); derr == nil {
			c.Close()
			return nil, errAlreadyServing
		}
		fi, serr := os.Lstat(path)
		if serr != nil || fi.Mode()&fs.ModeSocket == 0 {
			return nil, err
		}
		if rerr := os.Remove(path); rerr != nil {
			return nil, err
		}
		if ln, err = net.Listen("unix", path); err != nil {
			return nil, err
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return unixListener{ln}, nil
}

// dial connects to the control socket in dir. A missing socket, or a socket file that refuses connections,
// means nothing runs there.
func dial(dir string) (io.ReadWriteCloser, error) {
	c, err := net.Dial("unix", filepath.Join(dir, socketName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, ErrNotRunning
		}
		return nil, err
	}
	return c, nil
}
