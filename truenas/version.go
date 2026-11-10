package truenas

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MinSupported is the oldest TrueNAS release this server supports.
var MinSupported = Version{Major: 25, Minor: 10, Patch: 0, Raw: "25.10"}

// Version is a TrueNAS release number, year.month.patch (for example
// 25.10.8 or 26.0.0). Suffixes such as -RC.1 are ignored.
type Version struct {
	Major, Minor, Patch int
	// Raw is the original system.info version string, when parsed from one.
	Raw string
}

// IsZero reports whether v is the zero value (not a parsed version).
func (v Version) IsZero() bool {
	return v.Major == 0 && v.Minor == 0 && v.Patch == 0 && v.Raw == ""
}

// String returns Raw when set, otherwise the numeric form.
func (v Version) String() string {
	if v.Raw != "" {
		return v.Raw
	}
	if v.Patch != 0 {
		return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	}
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

// Less reports whether v is strictly older than o.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// AtLeast reports whether v is o or newer.
func (v Version) AtLeast(o Version) bool {
	return !v.Less(o)
}

// versionNumberRe finds the first dotted version number in a string.
var versionNumberRe = regexp.MustCompile(`(?i)v?(\d+)\.(\d+)(?:\.(\d+))?`)

// ParseVersion reads a TrueNAS version string such as "25.10.8",
// "26.0.0-RC.1", or "TrueNAS-SCALE-25.10.0".
func ParseVersion(s string) (Version, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Version{}, fmt.Errorf("empty version")
	}
	m := versionNumberRe.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("not a TrueNAS version")
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch := 0
	if m[3] != "" {
		patch, _ = strconv.Atoi(m[3])
	}
	return Version{Major: major, Minor: minor, Patch: patch, Raw: s}, nil
}

// MustParseVersion parses s, or panics. For package-level tables only.
func MustParseVersion(s string) Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic("MustParseVersion: " + s + ": " + err.Error())
	}
	return v
}

// UnsupportedVersionError is returned when the connected TrueNAS release
// is older than MinSupported, or its version could not be read.
type UnsupportedVersionError struct {
	Reported string
	Parsed   Version
}

func (e *UnsupportedVersionError) Error() string {
	if e == nil {
		return "TrueNAS version is not supported"
	}
	if e.Reported == "" {
		return fmt.Sprintf("TrueNAS version could not be read from system.info; TrueNAS %s or later is required", MinSupported)
	}
	if e.Parsed.IsZero() {
		return fmt.Sprintf("TrueNAS %s is not supported: the version could not be parsed; TrueNAS %s or later is required", e.Reported, MinSupported)
	}
	return fmt.Sprintf("TrueNAS %s is not supported; TrueNAS %s or later is required", e.Reported, MinSupported)
}

func parseSystemInfoVersion(raw json.RawMessage) (Version, error) {
	var info map[string]interface{}
	if err := json.Unmarshal(raw, &info); err != nil {
		return Version{}, &UnsupportedVersionError{}
	}
	s, ok := info["version"].(string)
	if !ok || strings.TrimSpace(s) == "" {
		return Version{}, &UnsupportedVersionError{}
	}
	v, err := ParseVersion(s)
	if err != nil {
		return Version{}, &UnsupportedVersionError{Reported: s}
	}
	return v, nil
}

// ServerVersion returns the TrueNAS version for the current session,
// reading system.info when it has not been read since the last login
// (including after a reconnect).
func (c *Client) ServerVersion() (Version, error) {
	gen, err := c.SessionGeneration()
	if err != nil {
		return Version{}, err
	}
	if v, err, ok := c.cachedVersion(gen); ok {
		return v, err
	}
	c.versionFetchMu.Lock()
	defer c.versionFetchMu.Unlock()
	if v, err, ok := c.cachedVersion(gen); ok {
		return v, err
	}
	raw, err := c.Call("system.info")
	if err != nil {
		return Version{}, fmt.Errorf("failed to read TrueNAS version from system.info: %w", err)
	}
	v, verr := parseSystemInfoVersion(raw)
	c.storeVersion(gen, v, verr)
	if verr != nil {
		return Version{}, verr
	}
	return v, nil
}

// SessionVersion returns the cached TrueNAS version, refreshing it if the
// session has changed. ok is false when no version has ever been loaded,
// so callers that only gate after startup do not contact TrueNAS.
func (c *Client) SessionVersion() (v Version, err error, ok bool) {
	c.versionMu.Lock()
	loaded := c.versionLoaded
	c.versionMu.Unlock()
	if !loaded {
		return Version{}, nil, false
	}
	v, err = c.ServerVersion()
	return v, err, true
}

// CachedServerVersion returns the last loaded version without calling
// TrueNAS. ok is false when no version has been read yet.
func (c *Client) CachedServerVersion() (v Version, err error, ok bool) {
	c.versionMu.Lock()
	defer c.versionMu.Unlock()
	if !c.versionLoaded {
		return Version{}, nil, false
	}
	return c.nasVersion, c.versionErr, true
}

func (c *Client) cachedVersion(gen uint64) (Version, error, bool) {
	c.versionMu.Lock()
	defer c.versionMu.Unlock()
	if !c.versionLoaded || c.versionGen != gen {
		return Version{}, nil, false
	}
	return c.nasVersion, c.versionErr, true
}

func (c *Client) storeVersion(gen uint64, v Version, err error) {
	c.versionMu.Lock()
	defer c.versionMu.Unlock()
	// Keep a result already stored for this generation: concurrent
	// refreshers may finish out of order.
	if c.versionLoaded && c.versionGen == gen {
		return
	}
	c.versionLoaded = true
	c.versionGen = gen
	c.nasVersion = v
	c.versionErr = err
}

// RequireSupportedVersion reads the connected TrueNAS version and returns
// an error if it is older than MinSupported or cannot be determined.
func (c *Client) RequireSupportedVersion() error {
	v, err := c.ServerVersion()
	if err != nil {
		return err
	}
	if v.Less(MinSupported) {
		return &UnsupportedVersionError{Reported: v.Raw, Parsed: v}
	}
	return nil
}
