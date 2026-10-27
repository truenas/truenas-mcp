package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/tools"
	"github.com/truenas/truenas-mcp/truenas"
)

const fakeExportedKey = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

// revealingRegistry returns a fixed result for every call and reports one
// tool name as revealing a secret.
type revealingRegistry struct {
	secretRegistry
	reveals string
}

func (r revealingRegistry) RevealsSecret(name string) bool { return name == r.reveals }

func exportKeyResult() string {
	b, _ := json.MarshalIndent(map[string]interface{}{
		"status":               "exported",
		"dataset":              "tank/secure",
		"key_format":           "hex",
		tools.ExportedKeyField: fakeExportedKey,
		"warning":              "store it offline; password=hunter2hunter2",
	}, "", "  ")
	return string(b)
}

func callNamed(t *testing.T, reg mcp.ToolRegistry, name string) (mcp.ToolCallResult, *mcp.Response) {
	t.Helper()
	h := NewStdioHandler(reg, false)
	resp := h.handleToolsCall(&mcp.Request{JSONRPC: "2.0", ID: 1, Method: "tools/call",
		Params: map[string]interface{}{"name": name}})
	res, ok := resp.Result.(mcp.ToolCallResult)
	if !ok || len(res.Content) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	return res, resp
}

func TestRevealingToolKeepsOnlyItsKeyField(t *testing.T) {
	reg := revealingRegistry{secretRegistry{result: exportKeyResult()}, "export_dataset_key"}
	res, _ := callNamed(t, reg, "export_dataset_key")
	text := res.Content[0].Text
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, text)
	}
	if out[tools.ExportedKeyField] != fakeExportedKey {
		t.Errorf("key was altered: %v", out[tools.ExportedKeyField])
	}
	if out["dataset"] != "tank/secure" || out["key_format"] != "hex" || out["status"] != "exported" {
		t.Errorf("other fields were altered: %v", out)
	}
	if strings.Contains(text, "hunter2hunter2") {
		t.Errorf("the rest of the result was not redacted:\n%s", text)
	}
}

// The exemption is by tool, never by what a result contains: the same output
// from any other tool is fully redacted.
func TestOtherToolsStillRedactKeyField(t *testing.T) {
	reg := revealingRegistry{secretRegistry{result: exportKeyResult()}, "export_dataset_key"}
	res, _ := callNamed(t, reg, "query_datasets")
	if strings.Contains(res.Content[0].Text, fakeExportedKey) {
		t.Errorf("key leaked through a tool that does not reveal secrets:\n%s", res.Content[0].Text)
	}
	plain, _ := callNamed(t, secretRegistry{result: exportKeyResult()}, "export_dataset_key")
	if strings.Contains(plain.Content[0].Text, fakeExportedKey) {
		t.Errorf("key leaked through a registry that does not declare the exemption:\n%s", plain.Content[0].Text)
	}
}

// Errors from the revealing tool are redacted like any other error.
func TestRevealingToolErrorsStayRedacted(t *testing.T) {
	reg := revealingRegistry{secretRegistry{err: &keyErr{}}, "export_dataset_key"}
	res, _ := callNamed(t, reg, "export_dataset_key")
	if !res.IsError || strings.Contains(res.Content[0].Text, "hunter2hunter2") {
		t.Errorf("error not redacted:\n%s", res.Content[0].Text)
	}
}

type keyErr struct{}

func (*keyErr) Error() string { return "failed: password=hunter2hunter2" }

// The debug log of a response (see sendResponse) goes through
// RedactJSONForLog, which must hide the key even though the response
// carries it deliberately.
func TestDebugLogOfExportResponseHidesKey(t *testing.T) {
	reg := revealingRegistry{secretRegistry{result: exportKeyResult()}, "export_dataset_key"}
	_, resp := callNamed(t, reg, "export_dataset_key")
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), fakeExportedKey) {
		t.Fatal("test setup: the response should carry the key")
	}
	logged := string(truenas.RedactJSONForLog(data))
	if strings.Contains(logged, fakeExportedKey) {
		t.Errorf("debug log would contain the key:\n%s", logged)
	}
	if !strings.Contains(logged, redact.Marker) {
		t.Errorf("expected a redaction marker in the debug log:\n%s", logged)
	}
}
