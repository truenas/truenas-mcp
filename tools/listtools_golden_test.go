package tools

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateGoldens regenerates testdata/tools_list.json and
// testdata/tool_bindings.json: go test ./tools -update
var updateGoldens = flag.Bool("update", false, "update testdata goldens (tools_list.json, tool_bindings.json)")

// listToolsCanonicalJSON is the full tools/list payload: every tool's
// name, description, input schema and annotations, in ListTools order.
func listToolsCanonicalJSON(t *testing.T) []byte {
	t.Helper()
	b, err := json.MarshalIndent(NewRegistry(nil, nil).ListTools(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// TestListToolsMatchesGolden pins tools/list output so a mechanical
// split of registry.go cannot change names, descriptions, schemas,
// annotations or order.
func TestListToolsMatchesGolden(t *testing.T) {
	got := listToolsCanonicalJSON(t)
	path := filepath.Join("testdata", "tools_list.json")
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
		t.Errorf("ListTools output differs from testdata/tools_list.json (%d bytes got, %d bytes golden)", len(got), len(want))
	}
}
