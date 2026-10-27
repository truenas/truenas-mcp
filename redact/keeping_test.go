package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolOutputKeeping(t *testing.T) {
	const key = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	in := `{"dataset":"tank/x","key":"` + key + `","password":"pw-123456","nested":{"token":"tok-123456"}}`
	out := ToolOutputKeeping(in, "key")
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got["key"] != key {
		t.Errorf("key = %v, want it unchanged", got["key"])
	}
	if got["password"] != Marker || got["dataset"] != "tank/x" {
		t.Errorf("other fields: %v", got)
	}
	if strings.Contains(out, "pw-123456") || strings.Contains(out, "tok-123456") {
		t.Errorf("secrets leaked: %s", out)
	}
	if !strings.Contains(ToolOutput(in), Marker) || strings.Contains(ToolOutput(in), key) {
		t.Error("plain ToolOutput must still redact the key field")
	}
}

func TestToolOutputKeepingExemptsNothingByAccident(t *testing.T) {
	const key = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	for name, in := range map[string]string{
		"not an object":          `["` + key + `"]`,
		"no key field":           `{"dataset":"tank/x","encryption_key":"` + key + `"}`,
		"key is not a string":    `{"key":{"value":"` + key + `"}}`,
		"key is nested":          `{"data":{"key":"` + key + `"}}`,
		"plain text":             "key=" + key,
		"another sensitive name": `{"private_key":"` + key + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if out, want := ToolOutputKeeping(in, "key"), ToolOutput(in); out != want {
				t.Errorf("got %q, want the plain ToolOutput result %q", out, want)
			}
		})
	}
	// An object that has the field keeps only that field.
	out := ToolOutputKeeping(`{"key":"`+key+`","encryption_key":"`+key+`"}`, "key")
	if strings.Count(out, key) != 1 {
		t.Errorf("only the named field may survive:\n%s", out)
	}
}
