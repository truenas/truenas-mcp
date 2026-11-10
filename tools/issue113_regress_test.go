package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func issue113RepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..")
}

func issue113Read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The dead storage-volume helpers existed only to satisfy their own tests
// and are not part of the host-path install path. They must be gone from
// production source (parseStoragePath stays; it is used by live code).
func TestDeadStorageVolumeHelpersRemoved(t *testing.T) {
	src := issue113Read(t, issue113RepoRoot(t), "tools/apps.go")
	for _, decl := range []string{
		"type StorageVolume struct",
		"func validateStorageVolumes(",
		"func buildPersistenceConfig(",
		"func verifyDatasetsExist(",
		"func extractStorageVolumes(",
	} {
		if strings.Contains(src, decl) {
			t.Errorf("tools/apps.go still declares dead helper %s", decl)
		}
	}
	if !strings.Contains(src, "func parseStoragePath(") {
		t.Error("parseStoragePath was removed; it is still used by verifyDatasetPathsExist")
	}

	tests := issue113Read(t, issue113RepoRoot(t), "tools/apps_test.go")
	for _, name := range []string{
		"func TestValidateStorageVolumes(",
		"func TestBuildPersistenceConfig(",
		"func TestExtractStorageVolumes(",
		"func TestExtractStorageVolumesErrorMessages(",
	} {
		if strings.Contains(tests, name) {
			t.Errorf("tools/apps_test.go still has %s", name)
		}
	}
}

// make lint must run staticcheck (including U1000) and an errcheck pass,
// and CI must use that same target.
func TestMakefileLintRunsStaticcheckAndErrcheck(t *testing.T) {
	root := issue113RepoRoot(t)
	makefile := issue113Read(t, root, "Makefile")
	lint := makefileLintTarget(t, makefile)
	if !strings.Contains(lint, "staticcheck") {
		t.Error("make lint does not run staticcheck")
	}
	if !strings.Contains(lint, "errcheck") {
		t.Error("make lint does not run errcheck")
	}
	if !strings.Contains(makefile, "go vet") {
		t.Error("make lint dropped go vet")
	}

	for _, rel := range []string{".github/workflows/build.yml", ".github/workflows/release.yml"} {
		wf := issue113Read(t, root, rel)
		if !strings.Contains(wf, "make lint") {
			t.Errorf("%s does not run make lint", rel)
		}
	}
}

// makefileLintTarget returns the body of the lint recipe.
func makefileLintTarget(t *testing.T, makefile string) string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	var b strings.Builder
	in := false
	for _, line := range lines {
		if strings.HasPrefix(line, "lint:") {
			in = true
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}
		if in {
			if line == "" || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "#") {
				b.WriteString(line)
				b.WriteByte('\n')
				continue
			}
			break
		}
	}
	got := b.String()
	if got == "" {
		t.Fatal("Makefile has no lint target")
	}
	return got
}
