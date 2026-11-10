package tools

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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
	want, err := os.ReadFile(filepath.Join("testdata", "tools_list.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("ListTools output differs from testdata/tools_list.json (%d bytes got, %d bytes golden)", len(got), len(want))
	}
}
