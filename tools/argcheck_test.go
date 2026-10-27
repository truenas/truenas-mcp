package tools

import (
	"sort"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/truenas"
)

const secretValue = "s3cret-value-must-not-be-echoed"

func declaredKeys(tool Tool) []string {
	props, _ := tool.Definition.InputSchema["properties"].(map[string]interface{})
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestUnknownArgumentRejected(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)

	for _, tc := range []struct {
		tool string
		args map[string]interface{}
	}{
		{"query_pools", map[string]interface{}{"padding": secretValue}},
		{"query_apps", map[string]interface{}{"app_name": "plex", "padding": secretValue}},
		{"create_dataset", map[string]interface{}{"name": "tank/x", "compresion": secretValue}},
		{"system_reboot", map[string]interface{}{"confirm": true, "padding": secretValue}},
		{"delete_app", map[string]interface{}{"app_name": "plex", "dry_run": true, "padding": secretValue}},
	} {
		out, err := r.CallTool(tc.tool, tc.args)
		if err == nil {
			t.Errorf("%s with unknown key: no error, out=%s", tc.tool, out)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, tc.tool) || !strings.Contains(msg, "unknown argument") {
			t.Errorf("%s: error does not name the tool and the problem: %v", tc.tool, err)
		}
		unknown := "padding"
		if tc.tool == "create_dataset" {
			unknown = "compresion"
		}
		if !strings.Contains(msg, `"`+unknown+`"`) {
			t.Errorf("%s: error does not name %q: %v", tc.tool, unknown, err)
		}
		if strings.Contains(msg, secretValue) {
			t.Errorf("%s: error echoes an argument value: %v", tc.tool, err)
		}
		if len(seen[tc.tool]) != 0 {
			t.Errorf("%s: handler ran despite unknown key", tc.tool)
		}
	}
}

func TestUnknownArgumentRejectedBeforeAnyMiddlewareCall(t *testing.T) {
	client, fake := newFakeClient(t)
	r := NewRegistry(client, nil)
	if _, err := r.CallTool("query_pools", map[string]interface{}{"padding": 1}); err == nil || !strings.Contains(err.Error(), `"padding"`) {
		t.Fatalf("err = %v", err)
	}
	if got := fake.called("pool.query"); len(got) != 0 {
		t.Errorf("pool.query was called %d time(s)", len(got))
	}
	// Without the extra key the same call goes through.
	if _, err := r.CallTool("query_pools", map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if got := fake.called("pool.query"); len(got) != 1 {
		t.Errorf("pool.query calls = %d, want 1", len(got))
	}
}

// TestEveryToolRejectsUnknownArguments covers all registered tools, with and
// without the confirm/dry_run keys that gated tools declare.
func TestEveryToolRejectsUnknownArguments(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	for name, tool := range r.tools {
		variants := []map[string]interface{}{
			{"padding": secretValue},
			{"padding": secretValue, "confirm": true},
			{"padding": secretValue, "dry_run": true},
		}
		for _, args := range variants {
			if (args["confirm"] != nil && !hasParam(tool, "confirm")) || (args["dry_run"] != nil && !hasParam(tool, "dry_run")) {
				// Those keys would be unknown too; still must be rejected,
				// but then the error legitimately names them as well.
				continue
			}
			_, err := r.CallTool(name, args)
			if err == nil {
				t.Errorf("%s accepted %v", name, args)
				continue
			}
			if !strings.Contains(err.Error(), `"padding"`) || strings.Contains(err.Error(), secretValue) {
				t.Errorf("%s: unexpected error %v", name, err)
			}
		}
		if len(seen[name]) != 0 {
			t.Errorf("%s: handler ran for a rejected call", name)
		}
		// A key that merely resembles a declared one is unknown too.
		if keys := declaredKeys(tool); len(keys) > 0 {
			if _, err := r.CallTool(name, map[string]interface{}{keys[0] + "_": "x"}); err == nil {
				t.Errorf("%s accepted %q", name, keys[0]+"_")
			}
		}
	}
}

// TestEveryDeclaredArgumentIsAccepted checks that validation never rejects a
// key the schema advertises, one key at a time and all together.
func TestEveryDeclaredArgumentIsAccepted(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	for name, tool := range r.tools {
		keys := declaredKeys(tool)
		all := map[string]interface{}{}
		for _, key := range keys {
			all[key] = "x"
			// confirm keeps destructive tools from stopping at the gate;
			// either way validation must let the key through.
			args := map[string]interface{}{key: "x", "confirm": hasParam(tool, "confirm")}
			if !hasParam(tool, "confirm") {
				delete(args, "confirm")
			}
			if _, err := r.CallTool(name, args); err != nil {
				t.Errorf("%s rejected declared key %q: %v", name, key, err)
			}
		}
		if _, err := r.CallTool(name, all); err != nil {
			t.Errorf("%s rejected all declared keys: %v", name, err)
		}
		if _, err := r.CallTool(name, nil); err != nil {
			t.Errorf("%s rejected nil arguments: %v", name, err)
		}
		if _, err := r.CallTool(name, map[string]interface{}{}); err != nil {
			t.Errorf("%s rejected empty arguments: %v", name, err)
		}
		if len(seen[name]) == 0 {
			t.Errorf("%s: handler never ran", name)
		}
	}
}

func TestGatedToolsStillAcceptConfirmAndDryRun(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	gated := 0
	for name, tool := range r.tools {
		if safetyOf(name).Access != Destructive {
			continue
		}
		gated++
		if !hasParam(tool, "confirm") {
			t.Errorf("%s: gated tool does not declare confirm", name)
		}
		if _, err := r.CallTool(name, map[string]interface{}{"confirm": true}); err != nil {
			t.Errorf("%s confirm=true: %v", name, err)
		}
		if hasParam(tool, "dry_run") {
			out, err := r.CallTool(name, map[string]interface{}{"dry_run": true})
			if err != nil {
				t.Errorf("%s dry_run=true: %v", name, err)
			}
			if strings.Contains(out, "confirmation_required") {
				t.Errorf("%s: dry_run=true was held for confirmation: %s", name, out)
			}
			if _, err := r.CallTool(name, map[string]interface{}{"dry_run": true, "confirm": true}); err != nil {
				t.Errorf("%s dry_run+confirm: %v", name, err)
			}
		}
		// Without confirm the call is still held for approval, not rejected.
		out, err := r.CallTool(name, map[string]interface{}{})
		if err != nil || !strings.Contains(out, "confirmation_required") {
			t.Errorf("%s without confirm: out=%s err=%v", name, out, err)
		}
		if len(seen[name]) == 0 {
			t.Errorf("%s: handler never ran", name)
		}
	}
	if gated == 0 {
		t.Fatal("no gated tools found")
	}
	// Write tools declare dry_run themselves and accept it.
	if _, err := r.CallTool("create_dataset", map[string]interface{}{"name": "tank/x", "dry_run": true}); err != nil {
		t.Errorf("create_dataset dry_run: %v", err)
	}
	// confirm is only declared by destructive tools.
	if _, err := r.CallTool("create_dataset", map[string]interface{}{"name": "tank/x", "confirm": true}); err == nil || !strings.Contains(err.Error(), `"confirm"`) {
		t.Errorf("create_dataset confirm: err = %v", err)
	}
}

func TestValidateArgsMessage(t *testing.T) {
	tool := Tool{Definition: mcp.Tool{InputSchema: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"b": map[string]interface{}{"type": "string"},
			"a": map[string]interface{}{"type": "string"},
		},
	}}}

	err := validateArgs("demo", tool, map[string]interface{}{"zeta": secretValue, "alpha": secretValue, "a": "ok"})
	want := `invalid arguments for tool demo: unknown arguments "alpha", "zeta" (valid arguments: a, b); nothing was executed`
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant %s", err, want)
	}

	err = validateArgs("demo", tool, map[string]interface{}{"one": 1})
	if err == nil || !strings.Contains(err.Error(), `unknown argument "one"`) {
		t.Errorf("singular form: %v", err)
	}

	// Names are quoted so control characters cannot forge extra lines, and
	// are truncated.
	err = validateArgs("demo", tool, map[string]interface{}{"bad\nname": 1})
	if err == nil || strings.Contains(err.Error(), "\n") || strings.Contains(err.Error(), "bad") {
		t.Errorf("control characters: %q", err)
	}
	wantInvalid := `invalid arguments for tool demo: unknown argument: 1 key with invalid characters (valid arguments: a, b); nothing was executed`
	if err == nil || err.Error() != wantInvalid {
		t.Errorf("control characters: got %v\nwant %s", err, wantInvalid)
	}

	// Only [A-Za-z0-9_.-] names are listed; the rest are counted. A name
	// that redaction would act on must not cut the message short.
	err = validateArgs("demo", tool, map[string]interface{}{
		"ok_name-1.x":      1,
		"password=hunter2": 1,
		"sp ace":           1,
		"üñí":              1,
	})
	wantMixed := `invalid arguments for tool demo: unknown arguments "ok_name-1.x" and 3 keys with invalid characters (valid arguments: a, b); nothing was executed`
	if err == nil || err.Error() != wantMixed {
		t.Fatalf("mixed names: got %v\nwant %s", err, wantMixed)
	}
	if got := redact.Text(err.Error()); got != wantMixed {
		t.Errorf("redaction changed the message:\n got %s\nwant %s", got, wantMixed)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("value leaked: %v", err)
	}
	long := strings.Repeat("x", 500)
	err = validateArgs("demo", tool, map[string]interface{}{long: 1})
	if err == nil || strings.Contains(err.Error(), long) || len(err.Error()) > 400 {
		t.Errorf("long name not truncated: %v", err)
	}

	many := map[string]interface{}{}
	for i := 0; i < 25; i++ {
		many[strings.Repeat("k", i+1)] = 1
	}
	err = validateArgs("demo", tool, many)
	if err == nil || !strings.Contains(err.Error(), "and 15 more") {
		t.Errorf("many unknown keys: %v", err)
	}

	empty := Tool{Definition: mcp.Tool{InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}}}
	err = validateArgs("none", empty, map[string]interface{}{"x": 1})
	if err == nil || !strings.Contains(err.Error(), "this tool takes no arguments") {
		t.Errorf("no-arg tool: %v", err)
	}
	if err := validateArgs("none", empty, nil); err != nil {
		t.Errorf("nil args: %v", err)
	}
}

func TestAdditionalPropertiesStayPermissive(t *testing.T) {
	for _, ap := range []interface{}{true, map[string]interface{}{"type": "string"}} {
		tool := Tool{Definition: mcp.Tool{InputSchema: map[string]interface{}{
			"type":                 "object",
			"properties":           map[string]interface{}{"a": map[string]interface{}{"type": "string"}},
			"additionalProperties": ap,
		}}}
		if err := validateArgs("open", tool, map[string]interface{}{"a": 1, "anything": 2}); err != nil {
			t.Errorf("additionalProperties=%v rejected: %v", ap, err)
		}
	}
	tool := Tool{Definition: mcp.Tool{InputSchema: map[string]interface{}{
		"type":                 "object",
		"properties":           map[string]interface{}{},
		"additionalProperties": false,
	}}}
	if err := validateArgs("closed", tool, map[string]interface{}{"x": 1}); err == nil {
		t.Error("additionalProperties=false accepted an unknown key")
	}

	// And through CallTool.
	r := NewRegistry(nil, nil)
	ran := false
	r.tools["test_open"] = Tool{
		Definition: mcp.Tool{Name: "test_open", InputSchema: map[string]interface{}{"type": "object", "additionalProperties": true}},
		Handler: func(*truenas.Client, map[string]interface{}) (string, error) {
			ran = true
			return "ok", nil
		},
	}
	if _, err := r.CallTool("test_open", map[string]interface{}{"anything": 1, "confirm": true}); err != nil || !ran {
		t.Errorf("permissive tool: err=%v ran=%v", err, ran)
	}
}

func TestPolicyRefusalComesBeforeArgumentValidation(t *testing.T) {
	r := NewRegistry(nil, nil)
	stubHandlers(r)
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	_, err := r.CallTool("create_dataset", map[string]interface{}{"padding": 1})
	if err == nil || !strings.Contains(err.Error(), "read-only mode") {
		t.Errorf("err = %v, want the read-only refusal", err)
	}
}

func TestUnknownToolStillReported(t *testing.T) {
	r := NewRegistry(nil, nil)
	if _, err := r.CallTool("no_such_tool", map[string]interface{}{"padding": 1}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("err = %v", err)
	}
}

func TestUnknownArgsErrorKeepsDirectoryServiceMigrationHint(t *testing.T) {
	client, fake := newFakeClient(t)
	r := NewRegistry(client, nil)
	_, err := r.CallTool("configure_directory_service", map[string]interface{}{"type": "activedirectory", "domain": "example.com", "bindpw": "s3cret-value", "enable": true, "confirm": true})
	if err == nil {
		t.Fatal("flat arguments accepted")
	}
	for _, want := range []string{"unknown arguments", "nothing was executed", "bindpw, domain, type are no longer top-level arguments", "Use service_type + credential + configuration"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "s3cret-value") {
		t.Errorf("error echoes a value: %v", err)
	}
	if len(fake.called("directoryservices.update")) != 0 {
		t.Error("flat arguments reached directoryservices.update")
	}
	// A typo that is not an old flat name gets no migration hint.
	_, err = r.CallTool("configure_directory_service", map[string]interface{}{"enabel": true})
	if err == nil || strings.Contains(err.Error(), "no longer top-level") {
		t.Errorf("err = %v", err)
	}
	// Other tools never get the hint.
	_, err = r.CallTool("query_pools", map[string]interface{}{"type": "x"})
	if err == nil || strings.Contains(err.Error(), "no longer top-level") {
		t.Errorf("err = %v", err)
	}
}
