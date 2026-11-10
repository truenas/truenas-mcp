package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQueryPoolsParseErrorOmitsRawResponse(t *testing.T) {
	const raw = "RAW_POOL_PAYLOAD_SHOULD_NOT_APPEAR"
	fake := &fakeMiddleware{handle: func(method string, _ json.RawMessage) (interface{}, map[string]interface{}, bool) {
		if method == "pool.query" {
			return raw, nil, true
		}
		return nil, nil, false
	}}
	client, _ := newFakeClientFrom(t, fake)
	_, err := handleQueryPools(client, map[string]interface{}{})
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if !strings.Contains(err.Error(), "failed to parse pools") {
		t.Errorf("error = %v", err)
	}
	if strings.Contains(err.Error(), raw) {
		t.Errorf("parse error embeds the raw response:\n%s", err.Error())
	}
}
