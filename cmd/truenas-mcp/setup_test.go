package main

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/truenas"
)

type setupResult struct {
	code           int
	stdout, stderr string
	dir            string // config directory
	env            map[string]string
}

type setupOpts struct {
	tty     bool
	hidden  []string // answers to hidden prompts, in order
	visible []string // answers to visible prompts, in order
	stdin   string
	env     map[string]string
	dir     string // config dir; default a fresh temp dir
	noTrust bool   // do not add --trust-fingerprint for the fake's certificate
	prompts *[]string
}

var setupTestNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// runSetupTest runs setup against f with the fake's certificate pinned
// (unless opts.noTrust) and --debug on, so debug output is checked too.
func runSetupTest(t *testing.T, f *fakeSetupNAS, opts setupOpts, args ...string) setupResult {
	t.Helper()
	dir := opts.dir
	if dir == "" {
		dir = filepath.Join(t.TempDir(), "cfg")
	}
	env := map[string]string{}
	for k, v := range opts.env {
		env[k] = v
	}
	var stdout, stderr bytes.Buffer
	hidden, visible := opts.hidden, opts.visible
	sio := &setupIO{
		stdin:  strings.NewReader(opts.stdin),
		stdout: &stdout,
		stderr: &stderr,
		isTTY:  opts.tty,
		readHidden: func(prompt string) ([]byte, error) {
			stderr.WriteString(prompt + "\n")
			if opts.prompts != nil {
				*opts.prompts = append(*opts.prompts, prompt)
			}
			if len(hidden) == 0 {
				return nil, errors.New("no more input")
			}
			v := hidden[0]
			hidden = hidden[1:]
			return []byte(v), nil
		},
		readVisible: func(prompt string) (string, error) {
			stderr.WriteString(prompt + "\n")
			if opts.prompts != nil {
				*opts.prompts = append(*opts.prompts, prompt)
			}
			if len(visible) == 0 {
				return "", errors.New("no more input")
			}
			v := visible[0]
			visible = visible[1:]
			return v, nil
		},
		lookupEnv:     func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		unsetenv:      func(k string) error { delete(env, k); return nil },
		hostname:      func() (string, error) { return "Test-Laptop", nil },
		userConfigDir: func() (string, error) { return filepath.Join(dir, "unused"), nil },
		executable:    func() (string, error) { return "/usr/local/bin/truenas-mcp", nil },
		now:           func() time.Time { return setupTestNow },
	}
	full := []string{"--config-dir", dir, "--debug"}
	if f != nil {
		full = append(full, "--host", f.host())
		if !opts.noTrust {
			full = append(full, "--trust-fingerprint", truenas.Fingerprint(f.Certificate().Raw))
		}
	}
	full = append(full, args...)
	code := runSetup(full, sio)
	return setupResult{code: code, stdout: stdout.String(), stderr: stderr.String(), dir: dir, env: env}
}

func (r setupResult) keyPath(f *fakeSetupNAS) string {
	return filepath.Join(r.dir, strings.ReplaceAll(f.host(), ":", "_")+".key")
}

func (r setupResult) pinPath(f *fakeSetupNAS) string {
	return filepath.Join(r.dir, strings.ReplaceAll(f.host(), ":", "_")+".pin")
}

func wantCode(t *testing.T, r setupResult, code int) {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit code %d, want %d\nstderr:\n%s\nstdout:\n%s", r.code, code, r.stderr, r.stdout)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func agentEnv() map[string]string {
	return map[string]string{setupPasswordEnv: fakePassword}
}

func TestSetupSuccess(t *testing.T) {
	f := newFakeSetupNAS(t, nil)
	r := runSetupTest(t, f, setupOpts{env: agentEnv()}, "--user", fakeUser)
	wantCode(t, r, exitSetupOK)

	k := f.keyByName("truenas-mcp-test-laptop")
	if k == nil {
		t.Fatalf("no key named truenas-mcp-test-laptop; keys: %+v", f.keys)
	}
	if got := readFile(t, r.keyPath(f)); got != k.secret+"\n" {
		t.Errorf("key file holds %q, want the new key", got)
	}
	want := []string{"auth.login_ex", "system.info", "api_key.query", "api_key.create", "auth.logout", "auth.login_ex", "system.info"}
	if got := f.methods(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("calls %v, want %v", got, want)
	}
	frames := f.snapshot()
	var login struct {
		Params []map[string]interface{} `json:"params"`
	}
	_ = json.Unmarshal([]byte(frames[0]), &login)
	p := login.Params[0]
	if p["mechanism"] != "PASSWORD_PLAIN" || p["username"] != fakeUser || p["login_options"].(map[string]interface{})["user_info"] != true {
		t.Errorf("login params %v", p)
	}
	// expires_at defaults to one year, sent as {"$date": ms}.
	exp := f.creates[0]["expires_at"].(map[string]interface{})["$date"].(float64)
	if int64(exp) != setupTestNow.Add(365*24*time.Hour).UnixMilli() {
		t.Errorf("expires_at $date = %v", exp)
	}
	if f.creates[0]["username"] != fakeUser {
		t.Errorf("key owner %v, want the login user", f.creates[0]["username"])
	}
	for _, want := range []string{r.keyPath(f), `"truenas-mcp-test-laptop"`, "id 11", "To revoke it", "Verified"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("summary lacks %q:\n%s", want, r.stderr)
		}
	}
	var snippet struct {
		Command string            `json:"command"`
		Env     map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &snippet); err != nil {
		t.Fatalf("stdout is not the JSON snippet: %v\n%s", err, r.stdout)
	}
	if snippet.Env["TRUENAS_API_KEY_FILE"] != r.keyPath(f) || snippet.Env["TRUENAS_API_USER"] != fakeUser ||
		snippet.Env["TRUENAS_URL"] != f.host() || snippet.Env["TRUENAS_TLS_PIN"] != truenas.Fingerprint(f.Certificate().Raw) {
		t.Errorf("snippet env %v", snippet.Env)
	}
	if _, ok := snippet.Env["TRUENAS_API_KEY"]; ok {
		t.Error("snippet contains TRUENAS_API_KEY")
	}
	if strings.Contains(r.stdout+r.stderr, k.secret) {
		t.Error("the key was printed")
	}
}

func TestSetupFileModesExact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permission bits")
	}
	f := newFakeSetupNAS(t, nil)
	r := runSetupTest(t, f, setupOpts{env: agentEnv()}, "--user", fakeUser)
	wantCode(t, r, exitSetupOK)
	for path, want := range map[string]fs.FileMode{r.dir: 0o700, r.keyPath(f): 0o600, r.pinPath(f): 0o600} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != want {
			t.Errorf("%s mode %04o, want %04o", path, st.Mode().Perm(), want)
		}
	}
	entries, _ := os.ReadDir(r.dir)
	if len(entries) != 2 {
		t.Errorf("config dir holds %d entries, want the key and pin only", len(entries))
	}
}

func TestSetupRefusesOverwriteWithoutForce(t *testing.T) {
	f := newFakeSetupNAS(t, nil)
	dir := filepath.Join(t.TempDir(), "cfg")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, strings.ReplaceAll(f.host(), ":", "_")+".key")
	if err := os.WriteFile(keyPath, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runSetupTest(t, f, setupOpts{env: agentEnv(), dir: dir}, "--user", fakeUser)
	wantCode(t, r, exitSetupIO)
	if n := len(f.snapshot()); n != 0 {
		t.Errorf("%d frames sent; an existing key file must stop setup before login", n)
	}
	if readFile(t, keyPath) != "old\n" {
		t.Error("existing key file changed")
	}

	r = runSetupTest(t, f, setupOpts{env: agentEnv(), dir: dir}, "--user", fakeUser, "--force")
	wantCode(t, r, exitSetupOK)
	if readFile(t, keyPath) == "old\n" {
		t.Error("--force did not replace the key file")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.key")
	if err := writeFileAtomic(path, []byte("one\n"), false); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("two\n"), false); !errors.Is(err, errFileExists) {
		t.Fatalf("second write without overwrite: %v, want errFileExists", err)
	}
	// A failing rename leaves the old file whole and no temporary file.
	orig := renameReplace
	renameReplace = func(string, string) error { return errors.New("injected rename failure") }
	err := writeFileAtomic(path, []byte("three\n"), true)
	renameReplace = orig
	if err == nil {
		t.Fatal("injected rename failure not reported")
	}
	if got := readFile(t, path); got != "one\n" {
		t.Errorf("file holds %q after a failed replace", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
	if err := writeFileAtomic(path, []byte("four\n"), true); err != nil || readFile(t, path) != "four\n" {
		t.Errorf("overwrite: %v", err)
	}
}

func TestSetupSaveFailureDeletesKey(t *testing.T) {
	f := newFakeSetupNAS(t, nil)
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := runSetupTest(t, f, setupOpts{env: agentEnv(), dir: notDir}, "--user", fakeUser)
	wantCode(t, r, exitSetupIO)
	if len(f.deleted) != 1 || f.deleted[0] != 11 {
		t.Errorf("deleted %v, want the new key 11", f.deleted)
	}
	if len(f.keys) != 0 {
		t.Errorf("keys left on the NAS: %+v", f.keys)
	}
	if !strings.Contains(r.stderr, "was deleted from TrueNAS") {
		t.Errorf("stderr: %s", r.stderr)
	}
}

func TestSetupVerificationFailure(t *testing.T) {
	f := newFakeSetupNAS(t, func(f *fakeSetupNAS) { f.keyLoginBad = true })
	r := runSetupTest(t, f, setupOpts{env: agentEnv()}, "--user", fakeUser)
	wantCode(t, r, exitSetupVerify)
	if !strings.Contains(r.stderr, "could not log in") || !strings.Contains(r.stderr, r.keyPath(f)) {
		t.Errorf("stderr: %s", r.stderr)
	}
	if r.stdout != "" {
		t.Errorf("snippet printed after a failed verification: %s", r.stdout)
	}
}

func TestSetupOTP(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		f := newFakeSetupNAS(t, func(f *fakeSetupNAS) { f.otp = fakeOTP })
		env := agentEnv()
		env[setupOTPEnv] = fakeOTP
		r := runSetupTest(t, f, setupOpts{env: env}, "--user", fakeUser)
		wantCode(t, r, exitSetupOK)
		if _, ok := r.env[setupOTPEnv]; ok {
			t.Error("TRUENAS_SETUP_OTP still set")
		}
		if f.count("auth.login_ex_continue") != 1 {
			t.Errorf("calls %v", f.methods())
		}
	})
	t.Run("wrong then right on a terminal", func(t *testing.T) {
		f := newFakeSetupNAS(t, func(f *fakeSetupNAS) { f.otp = fakeOTP })
		r := runSetupTest(t, f, setupOpts{tty: true, hidden: []string{fakePassword, "000000", fakeOTP}}, "--user", fakeUser)
		wantCode(t, r, exitSetupOK)
		if f.count("auth.login_ex_continue") != 2 || !strings.Contains(r.stderr, "rejected") {
			t.Errorf("calls %v\n%s", f.methods(), r.stderr)
		}
	})
	t.Run("three wrong codes", func(t *testing.T) {
		f := newFakeSetupNAS(t, func(f *fakeSetupNAS) { f.otp = fakeOTP })
		r := runSetupTest(t, f, setupOpts{tty: true, hidden: []string{fakePassword, "1", "2", "3", "4"}}, "--user", fakeUser)
		wantCode(t, r, exitSetupOTP)
		if n := f.count("auth.login_ex_continue"); n != 3 {
			t.Errorf("%d OTP attempts, want 3", n)
		}
	})
	t.Run("rejected env code is not retried", func(t *testing.T) {
		f := newFakeSetupNAS(t, func(f *fakeSetupNAS) { f.otp = fakeOTP })
		env := agentEnv()
		env[setupOTPEnv] = "999999"
		r := runSetupTest(t, f, setupOpts{env: env}, "--user", fakeUser)
		wantCode(t, r, exitSetupOTP)
		if n := f.count("auth.login_ex_continue"); n != 1 {
			t.Errorf("%d OTP attempts, want 1", n)
		}
	})
	t.Run("no source", func(t *testing.T) {
		f := newFakeSetupNAS(t, func(f *fakeSetupNAS) { f.otp = fakeOTP })
		r := runSetupTest(t, f, setupOpts{env: agentEnv()}, "--user", fakeUser)
		wantCode(t, r, exitSetupOTP)
		if !strings.Contains(r.stderr, setupOTPEnv) || !strings.Contains(r.stderr, "--otp-stdin") {
			t.Errorf("stderr: %s", r.stderr)
		}
	})
}

func TestSetupStdinPaths(t *testing.T) {
	f := newFakeSetupNAS(t, func(f *fakeSetupNAS) { f.otp = fakeOTP })
	r := runSetupTest(t, f, setupOpts{stdin: fakePassword + "\r\n" + fakeOTP + "\n"}, "--user", fakeUser, "--password-stdin", "--otp-stdin")
	wantCode(t, r, exitSetupOK)

	// --otp-stdin alone, with the password from the environment.
	f2 := newFakeSetupNAS(t, func(f *fakeSetupNAS) { f.otp = fakeOTP })
	r = runSetupTest(t, f2, setupOpts{env: agentEnv(), stdin: fakeOTP}, "--user", fakeUser, "--otp-stdin")
	wantCode(t, r, exitSetupOK)
}

func TestSetupEnvIsReadAndUnset(t *testing.T) {
	f := newFakeSetupNAS(t, nil)
	env := agentEnv()
	env[setupOTPEnv] = "unused"
	r := runSetupTest(t, f, setupOpts{env: env}, "--user", fakeUser)
	wantCode(t, r, exitSetupOK)
	if len(r.env) != 0 {
		t.Errorf("environment still holds %v", r.env)
	}
	// Unset even when setup fails on its first flag.
	r = runSetupTest(t, nil, setupOpts{env: agentEnv()}, "--bogus")
	wantCode(t, r, exitSetupUsage)
	if _, ok := r.env[setupPasswordEnv]; ok {
		t.Error("TRUENAS_SETUP_PASSWORD still set after a usage error")
	}
}

func TestSetupHasNoPasswordFlag(t *testing.T) {
	var f setupFlags
	fset := newSetupFlagSet(&f, &bytes.Buffer{})
	fset.VisitAll(func(fl *flag.Flag) {
		n := strings.ToLower(fl.Name)
		if n == "password-stdin" || n == "otp-stdin" {
			return
		}
		for _, bad := range []string{"pass", "pw", "otp", "secret", "token", "key"} {
			if strings.Contains(n, bad) && n != "key-owner" {
				t.Errorf("setup registers --%s, which could carry a secret on the command line", fl.Name)
			}
		}
	})
	for _, args := range [][]string{
		{"--password", fakePassword},
		{"--password=" + fakePassword},
		{"--otp", fakeOTP},
		{"--host", "192.0.2.10", "--user", "u", fakePassword},
	} {
		r := runSetupTest(t, nil, setupOpts{}, args...)
		wantCode(t, r, exitSetupUsage)
		if strings.Contains(r.stderr, fakePassword) || strings.Contains(r.stderr, fakeOTP) {
			t.Errorf("args %q: the value was echoed: %s", args, r.stderr)
		}
	}
}

func TestSetupAuthErr(t *testing.T) {
	t.Run("not retried without a terminal", func(t *testing.T) {
		f := newFakeSetupNAS(t, nil)
		r := runSetupTest(t, f, setupOpts{env: map[string]string{setupPasswordEnv: "wrong"}}, "--user", fakeUser)
		wantCode(t, r, exitSetupAuth)
		if n := f.count("auth.login_ex"); n != 1 {
			t.Errorf("%d logins, want 1", n)
		}
	})
	t.Run("one re-prompt on a terminal", func(t *testing.T) {
		f := newFakeSetupNAS(t, nil)
		r := runSetupTest(t, f, setupOpts{tty: true, hidden: []string{"wrong", fakePassword}}, "--user", fakeUser)
		wantCode(t, r, exitSetupOK)
		f2 := newFakeSetupNAS(t, nil)
		r = runSetupTest(t, f2, setupOpts{tty: true, hidden: []string{"wrong", "wrong2", fakePassword}}, "--user", fakeUser)
		wantCode(t, r, exitSetupAuth)
		if n := f2.count("auth.login_ex"); n != 2 {
			t.Errorf("%d logins, want 2", n)
		}
	})
}

func TestSetupLoginOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  func(*fakeSetupNAS)
		code int
		want string
	}{
		{"expired", func(f *fakeSetupNAS) { f.loginType = "EXPIRED" }, exitSetupAuth, "web UI"},
		{"redirect", func(f *fakeSetupNAS) { f.loginType = "REDIRECT" }, exitSetupAuth, "active controller"},
		{"mechanism refused", func(f *fakeSetupNAS) {
			f.loginErr = callErr(95, "EOPNOTSUPP", "PASSWORD_PLAIN: mechanism is not supported at current authenticator level.")
		}, exitSetupAuth, "STIG"},
		{"stig create", func(f *fakeSetupNAS) {
			f.createErr = callErr(13, "EACCES", "Changes to API keys are not permitted in GPOS STIG mode")
		}, exitSetupPermission, "GPOS STIG mode"},
		{"privilege", func(f *fakeSetupNAS) {
			f.createErr = callErr(13, "EACCES", "mcpadmin: authenticated user lacks privileges to create or modify API keys of other users.")
		}, exitSetupPermission, "refused to create"},
		{"old version", func(f *fakeSetupNAS) { f.version = "25.04.2" }, exitSetupVersion, "25.10 or later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSetupNAS(t, tc.cfg)
			r := runSetupTest(t, f, setupOpts{env: agentEnv()}, "--user", fakeUser)
			wantCode(t, r, tc.code)
			if !strings.Contains(r.stderr, tc.want) {
				t.Errorf("stderr lacks %q:\n%s", tc.want, r.stderr)
			}
			if f.count("api_key.create") > 0 && len(f.keys) > 0 {
				t.Errorf("a key was created: %+v", f.keys)
			}
		})
	}
}

func TestSetupFullAdmin(t *testing.T) {
	full := func(f *fakeSetupNAS) { f.roles = []string{"FULL_ADMIN"} }
	f := newFakeSetupNAS(t, full)
	r := runSetupTest(t, f, setupOpts{env: agentEnv()}, "--user", fakeUser)
	wantCode(t, r, exitSetupPermission)
	if f.count("api_key.create") != 0 || !strings.Contains(r.stderr, "--key-owner") {
		t.Errorf("calls %v\n%s", f.methods(), r.stderr)
	}

	f = newFakeSetupNAS(t, full)
	r = runSetupTest(t, f, setupOpts{env: agentEnv()}, "--user", fakeUser, "--yes-full-admin")
	wantCode(t, r, exitSetupOK)

	f = newFakeSetupNAS(t, full)
	r = runSetupTest(t, f, setupOpts{tty: true, hidden: []string{fakePassword}, visible: []string{fakeUser}}, "--user", fakeUser)
	wantCode(t, r, exitSetupOK)

	f = newFakeSetupNAS(t, full)
	r = runSetupTest(t, f, setupOpts{tty: true, hidden: []string{fakePassword}, visible: []string{"yes"}}, "--user", fakeUser)
	wantCode(t, r, exitSetupPermission)
}

func TestSetupKeyOwner(t *testing.T) {
	f := newFakeSetupNAS(t, func(f *fakeSetupNAS) {
		f.roles = []string{"FULL_ADMIN"}
		f.users["mcp-bot"] = []string{"READONLY_ADMIN"}
	})
	r := runSetupTest(t, f, setupOpts{env: agentEnv()}, "--user", fakeUser, "--key-owner", "mcp-bot")
	wantCode(t, r, exitSetupOK)
	if f.creates[0]["username"] != "mcp-bot" {
		t.Errorf("owner %v", f.creates[0]["username"])
	}
	if !strings.Contains(r.stdout, `"TRUENAS_API_USER": "mcp-bot"`) {
		t.Errorf("snippet: %s", r.stdout)
	}

	f = newFakeSetupNAS(t, nil)
	r = runSetupTest(t, f, setupOpts{env: agentEnv()}, "--user", fakeUser, "--key-owner", "nobody")
	wantCode(t, r, exitSetupFailure)
	for _, m := range f.methods() {
		if strings.HasPrefix(m, "user.") && m != "user.query" {
			t.Errorf("setup called %s; it must not create users", m)
		}
	}
	if f.count("api_key.create") != 0 {
		t.Error("key created for a missing user")
	}
}

func TestSetupNameCollisionAndRotate(t *testing.T) {
	f := newFakeSetupNAS(t, nil)
	f.addKey("truenas-mcp-test-laptop", fakeUser)
	f.addKey("truenas-mcp-test-laptop-2", "someone-else")
	r := runSetupTest(t, f, setupOpts{env: agentEnv()}, "--user", fakeUser)
	wantCode(t, r, exitSetupOK)
	if f.keyByName("truenas-mcp-test-laptop-3") == nil {
		t.Errorf("no -3 key; creates %v", f.creates)
	}

	// --rotate resets the key we own instead of creating one, and
	// replaces the key file without --force.
	f = newFakeSetupNAS(t, nil)
	old := f.addKey("truenas-mcp-test-laptop", fakeUser)
	oldSecret := old.secret
	dir := filepath.Join(t.TempDir(), "cfg")
	r = runSetupTest(t, f, setupOpts{env: agentEnv(), dir: dir}, "--user", fakeUser, "--rotate", "--expires", "never")
	wantCode(t, r, exitSetupOK)
	if f.count("api_key.create") != 0 || len(f.updates) != 1 || f.updates[0]["reset"] != true {
		t.Fatalf("calls %v updates %v", f.methods(), f.updates)
	}
	if v, ok := f.updates[0]["expires_at"]; !ok || v != nil {
		t.Errorf("expires_at %v, want null for never", v)
	}
	k := f.keyByName("truenas-mcp-test-laptop")
	if k.secret == oldSecret || readFile(t, r.keyPath(f)) != k.secret+"\n" {
		t.Error("key file does not hold the rotated key")
	}
	r = runSetupTest(t, f, setupOpts{env: agentEnv(), dir: dir}, "--user", fakeUser, "--rotate")
	wantCode(t, r, exitSetupOK)

	// A key with that name owned by someone else is not reset.
	f = newFakeSetupNAS(t, nil)
	f.addKey("truenas-mcp-test-laptop", "someone-else")
	r = runSetupTest(t, f, setupOpts{env: agentEnv()}, "--user", fakeUser, "--rotate")
	wantCode(t, r, exitSetupPermission)
	if len(f.updates) != 0 {
		t.Error("reset another user's key")
	}
}

func TestSetupMyKeysFallback(t *testing.T) {
	f := newFakeSetupNAS(t, func(f *fakeSetupNAS) { f.queryDenied = true })
	f.addKey("truenas-mcp-test-laptop", "someone-else") // invisible through my_keys
	r := runSetupTest(t, f, setupOpts{env: agentEnv()}, "--user", fakeUser)
	wantCode(t, r, exitSetupOK)
	if f.count("api_key.my_keys") != 1 || f.keyByName("truenas-mcp-test-laptop-2") == nil {
		t.Errorf("calls %v", f.methods())
	}
}

func TestSetupNameAndExpiryFlags(t *testing.T) {
	if got := cleanName("My Laptop_01.Local!"); got != "my-laptop-01.local" {
		t.Errorf("cleanName = %q", got)
	}
	for in, want := range map[string]time.Duration{"90d": 90 * 24 * time.Hour, "1y": 365 * 24 * time.Hour, "2w": 14 * 24 * time.Hour, "12h": 12 * time.Hour, "never": 0, "NEVER": 0} {
		if got, err := parseExpiry(in); err != nil || got != want {
			t.Errorf("parseExpiry(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0d", "1m", "90", "-1d", "11y", "1.5y"} {
		if _, err := parseExpiry(bad); err == nil {
			t.Errorf("parseExpiry(%q) accepted", bad)
		}
	}
	f := newFakeSetupNAS(t, nil)
	r := runSetupTest(t, f, setupOpts{env: agentEnv()}, "--user", fakeUser, "--name", "Office MCP", "--expires", "90d")
	wantCode(t, r, exitSetupOK)
	if f.creates[0]["name"] != "office-mcp" {
		t.Errorf("name %v", f.creates[0]["name"])
	}
}

func TestSetupPrintConfigFormats(t *testing.T) {
	env := map[string]string{"TRUENAS_URL": "truenas.example.com", "TRUENAS_API_USER": "mcp-bot", "TRUENAS_API_KEY_FILE": "/home/u/.config/truenas-mcp/truenas.example.com.key"}
	for _, format := range printConfigFormats {
		out, err := clientSnippet(format, "/usr/local/bin/truenas-mcp", env)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "TRUENAS_API_KEY_FILE") || strings.Contains(out, "TRUENAS_API_KEY\"") {
			t.Errorf("%s snippet: %s", format, out)
		}
		if format != "claude-code" {
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(out), &v); err != nil {
				t.Errorf("%s snippet is not JSON: %v", format, err)
			}
		}
	}
	out, _ := clientSnippet("claude-code", "/opt/my tools/truenas-mcp", env)
	if !strings.HasPrefix(out, "claude mcp add truenas --env TRUENAS_API_KEY_FILE=") || !strings.Contains(out, "-- '/opt/my tools/truenas-mcp'") {
		t.Errorf("claude-code snippet: %s", out)
	}
	if _, err := clientSnippet("vscode", "x", env); err == nil {
		t.Error("unknown format accepted")
	}
	r := runSetupTest(t, nil, setupOpts{}, "--host", "192.0.2.10", "--user", "u", "--print-config", "vscode")
	wantCode(t, r, exitSetupUsage)
}

func TestSetupTLS(t *testing.T) {
	t.Run("untrusted certificate stops setup", func(t *testing.T) {
		f := newFakeSetupNAS(t, nil)
		r := runSetupTest(t, f, setupOpts{env: agentEnv(), noTrust: true}, "--user", fakeUser)
		wantCode(t, r, exitSetupTLS)
		fp := truenas.Fingerprint(f.Certificate().Raw)
		if !strings.Contains(r.stderr, fp) || !strings.Contains(r.stderr, "--trust-fingerprint") {
			t.Errorf("stderr lacks the fingerprint and fix:\n%s", r.stderr)
		}
		if n := len(f.snapshot()); n != 0 {
			t.Errorf("%d frames sent over an untrusted connection", n)
		}
	})
	t.Run("typed yes pins the certificate", func(t *testing.T) {
		f := newFakeSetupNAS(t, nil)
		r := runSetupTest(t, f, setupOpts{tty: true, noTrust: true, hidden: []string{fakePassword}, visible: []string{"yes"}}, "--user", fakeUser)
		wantCode(t, r, exitSetupOK)
		fp := truenas.Fingerprint(f.Certificate().Raw)
		if got := readFile(t, r.pinPath(f)); got != fp+"\n" {
			t.Errorf("pin file %q", got)
		}
		// A later run uses the saved pin without asking.
		var prompts []string
		r = runSetupTest(t, f, setupOpts{tty: true, noTrust: true, dir: r.dir, hidden: []string{fakePassword}, prompts: &prompts}, "--user", fakeUser, "--force")
		wantCode(t, r, exitSetupOK)
		for _, p := range prompts {
			if strings.Contains(p, "trust") {
				t.Errorf("asked again: %q", p)
			}
		}
	})
	t.Run("anything but yes refuses", func(t *testing.T) {
		f := newFakeSetupNAS(t, nil)
		r := runSetupTest(t, f, setupOpts{tty: true, noTrust: true, visible: []string{"y"}}, "--user", fakeUser)
		wantCode(t, r, exitSetupTLS)
		if n := len(f.snapshot()); n != 0 {
			t.Errorf("%d frames sent", n)
		}
	})
	t.Run("wrong pin is refused", func(t *testing.T) {
		f := newFakeSetupNAS(t, nil)
		r := runSetupTest(t, f, setupOpts{env: agentEnv(), noTrust: true}, "--user", fakeUser, "--trust-fingerprint", "SHA256:"+strings.Repeat("00", 32))
		wantCode(t, r, exitSetupTLS)
		if n := len(f.snapshot()); n != 0 {
			t.Errorf("%d frames sent", n)
		}
		if !strings.Contains(r.stderr, "does not match") {
			t.Errorf("stderr: %s", r.stderr)
		}
	})
	t.Run("tls-ca", func(t *testing.T) {
		f := newFakeSetupNAS(t, nil)
		ca := writePEM(t, f.Certificate().Raw)
		r := runSetupTest(t, f, setupOpts{env: agentEnv(), noTrust: true}, "--user", fakeUser, "--tls-ca", ca)
		wantCode(t, r, exitSetupOK)
		if !strings.Contains(r.stdout, `"TRUENAS_TLS_CA": "`+ca+`"`) {
			t.Errorf("snippet: %s", r.stdout)
		}
	})
	t.Run("insecure needs typed opt-in", func(t *testing.T) {
		f := newFakeSetupNAS(t, nil)
		r := runSetupTest(t, f, setupOpts{env: agentEnv(), noTrust: true}, "--user", fakeUser, "--insecure")
		wantCode(t, r, exitSetupTLS)
		r = runSetupTest(t, f, setupOpts{tty: true, noTrust: true, visible: []string{"yes"}}, "--user", fakeUser, "--insecure")
		wantCode(t, r, exitSetupTLS)
		if n := len(f.snapshot()); n != 0 {
			t.Errorf("%d frames sent without confirmation", n)
		}
		r = runSetupTest(t, f, setupOpts{tty: true, noTrust: true, hidden: []string{fakePassword}, visible: []string{"insecure"}}, "--user", fakeUser, "--insecure")
		wantCode(t, r, exitSetupOK)
		if !strings.Contains(r.stdout, `"TRUENAS_INSECURE": "1"`) {
			t.Errorf("snippet: %s", r.stdout)
		}
	})
	t.Run("one TLS mode only", func(t *testing.T) {
		r := runSetupTest(t, nil, setupOpts{}, "--host", "192.0.2.10", "--user", "u", "--insecure", "--tls-ca", "x.pem")
		wantCode(t, r, exitSetupUsage)
	})
}

// The password, OTP and key never reach logs, debug output, error text or
// any file other than the key file.
func TestSetupNoSecretLeaks(t *testing.T) {
	check := func(t *testing.T, r setupResult, secrets map[string]string, keyFile string) {
		t.Helper()
		texts := map[string]string{"stdout": r.stdout, "stderr (with debug log)": r.stderr}
		_ = filepath.WalkDir(r.dir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && path != keyFile {
				texts[path] = readFile(t, path)
			}
			return nil
		})
		for where, text := range texts {
			for what, secret := range secrets {
				if secret != "" && strings.Contains(text, secret) {
					t.Errorf("%s contains the %s", where, what)
				}
			}
		}
	}

	f := newFakeSetupNAS(t, func(f *fakeSetupNAS) { f.otp = fakeOTP })
	env := agentEnv()
	env[setupOTPEnv] = fakeOTP
	r := runSetupTest(t, f, setupOpts{env: env}, "--user", fakeUser)
	wantCode(t, r, exitSetupOK)
	if !strings.Contains(r.stderr, "Sending request:") {
		t.Fatal("debug log not captured")
	}
	key := f.keyByName("truenas-mcp-test-laptop").secret
	check(t, r, map[string]string{"password": fakePassword, "OTP": fakeOTP, "API key": key}, r.keyPath(f))

	// The server echoes the password and OTP in an error: both are scrubbed.
	f = newFakeSetupNAS(t, func(f *fakeSetupNAS) {
		f.otp = fakeOTP
		f.otpErr = callErr(22, "EINVAL", "bad token "+fakeOTP+" for password "+fakePassword)
	})
	r = runSetupTest(t, f, setupOpts{env: env}, "--user", fakeUser)
	wantCode(t, r, exitSetupOTP)
	if !strings.Contains(r.stderr, redact.Marker) {
		t.Errorf("echoed secrets not replaced by %s:\n%s", redact.Marker, r.stderr)
	}
	check(t, r, map[string]string{"password": fakePassword, "OTP": fakeOTP}, "")

	// Wrong password from a terminal, then a failure: same.
	f = newFakeSetupNAS(t, func(f *fakeSetupNAS) { f.createErr = callErr(13, "EACCES", "denied for "+fakePassword) })
	r = runSetupTest(t, f, setupOpts{tty: true, hidden: []string{"WRONG-PW-MARKER", fakePassword}}, "--user", fakeUser)
	wantCode(t, r, exitSetupPermission)
	check(t, r, map[string]string{"password": fakePassword, "first password": "WRONG-PW-MARKER"}, "")
}

func writePEM(t *testing.T, der []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServerTLSConfigPin(t *testing.T) {
	pin := "SHA256:" + strings.Repeat("ab", 32)
	if _, err := serverTLSConfig(false, "", pin); err != nil {
		t.Errorf("pin alone: %v", err)
	}
	if _, err := serverTLSConfig(true, "", pin); err == nil {
		t.Error("pin with --insecure accepted")
	}
	if _, err := serverTLSConfig(false, "ca.pem", pin); err == nil {
		t.Error("pin with --tls-ca accepted")
	}
	if _, err := serverTLSConfig(false, "", "SHA256:1234"); err == nil {
		t.Error("short pin accepted")
	}
}
