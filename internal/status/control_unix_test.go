//go:build unix

package status

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/andrewloable/HoleBridge/internal/errs"
)

// errCodeOf returns the catalog code of err, or "" when err is not an errs.Error.
func errCodeOf(err error) string {
	var e *errs.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// A config dir that others can enter is refused with HB-CONFIG-DIR-PERMS, and no socket is bound in it.
func TestServeRefusesDirOpenToOthers(t *testing.T) {
	dir := socketDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	err := Serve(context.Background(), dir, &fakeHandler{})
	if code := errCodeOf(err); code != "HB-CONFIG-DIR-PERMS" {
		t.Fatalf("Serve error = %v, want HB-CONFIG-DIR-PERMS", err)
	}
	if _, serr := os.Lstat(filepath.Join(dir, socketName)); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatal("a socket was bound in the refused directory")
	}
}

// A directory owned by another user is refused. The root directory is owned by root, so the test needs a user
// that is not root.
func TestCheckDirRefusesDirOwnedByAnotherUser(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root owns the directory, so the owner check passes")
	}
	err := checkDir("/")
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "HB-CONFIG-DIR-PERMS" || e.Detail != "owned by another user" {
		t.Fatalf("checkDir(/) = %v, want HB-CONFIG-DIR-PERMS owned by another user", err)
	}
}

// A missing config dir is created with mode 0700 and serves.
func TestServeCreatesMissingDirOwnerOnly(t *testing.T) {
	dir := filepath.Join(socketDir(t), "config")
	done, _ := serveWith(t, dir, &fakeHandler{}, defaultDeadlines)
	awaitServing(t, dir, done)
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode = %o, want 700", got)
	}
}

// The socket file is never wider than 0600, not even while it is being set up. The umask is cleared for the test,
// so a bind without the restrictive umask would create the file 0777, and a tight poll of the file sees it. The
// process umask must also be restored once listen has bound.
func TestSocketNeverWiderThanOwnerOnly(t *testing.T) {
	dir := socketDir(t)
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	path := filepath.Join(dir, socketName)
	done, _ := serveWith(t, dir, &fakeHandler{}, defaultDeadlines)

	deadline := time.Now().Add(serveWait)
	for {
		if fi, err := os.Lstat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("socket mode = %o while it was being set up, want no group or other bits", fi.Mode().Perm())
		}
		select {
		case err := <-done:
			t.Fatalf("Serve returned before the socket answered: %v", err)
		default:
		}
		if _, err := Query(dir, "status"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the control socket did not answer within 5 s")
		}
	}
	if cur := syscall.Umask(old); cur != 0 {
		t.Fatalf("umask after listen = %03o, want 000 (the bind must restore it)", cur)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("socket mode = %o, want 600", got)
	}
}
