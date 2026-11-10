package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

type workflowStep struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
}

type workflowDoc struct {
	Jobs map[string]struct {
		Steps []workflowStep `yaml:"steps"`
	} `yaml:"jobs"`
}

// linuxStaticCheckSteps parses a GitHub Actions workflow and returns the
// steps whose run scripts inspect truenas-mcp-linux-amd64 with file and ldd.
func linuxStaticCheckSteps(src string) ([]workflowStep, error) {
	var doc workflowDoc
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		return nil, err
	}
	var found []workflowStep
	for _, job := range doc.Jobs {
		for _, step := range job.Steps {
			if isLinuxStaticCheckStep(step) {
				found = append(found, step)
			}
		}
	}
	return found, nil
}

func isLinuxStaticCheckStep(step workflowStep) bool {
	run := step.Run
	return strings.Contains(run, "truenas-mcp-linux-amd64") &&
		strings.Contains(run, "file") &&
		strings.Contains(run, "ldd")
}

func assertLinuxStaticCheckStep(t *testing.T, rel string, step workflowStep) {
	t.Helper()
	run := step.Run
	if !strings.Contains(run, "file") || !strings.Contains(run, "truenas-mcp-linux-amd64") {
		t.Errorf("%s step %q should run file on truenas-mcp-linux-amd64", rel, step.Name)
	}
	if !strings.Contains(run, "statically linked") {
		t.Errorf("%s step %q should assert that file reports the Linux binary as statically linked", rel, step.Name)
	}
	if !strings.Contains(run, "ldd") || !strings.Contains(run, "not a dynamic executable") {
		t.Errorf("%s step %q should also run ldd and accept 'not a dynamic executable'", rel, step.Name)
	}
	if !strings.Contains(run, "exit 1") {
		t.Errorf("%s step %q should fail (exit 1) when the Linux binary is dynamic", rel, step.Name)
	}
	// If linux-arm64 is added, ldd on a foreign-arch binary is not reliable.
	lower := strings.ToLower(run)
	if !strings.Contains(lower, "arm64") || !(strings.Contains(lower, "readelf") || strings.Contains(lower, "file or readelf")) {
		t.Errorf("%s step %q should comment that a future linux-arm64 check should use file or readelf rather than ldd", rel, step.Name)
	}
}

// TestBuildAndReleaseWorkflowsCheckLinuxBinaryIsStatic requires both
// build.yml and release.yml to contain a parsed step that runs file and
// ldd on truenas-mcp-linux-amd64 and fails when that binary is dynamic.
// Whole-file substring checks are not enough: "file" already appears on
// main as go-version-file.
func TestBuildAndReleaseWorkflowsCheckLinuxBinaryIsStatic(t *testing.T) {
	for _, rel := range []string{".github/workflows/build.yml", ".github/workflows/release.yml"} {
		src := issue114Read(t, rel)
		steps, err := linuxStaticCheckSteps(src)
		if err != nil {
			t.Fatalf("%s is not valid YAML: %v", rel, err)
		}
		if len(steps) != 1 {
			t.Errorf("%s: want exactly one Linux static-link check step, got %d", rel, len(steps))
			continue
		}
		assertLinuxStaticCheckStep(t, rel, steps[0])
	}
}

func TestLinuxStaticCheckStepFinder(t *testing.T) {
	t.Run("missing step", func(t *testing.T) {
		// Resembles main: go-version-file contains "file", but there is no
		// static-link check step.
		src := `
name: Build and Package
jobs:
  build:
    steps:
    - name: Set up Go
      uses: actions/setup-go@deadbeef
      with:
        go-version-file: go.mod
    - name: Build for Linux AMD64
      run: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o truenas-mcp-linux-amd64 ./cmd/truenas-mcp
`
		steps, err := linuxStaticCheckSteps(src)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(steps) != 0 {
			t.Fatalf("workflow missing the check step: got %+v", steps)
		}
	})

	t.Run("strings elsewhere are not a check step", func(t *testing.T) {
		src := `
name: Build
jobs:
  build:
    steps:
    - name: Set up Go
      with:
        go-version-file: go.mod
    - name: Notes
      run: |
        echo "statically linked"
        echo "not a dynamic executable"
        exit 1
`
		steps, err := linuxStaticCheckSteps(src)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(steps) != 0 {
			t.Fatalf("unrelated step must not count as the static-link check: got %+v", steps)
		}
	})

	t.Run("valid step is found", func(t *testing.T) {
		src := `
name: Release
jobs:
  release:
    steps:
    - name: Check Linux binary is statically linked
      run: |
        # If a linux-arm64 artifact is added, check it with file or
        # readelf -l (no INTERP) instead: ldd on a foreign-arch binary
        # is not reliable.
        bin=truenas-mcp-linux-amd64
        file_out=$(file "$bin")
        ldd_out=$(ldd "$bin" 2>&1 || true)
        if ! echo "$file_out" | grep -q 'statically linked'; then
          exit 1
        fi
        if ! echo "$ldd_out" | grep -qi 'not a dynamic executable'; then
          exit 1
        fi
`
		steps, err := linuxStaticCheckSteps(src)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(steps) != 1 {
			t.Fatalf("want 1 step, got %d", len(steps))
		}
		assertLinuxStaticCheckStep(t, "fixture", steps[0])
	})
}
