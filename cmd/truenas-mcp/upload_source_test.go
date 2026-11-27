package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestUploadSourceDirsSetting(t *testing.T) {
	sep := string(os.PathListSeparator)
	env := func(v string, set bool) func(string) (string, bool) {
		return func(name string) (string, bool) {
			if name == "TRUENAS_MCP_UPLOAD_SOURCE_DIRS" {
				return v, set
			}
			return "", false
		}
	}
	a, b := filepath.Join("/srv", "isos"), filepath.Join("/srv", "images")
	for _, tc := range []struct {
		name  string
		flags map[string]string
		env   func(string) (string, bool)
		want  []string
	}{
		{"unset", nil, env("", false), nil},
		{"env", nil, env(a+sep+" "+sep+b, true), []string{a, b}},
		{"flag wins", map[string]string{"upload-source-dirs": a}, env(b, true), []string{a}},
		{"empty flag disables", map[string]string{"upload-source-dirs": ""}, env(b, true), nil},
	} {
		if got := uploadSourceDirs(tc.flags, tc.env); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestProtectedSourcePaths(t *testing.T) {
	getenv := func(name string) string {
		return map[string]string{"TRUENAS_API_KEY_FILE": "/etc/keys/env.key", "TRUENAS_TLS_CA": "/etc/keys/env-ca.pem"}[name]
	}
	got := protectedSourcePaths("/etc/keys/flag.key", "/etc/keys/ca.pem", getenv, func() (string, error) { return "/home/me/cfg", nil })
	want := []string{"/etc/keys/flag.key", "/etc/keys/env.key", "/etc/keys/ca.pem", "/etc/keys/env-ca.pem", filepath.Join("/home/me/cfg", "truenas-mcp")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	got = protectedSourcePaths("", "", func(string) string { return "" }, func() (string, error) { return "", errors.New("no home") })
	if len(got) != 0 {
		t.Errorf("got %q", got)
	}
}

func TestUploadSourceDirsFlagRegistered(t *testing.T) {
	doc := repoFile(t, "docs/configuration.md")
	if !strings.Contains(doc, "TRUENAS_MCP_UPLOAD_SOURCE_DIRS") {
		t.Error("docs/configuration.md does not name TRUENAS_MCP_UPLOAD_SOURCE_DIRS")
	}
}
