package redact

import (
	"strings"
	"testing"
)

func TestToolOutputKeeping(t *testing.T) {
	keep := func(v string) bool { return v == "issued" }
	in := "{\n  \"confirmation_token\": \"issued\",\n  \"preview\": {\"confirmation_token\": \"issued\", \"bindpw\": \"pw1\"},\n  \"password\": \"pw2\"\n}"
	out := ToolOutputKeeping(in, "confirmation_token", keep)
	if !strings.Contains(out, `"confirmation_token": "issued"`) || strings.Count(out, "issued") != 1 {
		t.Errorf("only the top-level issued token may stay visible:\n%s", out)
	}
	if strings.Contains(out, "pw1") || strings.Contains(out, "pw2") {
		t.Errorf("secrets leaked:\n%s", out)
	}
	if !strings.Contains(out, "\n") {
		t.Errorf("indentation lost:\n%s", out)
	}

	for name, tc := range map[string]struct {
		in   string
		keep func(string) bool
	}{
		"not issued":   {`{"confirmation_token":"other"}`, keep},
		"no keep func": {`{"confirmation_token":"issued"}`, nil},
		"not a string": {`{"confirmation_token":["issued"]}`, keep},
		"not object":   {`["issued"]`, keep},
		"text":         {`confirmation_token="issued"`, keep},
	} {
		if got, want := ToolOutputKeeping(tc.in, "confirmation_token", tc.keep), ToolOutput(tc.in); got != want {
			t.Errorf("%s: got %s, want ToolOutput's %s", name, got, want)
		}
	}
	// Without the key the result is exactly ToolOutput's.
	plain := "{\n  \"bindpw\": \"pw\"\n}"
	if ToolOutputKeeping(plain, "confirmation_token", keep) != ToolOutput(plain) {
		t.Error("result without the key differs from ToolOutput")
	}
}
