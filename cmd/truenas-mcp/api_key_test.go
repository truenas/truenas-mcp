package main

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testAPIKeySecret = "1-fake-api-key-do-not-leak"

func writeKeyFile(t *testing.T, dir, name, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadAPIKeyFileMode0600(t *testing.T) {
	dir := t.TempDir()
	path := writeKeyFile(t, dir, "key", testAPIKeySecret+"\n", 0o600)
	got, err := readAPIKeyFile(path)
	if err != nil {
		t.Fatalf("0600 file rejected: %v", err)
	}
	if got != testAPIKeySecret {
		t.Errorf("key = %q; want %q (one trailing newline trimmed)", got, testAPIKeySecret)
	}
}

func TestReadAPIKeyFileTrimsOneTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ in, want string }{
		{testAPIKeySecret, testAPIKeySecret},
		{testAPIKeySecret + "\n", testAPIKeySecret},
		{testAPIKeySecret + "\r\n", testAPIKeySecret},
		{testAPIKeySecret + "\n\n", testAPIKeySecret + "\n"},
	} {
		path := writeKeyFile(t, dir, "key", tc.in, 0o600)
		got, err := readAPIKeyFile(path)
		if err != nil {
			t.Errorf("contents %q: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("contents %q: got %q, want %q", tc.in, got, tc.want)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadAPIKeyFileMode0644(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("group/other permission bits are not enforced on Windows")
	}
	dir := t.TempDir()
	path := writeKeyFile(t, dir, "key", testAPIKeySecret+"\n", 0o644)
	got, err := readAPIKeyFile(path)
	if err == nil {
		t.Fatalf("0644 file accepted, key %q", got)
	}
	msg := err.Error()
	if strings.Contains(msg, testAPIKeySecret) {
		t.Errorf("error includes file contents: %s", msg)
	}
	if !strings.Contains(msg, "readable by group or others") && !strings.Contains(msg, "chmod 600") {
		t.Errorf("error is not clear about permissions: %s", msg)
	}
}

func TestReadAPIKeyFileMissing(t *testing.T) {
	dir := t.TempDir()
	// A key file exists next to the missing path so a buggy error that
	// printed a nearby file's contents would be caught.
	writeKeyFile(t, dir, "other", testAPIKeySecret+"\n", 0o600)
	path := filepath.Join(dir, "missing")
	got, err := readAPIKeyFile(path)
	if err == nil {
		t.Fatalf("missing file accepted, key %q", got)
	}
	msg := err.Error()
	if strings.Contains(msg, testAPIKeySecret) {
		t.Errorf("error includes file contents: %s", msg)
	}
	if !strings.Contains(msg, "cannot read") && !strings.Contains(msg, "no such file") {
		t.Errorf("error does not say the file could not be read: %s", msg)
	}
}

func TestReadAPIKeyFileEmpty(t *testing.T) {
	dir := t.TempDir()
	path := writeKeyFile(t, dir, "key", "\n", 0o600)
	got, err := readAPIKeyFile(path)
	if err == nil {
		t.Fatalf("empty file accepted, key %q", got)
	}
	if strings.Contains(err.Error(), testAPIKeySecret) {
		t.Errorf("error includes file contents: %s", err)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error does not say the file is empty: %s", err)
	}
}

func TestReadAPIKeyFileRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	_, err := readAPIKeyFile(dir)
	if err == nil {
		t.Fatal("directory accepted as an API key file")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Errorf("error does not say it must be a regular file: %s", err)
	}
	if strings.Contains(err.Error(), testAPIKeySecret) {
		t.Errorf("error includes file contents: %s", err)
	}
}

func TestResolveAPIKeySources(t *testing.T) {
	dir := t.TempDir()
	path := writeKeyFile(t, dir, "key", testAPIKeySecret+"\n", 0o600)
	envOf := func(env map[string]string) func(string) string {
		return func(k string) string { return env[k] }
	}

	got, err := resolveAPIKey(testAPIKeySecret, "", envOf(nil))
	if err != nil || got != testAPIKeySecret {
		t.Errorf("--api-key: got %q, %v", got, err)
	}
	got, err = resolveAPIKey("", path, envOf(nil))
	if err != nil || got != testAPIKeySecret {
		t.Errorf("--api-key-file: got %q, %v", got, err)
	}
	got, err = resolveAPIKey(testAPIKeySecret, path, envOf(nil))
	if err == nil || !strings.Contains(err.Error(), "--api-key") || !strings.Contains(err.Error(), "--api-key-file") {
		t.Errorf("both flags: got %q, %v; want a conflict error", got, err)
	}

	got, err = resolveAPIKey("", "", envOf(map[string]string{"TRUENAS_API_KEY": testAPIKeySecret}))
	if err != nil || got != testAPIKeySecret {
		t.Errorf("TRUENAS_API_KEY: got %q, %v", got, err)
	}
	got, err = resolveAPIKey("", "", envOf(map[string]string{"TRUENAS_API_KEY_FILE": path}))
	if err != nil || got != testAPIKeySecret {
		t.Errorf("TRUENAS_API_KEY_FILE: got %q, %v", got, err)
	}
	got, err = resolveAPIKey("", "", envOf(map[string]string{
		"TRUENAS_API_KEY":      testAPIKeySecret,
		"TRUENAS_API_KEY_FILE": path,
	}))
	if err == nil || !strings.Contains(err.Error(), "TRUENAS_API_KEY") || !strings.Contains(err.Error(), "TRUENAS_API_KEY_FILE") {
		t.Errorf("both env vars: got %q, %v; want a conflict error", got, err)
	}

	// A set flag wins over the other source's environment variable.
	got, err = resolveAPIKey("from-flag", "", envOf(map[string]string{"TRUENAS_API_KEY_FILE": path}))
	if err != nil || got != "from-flag" {
		t.Errorf("--api-key should win over TRUENAS_API_KEY_FILE: got %q, %v", got, err)
	}
	got, err = resolveAPIKey("", path, envOf(map[string]string{"TRUENAS_API_KEY": "from-env"}))
	if err != nil || got != testAPIKeySecret {
		t.Errorf("--api-key-file should win over TRUENAS_API_KEY: got %q, %v", got, err)
	}

	got, err = resolveAPIKey("", "", envOf(nil))
	if err != nil || got != "" {
		t.Errorf("no source: got %q, %v; want empty, no error", got, err)
	}
}

func TestAPIKeyFileFlagRegistered(t *testing.T) {
	f := flag.Lookup("api-key-file")
	if f == nil {
		t.Fatal("flag --api-key-file is not registered")
	}
	if !strings.Contains(f.Usage, "TRUENAS_API_KEY_FILE") {
		t.Errorf("flag --api-key-file usage does not name its environment variable: %q", f.Usage)
	}
}

func TestExampleConfigHasNoAPIKeyFlag(t *testing.T) {
	s := repoFile(t, "claude_desktop_config.example.json")
	if strings.Contains(s, "--api-key") {
		t.Error("claude_desktop_config.example.json passes --api-key on the command line; put the key in TRUENAS_API_KEY instead")
	}
	if !strings.Contains(s, "TRUENAS_API_KEY") {
		t.Error("claude_desktop_config.example.json does not set TRUENAS_API_KEY")
	}
}

func TestREADMEDoesNotRecommendAdminKey(t *testing.T) {
	for _, doc := range markdownFiles(t) {
		text := repoFile(t, doc)
		if strings.Contains(text, "admin recommended") {
			t.Errorf("%s still says \"admin recommended\" for the API key", doc)
		}
		if strings.Contains(text, "has admin permissions") {
			t.Errorf("%s still tells the reader to give the API key admin permissions", doc)
		}
	}
}
