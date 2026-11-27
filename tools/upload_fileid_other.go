//go:build !unix

package tools

import (
	"os"
	"path/filepath"
	"strings"
)

// fileID is "" where no device and inode are available; os.SameFile on
// the opened file still catches a swapped file.
func fileID(os.FileInfo) string { return "" }

func pathEqual(a, b string) bool { return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) }
