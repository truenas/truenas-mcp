package main

import (
	"strings"
	"testing"
)

func TestCheckRequired(t *testing.T) {
	if err := checkRequired("truenas.local", "1-key", "svc-mcp"); err != nil {
		t.Fatalf("complete configuration rejected: %v", err)
	}
	for _, tc := range []struct{ url, key, user, want string }{
		{"truenas.local", "1-key", "", "--api-user"},
		{"", "1-key", "svc-mcp", "--truenas-url"},
		{"truenas.local", "", "svc-mcp", "--api-key"},
	} {
		err := checkRequired(tc.url, tc.key, tc.user)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("checkRequired(%q, key, %q) = %v; want error naming %s", tc.url, tc.user, err, tc.want)
		}
	}
}
