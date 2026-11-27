//go:build unix

package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// fileID is the device and inode of fi, bound into the confirmation so a
// file swapped in at the same path is refused.
func fileID(fi os.FileInfo) string {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino))
	}
	return ""
}

func pathEqual(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }
