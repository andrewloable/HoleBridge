//go:build windows

package status

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ownerOnly is the security descriptor of the pipe: the owner gets full access, nobody else any.
const ownerOnly = "D:P(A;;GA;;;OW)"

// pipeName is the named pipe of the control socket for dir: \\.\pipe\holebridge-<hash of dir>.
func pipeName(dir string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(dir))))
	return `\\.\pipe\holebridge-` + hex.EncodeToString(sum[:8])
}

// pipeListener serves the control socket as a named pipe. Each Accept hands out one connected instance.
type pipeListener struct {
	name   string
	wname  *uint16
	sa     *windows.SecurityAttributes
	mu     sync.Mutex
	first  windows.Handle // the instance listen created; the first Accept takes it
	closed bool
}

// createPipe creates one instance of the pipe. Byte mode, blocking, no remote clients.
func createPipe(name *uint16, sa *windows.SecurityAttributes, flags uint32) (windows.Handle, error) {
	return windows.CreateNamedPipe(name,
		windows.PIPE_ACCESS_DUPLEX|flags,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		windows.PIPE_UNLIMITED_INSTANCES, 4096, 4096, 0, sa)
}

// listen creates the first instance of the pipe in dir. FILE_FLAG_FIRST_PIPE_INSTANCE makes a second listen
// fail while the first one lives.
func listen(dir string) (listener, error) {
	name := pipeName(dir)
	wname, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString(ownerOnly)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	h, err := createPipe(wname, sa, windows.FILE_FLAG_FIRST_PIPE_INSTANCE)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, errAlreadyServing
		}
		return nil, err
	}
	return &pipeListener{name: name, wname: wname, sa: sa, first: h}, nil
}

// instance returns an unconnected pipe instance: the one listen made, or a new one.
func (l *pipeListener) instance() (windows.Handle, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, net.ErrClosed
	}
	if h := l.first; h != 0 {
		l.first = 0
		return h, nil
	}
	return createPipe(l.wname, l.sa, 0)
}

// Accept waits for a client and returns its connection. Close wakes it by connecting once.
func (l *pipeListener) Accept() (io.ReadWriteCloser, error) {
	for {
		h, err := l.instance()
		if err != nil {
			return nil, err
		}
		err = windows.ConnectNamedPipe(h, nil)
		l.mu.Lock()
		closed := l.closed
		l.mu.Unlock()
		if closed {
			windows.CloseHandle(h)
			return nil, net.ErrClosed
		}
		if err == nil || errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			return os.NewFile(uintptr(h), l.name), nil
		}
		windows.CloseHandle(h)
		// ERROR_NO_DATA: the client connected and left before this call. Wait for the next client.
		if !errors.Is(err, windows.ERROR_NO_DATA) {
			return nil, err
		}
	}
}

// Close stops the listener. A pending ConnectNamedPipe is woken by a connection from this process, which
// Accept then drops.
func (l *pipeListener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	if l.first != 0 {
		windows.CloseHandle(l.first)
		l.first = 0
	}
	l.mu.Unlock()
	if c, err := os.OpenFile(l.name, os.O_RDWR, 0); err == nil {
		c.Close()
	}
	return nil
}

// dial connects to the control socket in dir. A missing pipe means nothing runs there.
func dial(dir string) (io.ReadWriteCloser, error) {
	f, err := os.OpenFile(pipeName(dir), os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotRunning
		}
		return nil, err
	}
	return f, nil
}
