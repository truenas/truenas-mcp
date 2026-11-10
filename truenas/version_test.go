package truenas

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in      string
		major   int
		minor   int
		patch   int
		wantErr bool
	}{
		{"25.10.0", 25, 10, 0, false},
		{"25.10.8", 25, 10, 8, false},
		{"25.10", 25, 10, 0, false},
		{"25.04.2", 25, 4, 2, false},
		{"26.0.0-RC.1", 26, 0, 0, false},
		{"27.0.0", 27, 0, 0, false},
		{"TrueNAS-SCALE-25.10.0", 25, 10, 0, false},
		{"v25.10.1", 25, 10, 1, false},
		{"", 0, 0, 0, true},
		{"Master", 0, 0, 0, true},
		{"not-a-version", 0, 0, 0, true},
	}
	for _, tt := range tests {
		v, err := ParseVersion(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseVersion(%q) = %v, want error", tt.in, v)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVersion(%q): %v", tt.in, err)
			continue
		}
		if v.Major != tt.major || v.Minor != tt.minor || v.Patch != tt.patch {
			t.Errorf("ParseVersion(%q) = %d.%d.%d, want %d.%d.%d", tt.in, v.Major, v.Minor, v.Patch, tt.major, tt.minor, tt.patch)
		}
		if v.Raw != tt.in {
			t.Errorf("ParseVersion(%q).Raw = %q", tt.in, v.Raw)
		}
	}
}

func TestVersionOrdering(t *testing.T) {
	older := MustParseVersion("25.04.2")
	min := MinSupported
	later := MustParseVersion("25.10.8")
	next := MustParseVersion("26.0.0-RC.1")
	if !older.Less(min) || min.AtLeast(later) || !later.Less(next) || !next.AtLeast(min) {
		t.Fatalf("ordering failed: %s %s %s %s", older, min, later, next)
	}
	if min.Less(min) || !min.AtLeast(min) {
		t.Fatal("a version should be at least itself")
	}
}

func TestRequireSupportedVersionRejects2504(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) {
		f.infoVersion = "25.04.2"
	})
	c := insecureClient(t, f, Options{})
	if err := c.Authenticate(); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	err := c.RequireSupportedVersion()
	var u *UnsupportedVersionError
	if err == nil || !errors.As(err, &u) {
		t.Fatalf("got %v, want UnsupportedVersionError", err)
	}
	if !strings.Contains(err.Error(), "25.04.2") || !strings.Contains(err.Error(), "25.10") {
		t.Errorf("error %q does not name 25.04.2 and the 25.10 minimum", err)
	}
}

func TestRequireSupportedVersionAccepts2510(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) {
		f.infoVersion = "25.10.1"
	})
	c := insecureClient(t, f, Options{})
	if err := c.Authenticate(); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if err := c.RequireSupportedVersion(); err != nil {
		t.Fatalf("25.10.1 should be accepted: %v", err)
	}
}

func TestServerVersionRereadsAfterReconnect(t *testing.T) {
	f := newFakeTrueNAS(t, func(f *fakeTrueNAS) {
		f.infoVersion = "25.10.0"
	})
	c := insecureClient(t, f, Options{})
	if err := c.RequireSupportedVersion(); err != nil {
		t.Fatalf("first read: %v", err)
	}
	f.mu.Lock()
	f.infoVersion = "25.04.2"
	f.mu.Unlock()
	f.closeSocket(1)
	waitDisconnected(t, c)

	err := c.RequireSupportedVersion()
	if err == nil || !strings.Contains(err.Error(), "25.04.2") {
		t.Fatalf("after reconnect got %v, want a 25.04.2 refusal", err)
	}
}

func TestParseSystemInfoVersionRejectsMissingOrNonString(t *testing.T) {
	cases := []json.RawMessage{
		[]byte(`{}`),
		[]byte(`{"version": null}`),
		[]byte(`{"version": 42}`),
		[]byte(`{"version": ""}`),
		[]byte(`{"version": "Master"}`),
		[]byte(`"garbage"`),
	}
	for _, raw := range cases {
		if _, err := parseSystemInfoVersion(raw); err == nil {
			t.Errorf("parseSystemInfoVersion(%s) succeeded, want error", raw)
		}
	}
}
