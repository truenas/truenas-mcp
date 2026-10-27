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
			redacted("token"),
			// A typed credential keeps credential_type; see TestTypedCredential.
			kept("LDAP_PLAIN", "credential", "credential_type"), redacted("credential", "bindpw"),
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

// TestTextMySQLAttachedPassword: the attached password of the MySQL and
// MariaDB clients is redacted wherever it comes on the command, including
// right after the program name; a separate -p (a prompt) is left alone.
func TestTextMySQLAttachedPassword(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"mysql -pfake-db-pw appdb", "mysql -p" + Marker + " appdb"},
		{"mysqldump -pfake-db-pw --all-databases", "mysqldump -p" + Marker + " --all-databases"},
		{"mariadb-admin -pfake-db-pw ping", "mariadb-admin -p" + Marker + " ping"},
		{"mysqldump x | gzip > y && mysql -pfake-db-pw", "mysqldump x | gzip > y && mysql -p" + Marker},
		{`mysql -p"fake db pw" appdb`, `mysql -p"` + Marker + `" appdb`},
		{"mysql -p appdb", "mysql -p appdb"},
		{"mysql -p", "mysql -p"},
		{"mysql appdb | grep -p x", "mysql appdb | grep -p x"},
		// Every attached password on the same command.
		{"mysql -pfake-db-pw -pfake-user-pw appdb", "mysql -p" + Marker + " -p" + Marker + " appdb"},
		{"mysql -p -u root -pfake-db-pw", "mysql -p -u root -p" + Marker},
		{"mysql -p'a|b' -pfake-db-pw | grep -px", "mysql -p'" + Marker + "' -p" + Marker + " | grep -px"},
		{"mysql -pfake-db-pw ; grep -px", "mysql -p" + Marker + " ; grep -px"},
	})
}

// TestTextEveryProgramOption: every password option on a matched command
// is redacted, not only the first.
func TestTextEveryProgramOption(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"curl -u a:fake-user-pw --proxy-user b:fake-db-pw https://h/",
			"curl -u a:" + Marker + " --proxy-user b:" + Marker + " https://h/"},
		{"curl -u a:fake-user-pw https://h/ | curl -u b https://h/",
			"curl -u a:" + Marker + " https://h/ | curl -u b https://h/"},
		{"ipmitool -P fake-db-pw -H h -P fake-user-pw", "ipmitool -P " + Marker + " -H h -P " + Marker},
		{"smbclient -U a%fake-smb-pw -U b%fake-db-pw", "smbclient -U a%" + Marker + " -U b%" + Marker},
	})
}

// TestMetadataValueMustEnd: a metadata value followed by more than a
// separator (token_ttl=5'x) is not a clean scalar and is redacted.
func TestMetadataValueMustEnd(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"token_ttl=5'fake-oauth-token", "token_ttl=" + Marker},
		{`token_ttl="5"fake-oauth-token`, "token_ttl=" + Marker},
		{"token_ttl: 5\\fake-oauth-token", "token_ttl: " + Marker},
		{`token_ttl="5" next`, `token_ttl="5" next`},
		{"token_ttl=5, other=1", "token_ttl=5, other=1"},
	})
}

// TestTextFlowPairsCommasAndExtraKeys: an unquoted value with commas is
// redacted up to the next "key:" entry, and other entries may come between
// the name and the value.
func TestTextFlowPairsCommasAndExtraKeys(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"{name: DB_PASS, value: fake-db-pw,with,commas}", "{name: DB_PASS, value: " + Marker + "}"},
		{"{name: DB_PASS, value: fake-db-pw,with,commas, other: 1}", "{name: DB_PASS, value: " + Marker + ", other: 1}"},
		{"{name: DB_PASS, description: x, value: fake-db-pw}", "{name: DB_PASS, description: x, value: " + Marker + "}"},
		{"{name: TZ, description: x, value: UTC}, {name: DB_PASS, value: fake-db-pw}",
			"{name: TZ, description: x, value: UTC}, {name: DB_PASS, value: " + Marker + "}"},
		{"{value: fake-db-pw,with,commas, name: DB_PASS}", "{value: " + Marker + ", name: DB_PASS}"},
		{`[{'name': 'DB_PASS', 'value': 'a\'b fake-db-pw'}]`, `[{'name': 'DB_PASS', 'value': '` + Marker + `'}]`},
		{"{name: TZ, value: a,b,c}", "{name: TZ, value: a,b,c}"},
	})
}

// TestTextYAMLInsideBlockScalars: YAML embedded in a non-sensitive block
// scalar (a ConfigMap's "app.yaml: |", notes) is scanned too.
func TestTextYAMLInsideBlockScalars(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"data:\n  app.yaml: |\n    env:\n      - name: DB_PASS\n        value: fake-db-pw\n  other: x\n",
			"data:\n  app.yaml: |\n    env:\n      - name: DB_PASS\n        value: " + Marker + "\n  other: x\n"},
		{"notes: |\n  - name: DB_PASS\n    value: fake-db-pw\n", "notes: |\n  - name: DB_PASS\n    value: " + Marker + "\n"},
		{"cfg: |\n  password: fake-user-pw\n  user: bob\n", "cfg: |\n  password: " + Marker + "\n  user: bob\n"},
		{"cfg: |\n  ssh_key: |\n    fake-hex-key-0000\n    fake-hex-key-0000\n  user: bob\n",
			"cfg: |\n  ssh_key: " + Marker + "\n  user: bob\n"},
		{"notes: |\n  - name: TZ\n    value: UTC\n", "notes: |\n  - name: TZ\n    value: UTC\n"},
	})
}

// TestCredentialReferenceTypeFields: only known type fields with enum-like
// values stay visible in a credential reference.
func TestCredentialReferenceTypeFields(t *testing.T) {
	runChecks(t, `{"credentials":{"id":3,"type":"S3","provider_type":"AWS","credential_type":"LDAP_PLAIN",
		"auth_type":"KEY","key_type":"fake-b2-key","value_type":"fake-api-key-0000"},
		"credential":{"id":4,"type":"fake user pw with spaces"}}`,
		[]check{
			kept(json.Number("3"), "credentials", "id"), kept("S3", "credentials", "type"),
			kept("AWS", "credentials", "provider_type"), kept("LDAP_PLAIN", "credentials", "credential_type"),
			kept("KEY", "credentials", "auth_type"), redacted("credentials", "key_type"),
			redacted("credentials", "value_type"), redacted("credential", "type"),
		})
}

// TestTextOptionValues: quoted values with escaped quotes, and values that
// start with '-' but are not options.
func TestTextOptionValues(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{`cmd --password "a\"b fake-user-pw" next`, `cmd --password "` + Marker + `" next`},
		{"cmd --password -fake-user-pw next", "cmd --password " + Marker + " next"},
		{"cmd --password a\\ fake-user-pw next", "cmd --password " + Marker + " next"},
		{"cmd --password -v next", "cmd --password -v next"},
		{"cmd --password --verbose", "cmd --password --verbose"},
		{"cmd --password - next", "cmd --password - next"},
	})
	runChecks(t, `{"a":["tool","--password","-fake-user-pw"],"b":["tool","--password","-v"]}`,
		[]check{redacted("a", 2), kept("-v", "b", 2)})
}

// TestMetadataDatesDurationsAlgorithms: metadata keys show dates, durations
// and small numbers written as strings; algorithm keys show identifiers.
func TestMetadataDatesDurationsAlgorithms(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"password_expires: 2026-12-01", "password_expires: 2026-12-01"},
		{"secret_expires=2026-10-26T00:00:00Z user=bob", "secret_expires=2026-10-26T00:00:00Z user=bob"},
		{"TOKEN_TTL=1h", "TOKEN_TTL=1h"},
		{`token_lifetime: "1h"`, `token_lifetime: "1h"`},
		{`PASSWORD_LENGTH="12"`, `PASSWORD_LENGTH="12"`},
		{"JWT_SECRET_ALGORITHM=HS256", "JWT_SECRET_ALGORITHM=HS256"},
		{"x JWT_SECRET_ALGORITHM=HS256 y", "x JWT_SECRET_ALGORITHM=HS256 y"},
		{"--token-ttl 1h30m --secret-expires P30D", "--token-ttl 1h30m --secret-expires P30D"},
		{"password_expires: fake-user-pw", "password_expires: " + Marker},
		{"TOKEN_TTL=fake-oauth-token", "TOKEN_TTL=" + Marker},
		{"JWT_SECRET_ALGORITHM='fake user pw'", "JWT_SECRET_ALGORITHM='" + Marker + "'"},
	})
	runChecks(t, `{"password_expires":"2026-12-01","token_ttl":"1h","password_length":"12",
		"jwt_secret_algorithm":"HS256","secret_expires":"fake-client-secret","token_lifetime":"fake oauth token"}`,
		[]check{
			kept("2026-12-01", "password_expires"), kept("1h", "token_ttl"), kept("12", "password_length"),
			kept("HS256", "jwt_secret_algorithm"), redacted("secret_expires"), redacted("token_lifetime"),
		})
}

// TestOptionsNamingFilesOrNegations: --password-file, --passphrase-file and
// --no-password do not take the secret itself.
func TestOptionsNamingFilesOrNegations(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"restic --password-file /etc/restic/pw backup", "restic --password-file /etc/restic/pw backup"},
		{"gpg --passphrase-file /root/pp -d x", "gpg --passphrase-file /root/pp -d x"},
		{"pg_dump --no-password appdb", "pg_dump --no-password appdb"},
		{"cmd --password-stdin registry", "cmd --password-stdin registry"},
		{"cmd --passphrase fake-passphrase", "cmd --passphrase " + Marker},
		{"cmd --ask-password --skip-password x", "cmd --ask-password --skip-password x"},
		// Only no, skip, without, disable and ask negate, and only when the
		// value is a separate word; an attached value is redacted.
		{"cmd --allow-password fake-db-pw", "cmd --allow-password " + Marker},
		{"cmd --prompt-password fake-db-pw", "cmd --prompt-password " + Marker},
		{"cmd --no-password=fake-db-pw", "cmd --no-password=" + Marker},
		{"cmd --disable-password=fake-db-pw", "cmd --disable-password=" + Marker},
		{"cmd --password-file=/etc/pw", "cmd --password-file=/etc/pw"},
	})
	runChecks(t, `{"c":["cmd","--allow-password","fake-db-pw"],"d":["cmd","--no-password=fake-db-pw"]}`,
		[]check{redacted("c", 2), kept("--no-password="+Marker, "d", 1)})
	runChecks(t, `{"a":["restic","--password-file","/etc/restic/pw"],"b":["pg_dump","--no-password","appdb"]}`,
		[]check{kept("/etc/restic/pw", "a", 2), kept("appdb", "b", 2)})
}

// TestPrivateLastWord: "private" or "priv" as the last word of a key is
// private key material (client_private, peer_private, x_priv), except
// after a word that makes it a flag or a network name (is_private,
// network_private, dhcp_private). Under these keys, boolean values stay
// visible; other values are redacted.
func TestPrivateLastWord(t *testing.T) {
	for _, k := range []string{"network_private", "dhcp_private", "is_private", "private"} {
		if IsSensitiveKey(k) {
			t.Errorf("IsSensitiveKey(%q) = true", k)
		}
	}
	for _, k := range []string{"wg_private", "ssh_private", "tls_private", "host_priv", "private_key",
		"client_private", "server_private", "peer_private", "node_private", "x_priv", "PeerPrivate"} {
		if !IsSensitiveKey(k) {
			t.Errorf("IsSensitiveKey(%q) = false", k)
		}
	}
	runChecks(t, `{"client_private":"fake-hex-key-0000","server_private":"fake-hex-key-0000",`+
		`"peer_private":true,"node_private":12345,"x_priv":"fake-hex-key-0000","is_private":true,`+
		`"network_private":"10.0.0.0/8","node_priv":"yes"}`, []check{
		redacted("client_private"), redacted("server_private"), kept(true, "peer_private"),
		redacted("node_private"), redacted("x_priv"), kept(true, "is_private"),
		kept("10.0.0.0/8", "network_private"), kept("yes", "node_priv"),
	})
	checkText(t, []struct{ in, want string }{
		{"network_private: true dhcp_private: 10.0.0.0/8", "network_private: true dhcp_private: 10.0.0.0/8"},
		{"client_private: fake-hex-key-0000", "client_private: " + Marker},
		{"peer_private=fake-hex-key-0000", "peer_private=" + Marker},
		{"x_priv=12345", "x_priv=" + Marker},
		{"server_private: false node_private: no", "server_private: false node_private: no"},
		{"server_private: 5", "server_private: " + Marker},
	})
}

// TestTextClientCredentialOptions: smbclient -U user%pass, curl -u
// user:pass and ipmitool -P pass, keeping the user name.
func TestTextClientCredentialOptions(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"smbclient //srv/share -U bob%fake-smb-pw", "smbclient //srv/share -U bob%" + Marker},
		{"smbclient //srv/share -Ubob%fake-smb-pw -c ls", "smbclient //srv/share -Ubob%" + Marker + " -c ls"},
		{"smbclient //srv/share --user=bob%fake-smb-pw", "smbclient //srv/share --user=bob%" + Marker},
		{"rpcclient -U 'DOM\\bob%fake smb pw' srv", "rpcclient -U 'DOM\\bob%" + Marker + "' srv"},
		{"smbclient //srv/share -U bob", "smbclient //srv/share -U bob"},
		{"curl -u bob:fake-user-pw https://192.0.2.1/", "curl -u bob:" + Marker + " https://192.0.2.1/"},
		{"curl --user bob:fake-user-pw https://h/", "curl --user bob:" + Marker + " https://h/"},
		{"curl --user=bob:fake-user-pw https://h/", "curl --user=bob:" + Marker + " https://h/"},
		{"curl -ubob:fake-user-pw https://h/", "curl -ubob:" + Marker + " https://h/"},
		{"curl -s -u 'bob:fake user pw' https://h/", "curl -s -u 'bob:" + Marker + "' https://h/"},
		{"curl -u bob https://h/", "curl -u bob https://h/"},
		{"ipmitool -I lanplus -H 192.0.2.5 -U admin -P fake-user-pw chassis status",
			"ipmitool -I lanplus -H 192.0.2.5 -U admin -P " + Marker + " chassis status"},
		{"ipmitool -Pfake-user-pw power on", "ipmitool -P" + Marker + " power on"},
		{"ipmitool -E power on", "ipmitool -E power on"},
	})
	runChecks(t, `{"smb":["smbclient","//s/x","-U","bob%fake-smb-pw"],"smb2":["smbclient","//s/x","-Ubob%fake-smb-pw"],
		"smb3":["/usr/bin/smbclient","--user=bob%fake-smb-pw"],"smb4":["smbclient","-U","bob"],
		"curl":["curl","-u","bob:fake-user-pw","https://h"],"curl2":["curl","--user=bob:fake-user-pw"],
		"ipmi":["/usr/bin/ipmitool","-U","admin","-P","fake-user-pw","power","on"],"ipmi2":["ipmitool","-Pfake-user-pw"],
		"db":["mysql","-pfake-db-pw","appdb"],"db2":["mysql","-p","appdb"]}`,
		[]check{
			kept("bob%"+Marker, "smb", 3), kept("-Ubob%"+Marker, "smb2", 2), kept("--user=bob%"+Marker, "smb3", 1),
			kept("bob", "smb4", 2), kept("bob:"+Marker, "curl", 2), kept("--user=bob:"+Marker, "curl2", 1),
			kept("admin", "ipmi", 2), redacted("ipmi", 4), kept("power", "ipmi", 5), kept("-P"+Marker, "ipmi2", 1),
			kept("-p"+Marker, "db", 1), kept("appdb", "db", 2), kept("-p", "db2", 1), kept("appdb", "db2", 2),
		})
}

// TestTextURLWithEscapedSlashes: a URL inside JSON text with escaped
// slashes still has its password redacted.
func TestTextURLWithEscapedSlashes(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{`postgres:\/\/u:p@fake-db-pw@db.example\/app`, `postgres:\/\/u:` + Marker + `@db.example\/app`},
		{"postgres://u:p@fake-db-pw@db.example/app", "postgres://u:" + Marker + "@db.example/app"},
	})
}

// TestTypedCredential: the object under a "credential" key that has a
// credential_type shows only its allowlisted non-secret fields; every other
// field is redacted. Any other value under a credential key keeps being
// redacted wholesale.
func TestTypedCredential(t *testing.T) {
	runChecks(t, `{
		"user": {"credential": {"credential_type":"KERBEROS_USER","username":"admin","password":"fake-user-pw"}},
		"principal": {"credential": {"credential_type":"KERBEROS_PRINCIPAL","principal":"host/nas.example.com@EXAMPLE.COM"}},
		"plain": {"credential": {"credential_type":"LDAP_PLAIN","binddn":"cn=admin,dc=example,dc=com","bindpw":"fake-bind-pw"}},
		"anon": {"credential": {"credential_type":"LDAP_ANONYMOUS"}},
		"mtls_id": {"credential": {"credential_type":"LDAP_MTLS","client_certificate":7}},
		"mtls_name": {"credential": {"credential_type":"LDAP_MTLS","client_certificate":"nas-cert"}},
		"mtls_obj": {"credential": {"credential_type":"LDAP_MTLS","client_certificate":{"id":7,"name":"nas-cert","privatekey":"fake-hex-key-0000","certificate":"fake-b2-key"}}},
		"unknown": {"credential": {"credential_type":"LDAP_PLAIN","binddn":"cn=a","extra":"fake-oauth-token","count":5,"nested":{"x":"fake-client-secret"},"list":["fake-ssh-pw"],"flag":true,"empty":""}},
		"badtype": {"credential": {"credential_type":"fake-totp-secret with spaces","username":"admin"}},
		"obj_username": {"credential": {"credential_type":"KERBEROS_USER","username":{"v":"fake-db-pw"}}},
		"inline_text": {"credential": {"credential_type":"KERBEROS_USER","username":"admin password=fake-smb-pw"}},
		"with_id": {"credential": {"credential_type":"KERBEROS_USER","id":3,"name":"svc","username":"admin","password":"fake-vnc-pw"}},
		"other": {"auth_credential": {"credential_type":"KERBEROS_USER","username":"admin","password":"fake-ftp-pw"}},
		"no_type": {"credential": {"username":"admin","password":"fake-user-pw"}},
		"string": {"credential": "fake-passphrase"},
		"list": {"credential": [{"credential_type":"KERBEROS_USER","username":"admin"}]},
		"empty": {"credential": {}}}`,
		[]check{
			kept("KERBEROS_USER", "user", "credential", "credential_type"), kept("admin", "user", "credential", "username"),
			redacted("user", "credential", "password"),
			kept("KERBEROS_PRINCIPAL", "principal", "credential", "credential_type"),
			kept("host/nas.example.com@EXAMPLE.COM", "principal", "credential", "principal"),
			kept("cn=admin,dc=example,dc=com", "plain", "credential", "binddn"), redacted("plain", "credential", "bindpw"),
			kept("LDAP_ANONYMOUS", "anon", "credential", "credential_type"),
			kept(json.Number("7"), "mtls_id", "credential", "client_certificate"),
			kept("nas-cert", "mtls_name", "credential", "client_certificate"),
			kept(json.Number("7"), "mtls_obj", "credential", "client_certificate", "id"),
			kept("nas-cert", "mtls_obj", "credential", "client_certificate", "name"),
			redacted("mtls_obj", "credential", "client_certificate", "privatekey"),
			redacted("mtls_obj", "credential", "client_certificate", "certificate"),
			// Unknown inner fields stay redacted; booleans and empty strings are not secrets.
			kept("LDAP_PLAIN", "unknown", "credential", "credential_type"), kept("cn=a", "unknown", "credential", "binddn"),
			redacted("unknown", "credential", "extra"), redacted("unknown", "credential", "count"),
			redacted("unknown", "credential", "nested"), redacted("unknown", "credential", "list"),
			kept(true, "unknown", "credential", "flag"), kept("", "unknown", "credential", "empty"),
			// A credential_type that is not an enum value, or an allowlisted field that is not
			// a scalar, is redacted; allowlisted text is still scrubbed.
			redacted("badtype", "credential", "credential_type"), kept("admin", "badtype", "credential", "username"),
			redacted("obj_username", "credential", "username"),
			kept("admin password="+Marker, "inline_text", "credential", "username"),
			kept(json.Number("3"), "with_id", "credential", "id"), kept("svc", "with_id", "credential", "name"),
			redacted("with_id", "credential", "password"),
			// Other shapes keep today's behaviour.
			redacted("other", "auth_credential"), redacted("no_type", "credential"), redacted("string", "credential"), redacted("list", "credential"),
			kept(map[string]interface{}{}, "empty", "credential"),
		})
}

// TestTypedCredentialJSONString: a JSON document held in a string is
// redacted the same way.
func TestTypedCredentialJSONString(t *testing.T) {
	out := ToolOutput(`{"args":"{\"credential\":{\"credential_type\":\"KERBEROS_USER\",\"username\":\"admin\",\"password\":\"fake-user-pw\"}}"}`)
	assertNoSecrets(t, out)
	for _, want := range []string{"KERBEROS_USER", "admin"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing from %s", want, out)
		}
	}
}

// TestTextQuotedSeparators: a '|', ';', '&' or newline inside a quoted
// argument of another option does not end the command, so a password option
// after it is still redacted, for every supported program.
func TestTextQuotedSeparators(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		// mysql / mariadb
		{"mysql -pfake-db-pw -e 'x|y' -pfake-user-pw", "mysql -p" + Marker + " -e 'x|y' -p" + Marker},
		{`mysql -e "a;b" -pfake-db-pw`, `mysql -e "a;b" -p` + Marker},
		{`mysql -e "a&b" -pfake-db-pw db`, `mysql -e "a&b" -p` + Marker + ` db`},
		{`mariadb -e "a\"|b" -pfake-db-pw`, `mariadb -e "a\"|b" -p` + Marker},
		{"mysql -e 'a\\' -pfake-db-pw", "mysql -e 'a\\' -p" + Marker},
		{"mysqldump -r 'a|b' -r \"c;d\" -pfake-db-pw db", "mysqldump -r 'a|b' -r \"c;d\" -p" + Marker + " db"},
		{`mysql -e "a;b" -p"fake db pw" db`, `mysql -e "a;b" -p"` + Marker + `" db`},
		{`mysql -e "a;b" -p'x|y' -e 'c|d' -pfake-user-pw`, `mysql -e "a;b" -p'` + Marker + `' -e 'c|d' -p` + Marker},
		// smbclient, curl, ipmitool
		{"smbclient //srv/share -c 'ls|more' -U bob%fake-smb-pw", "smbclient //srv/share -c 'ls|more' -U bob%" + Marker},
		{`rpcclient -c "a;b" -Ubob%fake-smb-pw srv`, `rpcclient -c "a;b" -Ubob%` + Marker + ` srv`},
		{`smbclient -c "a&b" --user=bob%fake-smb-pw`, `smbclient -c "a&b" --user=bob%` + Marker},
		{`curl -H "X: a;b" -u bob:fake-user-pw https://h/`, `curl -H "X: a;b" -u bob:` + Marker + ` https://h/`},
		{`curl -d 'a=1&b=2' --proxy-user bob:fake-user-pw https://h/`, `curl -d 'a=1&b=2' --proxy-user bob:` + Marker + ` https://h/`},
		{`curl -d "a|b" --user=bob:fake-user-pw https://h/`, `curl -d "a|b" --user=bob:` + Marker + ` https://h/`},
		{`curl -d 'a|b' -ubob:fake-user-pw -d 'c;d' -U bob:fake-db-pw`, `curl -d 'a|b' -ubob:` + Marker + ` -d 'c;d' -U bob:` + Marker},
		{`ipmitool -H 'h;1' -P fake-user-pw chassis status`, `ipmitool -H 'h;1' -P ` + Marker + ` chassis status`},
		{`ipmitool -I "lan|plus" -Pfake-user-pw`, `ipmitool -I "lan|plus" -P` + Marker},
		// sshpass: quoted values of its own options, and -p inside the command's quotes
		{`sshpass -f '/tmp/a;b' -p fake-ssh-pw ssh h`, `sshpass -f '/tmp/a;b' -p ` + Marker + ` ssh h`},
		{`sshpass -P "pass;word" -pfake-ssh-pw ssh h`, `sshpass -P "pass;word" -p` + Marker + ` ssh h`},
		// the program itself inside quotes
		{`ssh h "mysql -e 'x|y' -pfake-db-pw"`, `ssh h "mysql -e 'x|y' -p` + Marker + `"`},
		{`sh -c 'mysql -e "a;b" -pfake-db-pw'`, `sh -c 'mysql -e "a;b" -p` + Marker + `'`},
		// long options with quoted separators around them
		{"cmd --note 'a|b' --password fake-user-pw --other 'c;d'", "cmd --note 'a|b' --password " + Marker + " --other 'c;d'"},
		{`cmd --password "a;b|c" --note 'x'`, `cmd --password "` + Marker + `" --note 'x'`},
		// separators outside quotes still end the command
		{"mysql -e 'x' | grep -pfake-db-pw", "mysql -e 'x' | grep -pfake-db-pw"},
		{`mysql -e "a" ; grep -pfake-db-pw`, `mysql -e "a" ; grep -pfake-db-pw`},
		{"mysql -e 'a' && grep -px", "mysql -e 'a' && grep -px"},
		{"mysql -e 'a\nb'\ngrep -px", "mysql -e 'a\nb'\ngrep -px"},
		{`mysql -e a\;b -pfake-db-pw`, `mysql -e a\;b -p` + Marker},
		{"curl -d 'a' https://h/ | curl -u bob https://h/ ; grep -u x:y", "curl -d 'a' https://h/ | curl -u bob https://h/ ; grep -u x:y"},
		// a later command is still found after quoted separators
		{`mysql -e "a;b" db; mysql -pfake-db-pw`, `mysql -e "a;b" db; mysql -p` + Marker},
	})
}

// TestTextUnbalancedQuotes: a quote that is never closed runs to the end
// of the line, so everything after it on that line is part of the command
// (and a password option there is redacted), but the next line is not.
func TestTextUnbalancedQuotes(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"mysql -e 'a|b -pfake-db-pw", "mysql -e 'a|b -p" + Marker},
		{`mysql -e "a;b -pfake-db-pw -pfake-user-pw`, `mysql -e "a;b -p` + Marker + ` -p` + Marker},
		{"mysql -e 'a|b -pfake-db-pw\nmysql -pfake-user-pw", "mysql -e 'a|b -p" + Marker + "\nmysql -p" + Marker},
		{"mysql -e 'a|b\ngrep -px", "mysql -e 'a|b\ngrep -px"},
		{`curl -d "a|b -u bob:fake-user-pw`, `curl -d "a|b -u bob:` + Marker},
		{`smbclient -c "a\" -U bob%fake-smb-pw`, `smbclient -c "a\" -U bob%` + Marker},
		// an apostrophe in text before the program does not hide it
		{"it's: mysql -pfake-db-pw", "it's: mysql -p" + Marker},
		{`echo "don't" ; mysql -pfake-db-pw`, `echo "don't" ; mysql -p` + Marker},
		// a quote that closes a quoted command leaves later commands visible to the scan
		{"sh -c 'mysql -pfake-db-pw' ; mysql -pfake-user-pw", "sh -c 'mysql -p" + Marker + "' ; mysql -p" + Marker},
		{"sh -c 'curl -u a:fake-user-pw' ; curl -u b:fake-db-pw", "sh -c 'curl -u a:" + Marker + "' ; curl -u b:" + Marker},
		// an unbalanced quote in a password option's own value
		{`mysql -p"fake-db-pw -e x | y`, `mysql -p"` + Marker},
		{`curl -u 'bob:fake user pw -d a|b`, `curl -u 'bob:` + Marker},
	})
}

// TestTextQuotedSeparatorsNoOverRedaction: ordinary text with quotes and
// separators is left alone.
func TestTextQuotedSeparatorsNoOverRedaction(t *testing.T) {
	for _, in := range []string{
		`mysql -e "select 1; select 2" db`,
		`mysql -e 'a|b' -p`,
		`mysql -e 'a|b' -p db`,
		`mysql -u root -e "show tables" | grep -p x`,
		`curl -s -H 'Accept: a;b' https://h/ | jq .`,
		`curl -d 'a=1&b=2' -u bob https://h/`,
		`smbclient //srv/share -c 'ls; pwd' -U bob`,
		`ipmitool -H 'h;1' -E chassis status`,
		`sshpass -f '/tmp/a;b' ssh h -p 22`,
		`sshpass -e ssh -o 'ProxyCommand=nc %h %p|cat' h`,
		`echo "mysql is a database; -px is not an option" | wc`,
		`echo 'the curl; command -u x:y' done`,
		`grep -e 'a|b' -pfake file; mysql -e "x;y" db`,
		"the quote ' in 'text' and \" more | mysql -e x",
	} {
		if got := Text(in); got != in {
			t.Errorf("Text(%q) = %q, want it unchanged", in, got)
		}
	}
}

// TestTextQuotedSeparatorsFollowUps pins the handling of escaped quotes,
// line continuations, separators glued to a secret, redirections, bare
// carriage returns and the quote idiom.
func TestTextQuotedSeparatorsFollowUps(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		// Escaped quotes inside an outer double-quoted string, and in raw JSON text.
		{`sh -c "mysql -e \"a;b\" -pfake-db-pw"`, `sh -c "mysql -e \"a;b\" -p` + Marker + `"`},
		{`ssh h "curl -d \"a|b\" -u bob:fake-user-pw https://h/"`, `ssh h "curl -d \"a|b\" -u bob:` + Marker + ` https://h/"`},
		{`{"cmd":"sh -c \"mysql -e \\\"a;b\\\" -pfake-db-pw\""}`, `{"cmd":"sh -c \"mysql -e \\\"a;b\\\" -p` + Marker + `"}`},
		{`sh -c "mysql -e \"\" -e \"a\\\\\" -pfake-db-pw"`, `sh -c "mysql -e \"\" -e \"a\\\\\" -p` + Marker + `"`},
		{`sh -c "mysql -e \"a;b -pfake-db-pw`, `sh -c "mysql -e \"a;b -p` + Marker},
		{`sh -c "mysql -e \"a;b\" -p\"fake db pw\" db"`, `sh -c "mysql -e \"a;b\" -p` + Marker + ` db"`},
		{"mysql -e \"a\" -pfake-db-pw\nmysql -e \\\"a;b -pfake-user-pw", "mysql -e \"a\" -p" + Marker + "\nmysql -e \\\"a;b -p" + Marker},
		{"mysql -e \"a\" -pfake-db-pw\nmysql -e \\\"a;b\ngrep -px", "mysql -e \"a\" -p" + Marker + "\nmysql -e \\\"a;b\ngrep -px"},
		// Backslash-newline continues the command.
		{"mysql -u root \\\n  -pfake-db-pw db", "mysql -u root \\\n  -p" + Marker + " db"},
		{"mysql -u root \\\r\n  -pfake-db-pw db", "mysql -u root \\\r\n  -p" + Marker + " db"},
		{"mysql -u root \\\n-pfake-db-pw db", "mysql -u root \\\n-p" + Marker + " db"},
		{"curl -d 'a;b' \\\n  --user bob:fake-user-pw \\\n  https://h/", "curl -d 'a;b' \\\n  --user bob:" + Marker + " \\\n  https://h/"},
		{"smbclient //srv/share \\\n -U bob%fake-smb-pw", "smbclient //srv/share \\\n -U bob%" + Marker},
		{"ipmitool -H h \\\n -P fake-user-pw power on", "ipmitool -H h \\\n -P " + Marker + " power on"},
		{"sshpass -e \\\n -p fake-ssh-pw ssh h", "sshpass -e \\\n -p " + Marker + " ssh h"},
		{"mysql -pfake-db-pw \\\n  db\nmysql -e x\ngrep -px", "mysql -p" + Marker + " \\\n  db\nmysql -e x\ngrep -px"},
		{"mysql db \\\n\ngrep -px", "mysql db \\\n\ngrep -px"},
		// A separator glued to the secret ends the value, and the next command is still matched.
		{"mysql -pfake-db-pw;mysql -pfake-user-pw", "mysql -p" + Marker + ";mysql -p" + Marker},
		{"mysql -pfake-db-pw|mysql -pfake-user-pw", "mysql -p" + Marker + "|mysql -p" + Marker},
		{"mysql -pfake-db-pw&&mysql -pfake-user-pw", "mysql -p" + Marker + "&&mysql -p" + Marker},
		{"mysql -pfake-db-pw;grep -px", "mysql -p" + Marker + ";grep -px"},
		{"curl -u a:fake-user-pw;curl -u b:fake-db-pw", "curl -u a:" + Marker + ";curl -u b:" + Marker},
		{"ipmitool -P fake-user-pw;ipmitool -P fake-db-pw", "ipmitool -P " + Marker + ";ipmitool -P " + Marker},
		{"sshpass -p fake-ssh-pw;sshpass -p fake-user-pw ssh h", "sshpass -p " + Marker + ";sshpass -p " + Marker + " ssh h"},
		{"smbclient -U a%fake-smb-pw;smbclient -U b%fake-db-pw", "smbclient -U a%" + Marker + ";smbclient -U b%" + Marker},
		// Redirections do not end the command.
		{"mysql -e x 2>&1 -pfake-db-pw", "mysql -e x 2>&1 -p" + Marker},
		{"mysql -e x >&2 -pfake-db-pw", "mysql -e x >&2 -p" + Marker},
		{"mysql -e x &>/dev/null -pfake-db-pw", "mysql -e x &>/dev/null -p" + Marker},
		{"mysql -e x <&3 -pfake-db-pw", "mysql -e x <&3 -p" + Marker},
		{"curl -s https://h/ 2>&1 -u bob:fake-user-pw", "curl -s https://h/ 2>&1 -u bob:" + Marker},
		{"mysql -e x & mysql -pfake-db-pw", "mysql -e x & mysql -p" + Marker},
		{"mysql -e x 2>&1 | grep -px", "mysql -e x 2>&1 | grep -px"},
		{"mysql -e x 2>&1 && grep -px", "mysql -e x 2>&1 && grep -px"},
		{"mysql -e x &\ngrep -px", "mysql -e x &\ngrep -px"},
		// A bare carriage return ends a command and an unbalanced quote, like a newline.
		{"mysql -e 'a|b\rmysql -pfake-db-pw", "mysql -e 'a|b\rmysql -p" + Marker},
		{"mysql -e 'a|b\rgrep -px", "mysql -e 'a|b\rgrep -px"},
		{"mysql -e \"a|b\rgrep -px", "mysql -e \"a|b\rgrep -px"},
		{"mysql -e x\rgrep -px", "mysql -e x\rgrep -px"},
		{"mysql -e x\r\ngrep -px", "mysql -e x\r\ngrep -px"},
		{"mysql -pfake-db-pw\rgrep -px", "mysql -p" + Marker + "\rgrep -px"},
		{"mysql -e 'a\\\rgrep -px", "mysql -e 'a\\\rgrep -px"},
		// A password in the '\'' idiom of a single-quoted sh -c string.
		{`sh -c 'mysql -p'\''fake db pw'\'' db'`, `sh -c 'mysql -p'\''` + Marker + `'\'' db'`},
		{`sh -c 'mysql -e '\''a;b'\'' -p'\''fake db pw'\'' db'`, `sh -c 'mysql -e '\''a;b'\'' -p'\''` + Marker + `'\'' db'`},
		{`sh -c 'curl -u '\''bob:fake user pw'\'' https://h/'`, `sh -c 'curl -u '\''bob:` + Marker + `'\'' https://h/'`},
		{`sh -c 'mysql -p'\''fake db pw db'`, `sh -c 'mysql -p'` + Marker + `'`},
	})
}

// TestTextQuotedSeparatorsFollowUpsNoOverRedaction: negatives for the
// escaped-quote, continuation, separator and redirection handling.
func TestTextQuotedSeparatorsFollowUpsNoOverRedaction(t *testing.T) {
	for _, in := range []string{
		"mysql -e x \\\n  db\ngrep -px",
		"mysql -e x \\\r\n  db\r\ngrep -px",
		"mysql db;grep -px",
		"mysql db;mysql -p",
		"mysql db&&mysql -p db",
		"mysql db 2>&1 | grep -px",
		"mysql db >&2; grep -px",
		"mysql db &> /dev/null; grep -px",
		`sh -c 'mysql -e '\''a;b'\'' db' ; grep -px`,
		"mysql db\rgrep -px",
		"sshpass -e ssh h;sshpass -e ssh -p 22 h",
	} {
		if got := Text(in); got != in {
			t.Errorf("Text(%q) = %q, want it unchanged", in, got)
		}
	}
}
