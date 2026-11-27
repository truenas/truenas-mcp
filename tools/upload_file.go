package tools

// Upload tool (upload_file). It puts one file, downloaded by this server
// from an https URL or read from a local path on this host, onto a dataset
// below /mnt/<pool> with the /_upload endpoint and filesystem.put, a job
// tracked as a task. A URL source is downloaded to a private temporary
// file and its SHA-256 checked before anything is sent; the transfer runs
// in the background, so multi-gigabyte files are not bound by the tool
// call deadline. TrueNAS 25.10's filesystem.put does no path check of its
// own (it creates missing directories and follows symlinks), so every
// target rule is enforced here.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/truenas/truenas-mcp/internal/safego"
	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const (
	filesystemPutMethod = "filesystem.put"
)

const (
	// maxUploadBytes bounds one upload (and one download).
	maxUploadBytes = 64 << 30
	// uploadStallTimeout ends a download or upload that moves no data for
	// this long.
	uploadStallTimeout = 2 * time.Minute
	// uploadMaxDuration bounds the whole transfer: download, check and upload.
	uploadMaxDuration = 12 * time.Hour
	// uploadTaskTTL is how long an upload is tracked as a task.
	uploadTaskTTL = uploadMaxDuration + time.Hour
	// maxConcurrentUploads bounds the uploads running at once.
	maxConcurrentUploads = 2
	maxLocalSourcePath   = 4096
)

// errUploadTooLong is the cause of a transfer stopped at maxDuration.
var errUploadTooLong = errors.New("the transfer ran over its time limit")

var sha256Arg = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func (r *Registry) registerUploadTools() {
	r.tools["upload_file"] = Tool{
		Definition: mcp.Tool{
			Name:        "upload_file",
			Description: "Upload one file, such as an installer ISO, to a dataset on the TrueNAS system. The source is either source_url, an https URL that this MCP server downloads (sha256 required), or source_path, a file on the host running this MCP server (sha256 optional). source_path is off unless the operator lists allowed directories with --upload-source-dirs (TRUENAS_MCP_UPLOAD_SOURCE_DIRS); its real path must then be inside one of them, and hidden paths (any dot-prefixed component, such as .ssh), this server's key, certificate and configuration files, MCP client configurations and .key, .pem and .pin files are refused. A URL source is downloaded to a private temporary file on this host and its SHA-256 checked before anything is sent to TrueNAS; a mismatch uploads nothing. Only https is accepted, redirects are followed only to https, connections to loopback, link-local, shared, multicast and reserved addresses are always refused, and private LAN addresses are used only when the URL's host resolves only to them (redirects included); downloads do not use a proxy. The file is sent with TrueNAS's /_upload endpoint to filesystem.put, a job; the transfer runs in the background and is tracked with tasks_get (download, then upload, then the job). path is the absolute target file path in an existing directory of a mounted, unlocked, writable dataset below /mnt/<pool>; refused for files on a pool's root dataset, paths outside /mnt/<pool>, non-normalised paths, paths through a symlink, datasets TrueNAS manages itself (the boot pool, .system, ix-apps, ix-applications, .ix-virt or any dot-prefixed component, including a dot-prefixed file name), and missing parent directories (none are created). An existing file is refused unless overwrite=true; it is then truncated and rewritten in place, and the confirmation is bound to its inode, size and modification time. The preview shows the source, its size when known, the SHA-256, the target, its dataset and the free space, and refuses when the free space is clearly too small. Files up to 64 GiB; a transfer that moves no data for 2 minutes, or runs over 12 hours, is stopped. The API key's user needs the FULL_ADMIN role. If an upload fails partway, the target is left partly written. Use dry_run to preview.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 1024,
						"description": "Required: Absolute target file path below /mnt/<pool> in an existing directory of a dataset, e.g. /mnt/tank/isos/TrueNAS-SCALE.iso (not on the pool's root dataset; normalised, no symlinks)"},
					"source_url": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maxSourceURLLength,
						"description": "Optional: https URL of the file, downloaded by this MCP server (give source_url or source_path). Requires sha256"},
					"source_path": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maxLocalSourcePath,
						"description": "Optional: Absolute path of a regular file on the host running this MCP server (give source_url or source_path). Refused unless the operator allowed its directory with --upload-source-dirs"},
					"sha256": map[string]interface{}{"type": "string", "maxLength": 64,
						"description": "SHA-256 of the file as 64 hex characters. Required with source_url; optional with source_path. The file is checked against it before it is sent"},
					"overwrite": map[string]interface{}{"type": "boolean", "default": false,
						"description": "Optional: Replace an existing file at path (default: false). The confirmation is bound to that file's inode, size and modification time"},
					"dry_run": map[string]interface{}{"type": "boolean", "default": false, "description": "Optional: Preview without uploading (default: false)"},
				},
				"required": []string{"path"},
			},
		},
		Handler: r.handleUploadFile,
		Plan:    r.planUploadFile,
		Execute: r.executeUploadFile,
	}
}

// uploadSource is a parsed upload source.
type uploadSource struct {
	URL       *url.URL // nil for a local file
	LocalPath string   // resolved path of a local file
	Size      int64    // -1 when not known before the download
	ModTime   time.Time
	FileID    string      // device and inode of a local file ("" where not available)
	info      os.FileInfo // stat of a local file, for os.SameFile with the opened file
	SHA256    string      // lower case; "" when not given (local files only)
}

func (s uploadSource) display() string {
	if s.URL != nil {
		return displayURL(s.URL)
	}
	return s.LocalPath
}

func (s uploadSource) kind() string {
	if s.URL != nil {
		return "url"
	}
	return "local_file"
}

// parseUploadSource checks the source arguments and, for a local file,
// stats it.
func (r *Registry) parseUploadSource(args map[string]interface{}) (uploadSource, error) {
	rawURL, hasURL := args["source_url"].(string)
	rawPath, hasPath := args["source_path"].(string)
	sum, _ := args["sha256"].(string)
	if sum != "" && !sha256Arg.MatchString(sum) {
		return uploadSource{}, fmt.Errorf("sha256 must be 64 hex characters")
	}
	src := uploadSource{Size: -1, SHA256: strings.ToLower(sum)}
	switch {
	case hasURL == hasPath:
		return src, fmt.Errorf("give exactly one of source_url (an https URL) or source_path (a file on this host)")
	case hasURL:
		u, err := parseSourceURL(rawURL)
		if err != nil {
			return src, err
		}
		if src.SHA256 == "" {
			return src, fmt.Errorf("sha256 is required with source_url: the download is checked against it before anything is uploaded")
		}
		src.URL = u
		return src, nil
	}
	if strings.ContainsFunc(rawPath, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
		return src, fmt.Errorf("source_path contains control characters")
	}
	if !filepath.IsAbs(rawPath) {
		return src, fmt.Errorf("source_path must be an absolute path on the host running this MCP server")
	}
	ls := r.localSourceConfig()
	if len(ls.dirs) == 0 {
		return src, errors.New(ls.refusal(""))
	}
	real, err := filepath.EvalSymlinks(rawPath)
	if err != nil {
		// Say nothing about files outside the allowed directories, not
		// even whether they exist.
		if why := ls.refusal(filepath.Clean(rawPath)); why != "" {
			return src, errors.New(why)
		}
		return src, fmt.Errorf("source_path cannot be read: %w", localPathError(err))
	}
	if real, err = filepath.Abs(real); err != nil {
		return src, fmt.Errorf("source_path cannot be read: %w", err)
	}
	if why := ls.refusal(real); why != "" {
		return src, errors.New(why)
	}
	st, err := os.Stat(real)
	if err != nil {
		return src, fmt.Errorf("source_path cannot be read: %w", localPathError(err))
	}
	switch {
	case !st.Mode().IsRegular():
		return src, fmt.Errorf("source_path %s is not a regular file", real)
	case st.Size() == 0:
		return src, fmt.Errorf("source_path %s is empty", real)
	case st.Size() > r.uploadCfg.maxBytes:
		return src, fmt.Errorf("source_path %s is %s, over the %s limit", real, formatBytes(st.Size()), formatBytes(r.uploadCfg.maxBytes))
	}
	src.LocalPath, src.Size, src.ModTime = real, st.Size(), st.ModTime()
	src.FileID, src.info = fileID(st), st
	return src, nil
}

// sameLocalFile reports whether st, the stat of the opened source file, is
// the file src was planned with: same file (device and inode), size and
// modification time.
func (s uploadSource) sameLocalFile(st os.FileInfo) bool {
	return os.SameFile(s.info, st) && fileID(st) == s.FileID &&
		st.Mode().IsRegular() && st.Size() == s.Size && st.ModTime().Equal(s.ModTime)
}

// localPathError is err without the os package's operation prefix.
func localPathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Path, pe.Err)
	}
	return err
}

func (s uploadSource) bind() map[string]interface{} {
	if s.URL != nil {
		return map[string]interface{}{"url": s.URL.String(), "sha256": s.SHA256}
	}
	return map[string]interface{}{"path": s.LocalPath, "size": s.Size, "mtime_ns": s.ModTime.UnixNano(), "file_id": s.FileID, "sha256": s.SHA256}
}

// uploadTargetRefusal is why the validated path p is not a target
// upload_file writes, or "".
func uploadTargetRefusal(p string) string {
	rest := strings.TrimPrefix(p, "/mnt/")
	pool, sub, _ := strings.Cut(rest, "/")
	switch {
	case sub == "":
		return fmt.Sprintf("path %s is the root of pool '%s'; give the path of a file in a dataset below it", p, pool)
	case !strings.Contains(sub, "/"):
		return fmt.Sprintf("path %s is directly in the root dataset of pool '%s'; put the file in a dataset below it", p, pool)
	}
	if part, ok := managedDatasetPart(strings.ToLower(rest)); ok {
		return fmt.Sprintf("path %s is inside '%s', which TrueNAS manages itself", p, part)
	}
	return ""
}

func isENOENT(err error) bool {
	var apiErr *truenas.APICallError
	return errors.As(err, &apiErr) && strings.Contains(apiErr.Message, "[ENOENT]")
}

func (r *Registry) planUploadFile(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	return r.planUpload(ctx, client, args, true)
}

// planUpload builds the upload plan. probe asks a URL source for its size;
// the size is shown but not bound, so a plan built without it hashes the
// same.
func (r *Registry) planUpload(ctx context.Context, client *truenas.Client, args map[string]interface{}, probe bool) (*writePlan, error) {
	requested, _ := args["path"].(string)
	p, err := validateACLPath(requested)
	if err != nil {
		return nil, err
	}
	if why := uploadTargetRefusal(p); why != "" {
		return nil, fmt.Errorf("%s; nothing was uploaded", why)
	}
	src, err := r.parseUploadSource(args)
	if err != nil {
		return nil, err
	}
	overwrite, _ := args["overwrite"].(bool)

	var blocked, warnings []string
	if src.URL != nil && probe {
		size, err := r.probeSourceSize(ctx, src.URL)
		switch {
		case errors.Is(err, errSourcePolicy):
			blocked = append(blocked, fmt.Sprintf("source %s cannot be downloaded: %v", src.display(), err))
		case err != nil:
			warnings = append(warnings, fmt.Sprintf("The size of %s could not be read (%s); the free space cannot be checked before the download", src.display(), boundedErr(err)))
		case size < 0:
			warnings = append(warnings, fmt.Sprintf("%s does not report its size; the free space is checked again after the download", src.display()))
		case size == 0:
			blocked = append(blocked, fmt.Sprintf("source %s is empty", src.display()))
		case size > r.uploadCfg.maxBytes:
			blocked = append(blocked, fmt.Sprintf("source %s is %s, over the %s limit", src.display(), formatBytes(size), formatBytes(r.uploadCfg.maxBytes)))
		default:
			src.Size = size
		}
	}

	parent := path.Dir(p)
	target, parentSt, err := resolveACLPath(ctx, client, parent)
	if err != nil {
		return nil, fmt.Errorf("the directory %s cannot be used: %w (it must already exist; no directories are created)", parent, err)
	}
	if target != parent {
		return nil, fmt.Errorf("the directory %s goes through a symlink (it resolves to %s); give the real path", parent, target)
	}
	if typ, _ := parentSt["type"].(string); typ != "DIRECTORY" {
		return nil, fmt.Errorf("%s is a %v, not a directory", parent, parentSt["type"])
	}

	exists := false
	var existing map[string]interface{}
	st, err := statACLPath(ctx, client, p)
	switch {
	case err == nil:
		exists = true
		existing = st
	case isENOENT(err):
	default:
		return nil, err
	}
	var existingSize int64
	if exists {
		typ, _ := st["type"].(string)
		switch {
		case typ == "SYMLINK":
			return nil, fmt.Errorf("path %s is a symlink; give the real path of a regular file", p)
		case typ != "FILE":
			return nil, fmt.Errorf("path %s is a %v, not a regular file", p, st["type"])
		case st["realpath"] != p:
			return nil, fmt.Errorf("path %s resolves to %v; give the real path", p, st["realpath"])
		}
		if n, ok := st["size"].(float64); ok {
			existingSize = int64(n)
		}
		if !overwrite {
			blocked = append(blocked, fmt.Sprintf("%s already exists (%s); set overwrite to true to replace it", p, formatBytes(existingSize)))
		}
	}

	fs, err := readACLPathFS(ctx, client, parent)
	if err != nil {
		return nil, err
	}
	pool, _, _ := strings.Cut(strings.TrimPrefix(p, "/mnt/"), "/")
	dsPool, _, _ := strings.Cut(fs.Source, "/")
	switch {
	case fs.FSType != "zfs":
		blocked = append(blocked, fmt.Sprintf("%s is not on a ZFS dataset (filesystem type %q)", parent, fs.FSType))
	case fs.Source == "" || dsPool != pool:
		blocked = append(blocked, fmt.Sprintf("%s is on %q, not on a dataset of pool '%s'", parent, fs.Source, pool))
	case fs.Source == pool:
		blocked = append(blocked, fmt.Sprintf("%s is on the root dataset of pool '%s'; put the file in a dataset below it", parent, pool))
	}
	if part, ok := managedDatasetPart(strings.ToLower(fs.Source)); ok {
		blocked = append(blocked, fmt.Sprintf("%s is on dataset %s, inside '%s', which TrueNAS manages itself", parent, fs.Source, part))
	}
	for _, f := range fs.Flags {
		if f == "RO" {
			blocked = append(blocked, fmt.Sprintf("dataset %s is mounted read-only", fs.Source))
		}
	}
	dataset, children, err := datasetAndChildren(ctx, client, fs.Source)
	if err != nil {
		return nil, err
	}
	switch {
	case dataset == nil:
		blocked = append(blocked, fmt.Sprintf("dataset %s of %s was not found with pool.dataset.query", fs.Source, parent))
	case dataset["locked"] != false:
		blocked = append(blocked, fmt.Sprintf("dataset %s is locked, or its lock state could not be read", fs.Source))
	}
	for _, c := range children {
		if c.Locked != false && below(parent, c.Mountpoint) {
			blocked = append(blocked, fmt.Sprintf("%s is inside dataset %s, which is locked or whose lock state could not be read", parent, c.Name))
		}
	}

	credit := int64(0)
	if exists && overwrite {
		credit = existingSize
	}
	freeText := "unknown"
	if fs.Avail >= 0 {
		freeText = formatBytes(fs.Avail)
		if src.Size > 0 && src.Size > fs.Avail+credit {
			blocked = append(blocked, fmt.Sprintf("dataset %s has %s available, which is not enough for %s", fs.Source, formatBytes(fs.Avail), formatBytes(src.Size)))
		} else if src.Size > 0 && src.Size > (fs.Avail+credit)*9/10 {
			warnings = append(warnings, fmt.Sprintf("The file (%s) would use most of the %s available on dataset %s", formatBytes(src.Size), formatBytes(fs.Avail), fs.Source))
		}
	} else {
		warnings = append(warnings, fmt.Sprintf("The free space of dataset %s could not be read", fs.Source))
	}

	sizeText := "unknown until downloaded"
	if src.Size > 0 {
		sizeText = fmt.Sprintf("%s (%d bytes)", formatBytes(src.Size), src.Size)
	}
	shaText := src.SHA256
	if shaText == "" {
		shaText = "not given; computed while uploading and reported"
	}
	if src.URL != nil {
		warnings = append(warnings, fmt.Sprintf("The file is downloaded by this MCP server to a temporary file in %s first (it needs that much free space there), checked against the SHA-256, and only then uploaded", tempDirName(r.uploadCfg.tempDir)))
	} else if src.SHA256 != "" {
		warnings = append(warnings, "The local file is read twice: once to check the SHA-256, then while uploading (a change in between fails the upload)")
	}
	if exists && overwrite {
		warnings = append(warnings, fmt.Sprintf("%s (%s) is truncated and rewritten in place; its current contents are lost unless a snapshot of dataset %s holds them, and a failed upload leaves it partly written", p, formatBytes(existingSize), fs.Source))
	} else {
		warnings = append(warnings, fmt.Sprintf("If the upload fails partway, a partly written %s is left behind", p))
	}
	warnings = append(warnings, "The transfer runs in the background; follow it with tasks_get")

	bind := map[string]interface{}{
		"source": src.bind(),
		"parent": map[string]interface{}{
			"realpath": parentSt["realpath"], "type": parentSt["type"], "inode": parentSt["inode"],
			"dev": parentSt["dev"], "mount_id": parentSt["mount_id"],
		},
		"target":     map[string]interface{}{"exists": exists},
		"filesystem": fs.bind(),
		"dataset":    dataset,
	}
	if exists {
		bind["target"] = map[string]interface{}{
			"exists": true, "realpath": existing["realpath"], "type": existing["type"],
			"inode": existing["inode"], "dev": existing["dev"], "mount_id": existing["mount_id"],
			"size": existing["size"], "mtime": existing["mtime"],
			"uid": existing["uid"], "gid": existing["gid"], "mode": existing["mode"],
		}
	}

	options := map[string]interface{}{"append": false}
	current := map[string]interface{}{
		"path": p, "exists": exists, "dataset": fs.Source, "dataset_mountpoint": fs.Dest,
		"available_bytes": fs.Avail,
	}
	if exists {
		current["existing_size"] = existingSize
		current["existing_mtime"] = existing["mtime"]
	}
	sourceDetails := map[string]interface{}{"type": src.kind(), "sha256": shaText}
	if src.URL != nil {
		sourceDetails["url"] = src.display()
	} else {
		sourceDetails["path"] = src.LocalPath
	}
	if src.Size > 0 {
		sourceDetails["size"] = src.Size
	}
	details := map[string]interface{}{
		"source": sourceDetails, "target": p, "dataset": fs.Source,
		"available_bytes": fs.Avail, "overwrite": exists && overwrite,
		"method": filesystemPutMethod, "endpoint": "/_upload", "options": options,
	}
	verb := "Upload"
	if exists && overwrite {
		verb = "Upload and replace"
	}
	headline := fmt.Sprintf("%s %s to %s", verb, src.display(), p)
	overwriteText := "no (new file)"
	if exists {
		overwriteText = fmt.Sprintf("yes: replaces the existing %s file", formatBytes(existingSize))
	}
	return &writePlan{
		Tool:         "upload_file",
		Method:       filesystemPutMethod,
		CallParams:   []interface{}{p, options},
		ParamNames:   []string{"path", "options"},
		JobMethod:    filesystemPutMethod,
		TargetName:   p,
		CurrentState: current,
		Actions: []PlannedAction{{
			Step:        1,
			Description: headline,
			Operation:   "upload",
			Target:      p,
			Details:     details,
		}},
		Warnings: warnings,
		Blocked:  blocked,
		Bind:     bind,
		ConfirmSummary: newConfirmSummary(headline,
			confirmFact("source", src.display()),
			confirmFact("size", sizeText),
			confirmFact("sha256", shaText),
			confirmFact("target", p),
			confirmFact("dataset", fs.Source),
			confirmFact("free space", freeText),
			confirmFact("overwrite", overwriteText),
		),
		Result: map[string]interface{}{
			"source": src, "dataset": fs.Source, "available": fs.Avail, "credit": credit,
		},
	}, nil
}

func (r *Registry) probeSourceSize(ctx context.Context, u *url.URL) (int64, error) {
	sc, err := r.newSourceClient(ctx, u)
	if err != nil {
		return -1, err
	}
	defer sc.close()
	return sc.probeSize(ctx, u)
}

func (r *Registry) handleUploadFile(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := r.planUploadFile(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeUploadFile(ctx, client, args, plan)
}

// acquireUpload reserves an upload slot for target. The returned func
// releases it.
func (r *Registry) acquireUpload(target string) (func(), error) {
	r.uploadMu.Lock()
	defer r.uploadMu.Unlock()
	if r.uploads[target] {
		return nil, fmt.Errorf("an upload to %s is already running; wait for it to finish (tasks_get)", target)
	}
	if len(r.uploads) >= maxConcurrentUploads {
		return nil, fmt.Errorf("%d uploads are already running; wait for one to finish (tasks_get)", len(r.uploads))
	}
	r.uploads[target] = true
	return func() {
		r.uploadMu.Lock()
		defer r.uploadMu.Unlock()
		delete(r.uploads, target)
	}, nil
}

func (r *Registry) executeUploadFile(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	src, ok := plan.Result["source"].(uploadSource)
	if !ok {
		return "", fmt.Errorf("upload_file plan has no source; nothing was uploaded")
	}
	release, err := r.acquireUpload(plan.TargetName)
	if err != nil {
		return "", fmt.Errorf("%w; nothing was uploaded", err)
	}
	response := map[string]interface{}{"path": plan.TargetName, "source": src.display()}

	if r.taskManager == nil {
		defer release()
		res, err := r.runUpload(ctx, client, args, plan, func(string) {})
		if err != nil {
			return "", err
		}
		response["job_id"] = res.jobID
		response["size"] = res.size
		response["sha256"] = res.sha256
		response["message"] = fmt.Sprintf("Uploaded %s to %s (job %d); follow the job with query_jobs", formatBytes(res.size), plan.TargetName, res.jobID)
		return marshalJSON(response)
	}

	task, err := r.taskManager.CreateLocalTask("upload_file", args, uploadTaskTTL, "Starting the transfer", tasks.WithExpectedMethod(filesystemPutMethod))
	if err != nil {
		release()
		return "", fmt.Errorf("the upload could not be tracked as a task (%s); nothing was uploaded", boundedErr(err))
	}
	go r.runUploadTask(client, args, plan, task.TaskID, release)

	steps := "upload"
	if src.URL != nil {
		steps = "download, SHA-256 check, then upload"
	} else if src.SHA256 != "" {
		steps = "SHA-256 check, then upload"
	}
	response["status"] = "started"
	response["task_id"] = task.TaskID
	response["task_status"] = task.Status
	response["poll_interval"] = task.PollInterval
	response["message"] = fmt.Sprintf("The upload of %s to %s started in the background (%s). Track it with tasks_get task_id %s; the task completes when TrueNAS has written the file", src.display(), plan.TargetName, steps, task.TaskID)
	return marshalJSON(response)
}

// runUploadTask runs the transfer of a confirmed plan outside the tool
// call and reports it on the task: progress while it runs, then the job
// once TrueNAS accepted the file, or why it failed.
func (r *Registry) runUploadTask(client *truenas.Client, args map[string]interface{}, plan *writePlan, taskID string, release func()) {
	defer release()
	mgr := r.taskManager
	defer func() {
		if p := recover(); p != nil {
			log.Printf("upload_file transfer panicked: %s\n%s", safego.Describe(p), debug.Stack())
			_ = mgr.FailLocal(taskID, "Internal error during the transfer; check the target file before using it")
		}
	}()
	limit := r.uploadCfg.maxDuration
	ctx, cancel := context.WithTimeoutCause(context.Background(), limit, errUploadTooLong)
	defer cancel()
	res, err := r.runUpload(ctx, client, args, plan, func(msg string) {
		_ = mgr.SetLocalMessage(taskID, msg)
	})
	if err != nil {
		msg := err.Error()
		if errors.Is(context.Cause(ctx), errUploadTooLong) {
			msg = fmt.Sprintf("The transfer was stopped because it ran over the %s limit: %s", limit, msg)
		}
		_ = mgr.FailLocal(taskID, msg)
		return
	}
	target := plan.TargetName
	outcome := func(map[string]interface{}) (string, interface{}) {
		return fmt.Sprintf("Uploaded %s to %s (SHA-256 %s)", formatBytes(res.size), target, res.sha256),
			map[string]interface{}{"path": target, "size": res.size, "sha256": res.sha256}
	}
	msg := fmt.Sprintf("All %s sent; waiting for job %d (filesystem.put) to finish writing %s", formatBytes(res.size), res.jobID, target)
	if err := mgr.AttachJob(ctx, taskID, res.jobID, msg, tasks.WithJobOutcome(outcome)); err != nil {
		log.Printf("upload_file: task %s could not track job %d: %s", taskID, res.jobID, boundedErr(err))
	}
}

type uploadResult struct {
	jobID  int
	size   int64
	sha256 string
}

// runUpload prepares the source (downloading and checking a URL source,
// checking a local one), plans again and refuses if anything bound by the
// confirmation changed, then uploads.
func (r *Registry) runUpload(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan, progress func(string)) (uploadResult, error) {
	var res uploadResult
	src, _ := plan.Result["source"].(uploadSource)
	target := plan.TargetName

	var file *os.File
	switch {
	case src.URL != nil:
		sc, err := r.newSourceClient(ctx, src.URL)
		if err != nil {
			return res, fmt.Errorf("download of %s failed: %s; nothing was uploaded", src.display(), boundedErr(err))
		}
		progress(fmt.Sprintf("Downloading %s", src.display()))
		d, err := sc.download(ctx, src.URL, src.SHA256, func(got, total int64) {
			progress(fmt.Sprintf("Downloading %s: %s", src.display(), progressText(got, total)))
		})
		sc.close()
		if err != nil {
			return res, fmt.Errorf("%s: %s; nothing was uploaded", src.display(), boundedErr(err))
		}
		defer removeTemp(d.path)
		if file, err = os.Open(d.path); err != nil {
			return res, fmt.Errorf("could not reopen the downloaded file: %s; nothing was uploaded", boundedErr(err))
		}
		res.size, res.sha256 = d.size, d.sha256
	default:
		f, err := os.Open(src.LocalPath)
		if err != nil {
			return res, fmt.Errorf("source_path cannot be read: %s; nothing was uploaded", boundedErr(localPathError(err)))
		}
		file = f
		// The file is opened once: this descriptor is hashed and then
		// uploaded, and it must be the very file the preview showed.
		st, err := f.Stat()
		if err != nil || !src.sameLocalFile(st) {
			f.Close()
			return res, fmt.Errorf("%s changed since the confirmation (a different file, size or modification time); nothing was uploaded", src.LocalPath)
		}
		res.size = st.Size()
		if src.SHA256 != "" {
			progress(fmt.Sprintf("Checking the SHA-256 of %s (%s)", src.LocalPath, formatBytes(res.size)))
			got, n, err := hashFile(ctx, f)
			if err != nil {
				f.Close()
				return res, fmt.Errorf("could not read %s: %s; nothing was uploaded", src.LocalPath, boundedErr(err))
			}
			if n != res.size {
				f.Close()
				return res, fmt.Errorf("%s changed while it was read (%d of %d bytes); nothing was uploaded", src.LocalPath, n, res.size)
			}
			if got != src.SHA256 {
				f.Close()
				return res, fmt.Errorf("SHA-256 mismatch: %s has SHA-256 %s, not the expected %s; nothing was uploaded", src.LocalPath, got, src.SHA256)
			}
			res.sha256 = got
		}
	}
	defer file.Close()

	progress(fmt.Sprintf("Checking %s again before the upload", target))
	again, err := r.planUpload(ctx, client, args, false)
	if err != nil {
		return res, fmt.Errorf("%s can no longer be used: %s; nothing was uploaded", target, boundedErr(err))
	}
	if len(again.Blocked) > 0 {
		return res, fmt.Errorf("%s; nothing was uploaded", strings.Join(again.Blocked, "; "))
	}
	h1, err1 := plan.planHash()
	h2, err2 := again.planHash()
	if err1 != nil || err2 != nil || h1 != h2 {
		return res, fmt.Errorf("%s, its directory or its dataset changed since the confirmation; nothing was uploaded. Preview the upload again", target)
	}
	avail, _ := again.Result["available"].(int64)
	credit, _ := again.Result["credit"].(int64)
	if avail >= 0 && res.size > avail+credit {
		return res, fmt.Errorf("dataset %v has %s available, which is not enough for %s; nothing was uploaded", again.Result["dataset"], formatBytes(avail), formatBytes(res.size))
	}

	progress(fmt.Sprintf("Uploading to %s: %s", target, progressText(0, res.size)))
	hr := &hashingReader{r: file, h: sha256.New()}
	var reported time.Time
	var sent atomic.Int64
	jobID, err := client.UploadFile(ctx, filesystemPutMethod, plan.CallParams, path.Base(target), hr, res.size, truenas.UploadOptions{
		StallTimeout: r.uploadCfg.stallTimeout,
		Progress: func(n int64) {
			sent.Store(n)
			if time.Since(reported) >= progressReportPeriod {
				reported = time.Now()
				progress(fmt.Sprintf("Uploading to %s: %s", target, progressText(n, res.size)))
			}
		},
	})
	if err != nil {
		var se *truenas.UploadStatusError
		if errors.As(err, &se) && se.NothingWritten() {
			return res, fmt.Errorf("%s; nothing was written to %s", boundedErr(err), target)
		}
		return res, fmt.Errorf("%s after %s of %s were sent; %s may be partly written, so check it before using it or retrying", boundedErr(err), formatBytes(sent.Load()), formatBytes(res.size), target)
	}
	res.jobID = jobID
	got := hex.EncodeToString(hr.h.Sum(nil))
	if res.sha256 != "" && got != res.sha256 {
		return res, fmt.Errorf("the data sent to %s (job %d) has SHA-256 %s, not the verified %s: the source changed during the upload. Do not use %s", target, jobID, got, res.sha256, target)
	}
	res.sha256 = got
	return res, nil
}

func progressText(got, total int64) string {
	if total > 0 {
		return fmt.Sprintf("%s of %s (%.0f%%)", formatBytes(got), formatBytes(total), float64(got)/float64(total)*100)
	}
	return formatBytes(got)
}
