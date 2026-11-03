package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	runtimedebug "runtime/debug"
	"strings"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	info := func(v string) *runtimedebug.BuildInfo {
		return &runtimedebug.BuildInfo{Main: runtimedebug.Module{Path: "github.com/truenas/truenas-mcp", Version: v}}
	}
	for _, c := range []struct {
		injected string
		info     *runtimedebug.BuildInfo
		ok       bool
		want     string
	}{
		{"0.0.8", info("v0.0.7"), true, "0.0.8"},                         // -ldflags -X wins
		{"0.0.7-3-gabc1234-dirty", nil, false, "0.0.7-3-gabc1234-dirty"}, // git describe output
		{"dev", info("v0.0.7"), true, "0.0.7"},                           // go install ...@v0.0.7
		{"", info("v0.0.7+dirty"), true, "0.0.7+dirty"},                  // empty -X (git describe failed)
		{"dev", info("(devel)"), true, "dev"},
		{"dev", info(""), true, "dev"},
		{"dev", nil, false, "dev"},
		{"", nil, false, "dev"},
	} {
		if got := resolveVersion(c.injected, c.info, c.ok); got != c.want {
			t.Errorf("resolveVersion(%q, %v) = %q, want %q", c.injected, c.info, got, c.want)
		}
	}
}

// serverInfo.version in the initialize result is the Version variable.
func TestInitializeReportsVersion(t *testing.T) {
	old := Version
	Version = "1.2.3-test"
	t.Cleanup(func() { Version = old })
	var stdout bytes.Buffer
	in := rpcLine(t, map[string]interface{}{"id": 1, "method": "initialize"})
	h := newStdioHandler(fakeRegistry{}, false, strings.NewReader(in), &stdout)
	if err := h.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := checkOnlyJSONRPC(t, stdout.Bytes())
	if len(msgs) != 1 {
		t.Fatalf("responses = %v", msgs)
	}
	res, _ := msgs[0]["result"].(map[string]interface{})
	si, _ := res["serverInfo"].(map[string]interface{})
	if si["name"] != "truenas-mcp" || si["version"] != "1.2.3-test" {
		t.Errorf("serverInfo = %v, want version 1.2.3-test", si)
	}
}

// A binary built with -ldflags "-X main.Version=..." reports that version.
func TestLdflagsSetVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	bin := filepath.Join(t.TempDir(), "truenas-mcp")
	build := exec.Command("go", "build", "-ldflags", "-X main.Version=9.8.7-ldflags", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("--version: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "truenas-mcp version 9.8.7-ldflags" {
		t.Errorf("--version = %q", got)
	}
}

// The Makefile and the build and release workflows inject main.Version, so
// a rename of the variable cannot silently leave release builds at "dev".
func TestBuildsInjectVersion(t *testing.T) {
	for _, f := range []string{"Makefile", ".github/workflows/build.yml", ".github/workflows/release.yml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", f))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if !strings.Contains(s, "-X main.Version=$") {
			t.Errorf("%s does not set -X main.Version", f)
		}
		for _, line := range strings.Split(s, "\n") {
			injects := strings.Contains(line, "-X main.Version=") || strings.Contains(line, "$(LDFLAGS)")
			if strings.Contains(line, "go build") && !injects {
				t.Errorf("%s: build without version: %s", f, strings.TrimSpace(line))
			}
		}
	}
	mk, _ := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if !strings.Contains(string(mk), "git describe --tags --always --dirty") {
		t.Error("Makefile does not take VERSION from git describe")
	}
}
