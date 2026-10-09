//go:build unix

package status

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/andrewloable/HoleBridge/internal/errs"
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

// umaskMu serializes the umask change that bind makes. The umask belongs to the whole process, so two binds must
// not interleave their save and restore.
var umaskMu sync.Mutex

// bind creates the socket file at path. The umask is 0177 during the bind, so the file is created with mode 0600
// and is never wider than that, not even for the moment before a chmod.
func bind(path string) (net.Listener, error) {
	umaskMu.Lock()
	defer umaskMu.Unlock()
	old := syscall.Umask(0o177)
	defer syscall.Umask(old)
	return net.Listen("unix", path)
}

// checkDir makes sure the control socket can only be reached by its owner. A missing dir is created with mode
// 0700. An existing dir must be owned by the current user and have mode 0700, or listen refuses with
// HB-CONFIG-DIR-PERMS: another user who can enter the dir could reach the socket before it is locked down.
func checkDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return errs.E("HB-CONFIG-DIR-PERMS", "owned by another user", nil)
	}
	if mode := fi.Mode().Perm(); mode != 0o700 {
		return errs.E("HB-CONFIG-DIR-PERMS", fmt.Sprintf("mode %04o", mode), nil)
	}
	return nil
}

// listen binds the control socket in dir and makes it owner-only (mode 0600). dir must pass checkDir first. A socket
// file left by a server that stopped without cleaning up is replaced. A socket that still answers belongs to a live
// server, and listen fails with errAlreadyServing.
func listen(dir string) (listener, error) {
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, socketName)
	ln, err := bind(path)
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
		if ln, err = bind(path); err != nil {
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
