package tools

import (
	"fmt"
	"math"
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

// sampleDeclaredArg returns a value for the named argument that the
// tool's schema accepts, so tests that check names are not tripped up by
// the value check.
func sampleDeclaredArg(tool Tool, key string) interface{} {
	props, _ := tool.Definition.InputSchema["properties"].(map[string]interface{})
	sub, _ := props[key].(map[string]interface{})
	return schemaSample(sub, true)
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
		out, err := r.CallTool(t.Context(), tc.tool, tc.args)
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
	if _, err := r.CallTool(t.Context(), "query_pools", map[string]interface{}{"padding": 1}); err == nil || !strings.Contains(err.Error(), `"padding"`) {
		t.Fatalf("err = %v", err)
	}
	if got := fake.called("pool.query"); len(got) != 0 {
		t.Errorf("pool.query was called %d time(s)", len(got))
	}
	// Without the extra key the same call goes through.
	if _, err := r.CallTool(t.Context(), "query_pools", map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if got := fake.called("pool.query"); len(got) != 1 {
		t.Errorf("pool.query calls = %d, want 1", len(got))
	}
}

// TestEveryToolRejectsUnknownArguments covers all registered tools, with and
// without the confirmation_token/dry_run keys that gated tools declare and
// the legacy confirm flag they still accept.
func TestEveryToolRejectsUnknownArguments(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	for name, tool := range r.tools {
		variants := []map[string]interface{}{
			{"padding": secretValue},
			{"padding": secretValue, "confirm": true},
			{"padding": secretValue, "confirmation_token": "x"},
			{"padding": secretValue, "dry_run": true},
		}
		for _, args := range variants {
			if (args["confirm"] != nil && safetyOf(name).Access != Destructive) ||
				(args["confirmation_token"] != nil && !hasParam(tool, "confirmation_token")) ||
				(args["dry_run"] != nil && !hasParam(tool, "dry_run")) {
				// Those keys would be unknown too; still must be rejected,
				// but then the error legitimately names them as well.
				continue
			}
			_, err := r.CallTool(t.Context(), name, args)
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
			if _, err := r.CallTool(t.Context(), name, map[string]interface{}{keys[0] + "_": "x"}); err == nil {
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
		all := minimalArgs(tool)
		for _, key := range keys {
			val := sampleDeclaredArg(tool, key)
			all[key] = val
			// A valid token keeps destructive tools from stopping at the
			// gate; either way validation must let the key through.
			args := minimalArgs(tool)
			args[key] = val
			if hasParam(tool, "confirmation_token") {
				args = approve(t, r, name, args)
			}
			if _, err := r.CallTool(t.Context(), name, args); err != nil {
				t.Errorf("%s rejected declared key %q: %v", name, key, err)
			}
		}
		if hasParam(tool, "confirmation_token") {
			all = approve(t, r, name, all)
		}
		if _, err := r.CallTool(t.Context(), name, all); err != nil {
			t.Errorf("%s rejected all declared keys: %v", name, err)
		}
		// With only the required arguments (none for most tools, so
		// nil and empty arguments too) the call goes through.
		if len(schemaRequired(tool.Definition.InputSchema["required"])) == 0 {
			if _, err := r.CallTool(t.Context(), name, nil); err != nil {
				t.Errorf("%s rejected nil arguments: %v", name, err)
			}
		}
		if _, err := r.CallTool(t.Context(), name, minimalArgs(tool)); err != nil {
			t.Errorf("%s rejected its required arguments alone: %v", name, err)
		}
		if len(seen[name]) == 0 {
			t.Errorf("%s: handler never ran", name)
		}
	}
}

func TestGatedToolsAcceptTokenConfirmAndDryRun(t *testing.T) {
	r := NewRegistry(nil, nil)
	seen := stubHandlers(r)
	gated := 0
	for name, tool := range r.tools {
		if safetyOf(name).Access != Destructive {
			continue
		}
		gated++
		if !hasParam(tool, "confirmation_token") || hasParam(tool, "confirm") {
			t.Errorf("%s: gated tool must declare confirmation_token and not confirm", name)
		}
		with := func(extra map[string]interface{}) map[string]interface{} {
			args := minimalArgs(tool)
			for k, v := range extra {
				args[k] = v
			}
			return args
		}
		// The legacy confirm flag is accepted (not an unknown argument) but
		// only previews.
		if out, err := r.CallTool(t.Context(), name, with(map[string]interface{}{"confirm": true})); err != nil || !strings.Contains(out, "confirmation_required") {
			t.Errorf("%s confirm=true: out=%s err=%v", name, out, err)
		}
		if _, err := r.CallTool(t.Context(), name, approve(t, r, name, with(nil))); err != nil {
			t.Errorf("%s with a valid token: %v", name, err)
		}
		if hasParam(tool, "dry_run") {
			out, err := r.CallTool(t.Context(), name, with(map[string]interface{}{"dry_run": true}))
			if err != nil {
				t.Errorf("%s dry_run=true: %v", name, err)
			}
			if strings.Contains(out, "confirmation_required") {
				t.Errorf("%s: dry_run=true was held for confirmation: %s", name, out)
			}
			if _, err := r.CallTool(t.Context(), name, with(map[string]interface{}{"dry_run": true, "confirm": true})); err != nil {
				t.Errorf("%s dry_run+confirm: %v", name, err)
			}
		}
		// Without confirm the call is still held for approval, not rejected.
		out, err := r.CallTool(t.Context(), name, with(nil))
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
	if _, err := r.CallTool(t.Context(), "create_dataset", map[string]interface{}{"name": "tank/x", "dry_run": true}); err != nil {
		t.Errorf("create_dataset dry_run: %v", err)
	}
	// confirm is only declared by destructive tools.
	if _, err := r.CallTool(t.Context(), "create_dataset", map[string]interface{}{"name": "tank/x", "confirm": true}); err == nil || !strings.Contains(err.Error(), `"confirm"`) {
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
	if _, err := r.CallTool(t.Context(), "test_open", approve(t, r, "test_open", map[string]interface{}{"anything": 1})); err != nil || !ran {
		t.Errorf("permissive tool: err=%v ran=%v", err, ran)
	}
}

func TestPolicyRefusalComesBeforeArgumentValidation(t *testing.T) {
	r := NewRegistry(nil, nil)
	stubHandlers(r)
	if err := r.SetPolicy(Policy{ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	_, err := r.CallTool(t.Context(), "create_dataset", map[string]interface{}{"padding": 1})
	if err == nil || !strings.Contains(err.Error(), "read-only mode") {
		t.Errorf("err = %v, want the read-only refusal", err)
	}
}

func TestUnknownToolStillReported(t *testing.T) {
	r := NewRegistry(nil, nil)
	if _, err := r.CallTool(t.Context(), "no_such_tool", map[string]interface{}{"padding": 1}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("err = %v", err)
	}
}

func TestUnknownArgsErrorKeepsDirectoryServiceMigrationHint(t *testing.T) {
	client, fake := newFakeClient(t)
	r := NewRegistry(client, nil)
	_, err := r.CallTool(t.Context(), "configure_directory_service", map[string]interface{}{"type": "activedirectory", "domain": "example.com", "bindpw": "s3cret-value", "enable": true, "confirm": true})
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
	_, err = r.CallTool(t.Context(), "configure_directory_service", map[string]interface{}{"enabel": true})
	if err == nil || strings.Contains(err.Error(), "no longer top-level") {
		t.Errorf("err = %v", err)
	}
	// Other tools never get the hint.
	_, err = r.CallTool(t.Context(), "query_pools", map[string]interface{}{"type": "x"})
	if err == nil || strings.Contains(err.Error(), "no longer top-level") {
		t.Errorf("err = %v", err)
	}
}

// TestToolSchemasUseOnlySupportedKeywords keeps the validator honest: a
// schema keyword it does not implement, or a keyword whose value has a Go
// type it does not read (minimum as a string, enum as a []int, ...), would
// otherwise be silently ignored. Every pattern must compile, every property
// must declare a type, and defaults must match their own schema.
func TestToolSchemasUseOnlySupportedKeywords(t *testing.T) {
	r := NewRegistry(nil, nil)
	for name, tool := range r.tools {
		for _, problem := range schemaKeywordProblems(name, tool.Definition.InputSchema) {
			t.Error(problem)
		}
	}
}

// TestSchemaKeywordGuardCatchesBadKeywords checks that the guard above
// really flags wrongly typed keyword values.
func TestSchemaKeywordGuardCatchesBadKeywords(t *testing.T) {
	for label, s := range map[string]map[string]interface{}{
		"unknown keyword":             {"type": "string", "format": "uuid"},
		"no type":                     {"description": "x"},
		"unknown type name":           {"type": "int"},
		"type of wrong Go type":       {"type": 1},
		"minimum as string":           {"type": "integer", "minimum": "0"},
		"maximum as int64":            {"type": "integer", "maximum": int64(5)},
		"minimum on string":           {"type": "string", "minimum": 0},
		"minimum above maximum":       {"type": "integer", "minimum": 5, "maximum": 1},
		"enum as []int":               {"type": "integer", "enum": []int{1, 2}},
		"enum of wrong type":          {"type": "string", "enum": []interface{}{"a", 1.0}},
		"empty enum":                  {"type": "string", "enum": []string{}},
		"string enum on integer":      {"type": "integer", "enum": []string{"1"}},
		"const of wrong type":         {"type": "boolean", "const": "true"},
		"maxLength fraction":          {"type": "string", "maxLength": 1.5},
		"maxLength on integer":        {"type": "integer", "maxLength": 3},
		"pattern not a string":        {"type": "string", "pattern": 1},
		"pattern does not compile":    {"type": "string", "pattern": "("},
		"required as string":          {"type": "object", "required": "a", "properties": map[string]interface{}{"a": map[string]interface{}{"type": "string"}}},
		"required undeclared":         {"type": "object", "required": []string{"b"}, "properties": map[string]interface{}{"a": map[string]interface{}{"type": "string"}}},
		"properties wrong Go type":    {"type": "object", "properties": map[string]map[string]interface{}{}},
		"property not an object":      {"type": "object", "properties": map[string]interface{}{"a": "string"}},
		"additionalProperties string": {"type": "object", "additionalProperties": "no"},
		"items wrong Go type":         {"type": "array", "items": []interface{}{}},
		"items on string":             {"type": "string", "items": map[string]interface{}{"type": "string"}},
		"description not string":      {"type": "string", "description": 1},
		"default of wrong type":       {"type": "integer", "default": "50"},
	} {
		if problems := schemaKeywordProblems(label, s); len(problems) == 0 {
			t.Errorf("%s: not flagged", label)
		}
	}
	good := map[string]interface{}{"type": "object", "required": []string{"a"}, "additionalProperties": false, "properties": map[string]interface{}{
		"a": map[string]interface{}{"type": "integer", "minimum": 0, "maximum": 10.0, "default": 5},
		"b": map[string]interface{}{"type": "string", "enum": []string{"x"}, "pattern": "^x$", "maxLength": 3},
		"c": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string", "enum": []interface{}{"y"}}},
	}}
	if problems := schemaKeywordProblems("good", good); len(problems) != 0 {
		t.Errorf("good schema flagged: %v", problems)
	}
}

// schemaKeywordProblems lists every keyword in s (recursively) that the
// validator would not enforce as written.
func schemaKeywordProblems(path string, s map[string]interface{}) []string {
	var problems []string
	bad := func(format string, args ...interface{}) {
		problems = append(problems, path+": "+fmt.Sprintf(format, args...))
	}
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !supportedSchemaKeywords[k] {
			bad("unsupported schema keyword %q", k)
		}
	}

	knownTypes := map[string]bool{"string": true, "integer": true, "number": true, "boolean": true, "object": true, "array": true, "null": true}
	var types []string
	switch tv := s["type"].(type) {
	case string:
		types = []string{tv}
	case []string:
		types = tv
	case nil:
		bad("no type")
	default:
		bad("type has Go type %T, want string or []string", s["type"])
	}
	has := map[string]bool{}
	for _, ty := range types {
		if !knownTypes[ty] {
			bad("unknown type %q", ty)
		}
		has[ty] = true
	}
	numeric := has["integer"] || has["number"]

	// matches reports whether a constant is a valid value of the field.
	matches := func(v interface{}) bool {
		return len(types) > 0 && matchesSchemaType(types, v)
	}

	for _, kw := range []string{"minimum", "maximum"} {
		v, ok := s[kw]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			if math.IsNaN(n) || math.IsInf(n, 0) {
				bad("%s is not finite", kw)
			}
		case int:
		default:
			bad("%s has Go type %T, want float64 or int", kw, v)
		}
		if !numeric {
			bad("%s on a non-numeric field", kw)
		}
	}
	if min, ok := schemaNumber(s["minimum"]); ok {
		if max, ok := schemaNumber(s["maximum"]); ok && min > max {
			bad("minimum is above maximum")
		}
	}
	for _, kw := range []string{"minLength", "maxLength"} {
		v, ok := s[kw]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case int:
			if n < 0 {
				bad("%s is negative", kw)
			}
		case float64:
			if n < 0 || n != math.Trunc(n) {
				bad("%s is not a non-negative whole number", kw)
			}
		default:
			bad("%s has Go type %T, want int or float64", kw, v)
		}
		if !has["string"] {
			bad("%s on a non-string field", kw)
		}
	}
	if v, ok := s["pattern"]; ok {
		p, isStr := v.(string)
		if !isStr {
			bad("pattern has Go type %T, want string", v)
		} else if _, err := schemaPattern(p); err != nil {
			bad("pattern does not compile: %v", err)
		}
		if !has["string"] {
			bad("pattern on a non-string field")
		}
	}
	if v, ok := s["enum"]; ok {
		var values []interface{}
		switch e := v.(type) {
		case []interface{}:
			values = e
		case []string:
			for _, str := range e {
				values = append(values, str)
			}
		default:
			bad("enum has Go type %T, want []interface{} or []string", v)
		}
		if _, ok := v.([]string); ok && !has["string"] {
			bad("[]string enum on a non-string field")
		}
		if len(values) == 0 {
			bad("enum is empty")
		}
		for _, e := range values {
			if !matches(e) {
				bad("enum value of Go type %T does not match the field type", e)
			}
		}
	}
	if c, ok := s["const"]; ok && !matches(c) {
		bad("const of Go type %T does not match the field type", c)
	}
	for _, kw := range []string{"description", "title"} {
		if v, ok := s[kw]; ok {
			if _, isStr := v.(string); !isStr {
				bad("%s has Go type %T, want string", kw, v)
			}
		}
	}
	if v, ok := s["examples"]; ok {
		if _, isList := v.([]interface{}); !isList {
			bad("examples has Go type %T, want []interface{}", v)
		}
	}

	props := map[string]interface{}{}
	if v, ok := s["properties"]; ok {
		m, isMap := v.(map[string]interface{})
		if !isMap {
			bad("properties has Go type %T, want map[string]interface{}", v)
		}
		if !has["object"] {
			bad("properties on a non-object field")
		}
		props = m
	}
	if v, ok := s["required"]; ok {
		var req []string
		switch rv := v.(type) {
		case []string:
			req = rv
		case []interface{}:
			for _, x := range rv {
				str, isStr := x.(string)
				if !isStr {
					bad("required entry has Go type %T, want string", x)
				}
				req = append(req, str)
			}
		default:
			bad("required has Go type %T, want []string", v)
		}
		for _, k := range req {
			if _, ok := props[k]; !ok {
				bad("required %q is not declared in properties", k)
			}
		}
	}
	pk := make([]string, 0, len(props))
	for k := range props {
		pk = append(pk, k)
	}
	sort.Strings(pk)
	for _, k := range pk {
		sub, ok := props[k].(map[string]interface{})
		if !ok {
			bad("property %s has Go type %T, want map[string]interface{}", k, props[k])
			continue
		}
		problems = append(problems, schemaKeywordProblems(path+"."+k, sub)...)
	}
	if v, ok := s["items"]; ok {
		items, isMap := v.(map[string]interface{})
		if !isMap {
			bad("items has Go type %T, want map[string]interface{}", v)
		} else {
			problems = append(problems, schemaKeywordProblems(path+"[]", items)...)
		}
		if !has["array"] {
			bad("items on a non-array field")
		}
	}
	if v, ok := s["additionalProperties"]; ok {
		switch ap := v.(type) {
		case bool:
		case map[string]interface{}:
			problems = append(problems, schemaKeywordProblems(path+"{}", ap)...)
		default:
			bad("additionalProperties has Go type %T, want bool or map[string]interface{}", v)
		}
	}
	// Defaults must satisfy the schema they belong to.
	if d, ok := s["default"]; ok && len(problems) == 0 {
		if msg := schemaViolation(nil, s, d, false); msg != "" {
			bad("default does not match the schema: %s", msg)
		}
	}
	return problems
}
