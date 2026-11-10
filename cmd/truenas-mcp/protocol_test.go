package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/truenas/truenas-mcp/tools"
)

// An unknown tool, params that are not an object, and arguments that are
// not an object are JSON-RPC -32602 errors that keep the request's id.
func TestToolsCallInvalidParams(t *testing.T) {
	c, run := session(t, newTestHandler(fakeRegistry{}, false))
	c.initialize()
	for _, tc := range []struct {
		name   string
		id     interface{}
		params interface{}
	}{
		{"unknown tool", float64(1), toolArgs("nope", nil)},
		{"array params", "a", []interface{}{"echo"}},
		{"string arguments", float64(0), map[string]interface{}{"name": "echo", "arguments": "x"}},
		{"array arguments", float64(4), map[string]interface{}{"name": "echo", "arguments": []interface{}{1}}},
	} {
		if m := c.call(tc.id, "tools/call", tc.params); errorCode(m) != -32602 {
			t.Errorf("%s: %v; want -32602", tc.name, m)
		}
	}
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// initialize answers the version the client asks for when the server
// supports it, and its newest handshake version otherwise. The capabilities
// are tools only.
func TestInitializeNegotiatesProtocolVersion(t *testing.T) {
	for _, tc := range []struct{ requested, want string }{
		{"2024-11-05", "2024-11-05"},
		{"2025-03-26", "2025-03-26"},
		{"2025-06-18", "2025-06-18"},
		{"2025-11-25", "2025-11-25"},
		{"2099-01-01", "2025-11-25"},
	} {
		c, run := session(t, newTestHandler(fakeRegistry{}, false))
		m := c.call(float64(1), "initialize", initParams(tc.requested))
		res, _ := m["result"].(map[string]interface{})
		if res["protocolVersion"] != tc.want {
			t.Errorf("requested %s: negotiated %v, want %s", tc.requested, res["protocolVersion"], tc.want)
		}
		caps, _ := json.Marshal(res["capabilities"])
		if string(caps) != `{"tools":{}}` {
			t.Errorf("requested %s: capabilities %s, want {\"tools\":{}}", tc.requested, caps)
		}
		if err := run.wait(); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
}

// Before initialize, only ping is served.
func TestRequestsBeforeInitializeAreRejected(t *testing.T) {
	reg := &callCountingRegistry{}
	c, run := session(t, newTestHandler(reg, false))
	c.call(float64(1), "ping", nil)
	if m := c.call(float64(2), "tools/list", nil); m["error"] == nil {
		t.Errorf("tools/list before initialize: %v; want an error", m)
	}
	if m := c.call(float64(3), "tools/call", toolArgs("echo", nil)); m["error"] == nil {
		t.Errorf("tools/call before initialize: %v; want an error", m)
	}
	if reg.calls.Load() != 0 {
		t.Error("a tool ran before initialize")
	}
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// A tool the safety policy hides is still refused with the policy's reason
// as a tool error, as before; only names the registry does not know are
// -32602.
func TestHiddenToolKeepsPolicyError(t *testing.T) {
	reg := tools.NewRegistry(nil, nil)
	if err := reg.SetPolicy(tools.Policy{ReadOnly: true, Deny: []string{"query_pools"}}); err != nil {
		t.Fatal(err)
	}
	c, run := session(t, newTestHandler(reg, false))
	c.initialize()
	for name, want := range map[string]string{
		"system_reboot": "read-only mode",
		"query_pools":   "disabled by --deny-tools",
	} {
		m := c.call(float64(1), "tools/call", toolArgs(name, nil))
		if text, isError := toolText(t, m); !isError || !strings.Contains(text, want) {
			t.Errorf("%s: %q (isError %v); want a tool error containing %q", name, text, isError, want)
		}
	}
	if m := c.call(float64(2), "tools/call", toolArgs("nope", nil)); errorCode(m) != -32602 {
		t.Errorf("unknown tool: %v; want -32602", m)
	}
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// A non-boolean dry_run or confirm is refused by the registry as a tool
// error through the SDK dispatch, and nothing runs (the registry has no
// client, so a handler or preview that ran would fail differently).
func TestNonBooleanSafetyArgumentsOverStdio(t *testing.T) {
	c, run := session(t, newTestHandler(tools.NewRegistry(nil, nil), false))
	c.initialize()
	for _, key := range []string{"confirm", "dry_run"} {
		m := c.call(float64(1), "tools/call", toolArgs("delete_app", map[string]interface{}{"app_name": "plex", key: "true"}))
		text, isError := toolText(t, m)
		if !isError || !strings.Contains(text, key+" must be a JSON boolean") || !strings.Contains(text, "nothing was executed") {
			t.Errorf("%s=\"true\": %q (isError %v); want the invalid-arguments error", key, text, isError)
		}
	}
	if err := run.wait(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// tools/list on the wire is exactly the registry's tool list, in its order:
// the same names, descriptions, schemas and annotations.
func TestToolsListMatchesRegistry(t *testing.T) {
	for name, policy := range map[string]tools.Policy{
		"default":   {},
		"read-only": {ReadOnly: true},
		"allow":     {Allow: []string{"query_pools", "system_info"}},
	} {
		t.Run(name, func(t *testing.T) {
			reg := tools.NewRegistry(nil, nil)
			if err := reg.SetPolicy(policy); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(reg.ListTools())
			if err != nil {
				t.Fatal(err)
			}
			var want []interface{}
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}

			c, run := session(t, newTestHandler(reg, false))
			c.initialize()
			res, _ := c.call(float64(1), "tools/list", nil)["result"].(map[string]interface{})
			if err := run.wait(); err != nil {
				t.Fatalf("Run: %v", err)
			}
			got, _ := res["tools"].([]interface{})
			if len(got) == 0 || len(got) != len(want) {
				t.Fatalf("tools/list has %d tools, registry %d", len(got), len(want))
			}
			if _, ok := res["nextCursor"]; ok {
				t.Error("tools/list is paginated; all tools should fit in one page")
			}
			for i := range want {
				if !reflect.DeepEqual(got[i], want[i]) {
					g, _ := json.Marshal(got[i])
					w, _ := json.Marshal(want[i])
					t.Errorf("tool %d differs:\n got %s\nwant %s", i, g, w)
				}
			}
		})
	}
}

// The SDK's own client, which negotiates the newest protocol (2026-07-28,
// without the initialize handshake), can list and call tools over the stdio
// transport.
func TestSDKClientOverStdio(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := newStdioHandler(fakeRegistry{}, false, inR, outW)
	done := make(chan error, 1)
	go func() { done <- h.Run(context.Background()); outW.Close() }()

	ctx := t.Context()
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1.0"}, nil)
	cs, err := client.Connect(ctx, &sdk.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if v := cs.InitializeResult().ProtocolVersion; v != sdk.SupportedProtocolVersions()[0] {
		t.Errorf("negotiated %s, want the newest version %s", v, sdk.SupportedProtocolVersions()[0])
	}
	list, err := cs.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 1 || list.Tools[0].Name != "echo" {
		t.Fatalf("ListTools = %v, %v", list, err)
	}
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "echo", Arguments: map[string]interface{}{"data": "abc"}})
	if err != nil || res.IsError || len(res.Content) != 1 {
		t.Fatalf("CallTool = %+v, %v", res, err)
	}
	if text, _ := res.Content[0].(*sdk.TextContent); text == nil || !strings.HasPrefix(text.Text, "got 3 bytes;") {
		t.Errorf("CallTool content = %#v", res.Content[0])
	}
	_, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "nope"})
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc.CodeInvalidParams {
		t.Errorf("unknown tool: %v; want -32602", err)
	}
	if err := cs.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}
