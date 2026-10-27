package truenas

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestAPICallErrorLayoutAndMapServerText(t *testing.T) {
	c := &Client{}
	err := c.formatAPIErrorWithContext(&APIError{Code: 22, Message: "[EINVAL] bad xyz", Extra: []interface{}{"xyz"}, Trace: "trace xyz"}, "pool.dataset.create", []interface{}{map[string]interface{}{"name": "xyz"}})
	want := "API error: [EINVAL] bad xyz (code 22)\n\nRequest:\n  Method: pool.dataset.create\n  Params: [\n    {\n      \"name\": \"xyz\"\n    }\n  ]\n\nDetails: [\n  \"xyz\"\n]\n\nTrace: trace xyz"
	if err.Error() != want {
		t.Fatalf("layout changed:\n got %q\nwant %q", err.Error(), want)
	}
	var apiErr *APICallError
	if !errors.As(fmt.Errorf("wrapped: %w", err), &apiErr) {
		t.Fatal("not an *APICallError")
	}
	got := apiErr.MapServerText(func(s string) string { return strings.ReplaceAll(s, "xyz", "###") }).Error()
	wantMapped := "API error: [EINVAL] bad ### (code 22)\n\nRequest:\n  Method: pool.dataset.create\n  Params: [\n    {\n      \"name\": \"xyz\"\n    }\n  ]\n\nDetails: [\n  \"###\"\n]\n\nTrace: trace ###"
	if got != wantMapped {
		t.Errorf("mapped:\n got %q\nwant %q", got, wantMapped)
	}
	if err.Error() != want {
		t.Error("MapServerText modified the original")
	}
	// Without params, details or trace the optional blocks are absent.
	bare := c.formatAPIErrorWithContext(&APIError{Code: 1, Message: "m"}, "x.y", nil).Error()
	if bare != "API error: m (code 1)\n\nRequest:\n  Method: x.y" {
		t.Errorf("bare = %q", bare)
	}
}
