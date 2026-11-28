package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Local sources for upload_file. Reading files on the host running this
// server is off unless the operator lists the directories it may read
// (--upload-source-dirs / TRUENAS_MCP_UPLOAD_SOURCE_DIRS). Without that, a
// confirmation token returned in the tool reply (clients without form
// elicitation) would let text injected into the conversation copy SSH
// keys, cloud credentials or this server's own API key onto a dataset.

// UploadSourceSetting names the operator setting that enables source_path.
const UploadSourceSetting = "--upload-source-dirs / TRUENAS_MCP_UPLOAD_SOURCE_DIRS"

// uploadTempPrefix is the name prefix of upload_file's temporary
// downloads; the startup sweep removes only files with it.
const uploadTempPrefix = "truenas-mcp-upload-"

// refusedSourceNames are file names a source_path never has: MCP client
// configurations, which hold this server's API key.
var refusedSourceNames = map[string]bool{
	"claude_desktop_config.json": true,
	"mcp.json":                   true,
}

// refusedSourceExts are file extensions a source_path never has: key and
// certificate files, such as the API key file setup writes.
var refusedSourceExts = map[string]bool{".key": true, ".pem": true, ".pin": true}

// localSources is the operator's local source configuration.
type localSources struct {
	// dirs are the real paths of the directories source_path may read
	// below; empty means local sources are off.
	dirs []string
	// protected are real paths (files or directories) a source_path may
	// never be or be below: this server's API key file, CA file and
	// configuration directory.
	protected []string
	// protectedFiles are the stats of the protected paths that are files,
	// so a hard link to one (or, where names ignore case, another spelling
	// of its path) is refused as the same file.
	protectedFiles []os.FileInfo
}

// SetUploadSourceDirs enables source_path for files below dirs (absolute
// paths of existing directories) and refuses the protected paths
// (this server's key, certificate and configuration files or
// directories; missing ones are ignored). An empty dirs keeps local
// sources off.
func (r *Registry) SetUploadSourceDirs(dirs, protected []string) error {
	var ls localSources
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if !filepath.IsAbs(d) {
			return fmt.Errorf("upload source directory %q is not an absolute path", d)
		}
		real, err := filepath.EvalSymlinks(d)
		if err != nil {
			return fmt.Errorf("upload source directory %s: %w", d, localPathError(err))
		}
		st, err := os.Stat(real)
		if err != nil {
			return fmt.Errorf("upload source directory %s: %w", d, localPathError(err))
		}
		if !st.IsDir() {
			return fmt.Errorf("upload source directory %s is not a directory", d)
		}
		if isFilesystemRoot(real) {
			return fmt.Errorf("upload source directory %s is the root of a filesystem; list the directories that hold the files to upload", d)
		}
		if c := dotComponent(real); c != "" {
			return fmt.Errorf("upload source directory %s resolves to %s, which goes through the hidden directory %q; files there are never read", d, real, c)
		}
		ls.dirs = append(ls.dirs, real)
	}
	for _, p := range protected {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			ls.protected = append(ls.protected, filepath.Clean(abs))
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			if abs, err := filepath.Abs(real); err == nil {
				ls.protected = append(ls.protected, abs)
			}
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			loadFileIdentity(st)
			ls.protectedFiles = append(ls.protectedFiles, st)
		}
	}
	r.uploadMu.Lock()
	r.localSources = ls
	r.uploadMu.Unlock()
	return nil
}

func (r *Registry) localSourceConfig() localSources {
	r.uploadMu.Lock()
	defer r.uploadMu.Unlock()
	return r.localSources
}

func isFilesystemRoot(p string) bool {
	return filepath.Dir(p) == p
}

// dotComponent is the first dot-prefixed component of the clean absolute
// path p, or "".
func dotComponent(p string) string {
	p = filepath.ToSlash(strings.TrimPrefix(p, filepath.VolumeName(p)))
	for _, c := range strings.Split(p, "/") {
		if strings.HasPrefix(c, ".") {
			return c
		}
	}
	return ""
}

// within reports whether p is dir or below it (both clean, absolute).
func within(p, dir string) bool {
	if pathEqual(p, dir) {
		return true
	}
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// localSourceRefusal is why the resolved real path of a source_path may
// not be read, or "".
func (ls localSources) refusal(real string) string {
	if len(ls.dirs) == 0 {
		return fmt.Sprintf("source_path is disabled: this server reads local files only below directories the operator lists with %s. Use source_url, or ask the operator to set it", UploadSourceSetting)
	}
	inside := false
	for _, d := range ls.dirs {
		if within(real, d) {
			inside = true
			break
		}
	}
	if !inside {
		return fmt.Sprintf("source_path %s is not below an allowed upload source directory (%s); allowed: %s", real, UploadSourceSetting, strings.Join(ls.dirs, ", "))
	}
	if c := dotComponent(real); c != "" {
		return fmt.Sprintf("source_path %s goes through the hidden file or directory %q; hidden paths (such as .ssh, .aws or .config) are never read", real, c)
	}
	for _, p := range ls.protected {
		if within(real, p) {
			return fmt.Sprintf("source_path %s is this server's own key, certificate or configuration file, which is never uploaded", real)
		}
	}
	base := strings.ToLower(filepath.Base(real))
	if refusedSourceNames[base] {
		return fmt.Sprintf("source_path %s is an MCP client configuration file, which can hold API keys and is never uploaded", real)
	}
	if refusedSourceExts[filepath.Ext(base)] {
		return fmt.Sprintf("source_path %s is a key or certificate file (%s), which is never uploaded", real, filepath.Ext(base))
	}
	return ""
}

// isProtectedFile reports whether st is one of the protected files,
// compared as files rather than by path.
func (ls localSources) isProtectedFile(st os.FileInfo) bool {
	for _, pf := range ls.protectedFiles {
		if os.SameFile(pf, st) {
			return true
		}
	}
	return false
}

// loadFileIdentity makes st carry its file identity now. Where os.Stat
// does not fill it in (Windows), os.SameFile reads it lazily by path on
// first use, which would be from whatever is at the path by then.
func loadFileIdentity(st os.FileInfo) {
	_ = os.SameFile(st, st)
}

// SweepStaleUploadTemps removes temporary downloads left by an earlier run
// that stopped before cleaning up (a crash or a kill): regular files named
// truenas-mcp-upload-* directly in the temporary directory, last written
// longer ago than the longest a transfer may run, so a download of
// another running server is never touched. It returns how many it removed.
func (r *Registry) SweepStaleUploadTemps() int {
	return sweepUploadTemps(tempDirName(r.uploadCfg.tempDir), time.Now().Add(-(r.uploadCfg.maxDuration + time.Hour)))
}

func sweepUploadTemps(dir string, before time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), uploadTempPrefix) || !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() || !st.ModTime().Before(before) {
			continue
		}
		if err := os.Remove(p); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				log.Printf("upload_file: could not remove the stale temporary file %s: %v", p, err)
			}
			continue
		}
		removed++
	}
	return removed
}
