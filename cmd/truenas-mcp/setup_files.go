package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// cleanName lower-cases s and maps it onto [a-z0-9.-]: every other
// character becomes "-", runs of "-" collapse, and leading or trailing
// "-" and "." are dropped.
func cleanName(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-.")
}

// hostFileStem names the per-host files: the cleaned host name, plus
// "_<port>" when the port is not 443. IPv6 colons become "-".
func hostFileStem(u *url.URL) string {
	stem := cleanName(u.Hostname())
	if p := u.Port(); p != "" && p != "443" {
		stem += "_" + p
	}
	return stem
}

// ensureConfigDir creates dir (and missing parents) with mode 0700. An
// existing directory that group or others can write to is refused: another
// user could swap the key file between the write and the rename.
func ensureConfigDir(dir string) error {
	st, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("cannot create %s: %w", dir, err)
		}
		// MkdirAll applies the umask; set the mode exactly.
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("cannot set mode 0700 on %s: %w", dir, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot use %s: %w", dir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (mode %04o); use a private directory (chmod 700) or --config-dir", dir, st.Mode().Perm())
	}
	return nil
}

var errFileExists = errors.New("file exists")

// writeFileAtomic writes data to path with mode 0600: a temporary file in
// the same directory is written, fsynced and then moved into place, so a
// reader never sees a partial file. Without overwrite, an existing path is
// left alone and errFileExists is returned.
func writeFileAtomic(path string, data []byte, overwrite bool) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("cannot create a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		return fmt.Errorf("cannot set mode 0600 on %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("cannot write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("cannot sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cannot close %s: %w", tmpName, err)
	}
	if overwrite {
		if err := renameReplace(tmpName, path); err != nil {
			return fmt.Errorf("cannot move the new file to %s: %w", path, err)
		}
	} else {
		// A hard link fails if path exists, so a file created since the
		// earlier check is not clobbered. Fall back to check-then-rename
		// where links are not supported.
		if err := os.Link(tmpName, path); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return errFileExists
			}
			if _, statErr := os.Lstat(path); statErr == nil {
				return errFileExists
			}
			if err := os.Rename(tmpName, path); err != nil {
				return fmt.Errorf("cannot move the new file to %s: %w", path, err)
			}
		} else if err := os.Remove(tmpName); err != nil {
			return fmt.Errorf("cannot remove %s: %w", tmpName, err)
		}
	}
	syncDir(dir)
	return nil
}

// renameReplace moves src over dst.
var renameReplace = os.Rename

// syncDir fsyncs a directory so a rename survives a crash. Windows cannot
// open a directory for sync; the rename is still atomic there.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
