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

func TestSafetyPolicy(t *testing.T) {
	env := map[string]string{
		"TRUENAS_MCP_READ_ONLY":   "yes",
		"TRUENAS_MCP_ALLOW_TOOLS": " query_pools, ,list_alerts ",
		"TRUENAS_MCP_DENY_TOOLS":  "system_reboot",
	}
	p := safetyPolicy(false, "", "", func(k string) string { return env[k] })
	if !p.ReadOnly || strings.Join(p.Allow, ",") != "query_pools,list_alerts" || strings.Join(p.Deny, ",") != "system_reboot" {
		t.Errorf("env policy = %+v", p)
	}
	p = safetyPolicy(false, "query_apps", "", func(k string) string { return env[k] })
	if strings.Join(p.Allow, ",") != "query_apps" {
		t.Errorf("flag did not override env: %+v", p)
	}
	for _, v := range []string{"", "0", "false", "No", "off"} {
		if safetyPolicy(false, "", "", func(string) string { return v }).ReadOnly {
			t.Errorf("TRUENAS_MCP_READ_ONLY=%q enabled read-only", v)
		}
	}
	if !safetyPolicy(true, "", "", func(string) string { return "" }).ReadOnly {
		t.Error("--read-only ignored")
	}
}
