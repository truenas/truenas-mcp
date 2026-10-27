package proxy

import (
	"encoding/json"
	"reflect"
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

func TestRedactResponseStructuredContent(t *testing.T) {
	structured := map[string]interface{}{"content": []interface{}{map[string]interface{}{"type": "text", "text": `{"ok": true}`}},
		"structuredContent": map[string]interface{}{"user": "bob", "password": "fake-user-pw",
			"env": []interface{}{"DB_PASS=fake-db-pw"}, "count": json.Number("3")},
		"isError": false}
	raw := json.RawMessage(`{"structuredContent": {"api_key": "fake-api-key-0000", "id": 7}, "content": []}`)
	for _, result := range []interface{}{structured, raw} {
		out := marshal(t, redactResponse(&mcp.Response{JSONRPC: "2.0", ID: 1, Result: result}).Result)
		if strings.Contains(out, "fake-") || !strings.Contains(out, redact.Marker) {
			t.Errorf("structuredContent not redacted: %s", out)
		}
	}
	out := marshal(t, redactResponse(&mcp.Response{JSONRPC: "2.0", ID: 1, Result: structured}).Result)
	for _, want := range []string{`"user":"bob"`, `"count":3`, `"isError":false`, `"text":"{\"ok\": true}"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	if structured["structuredContent"].(map[string]interface{})["password"] != "fake-user-pw" {
		t.Error("input modified")
	}
	// A raw result that is not a tool call result is left alone.
	list := json.RawMessage(`{"tools":[{"name":"x","inputSchema":{"properties":{"password":{"type":"string"}}}}]}`)
	if got := redactResponse(&mcp.Response{JSONRPC: "2.0", ID: 2, Result: list}).Result; !reflect.DeepEqual(got, list) {
		t.Errorf("tools/list raw result changed: %v", got)
	}
}
