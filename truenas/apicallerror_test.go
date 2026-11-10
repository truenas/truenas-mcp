package truenas

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
)

func TestAPICallErrorLayoutAndMapServerText(t *testing.T) {
	c := &Client{}
	err := c.formatAPIErrorWithContext(&APIError{Code: 22, Message: "[EINVAL] bad xyz", Extra: []interface{}{"xyz"}, Trace: "trace xyz"}, "pool.dataset.create", []interface{}{map[string]interface{}{"name": "xyz"}})
	want := "API error: [EINVAL] bad xyz (code 22)\n\nRequest:\n  Method: pool.dataset.create\n  Params: [\n    {\n      \"name\": \"xyz\"\n    }\n  ]\n\nDetails: [\n  \"xyz\"\n]"
	if err.Error() != want {
		t.Fatalf("layout changed:\n got %q\nwant %q", err.Error(), want)
	}
	var apiErr *APICallError
	if !errors.As(fmt.Errorf("wrapped: %w", err), &apiErr) {
		t.Fatal("not an *APICallError")
	}
	got := apiErr.MapServerText(func(s string) string { return strings.ReplaceAll(s, "xyz", "###") }).Error()
	wantMapped := "API error: [EINVAL] bad ### (code 22)\n\nRequest:\n  Method: pool.dataset.create\n  Params: [\n    {\n      \"name\": \"xyz\"\n    }\n  ]\n\nDetails: [\n  \"###\"\n]"
	if got != wantMapped {
		t.Errorf("mapped:\n got %q\nwant %q", got, wantMapped)
	}
	mapped := apiErr.MapServerText(func(s string) string { return strings.ReplaceAll(s, "xyz", "###") })
	if mapped.Trace != "trace ###" {
		t.Errorf("mapped Trace = %q, want %q", mapped.Trace, "trace ###")
	}
	if err.Error() != want {
		t.Error("MapServerText modified the original")
	}
	if apiErr.Trace != "trace xyz" {
		t.Errorf("original Trace = %q, want it unchanged", apiErr.Trace)
	}
	// Without params, details or trace the optional blocks are absent.
	bare := c.formatAPIErrorWithContext(&APIError{Code: 1, Message: "m"}, "x.y", nil).Error()
	if bare != "API error: m (code 1)\n\nRequest:\n  Method: x.y" {
		t.Errorf("bare = %q", bare)
	}
}

func TestAPICallErrorOmitsTraceFromClient(t *testing.T) {
	c := &Client{}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	SetDebugLogging(false)
	t.Cleanup(func() { SetDebugLogging(false) })

	const trace = "Traceback hidden from the model"
	err := c.formatAPIErrorWithContext(&APIError{Code: 22, Message: "[EINVAL] bad", Trace: trace}, "pool.dataset.create", nil)
	if strings.Contains(err.Error(), "Trace:") || strings.Contains(err.Error(), trace) {
		t.Errorf("client error includes the server trace:\n%s", err.Error())
	}
	var apiErr *APICallError
	if !errors.As(err, &apiErr) || apiErr.Trace != trace {
		t.Errorf("Trace field = %+v", apiErr)
	}
	if strings.Contains(logs.String(), trace) {
		t.Errorf("trace logged without --debug:\n%s", logs.String())
	}

	logs.Reset()
	SetDebugLogging(true)
	err = c.formatAPIErrorWithContext(&APIError{Code: 22, Message: "[EINVAL] bad", Trace: trace}, "pool.dataset.create", nil)
	if strings.Contains(err.Error(), "Trace:") || strings.Contains(err.Error(), trace) {
		t.Errorf("client error includes the server trace when debug is on:\n%s", err.Error())
	}
	if !strings.Contains(logs.String(), trace) {
		t.Errorf("debug log does not contain the trace:\n%s", logs.String())
	}
}
