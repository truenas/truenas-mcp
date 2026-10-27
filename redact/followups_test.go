package redact

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func runChecks(t *testing.T, in string, checks []check) {
	t.Helper()
	out := ToolOutput(in)
	got := decodeJSONNumber(t, out)
	for _, c := range checks {
		if v := get(t, got, c.path...); !reflect.DeepEqual(v, c.want) {
			t.Errorf("%s: %v = %#v, want %#v", in, c.path, v, c.want)
		}
	}
	assertNoSecrets(t, out)
}

func checkText(t *testing.T, cases []struct{ in, want string }) {
	t.Helper()
	for _, c := range cases {
		if got := Text(c.in); got != c.want {
			t.Errorf("Text(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
		if again := Text(c.want); again != c.want {
			t.Errorf("Text not idempotent on %q: %q", c.want, again)
		}
	}
}

// TestMetadataKeysOnlyShowScalars: keys that describe a secret (token_ttl,
// password_set, secret_expires) show booleans, numbers and null, but a
// string, object or array under them is still redacted.
func TestMetadataKeysOnlyShowScalars(t *testing.T) {
	runChecks(t, `{"token_ttl":3600,"password_set":true,"secret_expires":null,"otp_enabled":false,
		"token_expires":"fake-oauth-token","password_set_to":"x","password_changed":"fake-user-pw",
		"secret_expiry":{"value":"fake-client-secret"},"token_timeout":["fake-oauth-token"],
		"additional_envs":[{"name":"REDIS_PASSWORD_SET","value":"fake-db-pw"},{"name":"REDIS_PASSWORD_SET","value":true},
		{"name":"API_TOKEN_TTL","value":300}]}`,
		[]check{
			kept(json.Number("3600"), "token_ttl"), kept(true, "password_set"), kept(nil, "secret_expires"),
			kept(false, "otp_enabled"), redacted("token_expires"), redacted("password_changed"),
			redacted("secret_expiry"), redacted("token_timeout"),
			redacted("additional_envs", 0, "value"), kept(true, "additional_envs", 1, "value"),
			kept(json.Number("300"), "additional_envs", 2, "value"),
		})
	checkText(t, []struct{ in, want string }{
		{"token_ttl=3600 user=bob", "token_ttl=3600 user=bob"},
		{"password_set: true", "password_set: true"},
		{"password_set: fake-user-pw", "password_set: " + Marker},
		{"PASSWORD_SET=fake-user-pw", "PASSWORD_SET=" + Marker},
		{"TOKEN_TTL=3600", "TOKEN_TTL=3600"},
		{`{"token_expires": "fake-oauth-token"}`, `{"token_expires": "` + Marker + `"}`},
		{`{\"secret_expiry\": \"fake-client-secret\"}`, `{\"secret_expiry\": \"` + Marker + `\"}`},
		{"--token-ttl 30 --secret-expires fake-client-secret", "--token-ttl 30 --secret-expires " + Marker},
	})
}

// TestCredentialReferences: an object under a sensitive key that refers to
// a stored credential keeps its id, name and type; its secrets are still
// redacted, and an object that is not a reference is redacted wholesale.
func TestCredentialReferences(t *testing.T) {
	runChecks(t, `{"credentials":{"id":4,"name":"backup-s3","provider":{"type":"S3","access_key_id":"AKIAFAKE",
		"secret_access_key":"fake-secret-access-key"},"attributes":{"key":"fake-b2-key"}},
		"ssh_credentials":{"id":2,"name":"repl-conn","type":"SSH_CREDENTIALS","attributes":{"private_key":1,
		"password":"fake-ssh-pw","host":"192.0.2.20"}},
		"credential":{"credential_type":"LDAP_PLAIN","bindpw":"fake-bind-pw"},
		"token":{"type":"oauth","value":"fake-oauth-token"},
		"secrets":{"name":"db","password":"fake-db-pw","enabled":true,"length":12}}`,
		[]check{
			kept(json.Number("4"), "credentials", "id"), kept("backup-s3", "credentials", "name"),
			kept("S3", "credentials", "provider", "type"), redacted("credentials", "provider", "access_key_id"),
			redacted("credentials", "provider", "secret_access_key"), redacted("credentials", "attributes"),
			kept(json.Number("2"), "ssh_credentials", "id"), kept("repl-conn", "ssh_credentials", "name"),
			kept("SSH_CREDENTIALS", "ssh_credentials", "type"), redacted("ssh_credentials", "attributes"),
			// Not references (no id or name): redacted wholesale.
			redacted("credential"), redacted("token"),
			kept("db", "secrets", "name"), redacted("secrets", "password"), kept(true, "secrets", "enabled"),
			redacted("secrets", "length"),
		})
}

// TestTextCommandLineOptions covers passwords given as command-line
// options in free text.
func TestTextCommandLineOptions(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"rclone --s3-secret-access-key fake-secret-access-key --s3-region us-east-1",
			"rclone --s3-secret-access-key " + Marker + " --s3-region us-east-1"},
		{"cmd --password fake-user-pw --user bob", "cmd --password " + Marker + " --user bob"},
		{`cmd --password "fake user pw" -v`, `cmd --password "` + Marker + `" -v`},
		{"cmd --password=fake-user-pw --user bob", "cmd --password=" + Marker},
		{"cmd --password --user bob", "cmd --password --user bob"},
		{"cmd --api-key fake-api-key-0000", "cmd --api-key " + Marker},
		{"cmd --name tank --recursive", "cmd --name tank --recursive"},
		{"cmd --public-key ssh-ed25519", "cmd --public-key ssh-ed25519"},
		{"sshpass -p fake-ssh-pw ssh -p 22 root@192.0.2.20", "sshpass -p " + Marker + " ssh -p 22 root@192.0.2.20"},
		{"sshpass -pfake-ssh-pw rsync -a src dst", "sshpass -p" + Marker + " rsync -a src dst"},
		{"sshpass -e -p 'fake ssh pw' ssh host", "sshpass -e -p '" + Marker + "' ssh host"},
		{"sshpass -f /root/pw ssh -p 22 host", "sshpass -f /root/pw ssh -p 22 host"},
		{"mysql -u root -pfake-db-pw appdb", "mysql -u root -p" + Marker + " appdb"},
		{"/usr/bin/mysqldump -h db -pfake-db-pw --all-databases", "/usr/bin/mysqldump -h db -p" + Marker + " --all-databases"},
		{"mariadb -u root -p appdb", "mariadb -u root -p appdb"},
		{"mkdir -p /mnt/tank/x && ssh -p 22 host", "mkdir -p /mnt/tank/x && ssh -p 22 host"},
	})
}

// TestArgvArrays covers argv arrays in JSON results (job arguments,
// container commands).
func TestArgvArrays(t *testing.T) {
	runChecks(t, `{"command":["rclone","sync","--s3-secret-access-key","fake-secret-access-key","--s3-region","us-east-1"],
		"args":["--password","fake-user-pw","--password=fake-user-pw","--user","bob","--token","--verbose"],
		"ssh":["sshpass","-p","fake-ssh-pw","ssh","-p","22","host"],
		"ssh2":["/usr/bin/sshpass","-pfake-ssh-pw","ssh","host"],
		"db":["mysql","-u","root","-pfake-db-pw","appdb"],
		"mk":["mkdir","-p","/mnt/tank/x"],
		"ttl":["--token-ttl","30"]}`,
		[]check{
			kept("--s3-secret-access-key", "command", 2), redacted("command", 3), kept("us-east-1", "command", 5),
			redacted("args", 1), kept("--password="+Marker, "args", 2), kept("bob", "args", 4), kept("--verbose", "args", 6),
			kept("-p", "ssh", 1), redacted("ssh", 2), kept("22", "ssh", 5), kept("host", "ssh", 6),
			kept("-p"+Marker, "ssh2", 1), kept("host", "ssh2", 3),
			kept("-p"+Marker, "db", 3), kept("appdb", "db", 4),
			kept("/mnt/tank/x", "mk", 2), kept("30", "ttl", 1),
		})
}

// TestTextYAML covers secrets in YAML text held in a string: Kubernetes
// style env lists and block scalars.
func TestTextYAML(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"env:\n- name: DB_PASS\n  value: fake-db-pw\n- name: TZ\n  value: UTC\n",
			"env:\n- name: DB_PASS\n  value: " + Marker + "\n- name: TZ\n  value: UTC\n"},
		{"containers:\n  - env:\n      - value: \"fake-db-pw\"\n        name: POSTGRES_PASSWORD\n      - name: LOG_LEVEL\n        value: debug\n",
			"containers:\n  - env:\n      - value: \"" + Marker + "\"\n        name: POSTGRES_PASSWORD\n      - name: LOG_LEVEL\n        value: debug\n"},
		{"- name: API_TOKEN_TTL\n  value: 300\n- name: API_TOKEN\n  valueFrom:\n    fieldRef: {fieldPath: x}\n",
			"- name: API_TOKEN_TTL\n  value: 300\n- name: API_TOKEN\n  valueFrom:\n    fieldRef: {fieldPath: x}\n"},
		{"tls:\n  private_key: |\n    fake-hex-key-0000\n    fake-hex-key-0000\n  cert: abc\n",
			"tls:\n  private_key: " + Marker + "\n  cert: abc\n"},
		{"config:\n  notes: |\n    hello\n  password: >-\n    fake-user-pw\n\nnext: 1\n",
			"config:\n  notes: |\n    hello\n  password: " + Marker + "\n\nnext: 1\n"},
		{"- name: DB_PASS\n  value: |\n    fake-db-pw\n- name: TZ\n  value: UTC",
			"- name: DB_PASS\n  value: " + Marker + "\n- name: TZ\n  value: UTC"},
		{"env: [{name: DB_PASS, value: fake-db-pw}, {name: TZ, value: UTC}]",
			"env: [{name: DB_PASS, value: " + Marker + "}, {name: TZ, value: UTC}]"},
		{"failed: {'name': 'DB_PASS', 'value': 'fake-db-pw'}", "failed: {'name': 'DB_PASS', 'value': '" + Marker + "'}"},
		{"failed: {'value': 'fake-db-pw', 'name': 'API_TOKEN'}", "failed: {'value': '" + Marker + "', 'name': 'API_TOKEN'}"},
		{"pool:\n  name: tank\n  value: 3\n", "pool:\n  name: tank\n  value: 3\n"},
	})
	// The Kubernetes form inside a JSON string field.
	runChecks(t, `{"manifest":"spec:\n  containers:\n  - name: app\n    env:\n    - name: DB_PASS\n      value: fake-db-pw\n"}`,
		[]check{kept("spec:\n  containers:\n  - name: app\n    env:\n    - name: DB_PASS\n      value: "+Marker+"\n", "manifest")})
}

func TestAdditionalKeyNames(t *testing.T) {
	for _, k := range []string{"pass2", "PASS2", "pw2", "password2", "dbpass2", "wg_private", "wgPrivate",
		"private_key", "wg_private_key", "privkey", "ssh_privkey", "preshared", "preshared_key", "presharedKey",
		"wg_preshared_key", "peer_preshared"} {
		if !IsSensitiveKey(k) {
			t.Errorf("IsSensitiveKey(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"pass_count", "private", "public_key", "passthrough2", "bypass2"} {
		if IsSensitiveKey(k) {
			t.Errorf("IsSensitiveKey(%q) = true, want false", k)
		}
	}
	out := ToolOutput(`{"wireguard":{"private_key":"fake-hex-key-0000","public_key":"pub","peers":[{"preshared_key":"fake-hex-key-0000","endpoint":"192.0.2.1"}]},"ftp":{"pass2":"fake-ftp-pw"}}`)
	assertNoSecrets(t, out)
	if !strings.Contains(out, `"public_key":"pub"`) || !strings.Contains(out, "192.0.2.1") {
		t.Errorf("over-redacted: %s", out)
	}
}
