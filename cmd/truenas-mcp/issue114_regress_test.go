package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func issue114Read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// testInvocations returns non-comment lines that run the test suite
// (go test or make test), ignoring go tool cover.
func testInvocations(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.Contains(trim, "go tool cover") {
			continue
		}
		if strings.Contains(trim, "go test") || strings.Contains(trim, "make test") {
			out = append(out, trim)
		}
	}
	return out
}

func makefileTestRecipe(t *testing.T, makefile string) string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	var b strings.Builder
	in := false
	for _, line := range lines {
		if strings.HasPrefix(line, "test:") {
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
		t.Fatal("Makefile has no test target")
	}
	return got
}

// The Build and Package workflow must run the suite once, with the race
// detector and a coverage profile in atomic mode, instead of a plain test
// run plus a separate coverage (or race) rerun.
func TestBuildWorkflowRunsTestsOnceWithRaceAndCoverage(t *testing.T) {
	src := issue114Read(t, ".github/workflows/build.yml")
	inv := testInvocations(src)
	if len(inv) != 1 {
		t.Fatalf("build.yml test invocations = %q, want exactly one go test that includes -race and coverage", inv)
	}
	got := inv[0]
	for _, want := range []string{"go test", "-race", "-coverprofile=", "-covermode=atomic"} {
		if !strings.Contains(got, want) {
			t.Errorf("build.yml test step %q does not include %s", got, want)
		}
	}
	if strings.Contains(got, "make test") {
		t.Errorf("build.yml should run the combined go test directly so coverage flags stay in CI: %q", got)
	}
}

// The release workflow must run tests with the race detector and attest
// the release archives with actions/attest-build-provenance, pinned to a
// full commit SHA, with the permissions that action needs.
func TestReleaseWorkflowRunsRaceAndAttestsProvenance(t *testing.T) {
	src := issue114Read(t, ".github/workflows/release.yml")
	makefile := issue114Read(t, "Makefile")
	inv := testInvocations(src)
	if len(inv) == 0 {
		t.Fatal("release.yml does not run tests")
	}
	hasRace := false
	for _, line := range inv {
		if strings.Contains(line, "-race") {
			hasRace = true
		}
		if strings.Contains(line, "make test") && strings.Contains(makefileTestRecipe(t, makefile), "-race") {
			hasRace = true
		}
	}
	if !hasRace {
		t.Errorf("release.yml test steps = %q, want -race (directly or via make test)", inv)
	}

	pin := regexp.MustCompile(`uses:\s+actions/attest-build-provenance@([0-9a-f]{40})\b`)
	m := pin.FindStringSubmatch(src)
	if m == nil {
		t.Error("release.yml does not pin actions/attest-build-provenance to a 40-character commit SHA")
	}

	for _, perm := range []string{"id-token: write", "attestations: write"} {
		if !strings.Contains(src, perm) {
			t.Errorf("release.yml is missing the %q permission required to mint attestations", perm)
		}
	}
	for _, artifact := range []string{
		"truenas-mcp-darwin-arm64.tar.gz",
		"truenas-mcp-linux-amd64.tar.gz",
		"truenas-mcp-windows-amd64.zip",
	} {
		if !strings.Contains(src, artifact) {
			t.Errorf("release.yml does not mention release artifact %s", artifact)
		}
	}
	// The attestation step must name those archives as subjects (path or checksums).
	attestIdx := strings.Index(src, "actions/attest-build-provenance@")
	if attestIdx >= 0 {
		// Look at a window after the uses: line for subject inputs.
		window := src[attestIdx:]
		if i := strings.Index(window, "\n    - "); i > 0 {
			window = window[:i]
		}
		if !strings.Contains(window, "subject-path") && !strings.Contains(window, "subject-checksums") {
			t.Error("the attest-build-provenance step has no subject-path or subject-checksums input")
		}
	}
}

// make test must enable the race detector, so a data race fails that target.
// This test does not change the lint target (that is issue #113 / PR #150).
func TestMakefileTestRunsRace(t *testing.T) {
	recipe := makefileTestRecipe(t, issue114Read(t, "Makefile"))
	if !strings.Contains(recipe, "go test") || !strings.Contains(recipe, "-race") {
		t.Errorf("Makefile test target = %q, want go test with -race", recipe)
	}
}
