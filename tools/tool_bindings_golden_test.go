package tools

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// toolBindingsCanonicalJSON is every registered tool's handler, Plan and
// Execute function names, in ListTools order. It pins the bindings so a
// mechanical split of registry.go cannot rewire a tool to a different
// function.
func toolBindingsCanonicalJSON(t *testing.T) []byte {
	t.Helper()
	r := NewRegistry(nil, nil)
	type binding struct {
		Name    string `json:"name"`
		Handler string `json:"handler"`
		Plan    string `json:"plan,omitempty"`
		Execute string `json:"execute,omitempty"`
	}
	var out []binding
	for _, def := range r.ListTools() {
		tool, ok := r.tools[def.Name]
		if !ok {
			t.Fatalf("ListTools returned %q which is not in the registry map", def.Name)
		}
		b := binding{Name: def.Name, Handler: funcName(tool.Handler)}
		if tool.Plan != nil {
			b.Plan = funcName(tool.Plan)
		}
		if tool.Execute != nil {
			b.Execute = funcName(tool.Execute)
		}
		out = append(out, b)
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func funcName(v interface{}) string {
	if v == nil {
		return ""
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Func || rv.IsNil() {
		return ""
	}
	fn := runtime.FuncForPC(rv.Pointer())
	if fn == nil {
		return ""
	}
	name := fn.Name()
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimPrefix(name, "tools.")
	return strings.TrimSuffix(name, "-fm")
}

// TestToolBindingsMatchGolden pins handler, Plan and Execute bindings so
// a mechanical split cannot change which function a tool calls.
func TestToolBindingsMatchGolden(t *testing.T) {
	got := toolBindingsCanonicalJSON(t)
	path := filepath.Join("testdata", "tool_bindings.json")
	if *updateGoldens {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("tool bindings differ from testdata/tool_bindings.json (%d bytes got, %d bytes golden)", len(got), len(want))
	}
}
