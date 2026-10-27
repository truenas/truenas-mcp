package main

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
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

// markdownFiles lists README.md and docs/*.md, relative to the repository root.
func markdownFiles(t *testing.T) []string {
	t.Helper()
	files := []string{"README.md"}
	entries, err := os.ReadDir(filepath.Join("..", "..", "docs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			files = append(files, "docs/"+e.Name())
		}
	}
	return files
}

// TestGoVersionMentionsMatchGoMod fails when any "Go X.Y[.Z]" mentioned in
// the README or docs differs from the go directive in go.mod, and when the
// README does not state the requirement at all.
func TestGoVersionMentionsMatchGoMod(t *testing.T) {
	m := regexp.MustCompile(`(?m)^go (\d+\.\d+(?:\.\d+)?)\s*$`).FindStringSubmatch(repoFile(t, "go.mod"))
	if m == nil {
		t.Fatal("go.mod has no go directive")
	}
	want := m[1]
	mention := regexp.MustCompile(`\bGo (\d+\.\d+(?:\.\d+)?)`)
	for _, doc := range markdownFiles(t) {
		for _, found := range mention.FindAllStringSubmatch(repoFile(t, doc), -1) {
			if found[1] != want {
				t.Errorf("%s mentions %q; go.mod requires go %s", doc, found[0], want)
			}
		}
	}
	if !strings.Contains(repoFile(t, "README.md"), "Go "+want+" or later") {
		t.Errorf("README.md does not say %q", "Go "+want+" or later")
	}
}

// headingAnchors returns the GitHub anchors of the headings in a markdown
// document: lower case, punctuation removed, spaces replaced by hyphens, and
// -1, -2, ... appended to repeated headings.
func headingAnchors(md string) map[string]bool {
	anchors := map[string]bool{}
	seen := map[string]int{}
	for _, line := range strings.Split(stripFences(md), "\n") {
		trimmed := strings.TrimLeft(line, "#")
		if len(trimmed) == len(line) || len(line)-len(trimmed) > 6 || !strings.HasPrefix(trimmed, " ") {
			continue
		}
		var b strings.Builder
		for _, r := range strings.ToLower(strings.TrimSpace(trimmed)) {
			switch {
			case r == ' ':
				b.WriteByte('-')
			case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r):
				b.WriteRune(r)
			}
		}
		slug := b.String()
		if n := seen[slug]; n > 0 {
			anchors[slug+"-"+strconv.Itoa(n)] = true
		} else {
			anchors[slug] = true
		}
		seen[slug]++
	}
	return anchors
}

// TestHeadingAnchors pins the anchor rules, including the emoji variation
// selector (U+FE0F), which GitHub keeps in the anchor while it drops the emoji.
func TestHeadingAnchors(t *testing.T) {
	md := "# Title\n## Step 1: Download or Build Binary\n## \u26a0\ufe0f Security Requirement\n## Repeat\n## Repeat\n```\n# not a heading\n```\n"
	got := headingAnchors(md)
	for _, want := range []string{
		"title", "step-1-download-or-build-binary", "\ufe0f-security-requirement", "repeat", "repeat-1",
	} {
		if !got[want] {
			t.Errorf("missing anchor %q in %v", want, got)
		}
	}
	if got["not-a-heading"] || got["-security-requirement"] {
		t.Errorf("unexpected anchors in %v", got)
	}
}

// TestMarkdownLinksResolve checks that relative links in the README and docs/
// point to files that exist, and that #anchors point to headings.
func TestMarkdownLinksResolve(t *testing.T) {
	link := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	for _, doc := range markdownFiles(t) {
		dir := filepath.Dir(filepath.FromSlash(doc))
		for _, m := range link.FindAllStringSubmatch(stripFences(repoFile(t, doc)), -1) {
			target, anchor := m[1], ""
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if i := strings.IndexByte(target, '#'); i >= 0 {
				target, anchor = target[:i], target[i+1:]
			}
			file := doc
			if target != "" {
				file = filepath.ToSlash(filepath.Join(dir, filepath.FromSlash(target)))
				if _, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(file))); err != nil {
					t.Errorf("%s links to %q, which does not exist", doc, m[1])
					continue
				}
			}
			if anchor == "" || !strings.HasSuffix(file, ".md") {
				continue
			}
			if !headingAnchors(repoFile(t, file))[anchor] {
				t.Errorf("%s links to %q, but %s has no heading with the anchor #%s", doc, m[1], file, anchor)
			}
		}
	}
}
