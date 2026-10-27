package redact

import (
	"strings"
	"testing"
)

// Tests for the review of the sshpass scan: a quoted password with a space
// that is attached to -p, a second sshpass after the quote idiom, an escaped
// separator in an option value, and a line continuation right after sshpass.

// TestTextSshpassAttachedQuoted: the password of -p"S 3CR3T" (attached) is
// redacted whole, like -p "S 3CR3T" (separate), with plain and escaped quotes
// and in the wrappers where a command is found as text: plain, sh -c "...",
// sh -c '...' and JSON.
func TestTextSshpassAttachedQuoted(t *testing.T) {
	const pw = "fake ssh pw"
	for _, sp := range []string{"", " "} { // attached, separate
		for _, opt := range []string{"-p", "-vp", "-v -p", "-f /tmp/f -p"} {
			for _, pass := range []string{"fake-ssh-pw", pw} {
				for _, q := range []string{`"`, `'`, `\"`} {
					inner := "sshpass " + opt + sp + q + pass + q + " ssh h"
					for name, in := range map[string]string{
						"plain":   inner,
						"sh -c":   `sh -c "` + dqEscape(inner) + `"`,
						"sh -c '": "sh -c '" + strings.ReplaceAll(inner, "'", `'\''`) + "'",
						"json":    `{"cmd": "` + jsonEscape(inner) + `"}`,
						"json-sh": `{"cmd": "` + jsonEscape(`sh -c "`+dqEscape(inner)+`"`) + `"}`,
					} {
						out := Text(in)
						if strings.Contains(out, "fake") || strings.Contains(out, "pw") {
							t.Errorf("%s: Text(%q) = %q: the password is visible", name, in, out)
						}
						if !strings.Contains(out, " ssh h") || strings.Count(out, Marker) != 1 {
							t.Errorf("%s: Text(%q) = %q: want one marker and the command kept", name, in, out)
						}
					}
				}
			}
		}
	}
	checkText(t, []struct{ in, want string }{
		{`sshpass -p\"fake ssh pw\" ssh h`, `sshpass -p` + Marker + ` ssh h`},
		{`sshpass -p \"fake ssh pw\" ssh h`, `sshpass -p ` + Marker + ` ssh h`},
		{`sshpass -vp\"fake ssh pw\" ssh h`, `sshpass -vp` + Marker + ` ssh h`},
		{`sshpass -p"fake ssh pw" ssh h`, `sshpass -p"` + Marker + `" ssh h`},
		{`sh -c "sshpass -p\"fake ssh pw\" ssh h"`, `sh -c "sshpass -p` + Marker + ` ssh h"`},
		{`{"cmd": "sshpass -p\"fake ssh pw\" ssh h"}`, `{"cmd": "sshpass -p` + Marker + ` ssh h"}`},
		{`{"cmd": "sh -c \"sshpass -p\\\"fake ssh pw\\\" ssh h\""}`, `{"cmd": "sh -c \"sshpass -p` + Marker + ` ssh h\""}`},
		// without a space, and the command after it keeps its options
		{`sshpass -p\"fake-ssh-pw\" ssh -p 22 h`, `sshpass -p` + Marker + ` ssh -p 22 h`},
		{`sshpass -p\"fake ssh pw\" ssh -p2222 h; ls`, `sshpass -p` + Marker + ` ssh -p2222 h; ls`},
	})
}

// TestTextSshpassSecondAfterIdiom: a quote idiom in an option value of one
// sshpass (-f 'it'\”s') does not hide the next sshpass on the line.
func TestTextSshpassSecondAfterIdiom(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{`sshpass -f 'it'\''s' ssh h; sshpass -p fake-ssh-pw ssh h`, `sshpass -f 'it'\''s' ssh h; sshpass -p ` + Marker + ` ssh h`},
		{`sshpass -f 'it'\''s' ssh h | sshpass -p fake-ssh-pw ssh h`, `sshpass -f 'it'\''s' ssh h | sshpass -p ` + Marker + ` ssh h`},
		{`for h in a b; do sshpass -f 'it'\''s' ssh $h; done; sshpass -p fake-ssh-pw ssh h`, `for h in a b; do sshpass -f 'it'\''s' ssh $h; done; sshpass -p ` + Marker + ` ssh h`},
		{`sshpass -f 'it'\''s' ssh h && sshpass -vp fake-ssh-pw ssh h`, `sshpass -f 'it'\''s' ssh h && sshpass -vp ` + Marker + ` ssh h`},
		{`sh -c 'sshpass -f '\''it'\''\'\'''\''s'\'' ssh h; sshpass -p fake-ssh-pw ssh h'`, `sh -c 'sshpass -f '\''it'\''\'\'''\''s'\'' ssh h; sshpass -p ` + Marker + ` ssh h'`},
	})
}

// TestTextSshpassEscapedSeparator: a doubled backslash and a separator (\\|,
// \\;, \\&) in a sshpass option value is an escaped separator in a wrapper,
// and does not end the options.
func TestTextSshpassEscapedSeparator(t *testing.T) {
	for _, sep := range []string{"|", ";", "&"} {
		checkText(t, []struct{ in, want string }{
			{`sh -c "sshpass -f a\\` + sep + `b -p fake-ssh-pw ssh h"`, `sh -c "sshpass -f a\\` + sep + `b -p ` + Marker + ` ssh h"`},
			{`{"cmd": "sshpass -f a\\` + sep + `b -p fake-ssh-pw ssh h"}`, `{"cmd": "sshpass -f a\\` + sep + `b -p ` + Marker + ` ssh h"}`},
			{`sshpass -vp a\\` + sep + `fake-ssh-pw ssh h`, `sshpass -vp ` + Marker + ` ssh h`},
		})
	}
	// a real separator still ends the options
	checkText(t, []struct{ in, want string }{
		{`sshpass -f a | mysql -pfake-db-pw`, `sshpass -f a | mysql -p` + Marker},
		{`sshpass -e; mysql -pfake-db-pw`, `sshpass -e; mysql -p` + Marker},
	})
}

// TestTextSshpassContinuation: a backslash and a line feed (or carriage
// return and line feed) continue the command right after sshpass, with or
// without a space before the backslash.
func TestTextSshpassContinuation(t *testing.T) {
	checkText(t, []struct{ in, want string }{
		{"sshpass \\\n -p fake-ssh-pw ssh h", "sshpass \\\n -p " + Marker + " ssh h"},
		{"sshpass\\\n -p fake-ssh-pw ssh h", "sshpass\\\n -p " + Marker + " ssh h"},
		{"sshpass\\\n-p fake-ssh-pw ssh h", "sshpass\\\n-p " + Marker + " ssh h"},
		{"sshpass\\\r\n -vp fake-ssh-pw ssh h", "sshpass\\\r\n -vp " + Marker + " ssh h"},
		{"sshpass\\\n  -f /tmp/f \\\n  -p fake-ssh-pw \\\n  ssh -p 22 h", "sshpass\\\n  -f /tmp/f \\\n  -p " + Marker + " \\\n  ssh -p 22 h"},
		// a command on the next line is not part of sshpass
		{"sshpass -e\nmysql -pfake-db-pw", "sshpass -e\nmysql -p" + Marker},
		{"sshpass\nmysql -pfake-db-pw", "sshpass\nmysql -p" + Marker},
	})
}

// TestTextSshpassMatrix: for each way to write the sshpass password option
// (attached or separate, bare, quoted, escaped-quoted; an escaped single
// quote is not in it, as the sh -c wrappers rewrite it), each option before
// it, each command after it, and each wrapper (plain text, sh -c "...", sh -c
// '...', JSON), none of the password is left visible. Where a form is not in
// this matrix the scanner has a documented gap (see the README). The matrix
// was compared with the redaction before the scan was rewritten: nothing
// that was redacted then is visible now.
func TestTextSshpassMatrix(t *testing.T) {
	const a, b = "ZQAAA", "ZQBBB"
	forms := []struct {
		name  string
		f     func(string) string
		space bool // the form can hold a password with a space
	}{
		{"bare", func(p string) string { return p }, false},
		{"sq", func(p string) string { return "'" + p + "'" }, true},
		{"dq", func(p string) string { return `"` + p + `"` }, true},
		{"esc-dq", func(p string) string { return `\"` + p + `\"` }, true},
	}
	options := []string{"", "-v ", "-f /tmp/f ", "-d 3 -P assword ", "-e "}
	flags := []string{"-p", "-vp", "-ep"}
	trails := []string{" ssh h", " ssh -p22 h", " ssh h; ls", " ssh h | grep x", " -- ssh h", ""}
	pres := []string{"", "echo ok; ", "x | ", "sudo -n "}
	wrappers := []struct {
		name string
		f    func(string) string
	}{
		{"plain", func(s string) string { return s }},
		{"sh -c", func(s string) string { return `sh -c "` + dqEscape(s) + `"` }},
		{"sh -c '", func(s string) string { return "sh -c '" + strings.ReplaceAll(s, "'", `'\''`) + "'" }},
		{"json", func(s string) string { return `{"cmd": "` + jsonEscape(s) + `"}` }},
		{"json sh -c", func(s string) string { return `{"cmd": "` + jsonEscape(`sh -c "`+dqEscape(s)+`"`) + `"}` }},
	}
	n := 0
	for _, form := range forms {
		secrets := []string{a + b}
		if form.space {
			secrets = append(secrets, a+" "+b)
		}
		for _, secret := range secrets {
			for _, attached := range []bool{true, false} {
				for _, opt := range options {
					for _, flag := range flags {
						for _, trail := range trails {
							for _, pre := range pres {
								sp := " "
								if attached {
									sp = ""
								}
								inner := pre + "sshpass " + opt + flag + sp + form.f(secret) + trail
								for _, w := range wrappers {
									in := w.f(inner)
									n++
									if out := Text(in); strings.Contains(out, a) || strings.Contains(out, b) {
										t.Errorf("%s %s attached=%v: Text(%q) = %q: the password is visible", form.name, w.name, attached, in, out)
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if n < 1000 {
		t.Errorf("the matrix has only %d cases", n)
	}
}
