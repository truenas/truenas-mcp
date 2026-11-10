package tools

import (
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

func TestEveryToolHasVersionRange(t *testing.T) {
	r := NewRegistry(nil, nil)
	for name := range r.tools {
		toolVersionMu.RLock()
		_, ok := toolVersion[name]
		toolVersionMu.RUnlock()
		if !ok {
			t.Errorf("tool %s has no entry in toolVersion (tools/version.go)", name)
		}
	}
	toolVersionMu.RLock()
	defer toolVersionMu.RUnlock()
	for name := range toolVersion {
		if _, ok := r.tools[name]; !ok {
			t.Errorf("toolVersion lists %s, which is not registered", name)
		}
	}
}

func TestStartupRejectsTrueNAS2504(t *testing.T) {
	client, _ := newFakeClientWith(t, map[string]interface{}{
		"system.info": map[string]interface{}{"hostname": "nas", "version": "25.04.2"},
	}, nil)
	if err := client.Authenticate(); err != nil {
		t.Fatalf("login should succeed: %v", err)
	}
	// main() calls this after Authenticate and exits on error.
	err := client.RequireSupportedVersion()
	if err == nil {
		t.Fatal("startup accepted TrueNAS 25.04.2")
	}
	if !strings.Contains(err.Error(), "25.04.2") || !strings.Contains(err.Error(), "25.10") {
		t.Fatalf("error %q does not name 25.04.2 and the 25.10 minimum", err)
	}
}

func TestStartupAcceptsTrueNAS2510(t *testing.T) {
	client, _ := newFakeClientWith(t, map[string]interface{}{
		"system.info": map[string]interface{}{"hostname": "nas", "version": "25.10.0"},
	}, nil)
	if err := client.RequireSupportedVersion(); err != nil {
		t.Fatalf("25.10.0 should be accepted: %v", err)
	}
}

// withGatedTool registers a test-only tool whose minimum version is 26.0
// and whose handler calls future.method if it is ever run.
func withGatedTool(t *testing.T, r *Registry, client *truenas.Client) {
	t.Helper()
	r.tools["future_tool"] = Tool{
		Definition: mcp.Tool{
			Name:        "future_tool",
			Description: "Test-only tool gated to TrueNAS 26.0+",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: func(c *truenas.Client, _ map[string]interface{}) (string, error) {
			if _, err := c.Call("future.method"); err != nil {
				return "", err
			}
			return `{"ran":true}`, nil
		},
	}
	prev, ok := setToolVersionForTest("future_tool", VersionRange{Min: truenas.MustParseVersion("26.0")})
	t.Cleanup(func() { restoreToolVersionForTest("future_tool", prev, ok) })
	// Classify it as read so CallTool does not hold it for confirm. The
	// safety table is fail-closed for missing names.
	prevSafety, hadSafety := toolSafety["future_tool"]
	toolSafety["future_tool"] = Safety{Access: Read}
	t.Cleanup(func() {
		if hadSafety {
			toolSafety["future_tool"] = prevSafety
		} else {
			delete(toolSafety, "future_tool")
		}
	})
}

func TestVersionGateHidesNewerToolsOn2510(t *testing.T) {
	client, _ := newFakeClientWith(t, map[string]interface{}{
		"system.info": map[string]interface{}{"hostname": "nas", "version": "25.10.1"},
	}, nil)
	if err := client.RequireSupportedVersion(); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(client, nil)
	withGatedTool(t, r, client)

	names := listed(r)
	if names["future_tool"] {
		t.Error("future_tool should be hidden on TrueNAS 25.10.1")
	}
	if !names["system_info"] {
		t.Error("system_info should stay listed on TrueNAS 25.10.1")
	}
}

func TestVersionGateRefusesNewerToolOn2510WithoutMiddlewareCall(t *testing.T) {
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"system.info": map[string]interface{}{"hostname": "nas", "version": "25.10.1"},
	}, nil)
	if err := client.RequireSupportedVersion(); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(client, nil)
	withGatedTool(t, r, client)

	before := append([]fakeCall(nil), fake.called("future.method")...)
	infoBefore := len(fake.called("system.info"))
	_, err := r.CallTool(t.Context(), "future_tool", map[string]interface{}{})
	if err == nil {
		t.Fatal("future_tool was allowed on TrueNAS 25.10.1")
	}
	if !strings.Contains(err.Error(), "future_tool") || !strings.Contains(err.Error(), "26.0") || !strings.Contains(err.Error(), "nothing was executed") {
		t.Fatalf("error %q does not name the tool, the 26.0 minimum, and that nothing was executed", err)
	}
	if n := len(fake.called("future.method")); n != len(before) {
		t.Errorf("future.method was called %d times after the refusal", n)
	}
	if n := len(fake.called("system.info")); n != infoBefore {
		t.Errorf("system.info was called %d times after the cached version was loaded, want %d", n, infoBefore)
	}
}

func TestVersionGateAllowsToolOnRequiredVersion(t *testing.T) {
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"system.info":   map[string]interface{}{"hostname": "nas", "version": "26.0.0"},
		"future.method": map[string]interface{}{"ok": true},
	}, nil)
	if err := client.RequireSupportedVersion(); err != nil {
		t.Fatal(err)
	}
	r := NewRegistry(client, nil)
	withGatedTool(t, r, client)

	if !listed(r)["future_tool"] {
		t.Fatal("future_tool should be listed on TrueNAS 26.0.0")
	}
	out, err := r.CallTool(t.Context(), "future_tool", map[string]interface{}{})
	if err != nil {
		t.Fatalf("future_tool on 26.0.0: %v", err)
	}
	if !strings.Contains(out, `"ran": true`) && !strings.Contains(out, `"ran":true`) {
		t.Errorf("result %q", out)
	}
	if len(fake.called("future.method")) == 0 {
		t.Error("future.method was not called")
	}
}

func TestVersionGateRefusesAllToolsWhenNASIsTooOld(t *testing.T) {
	client, fake := newFakeClientWith(t, map[string]interface{}{
		"system.info": map[string]interface{}{"hostname": "nas", "version": "25.04.2"},
	}, nil)
	if err := client.Authenticate(); err != nil {
		t.Fatal(err)
	}
	// Load the cached version even though it is below the minimum, so
	// CallTool uses the same message as startup.
	err := client.RequireSupportedVersion()
	if err == nil {
		t.Fatal("expected 25.04.2 to be rejected")
	}
	r := NewRegistry(client, nil)
	infoBefore := len(fake.called("system.info"))
	_, callErr := r.CallTool(t.Context(), "system_info", map[string]interface{}{})
	if callErr == nil {
		t.Fatal("system_info ran on TrueNAS 25.04.2")
	}
	if !strings.Contains(callErr.Error(), "25.04.2") || !strings.Contains(callErr.Error(), "25.10") {
		t.Fatalf("error %q is not the startup version message", callErr)
	}
	if n := len(fake.called("system.info")); n != infoBefore {
		t.Errorf("system.info was called %d times; the refusal should use the cache", n)
	}
}
