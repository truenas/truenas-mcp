package main

import (
	"bytes"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/truenas"
)

// Without a terminal the typed confirmations are never read, even when
// stdin would supply the right word.
func TestSetupNonTerminalGates(t *testing.T) {
	t.Run("insecure", func(t *testing.T) {
		f := newFakeSetupNAS(t, nil)
		r := runSetupTest(t, f, setupOpts{env: agentEnv(), noTrust: true, visible: []string{"insecure"}}, "--user", fakeUser, "--insecure")
		wantCode(t, r, exitSetupTLS)
		if n := len(f.snapshot()); n != 0 {
			t.Errorf("%d frames sent without a terminal confirmation", n)
		}
	})
	full := func(f *fakeSetupNAS) { f.roles = []string{"FULL_ADMIN"} }
	t.Run("full admin without a terminal", func(t *testing.T) {
		f := newFakeSetupNAS(t, full)
		r := runSetupTest(t, f, setupOpts{env: agentEnv(), visible: []string{fakeUser}}, "--user", fakeUser)
		wantCode(t, r, exitSetupPermission)
		if f.count("api_key.create") != 0 {
			t.Error("full-admin key created without a terminal confirmation")
		}
	})
	t.Run("full admin after stdin was used", func(t *testing.T) {
		f := newFakeSetupNAS(t, full)
		r := runSetupTest(t, f, setupOpts{tty: true, stdin: fakePassword + "\n", visible: []string{fakeUser}}, "--user", fakeUser, "--password-stdin")
		wantCode(t, r, exitSetupPermission)
		if f.count("api_key.create") != 0 {
			t.Error("full-admin key created with stdin already consumed")
		}
	})
}

func writeHostKeyFile(t *testing.T, f *fakeSetupNAS, dir, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, strings.ReplaceAll(f.host(), ":", "_")+".key")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSetupRotateKeyFileMismatch(t *testing.T) {
	const other = "99-OTHER-KEY-MARKER\n"
	t.Run("different key is kept", func(t *testing.T) {
		f := newFakeSetupNAS(t, nil)
		f.addKey("truenas-mcp-test-laptop", fakeUser) // id 11
		dir := filepath.Join(t.TempDir(), "cfg")
		p := writeHostKeyFile(t, f, dir, other)
		r := runSetupTest(t, f, setupOpts{env: agentEnv(), dir: dir}, "--user", fakeUser, "--rotate")
		wantCode(t, r, exitSetupIO)
		if len(f.updates) != 0 || f.count("api_key.create") != 0 {
			t.Errorf("TrueNAS was changed: updates %v, calls %v", f.updates, f.methods())
		}
		if readFile(t, p) != other || !strings.Contains(r.stderr, "--force") {
			t.Errorf("file replaced or no hint:\n%s", r.stderr)
		}
		r = runSetupTest(t, f, setupOpts{env: agentEnv(), dir: dir}, "--user", fakeUser, "--rotate", "--force")
		wantCode(t, r, exitSetupOK)
	})
	t.Run("unreadable prefix is kept", func(t *testing.T) {
		f := newFakeSetupNAS(t, nil)
		f.addKey("truenas-mcp-test-laptop", fakeUser)
		dir := filepath.Join(t.TempDir(), "cfg")
		writeHostKeyFile(t, f, dir, "no-id-here\n")
		r := runSetupTest(t, f, setupOpts{env: agentEnv(), dir: dir}, "--user", fakeUser, "--rotate")
		wantCode(t, r, exitSetupIO)
	})
	t.Run("no key to rotate keeps the file", func(t *testing.T) {
		f := newFakeSetupNAS(t, nil)
		dir := filepath.Join(t.TempDir(), "cfg")
		p := writeHostKeyFile(t, f, dir, other)
		r := runSetupTest(t, f, setupOpts{env: agentEnv(), dir: dir}, "--user", fakeUser, "--rotate")
		wantCode(t, r, exitSetupIO)
		if f.count("api_key.create") != 0 || readFile(t, p) != other {
			t.Errorf("calls %v", f.methods())
		}
	})
	t.Run("symlinked key file is refused", func(t *testing.T) {
		f := newFakeSetupNAS(t, nil)
		k := f.addKey("truenas-mcp-test-laptop", fakeUser)
		dir := filepath.Join(t.TempDir(), "cfg")
		p := writeHostKeyFile(t, f, dir, "x\n")
		real := filepath.Join(dir, "real.key")
		content := k.secret + "\n"
		if err := os.WriteFile(real, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("real.key", p); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		r := runSetupTest(t, f, setupOpts{env: agentEnv(), dir: dir}, "--user", fakeUser, "--rotate")
		wantCode(t, r, exitSetupIO)
		if len(f.updates) != 0 || f.count("api_key.create") != 0 {
			t.Errorf("TrueNAS was changed: updates %v, calls %v", f.updates, f.methods())
		}
		if !strings.Contains(r.stderr, "symlink") || !strings.Contains(r.stderr, "replaces the link itself, not the file it points to") || !strings.Contains(r.stderr, "--force") {
			t.Errorf("stderr = %s", r.stderr)
		}
		if fi, err := os.Lstat(p); err != nil || fi.Mode()&os.ModeSymlink == 0 || readFile(t, real) != content {
			t.Errorf("link or target changed: %v", err)
		}
	})
	t.Run("matching key is rotated", func(t *testing.T) {
		f := newFakeSetupNAS(t, nil)
		k := f.addKey("truenas-mcp-test-laptop", fakeUser)
		dir := filepath.Join(t.TempDir(), "cfg")
		p := writeHostKeyFile(t, f, dir, k.secret+"\n")
		r := runSetupTest(t, f, setupOpts{env: agentEnv(), dir: dir}, "--user", fakeUser, "--rotate")
		wantCode(t, r, exitSetupOK)
		if got := f.keyByName(k.Name).secret; readFile(t, p) != got+"\n" {
			t.Error("key file does not hold the rotated key")
		}
	})
}

func TestKeyFileID(t *testing.T) {
	dir := t.TempDir()
	for content, want := range map[string]int{
		"11-abc\n": 11, "123456-x": 123456, "-1-x": 0, "abc": 0, "12": 0, "": 0,
		"99999999999999999999-x": 0,
		// At most 18 digits: 19 could overflow int.
		"999999999999999999-x":  999999999999999999,
		"9223372036854775818-x": 0,
		"1000000000000000000-x": 0,
	} {
		p := filepath.Join(dir, "k")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := keyFileID(p); got != want {
			t.Errorf("keyFileID(%q) = %d, want %d", content, got, want)
		}
	}
	if got := keyFileID(filepath.Join(dir, "missing")); got != 0 {
		t.Errorf("missing file: %d", got)
	}
	// Only a regular file is read, not a symlink to one.
	if err := os.WriteFile(filepath.Join(dir, "real"), []byte("12-abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "link")); err == nil {
		if got := keyFileID(filepath.Join(dir, "link")); got != 0 {
			t.Errorf("symlink: %d, want 0", got)
		}
	}
}

func TestSetupRotateSaveFailureSaysRecordDeleted(t *testing.T) {
	f := newFakeSetupNAS(t, nil)
	f.addKey("truenas-mcp-test-laptop", fakeUser)
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := runSetupTest(t, f, setupOpts{env: agentEnv(), dir: notDir}, "--user", fakeUser, "--rotate")
	wantCode(t, r, exitSetupIO)
	if len(f.keys) != 0 {
		t.Errorf("key record left: %+v", f.keys)
	}
	if !strings.Contains(r.stderr, "key record") || !strings.Contains(r.stderr, "rather than rotating") {
		t.Errorf("stderr: %s", r.stderr)
	}
}

func TestSetupLostKeyReplyNamesKey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    func(*fakeSetupNAS)
		rotate bool
		want   string
	}{
		{"create unreadable reply", func(f *fakeSetupNAS) { f.badKeyReply = true }, false, "may have been created"},
		{"create connection lost", func(f *fakeSetupNAS) { f.dropOn = "api_key.create" }, false, "may have been created"},
		{"reset unreadable reply", func(f *fakeSetupNAS) { f.badKeyReply = true }, true, "may have been reset"},
		{"reset connection lost", func(f *fakeSetupNAS) { f.dropOn = "api_key.update" }, true, "may have been reset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSetupNAS(t, tc.cfg)
			args := []string{"--user", fakeUser}
			if tc.rotate {
				f.addKey("truenas-mcp-test-laptop", fakeUser)
				args = append(args, "--rotate")
			}
			r := runSetupTest(t, f, setupOpts{env: agentEnv()}, args...)
			wantCode(t, r, exitSetupFailure)
			if !strings.Contains(r.stderr, tc.want) || !strings.Contains(r.stderr, `"truenas-mcp-test-laptop"`) {
				t.Errorf("stderr does not name the key:\n%s", r.stderr)
			}
		})
	}
	// A refusal from TrueNAS is definite, so no orphan warning.
	f := newFakeSetupNAS(t, func(f *fakeSetupNAS) { f.createErr = callErr(13, "EACCES", "denied") })
	r := runSetupTest(t, f, setupOpts{env: agentEnv()}, "--user", fakeUser)
	wantCode(t, r, exitSetupPermission)
	if strings.Contains(r.stderr, "may have been") {
		t.Errorf("refusal reported as uncertain: %s", r.stderr)
	}
}

func TestSetupCertReasonIsPrintable(t *testing.T) {
	ue := &truenas.UntrustedCertError{
		Leaf: &x509.Certificate{Raw: []byte{1, 2, 3}},
		Err:  errors.New("x509: certificate is valid for evil\x1b[2J\x07.example.com, not nas"),
	}
	s := &setup{io: &setupIO{stderr: &bytes.Buffer{}}}
	if msg := s.connectError(ue).msg; strings.ContainsAny(msg, "\x1b\x07") {
		t.Errorf("connectError kept control characters: %q", msg)
	}
	var w bytes.Buffer
	s.io.stderr = &w
	s.io.isTTY = false
	_ = s.confirmCertificate(&truenas.UntrustedCertError{Leaf: &x509.Certificate{Raw: []byte{1}}, Err: ue.Err})
	if strings.ContainsAny(w.String(), "\x1b\x07") {
		t.Errorf("confirmCertificate kept control characters: %q", w.String())
	}
}

func TestSetupEmptyStdinRefused(t *testing.T) {
	f := newFakeSetupNAS(t, nil)
	r := runSetupTest(t, f, setupOpts{stdin: "\n"}, "--user", fakeUser, "--password-stdin")
	wantCode(t, r, exitSetupAuth)
	if f.count("auth.login_ex") != 0 || !strings.Contains(r.stderr, "empty") {
		t.Errorf("calls %v\n%s", f.methods(), r.stderr)
	}

	f = newFakeSetupNAS(t, func(f *fakeSetupNAS) { f.otp = fakeOTP })
	r = runSetupTest(t, f, setupOpts{stdin: fakePassword + "\n\r\n"}, "--user", fakeUser, "--password-stdin", "--otp-stdin")
	wantCode(t, r, exitSetupOTP)
	if f.count("auth.login_ex_continue") != 0 || !strings.Contains(r.stderr, "empty") {
		t.Errorf("calls %v\n%s", f.methods(), r.stderr)
	}
}

func TestSetupBoolFlagValueNotEchoed(t *testing.T) {
	for _, args := range [][]string{
		{"--password-stdin=" + fakePassword},
		{"-password-stdin=" + fakePassword},
		{"--host", "192.0.2.10", "--otp-stdin=" + fakeOTP},
		{"--user", "u", "--otp-stdin=" + fakeOTP},
		{"--force=" + fakePassword},
		// Bad flag syntax: the flag package would echo the argument.
		{"---password-stdin=" + fakePassword},
		{"----password-stdin=" + fakePassword},
		{"--=" + fakePassword},
		{"-=" + fakePassword},
	} {
		r := runSetupTest(t, nil, setupOpts{}, args...)
		wantCode(t, r, exitSetupUsage)
		if strings.Contains(r.stderr, fakePassword) || strings.Contains(r.stderr, fakeOTP) {
			t.Errorf("args %q: the value was echoed: %s", args, r.stderr)
		}
	}
	// Real booleans still parse.
	var f setupFlags
	fs := newSetupFlagSet(&f, &bytes.Buffer{})
	if err := checkBoolFlagValues(fs, []string{"--password-stdin=true", "--otp-stdin=false", "--name", "-x=y", "--force"}); err != nil {
		t.Errorf("valid booleans refused: %s", err.msg)
	}
	if err := checkBoolFlagValues(fs, []string{"--", "--password-stdin=x"}); err != nil {
		t.Error("checked past --")
	}
}
