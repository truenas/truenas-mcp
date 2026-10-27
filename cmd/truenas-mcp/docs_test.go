package main

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoFile reads a file relative to the repository root. Tests in this
// package run with the package directory as the working directory.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// stripFences removes fenced code blocks, so examples are not parsed as docs.
func stripFences(md string) string {
	var out []string
	in := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			in = !in
			continue
		}
		if !in {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// TestREADMEFlagsMatchBinary keeps the README flag list and the flags the
// binary registers in step, in both directions.
func TestREADMEFlagsMatchBinary(t *testing.T) {
	readme := stripFences(repoFile(t, "README.md"))
	start := strings.Index(readme, "### Flags")
	if start < 0 {
		t.Fatal("README.md has no '### Flags' section")
	}
	section := readme[start+len("### Flags"):]
	if end := strings.Index(section, "\n#"); end >= 0 {
		section = section[:end]
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^- `--([a-z][a-z0-9-]*)`").FindAllStringSubmatch(section, -1) {
		documented[m[1]] = true
	}
	if len(documented) == 0 {
		t.Fatal("no flags found in the README '### Flags' section")
	}

	registered := map[string]bool{}
	flag.CommandLine.VisitAll(func(f *flag.Flag) {
		// The test binary registers its own test.* flags.
		if !strings.HasPrefix(f.Name, "test.") {
			registered[f.Name] = true
		}
	})

	var problems []string
	for name := range documented {
		if !registered[name] {
			problems = append(problems, "README documents --"+name+", which the binary does not register")
		}
	}
	for name := range registered {
		if !documented[name] {
			problems = append(problems, "the binary registers --"+name+", which the README does not document")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// TestREADMEGoVersionMatchesGoMod fails when the Go version the README asks
// for differs from the go directive in go.mod.
func TestREADMEGoVersionMatchesGoMod(t *testing.T) {
	m := regexp.MustCompile(`(?m)^go (\d+\.\d+(?:\.\d+)?)\s*$`).FindStringSubmatch(repoFile(t, "go.mod"))
	if m == nil {
		t.Fatal("go.mod has no go directive")
	}
	want := "Go " + m[1] + " or later"
	if !strings.Contains(repoFile(t, "README.md"), want) {
		t.Errorf("README.md does not say %q; go.mod requires go %s", want, m[1])
	}
}

// TestMarkdownRelativeLinksExist checks that relative links in the README and
// docs/ point to files that exist.
func TestMarkdownRelativeLinksExist(t *testing.T) {
	docs := []string{"README.md"}
	entries, err := os.ReadDir(filepath.Join("..", "..", "docs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			docs = append(docs, "docs/"+e.Name())
		}
	}
	link := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	for _, doc := range docs {
		dir := filepath.Dir(filepath.FromSlash(doc))
		for _, m := range link.FindAllStringSubmatch(stripFences(repoFile(t, doc)), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if i := strings.IndexByte(target, '#'); i >= 0 {
				target = target[:i]
			}
			if _, err := os.Stat(filepath.Join("..", "..", dir, filepath.FromSlash(target))); err != nil {
				t.Errorf("%s links to %q, which does not exist", doc, m[1])
			}
		}
	}
}
