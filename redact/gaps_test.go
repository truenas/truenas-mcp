package redact

import (
	"reflect"
	"strings"
	"testing"
)

// The tests in this file pin the gaps of command-line redaction that remained
// after the quoted-separator work: sshpass scoping, escaped wrappers, bare
// carriage returns and quoted values after a user name.

// TestTextSshpassScope: sshpass reads its options like getopt (clusters,
// attached values) up to the program it runs, and only its own password
// options are redacted.
func TestTextSshpassScope(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"sshpass -vp fake-ssh-pw ssh h", "sshpass -vp " + Marker + " ssh h"},
		{"sshpass -vpfake-ssh-pw ssh h", "sshpass -vp" + Marker + " ssh h"},
		{"sshpass -ep fake-ssh-pw ssh h", "sshpass -ep " + Marker + " ssh h"},
		{"sshpass -v -p fake-ssh-pw -- ssh h", "sshpass -v -p " + Marker + " -- ssh h"},
		{"sshpass -d 3 -P assword -p fake-ssh-pw scp -P 2222 f u@h:", "sshpass -d 3 -P assword -p " + Marker + " scp -P 2222 f u@h:"},
		{"sshpass -f /tmp/f -p fake-ssh-pw ssh h", "sshpass -f /tmp/f -p " + Marker + " ssh h"},
		{"sshpass -f/tmp/f -pfake-ssh-pw ssh h", "sshpass -f/tmp/f -p" + Marker + " ssh h"},
		{"sudo -n sshpass -vp fake-ssh-pw ssh h", "sudo -n sshpass -vp " + Marker + " ssh h"},
		{"sshpass -p 'fake ssh pw' ssh h", "sshpass -p '" + Marker + "' ssh h"},
		{"sshpass -p'fake ssh pw' ssh h", "sshpass -p'" + Marker + "' ssh h"},
		{"sshpass -vp'fake ssh pw' ssh h", "sshpass -vp'" + Marker + "' ssh h"},
		{`sshpass -p "fake ssh pw" ssh h`, `sshpass -p "` + Marker + `" ssh h`},
		// an attached quoted value with a space, plain and escaped quotes
		{`sshpass -p\"fake ssh pw\" ssh h`, `sshpass -p` + Marker + ` ssh h`},
		{`sshpass -p \"fake ssh pw\" ssh h`, `sshpass -p ` + Marker + ` ssh h`},
		{`sh -c "sshpass -p\"fake ssh pw\" ssh h"`, `sh -c "sshpass -p` + Marker + ` ssh h"`},
		{`{"cmd": "sshpass -p\"fake ssh pw\" ssh h"}`, `{"cmd": "sshpass -p` + Marker + ` ssh h"}`},
		{`{"cmd": "sshpass -p\"fakesshpw\" ssh h"}`, `{"cmd": "sshpass -p` + Marker + ` ssh h"}`},
		{"sh -c 'sshpass -vp fake-ssh-pw ssh u@h; ls'", "sh -c 'sshpass -vp " + Marker + " ssh u@h; ls'"},
		{"sh -c 'sshpass -p'\\''fake ssh pw'\\'' ssh h'", "sh -c 'sshpass -p'\\''" + Marker + "'\\'' ssh h'"},
		// The command that sshpass runs has its own options.
		{"sshpass -e ssh -p 22 h", "sshpass -e ssh -p 22 h"},
		{"sshpass -f /tmp/f ssh -p2222 h", "sshpass -f /tmp/f ssh -p2222 h"},
		{"sshpass -vp fake-ssh-pw ssh -p 22 h", "sshpass -vp " + Marker + " ssh -p 22 h"},
		{"sshpass -e scp -P 2222 f h:", "sshpass -e scp -P 2222 f h:"},
		{"sshpass -e rsync -e 'ssh -p 22' a h:", "sshpass -e rsync -e 'ssh -p 22' a h:"},
		// A separator ends sshpass's options.
		{"sshpass -e; mysql -pfake-db-pw", "sshpass -e; mysql -p" + Marker},
		{"sshpass -e ssh h | mysql -pfake-db-pw", "sshpass -e ssh h | mysql -p" + Marker},
	})
}

// TestArgvSshpassCluster: the argv form of sshpass accepts clusters too.
func TestArgvSshpassCluster(t *testing.T) {
	in := map[string]interface{}{
		"a": []interface{}{"sshpass", "-vp", "fake-ssh-pw", "ssh", "-p", "22", "h"},
		"b": []interface{}{"sshpass", "-vpfake-ssh-pw", "ssh", "-p", "22", "h"},
		"c": []interface{}{"sshpass", "-e", "ssh", "-p", "22", "h"},
	}
	out := Value(in).(map[string]interface{})
	want := map[string]interface{}{
		"a": []interface{}{"sshpass", "-vp", Marker, "ssh", "-p", "22", "h"},
		"b": []interface{}{"sshpass", "-vp" + Marker, "ssh", "-p", "22", "h"},
		"c": []interface{}{"sshpass", "-e", "ssh", "-p", "22", "h"},
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("Value = %#v\nwant %#v", out, want)
	}
}

// TestTextOuterEscapedWrappers: a command inside a string that is escaped
// again (JSON text, sh -c "...") has its separators and quotes escaped, so a
// lone quote or an escaped separator before the option does not hide it.
func TestTextOuterEscapedWrappers(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{`mysql -e 'a|b' \\| -u root -pfake-db-pw db`, `mysql -e 'a|b' \\| -u root -p` + Marker + ` db`},
		{`mysql -e 'a|b' \\; -u root -pfake-db-pw db`, `mysql -e 'a|b' \\; -u root -p` + Marker + ` db`},
		{`mysql -e 'a|b' '\\'' -u root -pfake-db-pw db`, `mysql -e 'a|b' '\\'' -u root -p` + Marker + ` db`},
		{`{"cmd":"mysql -e \"a'|b\" \\| -pfake-db-pw db"}`, `{"cmd":"mysql -e \"a'|b\" \\| -p` + Marker + ` db"}`},
		{`{"cmd":"mysql -e \"a'|b\" \\; -pfake-db-pw db"}`, `{"cmd":"mysql -e \"a'|b\" \\; -p` + Marker + ` db"}`},
		{`{"cmd":"mysql -e \"a'|b\" '\\'' -pfake-db-pw db"}`, `{"cmd":"mysql -e \"a'|b\" '\\'' -p` + Marker + ` db"}`},
		{`{"cmd":"curl -e \"a;b\" \\| -u bob:fake-user-pw https://h/"}`, `{"cmd":"curl -e \"a;b\" \\| -u bob:` + Marker + ` https://h/"}`},
		{`{"cmd":"smbclient //h/s -c \"a|b\" \\; -U bob%fake-smb-pw"}`, `{"cmd":"smbclient //h/s -c \"a|b\" \\; -U bob%` + Marker + `"}`},
		{`{"cmd":"ipmitool -H h \\| -P fake-user-pw power on"}`, `{"cmd":"ipmitool -H h \\| -P ` + Marker + ` power on"}`},
		{`{"cmd":"sh -c \"mysql -e \\\"a|b\\\" \\\\| -pfake-db-pw\""}`, `{"cmd":"sh -c \"mysql -e \\\"a|b\\\" \\\\| -p` + Marker + `"}`},
		// A real separator still ends the command.
		{`mysql -u root | grep -px`, `mysql -u root | grep -px`},
		{`mysql -u root; grep -px`, `mysql -u root; grep -px`},
	})
}

// TestTextBareCarriageReturn: a bare carriage return between the words of a
// command does not end it when an option follows, or when an option is
// waiting for its value. Elsewhere it still starts a new line of output.
func TestTextBareCarriageReturn(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"curl \r -u a:fake-user-pw https://h/", "curl \r -u a:" + Marker + " https://h/"},
		{"curl\r-u a:fake-user-pw https://h/", "curl\r-u a:" + Marker + " https://h/"},
		{"curl -s\r -u\r a:fake-user-pw https://h/", "curl -s\r -u\r a:" + Marker + " https://h/"},
		{"curl -s --user\r a:fake-user-pw https://h/", "curl -s --user\r a:" + Marker + " https://h/"},
		{"mysql -u root\r-pfake-db-pw db", "mysql -u root\r-p" + Marker + " db"},
		{"smbclient //h/s\r-U adm%fake-smb-pw", "smbclient //h/s\r-U adm%" + Marker},
		{"smbclient //h/s -U\r adm%fake-smb-pw", "smbclient //h/s -U\r adm%" + Marker},
		{"ipmitool -H h\r -P\r fake-user-pw power on", "ipmitool -H h\r -P\r " + Marker + " power on"},
		{"sh -c 'curl\r -u a:fake-user-pw https://h/'", "sh -c 'curl\r -u a:" + Marker + " https://h/'"},
		// A carriage return that ends the output line, or a line feed after it.
		{"mysql -u root\rgrep -px", "mysql -u root\rgrep -px"},
		{"mysql -pfake-db-pw\rgrep -px", "mysql -p" + Marker + "\rgrep -px"},
		{"mysql -u root\r\ngrep -px", "mysql -u root\r\ngrep -px"},
		{"curl -s\r\n-u a:b", "curl -s\r\n-u a:b"},
	})
}

// TestTextQuotedValueAfterUser: the secret after user: or user% may be
// quoted, including with the quote idiom of sh -c '...'; all of it is redacted.
func TestTextQuotedValueAfterUser(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"curl -u adm:'fake user pw' https://h/", "curl -u adm:'" + Marker + "' https://h/"},
		{`curl -u adm:"fake user pw" https://h/`, `curl -u adm:"` + Marker + `" https://h/`},
		{"sh -c 'curl -u adm:'\\''fake user pw'\\'' https://h/'", "sh -c 'curl -u adm:'\\''" + Marker + "'\\'' https://h/'"},
		{"sh -c 'curl -u adm:'\\''fake-user-pw'\\'''", "sh -c 'curl -u adm:'\\''" + Marker + "'\\'''"},
		{"sh -c 'smbclient //h/s -U adm%'\\''fake smb pw'\\'''", "sh -c 'smbclient //h/s -U adm%'\\''" + Marker + "'\\'''"},
		{"sh -c 'curl --user adm:'\\''fake user pw'\\'' https://h/'", "sh -c 'curl --user adm:'\\''" + Marker + "'\\'' https://h/'"},
		{`sh -c "curl -u adm:\"fake user pw\" https://h/"`, `sh -c "curl -u adm:\"` + Marker + `\" https://h/"`},
		{"curl -u adm:fake-user-pw https://h/", "curl -u adm:" + Marker + " https://h/"},
		{"curl -u adm https://h/", "curl -u adm https://h/"},
	})
}

// TestGapsNeverLeak: none of the quoting wrappers around the secret may
// leave it visible, for every program and wrapper in the table.
func TestGapsNeverLeak(t *testing.T) {
	const secret = "fake-gap-pw"
	cmds := []string{
		"mysql -u root -p" + secret + " db",
		"curl -s -u adm:" + secret + " https://h/",
		"smbclient //h/s -U adm%" + secret,
		"ipmitool -H h -P " + secret + " power on",
		"sshpass -vp " + secret + " ssh h",
		"sshpass -p" + secret + " ssh h",
	}
	wrap := []func(string) string{
		func(s string) string { return s },
		func(s string) string { return "sh -c '" + strings.ReplaceAll(s, "'", `'\''`) + "'" },
		func(s string) string { return jsonEscape(`sh -c "` + dqEscape(s) + `"`) },
	}
	for _, c := range cmds {
		for _, pre := range []string{"", "echo 'a|b' \\\\| ", "x -e 'a|b'\r ", `curl -e "a;b" '\\'' `} {
			for _, w := range wrap {
				in := w(pre + c)
				if out := Text(in); strings.Contains(out, secret) {
					t.Errorf("Text(%q) = %q leaks the secret", in, out)
				}
			}
		}
	}
}

// TestTextStrayEscapedQuoteIsSafe: an escaped quote that is never closed
// swallows the separators after it, so an option that follows is read as an
// option of the program in front. That can hide a harmless option of another
// program (ssh -p2222) but it never leaves a password visible: when the text
// is unclear, it is redacted.
func TestTextStrayEscapedQuoteIsSafe(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{`mysql -u root \" ; ssh -p2222 h`, `mysql -u root \" ; ssh -p` + Marker + ` h`},
		{`mysql -e \"a; mysql -pfake-db-pw`, `mysql -e \"a; mysql -p` + Marker},
	})
}
