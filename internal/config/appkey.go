package config

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/andrewloable/HoleBridge/internal/errs"
	"github.com/andrewloable/HoleBridge/internal/keys"
)

// app.key holds the application key, a per-deployment secret (docs/security.md, the application
// key): 64 lowercase hex digits and a newline, mode 0600, in the config directory.

// LoadAppKey reads dir/app.key. A missing file returns HB-APPKEY-MISSING. On POSIX systems a file
// that group or others can read returns HB-APPKEY-PERMS. Malformed content returns
// HB-APPKEY-INVALID, without the content in the error.
func LoadAppKey(dir string) ([32]byte, error) {
	f, err := os.Open(filepath.Join(dir, "app.key"))
	if errors.Is(err, fs.ErrNotExist) {
		return [32]byte{}, errs.E("HB-APPKEY-MISSING", "", nil)
	}
	if err != nil {
		return [32]byte{}, err
	}
	defer f.Close()
	if runtime.GOOS != "windows" {
		info, err := f.Stat()
		if err != nil {
			return [32]byte{}, err
		}
		if info.Mode().Perm()&0o077 != 0 {
			return [32]byte{}, errs.E("HB-APPKEY-PERMS", "", nil)
		}
	}

	// The cap keeps a stray large file from being read whole; too long is INVALID anyway.
	raw, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return [32]byte{}, err
	}
	s := strings.TrimSuffix(string(raw), "\n")
	// The catalog says capital letters are invalid, so the key must round-trip exactly. The parse
	// error is not wrapped: its text can quote a byte of the key.
	k, err := keys.ParseAppKey(s)
	if err != nil || keys.FormatAppKey(k) != s {
		return [32]byte{}, errs.E("HB-APPKEY-INVALID", "want 64 lowercase hex digits", nil)
	}
	return k, nil
}

// CreateAppKey generates a random application key and writes it to dir/app.key. It works only
// when no app.key exists: an existing file is left alone and the error wraps fs.ErrExist.
func CreateAppKey(dir string) ([32]byte, error) {
	k := keys.NewAppKey()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return [32]byte{}, err
	}
	// O_EXCL makes the create fail, and not overwrite, when the file exists.
	f, err := os.OpenFile(filepath.Join(dir, "app.key"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return [32]byte{}, err
	}
	_, err = f.WriteString(keys.FormatAppKey(k) + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name()) // a partial file would block the next create
		return [32]byte{}, err
	}
	return k, nil
}

// SaveAppKey writes k to dir/app.key, replacing any file there, through an atomic rename, mode
// 0600. It is used by app-key --set and --rotate.
func SaveAppKey(dir string, k [32]byte) error {
	return writeAtomic(dir, "app.key", []byte(keys.FormatAppKey(k)+"\n"))
}

// writeAtomic writes data to dir/name through a temp file and a rename, mode 0600: CreateTemp
// makes the temp file 0600 and the rename keeps it. A failed write leaves the old file in place.
func writeAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has moved the file
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}
