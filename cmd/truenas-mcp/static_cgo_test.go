package main

import (
	"strings"
	"testing"
)

func goBuildLines(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.Contains(trim, "go build") {
			out = append(out, trim)
		}
	}
	return out
}

func makefileTargetRecipe(t *testing.T, makefile, target string) string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	var b strings.Builder
	in := false
	prefix := target + ":"
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) && (len(line) == len(prefix) || line[len(prefix)] == ' ' || line[len(prefix)] == '\t') {
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
		t.Fatalf("Makefile has no %s target", target)
	}
	return got
}

// TestBuildAndReleaseBinariesDisableCgo requires CGO_ENABLED=0 on every
// binary build in build.yml, release.yml, and the Makefile build targets,
// so the published Linux binary is statically linked. Test steps must not
// set CGO_ENABLED=0: the race detector needs cgo.
func TestBuildAndReleaseBinariesDisableCgo(t *testing.T) {
	for _, rel := range []string{".github/workflows/build.yml", ".github/workflows/release.yml"} {
		src := issue114Read(t, rel)
		builds := goBuildLines(src)
		if len(builds) == 0 {
			t.Errorf("%s has no go build steps", rel)
			continue
		}
		for _, line := range builds {
			if !strings.Contains(line, "CGO_ENABLED=0") {
				t.Errorf("%s build step %q is missing CGO_ENABLED=0", rel, line)
			}
		}
		for _, line := range testInvocations(src) {
			if strings.Contains(line, "CGO_ENABLED=0") {
				t.Errorf("%s test step %q sets CGO_ENABLED=0; the race detector needs cgo", rel, line)
			}
		}
	}

	makefile := issue114Read(t, "Makefile")
	for _, target := range []string{"build", "build-all"} {
		recipe := makefileTargetRecipe(t, makefile, target)
		builds := goBuildLines(recipe)
		if len(builds) == 0 {
			t.Errorf("Makefile %s target has no go build lines", target)
			continue
		}
		for _, line := range builds {
			if !strings.Contains(line, "CGO_ENABLED=0") {
				t.Errorf("Makefile %s line %q is missing CGO_ENABLED=0", target, line)
			}
		}
	}
	testRecipe := makefileTestRecipe(t, makefile)
	if strings.Contains(testRecipe, "CGO_ENABLED=0") {
		t.Errorf("Makefile test target sets CGO_ENABLED=0; the race detector needs cgo: %q", testRecipe)
	}
}

// TestBuildWorkflowChecksLinuxBinaryIsStatic requires a build.yml step
// that inspects the Linux amd64 binary with file (and ldd) and fails when
// that binary is dynamically linked.
func TestBuildWorkflowChecksLinuxBinaryIsStatic(t *testing.T) {
	src := issue114Read(t, ".github/workflows/build.yml")
	if !strings.Contains(src, "truenas-mcp-linux-amd64") {
		t.Fatal("build.yml does not mention truenas-mcp-linux-amd64")
	}
	if !strings.Contains(src, "file") || !strings.Contains(src, "truenas-mcp-linux-amd64") {
		t.Error("build.yml should run file on truenas-mcp-linux-amd64")
	}
	// A passing check must look for the static file(1) tag and fail otherwise.
	if !strings.Contains(src, "statically linked") {
		t.Error("build.yml should assert that file reports the Linux binary as statically linked")
	}
	if !strings.Contains(src, "ldd") || !strings.Contains(src, "not a dynamic executable") {
		t.Error("build.yml should also run ldd and accept 'not a dynamic executable'")
	}
	if !strings.Contains(src, "exit 1") {
		t.Error("build.yml static-link check should fail (exit 1) when the Linux binary is dynamic")
	}
}
