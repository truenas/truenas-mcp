package redact

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Obviously fake secrets. Every test asserts none of them survive.
const (
	fakePEM = "-----BEGIN RSA PRIVATE KEY-----\nFAKEFAKEFAKEnotarealkey0000\nAAAAfakefakefake\n-----END RSA PRIVATE KEY-----\n"
	fakeSSH = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAAfakefakefake\n-----END OPENSSH PRIVATE KEY-----"
	fakeCrt = "-----BEGIN CERTIFICATE-----\nMIIBfakeCERTpublicpart\n-----END CERTIFICATE-----"
)

var fakeSecrets = []string{
	"FAKEFAKEFAKEnotarealkey", "b3BlbnNzaC1rZXktdjEAAAAAfakefakefake", "AAAAfakefakefake",
	"fake-api-key-0000", "fake-secret-access-key", "fake-bind-pw", "fake-user-pw",
	"fake-passphrase", "fake-hex-key-0000", "fake-oauth-token", "fake-client-secret",
	"fake-smb-pw", "fake-db-pw", "fake-vnc-pw", "fake-keytab-b64", "fake-b2-key",
	"fake-ftp-pw", "fake-totp-secret", "fake-ssh-pw",
}

func TestIsSensitiveKey(t *testing.T) {
	sensitive := []string{
		"password", "Password", "PASSWORD", "admin_password", "smb-password", "bindpw",
		"passwd", "new_passwd", "passphrase", "secret", "client_secret", "secret_access_key",
		"token", "access_token", "refresh-token", "apikey", "api_key", "API-Key", "apiKey",
		"privatekey", "private_key", "privateKey", "PRIVATE-KEY", "passphrase",
		"credential", "credentials", "ssh_credentials", "service_account_credentials",
		"key", "KEY", "encryption_key", "encryptionKey", "encryptionkey", "dataset key",
		"dataset_key", "datasetkey", "ssh_key", "sshkey", "SECRET_KEY", "APP_KEY",
		"access_key", "host_ed25519_key", "pass", "pwd", "psk", "keytab", "kerberos_keytab",
	}
	for _, k := range sensitive {
		if !IsSensitiveKey(k) {
			t.Errorf("IsSensitiveKey(%q) = false, want true", k)
		}
	}
	harmless := []string{
		"keyboard", "monkey", "Monkey", "hotkey", "key_format", "keyformat", "keylength",
		"key_length", "key_loaded", "keys", "keyword", "keywords", "turnkey", "name",
		"username", "id", "certificate", "CSR", "public_key", "publicKey", "encryption",
		"encryption_algorithm", "pbkdf2iters", "path", "host", "", "description",
		"compression", "passthrough_mode", "credential_type", "token_type",
	}
	for _, k := range harmless {
		if IsSensitiveKey(k) {
			t.Errorf("IsSensitiveKey(%q) = true, want false", k)
		}
	}
}

func decodeJSON(t *testing.T, s string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad fixture: %v\n%s", err, s)
	}
	return v
}

// get walks a decoded JSON value by map keys and array indexes.
func get(t *testing.T, v interface{}, path ...interface{}) interface{} {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]interface{})
			if !ok {
				t.Fatalf("path %v: not an object at %q", path, k)
			}
			v = m[k]
		case int:
			a, ok := v.([]interface{})
			if !ok || k >= len(a) {
				t.Fatalf("path %v: not an array at %d", path, k)
			}
			v = a[k]
		}
	}
	return v
}

type check struct {
	path []interface{}
	want interface{}
}

func redacted(path ...interface{}) check { return check{path, Marker} }
func kept(want interface{}, path ...interface{}) check {
	return check{path, want}
}

// TestToolOutputRealisticShapes covers the shapes of TrueNAS API results
// that carry secrets: tools that query them directly, and query_jobs /
// tasks_get, which echo the arguments and results of any job.
func TestToolOutputRealisticShapes(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		checks []check
	}{
		{
			name: "api_key.query / api_key.create",
			input: `[{"id": 1, "name": "mcp", "username": "svc-mcp", "key": "1-fake-api-key-0000",
				"keyhash": "$pbkdf2-sha256$fake", "expired": false}]`,
			checks: []check{redacted(0, "key"), kept("mcp", 0, "name"), kept(false, 0, "expired")},
		},
		{
			name: "cloudsync.credentials.query (S3, B2, Google Drive)",
			input: `[{"id": 1, "name": "aws", "provider": {"type": "S3", "access_key_id": "AKIAFAKEFAKE",
				"secret_access_key": "fake-secret-access-key", "endpoint": ""}},
				{"id": 2, "name": "b2", "provider": "B2", "attributes": {"account": "fakeacct", "key": "fake-b2-key"}},
				{"id": 3, "name": "gdrive", "provider": {"type": "GOOGLE_DRIVE", "token": "{\"access_token\": \"fake-oauth-token\"}",
				"client_id": "fake-client-id", "client_secret": "fake-client-secret"}}]`,
			checks: []check{
				redacted(0, "provider", "secret_access_key"), kept("S3", 0, "provider", "type"),
				kept("", 0, "provider", "endpoint"), redacted(1, "attributes", "key"),
				kept("fakeacct", 1, "attributes", "account"), redacted(2, "provider", "token"),
				redacted(2, "provider", "client_secret"), kept("gdrive", 2, "name"),
			},
		},
		{
			name: "cloudsync.query embeds its credential",
			input: `[{"id": 7, "path": "/mnt/tank/data", "credentials": {"id": 4, "name": "ftp",
				"provider": {"type": "FTP", "host": "192.0.2.10", "user": "backup", "pass": "fake-ftp-pw"}}}]`,
			checks: []check{
				// An object under a sensitive key is redacted wholesale.
				redacted(0, "credentials"), kept("/mnt/tank/data", 0, "path"), kept(json.Number("7"), 0, "id"),
			},
		},
		{
			name: "keychaincredential.query (SSH key pair and SSH connection)",
			input: `[{"id": 1, "name": "repl-key", "type": "SSH_KEY_PAIR", "attributes": {
				"private_key": ` + jsonString(fakeSSH) + `, "public_key": "ssh-ed25519 AAAAC3fakepublic repl"}},
				{"id": 2, "name": "repl-conn", "type": "SSH_CREDENTIALS", "attributes": {"host": "192.0.2.20",
				"port": 22, "username": "root", "private_key": 1, "remote_host_key": "ssh-ed25519 AAAAfakehost",
				"connect_timeout": 10}}]`,
			checks: []check{
				redacted(0, "attributes", "private_key"), kept("ssh-ed25519 AAAAC3fakepublic repl", 0, "attributes", "public_key"),
				kept(json.Number("1"), 1, "attributes", "private_key"), kept("root", 1, "attributes", "username"),
			},
		},
		{
			name: "replication.query embeds ssh_credentials",
			input: `[{"id": 3, "name": "tank->backup", "transport": "SSH", "ssh_credentials": {"id": 2, "name": "repl-conn",
				"type": "SSH_CREDENTIALS", "attributes": {"host": "192.0.2.20", "username": "root",
				"private_key": 1, "password": "fake-ssh-pw"}}}]`,
			checks: []check{
				redacted(0, "ssh_credentials"), kept("SSH", 0, "transport"), kept("tank->backup", 0, "name"),
			},
		},
		{
			name: "certificate.query",
			input: `[{"id": 1, "name": "truenas_default", "certificate": ` + jsonString(fakeCrt) + `,
				"privatekey": ` + jsonString(fakePEM) + `, "CSR": null, "key_type": "RSA", "key_length": 2048}]`,
			checks: []check{
				redacted(0, "privatekey"), kept(fakeCrt, 0, "certificate"), kept("RSA", 0, "key_type"),
				kept(json.Number("2048"), 0, "key_length"), kept(nil, 0, "CSR"),
			},
		},
		{
			name: "user.query / user.create",
			input: `[{"id": 70, "username": "alice", "password": "fake-user-pw", "unixhash": "$6$fakehash",
				"smbhash": "*", "password_disabled": false, "sshpubkey": "ssh-rsa AAAAfakepub alice",
				"twofactor_auth_configured": false}]`,
			checks: []check{
				redacted(0, "password"), kept(false, 0, "password_disabled"), kept("alice", 0, "username"),
				redacted(0, "unixhash"), redacted(0, "smbhash"), kept("ssh-rsa AAAAfakepub alice", 0, "sshpubkey"),
			},
		},
		{
			name: "pool.dataset.create / pool.dataset.query encryption",
			input: `{"name": "tank/secure", "encrypted": true, "key_format": {"value": "PASSPHRASE"},
				"key_loaded": true, "encryption_algorithm": {"value": "AES-256-GCM"},
				"encryption_options": {"generate_key": false, "passphrase": "fake-passphrase", "pbkdf2iters": 350000},
				"keys": [{"name": "tank/raw", "key": "fake-hex-key-0000"}], "dataset key": "fake-hex-key-0000"}`,
			checks: []check{
				redacted("encryption_options", "passphrase"), kept(false, "encryption_options", "generate_key"),
				kept("PASSPHRASE", "key_format", "value"), kept(true, "key_loaded"),
				redacted("keys", 0, "key"), kept("tank/raw", "keys", 0, "name"), redacted("dataset key"),
			},
		},
		{
			name: "core.get_jobs pool.dataset.export_key(s) results",
			input: `[{"id": 42, "method": "pool.dataset.export_keys", "state": "SUCCESS",
				"result": {"tank/secure": "fake-hex-key-0000"}},
				{"id": 43, "method": "pool.dataset.export_key", "state": "SUCCESS", "result": "fake-hex-key-0000"},
				{"id": 44, "method": "pool.dataset.query", "result": [{"name": "tank/secure"}]}]`,
			checks: []check{
				redacted(0, "result"), redacted(1, "result"), kept("SUCCESS", 0, "state"),
				kept("tank/secure", 2, "result", 0, "name"),
			},
		},
		{
			name: "directoryservices.config (LDAP, AD)",
			input: `{"service_type": "LDAP", "enable": true, "credential": {"credential_type": "LDAP_PLAIN",
				"binddn": "cn=admin,dc=example,dc=com", "bindpw": "fake-bind-pw"},
				"configuration": {"server_urls": ["ldaps://ldap.example.com"], "kerberos_keytab": "fake-keytab-b64"}}`,
			checks: []check{
				redacted("credential"), kept("LDAP", "service_type"), kept(true, "enable"),
				kept("ldaps://ldap.example.com", "configuration", "server_urls", 0), redacted("configuration", "kerberos_keytab"),
			},
		},
		{
			name: "core.get_jobs echoes job arguments (directoryservices.update, user.create)",
			input: `[{"id": 9, "method": "directoryservices.update", "arguments": [{"credential":
				{"credential_type": "KERBEROS_USER", "username": "admin", "password": "fake-bind-pw"}}]},
				{"id": 10, "method": "user.create", "arguments": [{"username": "bob", "password": "fake-user-pw"}]}]`,
			checks: []check{
				redacted(0, "arguments", 0, "credential"), kept("directoryservices.update", 0, "method"),
				redacted(1, "arguments", 0, "password"), kept("bob", 1, "arguments", 0, "username"),
			},
		},
		{
			name: "sharing.smb / sharing.nfs / smb.config",
			input: `{"smb": {"name": "data", "path": "/mnt/tank/data", "auxsmbconf": ""},
				"smb_config": {"netbiosname": "truenas", "admin_password": "fake-smb-pw"},
				"nfs": {"path": "/mnt/tank/data", "security": ["KRB5"]}}`,
			checks: []check{redacted("smb_config", "admin_password"), kept("/mnt/tank/data", "nfs", "path")},
		},
		{
			name: "app.config with env name/value pairs",
			input: `{"postgres": {"user": "app", "password": "fake-db-pw"}, "TZ": "Etc/UTC",
				"additional_envs": [{"name": "POSTGRES_PASSWORD", "value": "fake-db-pw"},
				{"name": "LOG_LEVEL", "value": "debug"}, {"key": "API_TOKEN", "value": "fake-oauth-token"}],
				"secret_key_base": "fake-client-secret", "network": {"web_port": 8080}}`,
			checks: []check{
				redacted("postgres", "password"), redacted("additional_envs", 0, "value"),
				kept("POSTGRES_PASSWORD", "additional_envs", 0, "name"), kept("debug", "additional_envs", 1, "value"),
				redacted("additional_envs", 2, "value"), kept("API_TOKEN", "additional_envs", 2, "key"),
				redacted("secret_key_base"), kept("Etc/UTC", "TZ"),
			},
		},
		{
			name: "vm.query display device",
			input: `[{"id": 1, "name": "win", "devices": [{"attributes": {"dtype": "DISPLAY", "type": "SPICE",
				"bind": "0.0.0.0", "password": "fake-vnc-pw", "web": true}}]}]`,
			checks: []check{redacted(0, "devices", 0, "attributes", "password"), kept("SPICE", 0, "devices", 0, "attributes", "type")},
		},
		{
			name: "2FA secret and PEM in an unrelated field",
			input: `{"twofactor": {"secret": "fake-totp-secret", "interval": 30},
				"notes": "backup key:\n` + strings.ReplaceAll(strings.TrimSpace(fakePEM), "\n", `\n`) + `\nend"}`,
			checks: []check{redacted("twofactor", "secret"), kept(json.Number("30"), "twofactor", "interval")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decodeJSON(t, tc.input) // fixture must be valid JSON
			out := ToolOutput(tc.input)
			got := decodeJSONNumber(t, out)
			for _, c := range tc.checks {
				if v := get(t, got, c.path...); !reflect.DeepEqual(v, c.want) {
					t.Errorf("%v = %#v, want %#v", c.path, v, c.want)
				}
			}
			assertNoSecrets(t, out)
		})
	}
}

func decodeJSONNumber(t *testing.T, s string) interface{} {
	t.Helper()
	v, ok := decode([]byte(s))
	if !ok {
		t.Fatalf("output is not JSON:\n%s", s)
	}
	return v
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func assertNoSecrets(t *testing.T, out string) {
	t.Helper()
	for _, s := range fakeSecrets {
		if strings.Contains(out, s) {
			t.Errorf("secret %q survived redaction:\n%s", s, out)
		}
	}
	if strings.Contains(out, "PRIVATE KEY") {
		t.Errorf("PEM private key survived redaction:\n%s", out)
	}
}

func TestToolOutputUnchangedWithoutSecrets(t *testing.T) {
	in := "{\n  \"zeta\": 1,\n  \"alpha\": \"<b>&</b>\",\n  \"big\": 12345678901234567890\n}"
	if out := ToolOutput(in); out != in {
		t.Errorf("output without secrets changed:\n%s", out)
	}
	for _, s := range []string{"", "Alert abc dismissed successfully: null", "Weekly on Sunday at 00:00", "[]"} {
		if out := ToolOutput(s); out != s {
			t.Errorf("ToolOutput(%q) = %q", s, out)
		}
	}
}

func TestToolOutputKeepsIndentationAndNumbers(t *testing.T) {
	in := "{\n  \"id\": 12345678901234567890,\n  \"password\": \"fake-user-pw\",\n  \"note\": \"<ok>\"\n}"
	out := ToolOutput(in)
	want := "{\n  \"id\": 12345678901234567890,\n  \"note\": \"<ok>\",\n  \"password\": \"" + Marker + "\"\n}"
	if out != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	if got := ToolOutput(`{"password":"fake-user-pw"}`); got != `{"password":"`+Marker+`"}` {
		t.Errorf("compact output = %s", got)
	}
}

func TestToolOutputText(t *testing.T) {
	in := "Alert x dismissed: {\"password\": \"fake-user-pw\", \"name\": \"ok\"}\n" + fakePEM + "tail"
	out := ToolOutput(in)
	assertNoSecrets(t, out)
	if !strings.Contains(out, `"password": "`+Marker+`"`) || !strings.Contains(out, `"name": "ok"`) || !strings.HasSuffix(out, "tail") {
		t.Errorf("unexpected text redaction:\n%s", out)
	}
}

func TestText(t *testing.T) {
	cases := []struct{ in, want string }{
		{fakePEM, Marker + "\n"},
		{"key: " + fakeSSH + " done", "key: " + Marker + " done"},
		{"truncated -----BEGIN EC PRIVATE KEY-----\nAAAAfakefakefake", "truncated " + Marker},
		{"-----BEGIN ENCRYPTED PRIVATE KEY-----\nAAAAfakefakefake\n-----END ENCRYPTED PRIVATE KEY-----", Marker},
		{"-----BEGIN PGP PRIVATE KEY BLOCK-----\nAAAAfakefakefake\n-----END PGP PRIVATE KEY BLOCK-----", Marker},
		{fakeCrt, fakeCrt},
		{`[EINVAL] {'bindpw': 'fake-bind-pw', 'binddn': 'cn=x'}`, `[EINVAL] {'bindpw': '` + Marker + `', 'binddn': 'cn=x'}`},
		{`{"api_key": "fake-api-key-0000", "keyboard": "us"}`, `{"api_key": "` + Marker + `", "keyboard": "us"}`},
		{`{"password": ""}`, `{"password": ""}`},
		{`escaped {"token": "a\"fake-oauth-token"}`, `escaped {"token": "` + Marker + `"}`},
		{"plain error: connection refused", "plain error: connection refused"},
	}
	for _, c := range cases {
		if got := Text(c.in); got != c.want {
			t.Errorf("Text(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestValueDoesNotModifyInput(t *testing.T) {
	in := map[string]interface{}{
		"password": "fake-user-pw",
		"nested":   []interface{}{map[string]interface{}{"token": "fake-oauth-token"}},
	}
	out := Value(in).(map[string]interface{})
	if in["password"] != "fake-user-pw" || in["nested"].([]interface{})[0].(map[string]interface{})["token"] != "fake-oauth-token" {
		t.Error("Value mutated its input")
	}
	if out["password"] != Marker {
		t.Errorf("password = %v", out["password"])
	}
}

func TestSensitiveArrayAndMarkerIdempotent(t *testing.T) {
	out := ToolOutput(`{"tokens": ["fake-oauth-token", ""], "password": "` + Marker + `", "secrets": [], "token": {}}`)
	if out != `{"password":"`+Marker+`","secrets":[],"token":{},"tokens":"`+Marker+`"}` {
		t.Errorf("got %s", out)
	}
	if again := ToolOutput(out); again != out {
		t.Errorf("not idempotent: %s", again)
	}
}

func TestJSON(t *testing.T) {
	in := []byte(`{"jsonrpc":"2.0","result":{"privatekey":` + jsonString(fakePEM) + `,"name":"cert"}}`)
	out := string(JSON(in))
	assertNoSecrets(t, out)
	if !strings.Contains(out, `"name":"cert"`) {
		t.Errorf("JSON = %s", out)
	}
	clean := []byte(`{"b":1,"a":2}`)
	if string(JSON(clean)) != string(clean) {
		t.Error("JSON without secrets changed")
	}
	if got := string(JSON([]byte(`not json "secret": "fake-client-secret"`))); strings.Contains(got, "fake-client-secret") {
		t.Errorf("non-JSON text not redacted: %s", got)
	}
}

func TestIsSensitiveKeySegments(t *testing.T) {
	sensitive := []string{
		// pass / pw / pwd as name segments or run-together suffixes.
		"DB_PASS", "REDIS_PASS", "SMTP_PASS", "smtp_pass", "dbPass", "adminPwd", "DB_PWD",
		"monpwd", "dbpass", "rootpw", "ROOT_PW", "pass-word", "MYSQL_ROOT_PASSWORD", "user.pass",
		// Narrowed exemptions.
		"public_key_secret", "secret_type", "password_type", "passwordtype", "secrettype", "passwordType",
		// Additional names.
		"session", "session_id", "sessionToken", "cookie", "Set-Cookie", "authorization",
		"Proxy-Authorization", "otp", "otp_code", "pin", "sim_pin", "PIN", "pinCode", "community",
		"snmp_community", "unixhash", "smbhash", "pwhash", "password_hash", "keytab_file",
	}
	for _, k := range sensitive {
		if !IsSensitiveKey(k) {
			t.Errorf("IsSensitiveKey(%q) = false, want true", k)
		}
	}
	harmless := []string{
		"passthrough", "bypass", "compass", "passive", "pwm", "pwr", "pwm_fan", "PASSIVE_MODE",
		"pinned", "spinning", "spindown", "sessions",
		"hash", "checksum", "credential_type", "token_type", "credentialType", "key_type",
		"public_key", "sshpubkey", "ssh_public_key", "peer_public_key", "communities",
		"opinion", "options", "keyboard_layout", "pass_count", "session_timeout", "password_length",
		"token_ttl", "otp_enabled", "secret_expiry", "password_changed", "minPasswordLength",
	}
	for _, k := range harmless {
		if IsSensitiveKey(k) {
			t.Errorf("IsSensitiveKey(%q) = true, want false", k)
		}
	}
}

func TestSegments(t *testing.T) {
	cases := map[string][]string{
		"dbPass":              {"db", "pass"},
		"APIKey":              {"api", "key"},
		"MYSQL_ROOT_PASSWORD": {"mysql", "root", "password"},
		"dataset key":         {"dataset", "key"},
		"host_ed25519_key":    {"host", "ed25519", "key"},
		"Set-Cookie":          {"set", "cookie"},
	}
	for in, want := range cases {
		if got := segments(in); !reflect.DeepEqual(got, want) {
			t.Errorf("segments(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestReviewRepros covers app environment shapes and free text inside JSON
// results.
func TestReviewRepros(t *testing.T) {
	cases := []struct {
		in     string
		checks []check
	}{
		{`{"services":{"db":{"environment":["MYSQL_ROOT_PASSWORD=fake-db-pw","POSTGRES_PASSWORD=fake-db-pw","TZ=Etc/UTC"]}}}`,
			[]check{
				kept("MYSQL_ROOT_PASSWORD="+Marker, "services", "db", "environment", 0),
				kept("POSTGRES_PASSWORD="+Marker, "services", "db", "environment", 1),
				kept("TZ=Etc/UTC", "services", "db", "environment", 2),
			}},
		{`{"additional_envs":[{"name":"DB_PASS","value":"fake-db-pw"},{"name":"DB_USER","value":"app"}]}`,
			[]check{
				kept("DB_PASS", "additional_envs", 0, "name"), redacted("additional_envs", 0, "value"),
				kept("app", "additional_envs", 1, "value"),
			}},
		{`{"envs":[{"key":"DB_PWD","value":"fake-db-pw"},{"key":"color","value":"blue"}]}`,
			[]check{
				kept("DB_PWD", "envs", 0, "key"), redacted("envs", 0, "value"),
				kept("color", "envs", 1, "key"), kept("blue", "envs", 1, "value"),
			}},
		{`{"environment":{"DB_PASS":"fake-db-pw","REDIS_PASS":"fake-db-pw","DB_HOST":"db"}}`,
			[]check{redacted("environment", "DB_PASS"), redacted("environment", "REDIS_PASS"), kept("db", "environment", "DB_HOST")}},
		{`{"dbPass":"fake-db-pw","adminPwd":"fake-db-pw","smtp_pass":"fake-db-pw","bypass":"on","compass":"N"}`,
			[]check{redacted("dbPass"), redacted("adminPwd"), redacted("smtp_pass"), kept("on", "bypass"), kept("N", "compass")}},
		{`{"jobs":[{"method":"directoryservices.update","error":"[EFAULT] failed {'bindpw': 'fake-bind-pw'}"}]}`,
			[]check{kept("[EFAULT] failed {'bindpw': '"+Marker+"'}", "jobs", 0, "error")}},
		{`{"description":"{\"password\": \"fake-user-pw\", \"name\": \"x\"}"}`,
			[]check{kept(`{"name":"x","password":"`+Marker+`"}`, "description")}},
		{`{"exception":"Traceback ...\nCredentials(password='fake-user-pw', user='bob')"}`,
			[]check{kept("Traceback ...\nCredentials(password='"+Marker+"', user='bob')", "exception")}},
	}
	for _, tc := range cases {
		out := ToolOutput(tc.in)
		got := decodeJSONNumber(t, out)
		for _, c := range tc.checks {
			if v := get(t, got, c.path...); !reflect.DeepEqual(v, c.want) {
				t.Errorf("%s: %v = %#v, want %#v", tc.in, c.path, v, c.want)
			}
		}
		assertNoSecrets(t, out)
	}
}

func TestSensitiveSubtreesAndNumbers(t *testing.T) {
	in := `{"secrets":{"db":"fake-db-pw"},"token":{"value":"fake-oauth-token"},"credentials":["a"],
		"password":123456,"pin":1234,"secret":1.5,"private_key":1,"privatekey":2,"credentials_id":3,
		"ssh_credentials_id":"fake-ssh-pw","id":4,"password_disabled":true,"bindpw":null,"passphrase":"",
		"otp":false,"public_key_secret":"fake-client-secret"}`
	got := decodeJSONNumber(t, ToolOutput(in))
	for _, c := range []check{
		redacted("secrets"), redacted("token"), redacted("credentials"),
		redacted("password"), redacted("pin"), redacted("secret"),
		kept(json.Number("1"), "private_key"), kept(json.Number("2"), "privatekey"),
		kept(json.Number("3"), "credentials_id"), redacted("ssh_credentials_id"), kept(json.Number("4"), "id"),
		kept(true, "password_disabled"), kept(nil, "bindpw"), kept("", "passphrase"), kept(false, "otp"),
		redacted("public_key_secret"),
	} {
		if v := get(t, got, c.path...); !reflect.DeepEqual(v, c.want) {
			t.Errorf("%v = %#v, want %#v", c.path, v, c.want)
		}
	}
}

func TestContextualNames(t *testing.T) {
	in := `{"jobs":[{"id":1,"method":"kerberos.keytab.create","arguments":[{"name":"AD_MACHINE_ACCOUNT","file":"fake-keytab-b64"}]}],
		"kerberos":{"keytab":{"file":"fake-keytab-b64"}},"upload":{"file":"report.csv"},"kerberos_realm":{"realm":"EXAMPLE.COM","file":"fake-keytab-b64"},
		"users":[{"username":"bob","uid":1001,"hash":"$6$fake-user-pw"}],"snapshot":{"hash":"abc123"},
		"snmp":{"community":"fake-client-secret","location":"rack 1"},
		"twofactor":{"otp":"123456"},"http":{"cookie":"sid=fake-oauth-token","session":"fake-oauth-token"}}`
	out := ToolOutput(in)
	got := decodeJSONNumber(t, out)
	for _, c := range []check{
		redacted("jobs", 0, "arguments", 0, "file"), kept("AD_MACHINE_ACCOUNT", "jobs", 0, "arguments", 0, "name"),
		redacted("kerberos", "keytab"), kept("report.csv", "upload", "file"),
		redacted("kerberos_realm", "file"), kept("EXAMPLE.COM", "kerberos_realm", "realm"),
		redacted("users", 0, "hash"), kept("bob", "users", 0, "username"), kept("abc123", "snapshot", "hash"),
		redacted("snmp", "community"), kept("rack 1", "snmp", "location"), redacted("twofactor", "otp"),
		redacted("http", "cookie"), redacted("http", "session"),
	} {
		if v := get(t, got, c.path...); !reflect.DeepEqual(v, c.want) {
			t.Errorf("%v = %#v, want %#v", c.path, v, c.want)
		}
	}
	assertNoSecrets(t, out)
}

func TestTextStyles(t *testing.T) {
	cases := []struct{ in, want string }{
		{"login failed: password=fake-user-pw user=bob", "login failed: password=" + Marker + " user=bob"},
		{"api key: fake-api-key-0000", "api key: " + Marker},
		{"key: fake-hex-key-0000, keyboard: us", "key: " + Marker + ", keyboard: us"},
		{"DB_PASS=fake-db-pw", "DB_PASS=" + Marker},
		{"MYSQL_ROOT_PASSWORD=fake db pw with spaces", "MYSQL_ROOT_PASSWORD=" + Marker},
		{"TZ=Etc/UTC", "TZ=Etc/UTC"},
		{"Authorization: Bearer fake-oauth-token", "Authorization: Bearer " + Marker},
		{"Authorization: Basic ZmFrZTpmYWtl", "Authorization: Basic " + Marker},
		{"sent header 'Authorization': 'Bearer fake-oauth-token'", "sent header 'Authorization': '" + Marker + "'"},
		{"using bearer eyJfake.fake-oauth-token.sig now", "using bearer " + Marker + " now"},
		{`{\"password\": \"fake-user-pw\", \"name\": \"x\"}`, `{\"password\": \"` + Marker + `\", \"name\": \"x\"}`},
		{`{'password': "it's fake-user-pw"}`, `{'password': "` + Marker + `"}`},
		{"Credentials(password='fake-user-pw')", "Credentials(password='" + Marker + "')"},
		{"connect postgres://app:fake-db-pw@192.0.2.5:5432/db failed", "connect postgres://app:" + Marker + "@192.0.2.5:5432/db failed"},
		{"https://192.0.2.1:8443/api?token=fake-oauth-token&x=1", "https://192.0.2.1:8443/api?token=" + Marker + "&x=1"},
		{"see https://example.com:443/path and passthrough: on", "see https://example.com:443/path and passthrough: on"},
		{"Basic configuration saved", "Basic configuration saved"},
		{"pool: tank, state: ONLINE", "pool: tank, state: ONLINE"},
	}
	for _, c := range cases {
		if got := Text(c.in); got != c.want {
			t.Errorf("Text(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
		if again := Text(c.want); again != c.want {
			t.Errorf("Text not idempotent on %q: %q", c.want, again)
		}
	}
}

func TestValueTypedInputs(t *testing.T) {
	type creds struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	in := map[string]interface{}{
		"env":   map[string]string{"DB_PASS": "fake-db-pw", "TZ": "UTC"},
		"list":  []map[string]interface{}{{"token": "fake-oauth-token", "n": 1}},
		"creds": creds{User: "bob", Password: "fake-user-pw"},
		"ptr":   &creds{User: "amy", Password: "fake-user-pw"},
		"count": 3,
	}
	raw, err := json.Marshal(Value(in))
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	assertNoSecrets(t, out)
	for _, want := range []string{`"TZ":"UTC"`, `"user":"bob"`, `"user":"amy"`, `"count":3`, `"n":1`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	top, err := json.Marshal(Value(creds{User: "bob", Password: "fake-user-pw"}))
	if err != nil || strings.Contains(string(top), "fake-user-pw") {
		t.Errorf("typed top-level value not redacted: %s", top)
	}
}
