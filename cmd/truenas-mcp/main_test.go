package main

import (
	"flag"
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
	envOf := func(env map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	}
	env := envOf(map[string]string{
		"TRUENAS_MCP_READ_ONLY":   "yes",
		"TRUENAS_MCP_ALLOW_TOOLS": " query_pools, ,list_alerts ",
		"TRUENAS_MCP_DENY_TOOLS":  "system_reboot",
	})
	p, err := safetyPolicy(false, nil, env)
	if err != nil || !p.ReadOnly || strings.Join(p.Allow, ",") != "query_pools,list_alerts" || strings.Join(p.Deny, ",") != "system_reboot" {
		t.Errorf("env policy = %+v, %v", p, err)
	}
	p, err = safetyPolicy(false, map[string]string{"allow-tools": "query_apps", "deny-tools": ""}, env)
	if err != nil || strings.Join(p.Allow, ",") != "query_apps" || len(p.Deny) != 0 {
		t.Errorf("flags did not override env: %+v, %v", p, err)
	}
	for _, v := range []string{"", "0", "false", "No", "off"} {
		p, _ := safetyPolicy(false, nil, envOf(map[string]string{"TRUENAS_MCP_READ_ONLY": v}))
		if p.ReadOnly {
			t.Errorf("TRUENAS_MCP_READ_ONLY=%q enabled read-only", v)
		}
	}
	if p, _ := safetyPolicy(true, nil, envOf(nil)); !p.ReadOnly {
		t.Error("--read-only ignored")
	}
	if p, err := safetyPolicy(false, nil, envOf(nil)); err != nil || p.Allow != nil {
		t.Errorf("unset allow list = %+v, %v; want no restriction", p, err)
	}

	// An allow list that is set but names no tools must fail closed.
	for _, v := range []string{"", ",", " , ", " ,, "} {
		if _, err := safetyPolicy(false, nil, envOf(map[string]string{"TRUENAS_MCP_ALLOW_TOOLS": v})); err == nil {
			t.Errorf("TRUENAS_MCP_ALLOW_TOOLS=%q accepted", v)
		}
		if _, err := safetyPolicy(false, map[string]string{"allow-tools": v}, envOf(nil)); err == nil {
			t.Errorf("--allow-tools=%q accepted", v)
		}
	}
}

// The allow/deny flags are registered without package variables (main reads
// them through flag.Visit); make sure they stay registered and documented.
func TestSafetyFlagsRegistered(t *testing.T) {
	for _, name := range []string{"read-only", "allow-tools", "deny-tools"} {
		f := flag.Lookup(name)
		if f == nil {
			t.Errorf("flag --%s is not registered", name)
			continue
		}
		if !strings.Contains(f.Usage, "TRUENAS_MCP_") {
			t.Errorf("flag --%s usage does not name its environment variable: %q", name, f.Usage)
		}
	}
}
