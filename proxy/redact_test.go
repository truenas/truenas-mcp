package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
)

func marshal(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRedactResponseToolCall(t *testing.T) {
	orig := mcp.ToolCallResult{Content: []mcp.ContentBlock{{Type: "text", Text: `{"bindpw": "fake-bind-pw", "binddn": "cn=admin"}`}}}
	for _, result := range []interface{}{
		orig,
		&orig,
		map[string]interface{}{"content": []interface{}{map[string]interface{}{"type": "text", "text": `DB_PASS=fake-db-pw`}}},
	} {
		out := marshal(t, redactResponse(&mcp.Response{JSONRPC: "2.0", ID: 1, Result: result}))
		if strings.Contains(out, "fake-") || !strings.Contains(out, redact.Marker) {
			t.Errorf("not redacted: %s", out)
		}
	}
	if orig.Content[0].Text != `{"bindpw": "fake-bind-pw", "binddn": "cn=admin"}` {
		t.Error("input modified")
	}
}

func TestRedactResponseErrorAndOtherResults(t *testing.T) {
	out := marshal(t, redactResponse(&mcp.Response{JSONRPC: "2.0", ID: 1, Error: &mcp.Error{
		Code: -32000, Message: "failed {'bindpw': 'fake-bind-pw'}", Data: map[string]interface{}{"password": "fake-user-pw"}}}))
	if strings.Contains(out, "fake-") {
		t.Errorf("error not redacted: %s", out)
	}
	schema := map[string]interface{}{"tools": []interface{}{map[string]interface{}{"name": "x",
		"inputSchema": map[string]interface{}{"properties": map[string]interface{}{"password": map[string]interface{}{"type": "string"}}}}}}
	before := marshal(t, schema)
	if after := marshal(t, redactResponse(&mcp.Response{JSONRPC: "2.0", ID: 2, Result: schema}).Result); after != before {
		t.Errorf("tools/list result changed: %s", after)
	}
	if redactResponse(nil) != nil {
		t.Error("nil response")
	}
}
