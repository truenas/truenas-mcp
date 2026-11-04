package redact

import (
	"math/rand"
	"strings"
	"testing"
)

// A command separator ('&', '&&', ';', '|') glued to an unquoted secret
// argument may be part of the secret (sshpass -p ab&cd), so it does not end
// the value: the value runs to the end of the word. A quoted value still
// ends at its closing quote, and a separator after white space still ends
// the value.

func TestCLISeparatorInSecret(t *testing.T) {
	M := Marker
	checkText(t, []struct{ in, want string }{
		{"sshpass -p ab&cd x", "sshpass -p " + M + " x"},
		{"sshpass -p ab&&cd x", "sshpass -p " + M + " x"},
		{"sshpass -p ab;cd x", "sshpass -p " + M + " x"},
		{"sshpass -p ab|cd x", "sshpass -p " + M + " x"},
		{"sshpass -p ab&cd", "sshpass -p " + M},
		{"sshpass -pab&cd ssh h", "sshpass -p" + M + " ssh h"},
		{"sshpass -vp ab;cd ssh h", "sshpass -vp " + M + " ssh h"},
		{"sshpass -p 'ab&cd' ssh h", "sshpass -p '" + M + "' ssh h"},
		{`sshpass -p "ab;cd|ef" ssh h`, `sshpass -p "` + M + `" ssh h`},
		{"sshpass -p'ab&&cd' ssh h", "sshpass -p'" + M + "' ssh h"},
		{"sshpass -p ab & echo done", "sshpass -p " + M + " & echo done"},
		{"sshpass -p ab && ssh -p 22 h", "sshpass -p " + M + " && ssh -p 22 h"},
		{"mysql -pab&cd db", "mysql -p" + M + " db"},
		{"mysql -pab&&cd db", "mysql -p" + M + " db"},
		{"mysql -pab;cd db", "mysql -p" + M + " db"},
		{"mysql -pab|cd db", "mysql -p" + M + " db"},
		{"mysql -p'ab&cd' db", "mysql -p'" + M + "' db"},
		{"mysql -pab | grep x", "mysql -p" + M + " | grep x"},
		{"curl -u bob:ab&cd https://h/", "curl -u bob:" + M + " https://h/"},
		{"curl -u 'bob:ab;cd' https://h/", "curl -u 'bob:" + M + "' https://h/"},
		{"smbclient -U bob%ab|cd //s/x", "smbclient -U bob%" + M + " //s/x"},
		{"ipmitool -P ab&&cd chassis", "ipmitool -P " + M + " chassis"},
		{"x --password=ab&cd y", "x --password=" + M},
		{"cmd --password ab;cd y", "cmd --password " + M + " y"},
		{"cmd --password 'ab|cd' y", "cmd --password '" + M + "' y"},
		{`cmd --password="ab&&cd" y`, `cmd --password="` + M + `" y`},
		{"PASSWORD=ab&cd x", "PASSWORD=" + M},
		{"x DB_PASSWORD=ab;cd y", "x DB_PASSWORD=" + M},
		{"x DB_PASSWORD='ab|cd' y", "x DB_PASSWORD='" + M + "' y"},
	})
}

// Secrets in the property test are made of cliSecretLetters and the
// separators; the text around them has none of those letters.
const cliSecretLetters = "QZXJKVY79"

var cliSeps = []string{"&", "&&", ";", "|", ";;", "|&"}

func randomCLISecret(r *rand.Rand) string {
	var b strings.Builder
	b.WriteByte(cliSecretLetters[r.Intn(len(cliSecretLetters))])
	for n := r.Intn(4); n >= 0; n-- {
		if r.Intn(2) == 0 {
			b.WriteString(cliSeps[r.Intn(len(cliSeps))])
		}
		b.WriteByte(cliSecretLetters[r.Intn(len(cliSecretLetters))])
	}
	return b.String()
}

// cliForms put a secret into a command line: %s is the secret, already
// quoted as the form needs.
var cliForms = []string{
	"sshpass -p %s", "sshpass -p%s", "sshpass -vp %s", "sshpass -e -p %s",
	"mysql -p%s", "mysqldump -u root -p%s", "curl -u bob:%s", "curl --user=bob:%s",
	"smbclient -U bob%%%s", "ipmitool -I lan -P %s", "cmd --password %s",
	"cmd --password=%s", "cmd --db-password %s", "x PASSWORD=%s", "x api_key=%s",
}

var cliQuotes = []string{"", "'", `"`}

var cliSuffixes = []string{
	"", " ssh h", " db", " https://h/", " && echo done", " ; ls", " | grep a",
	" & wait", "\nnext line", " -v", " other=x",
}

// checkCLISecret checks that no byte of secret is visible on its line, and
// runs the differential check (checkCLIDiff) on the same line.
func checkCLISecret(t *testing.T, form, q, secret, suffix string) {
	t.Helper()
	in := "run " + strings.Replace(form, "%s", q+secret+q, 1)
	in = strings.ReplaceAll(in, "%%", "%") + suffix
	checkCLIDiff(t, in)
	got := Text(in)
	line := got
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if strings.ContainsAny(line, cliSecretLetters) {
		t.Errorf("Text(%q) = %q: part of the secret %q is visible", in, got, secret)
	}
}

func TestCLISeparatorNeverLeaks(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	n := 300
	if testing.Short() || raceEnabled {
		n = 30
	}
	for k := 0; k < n; k++ {
		secret := randomCLISecret(r)
		for _, form := range cliForms {
			for _, q := range cliQuotes {
				for _, suffix := range cliSuffixes {
					checkCLISecret(t, form, q, secret, suffix)
				}
			}
		}
	}
}

// FuzzCLISeparatorNeverLeaks is the fuzzing form of
// TestCLISeparatorNeverLeaks: the secret is built from the fuzz input.
//
//	go test ./redact -run '^$' -fuzz FuzzCLISeparatorNeverLeaks -fuzztime 1m
func FuzzCLISeparatorNeverLeaks(f *testing.F) {
	f.Add([]byte{0, 200, 1}, uint8(0), uint8(0), uint8(0))
	f.Add([]byte{3, 128, 4, 129, 5}, uint8(4), uint8(1), uint8(4))
	f.Fuzz(func(t *testing.T, raw []byte, form, q, suffix uint8) {
		if len(raw) == 0 || len(raw) > 64 {
			return
		}
		// Each byte is a letter, or (high bit set) a separator then a
		// letter, so separators are always between letters.
		var b strings.Builder
		for i, c := range raw {
			if c&0x80 != 0 && i > 0 {
				b.WriteString(cliSeps[int(c>>4&7)%len(cliSeps)])
			}
			b.WriteByte(cliSecretLetters[int(c&0x7f)%len(cliSecretLetters)])
		}
		checkCLISecret(t, cliForms[int(form)%len(cliForms)], cliQuotes[int(q)%len(cliQuotes)],
			b.String(), cliSuffixes[int(suffix)%len(cliSuffixes)])
	})
}

// The differential tests compare redactOptions with oldRedactOptions (see
// cli_old_test.go): redactOptions must hide at least what the old scanner
// hides (hidesMore, see main_diff_test.go), and may only hide more.

var cliDiffTokens = []string{
	"sshpass", "sshpass ", "mysql ", "curl ", "smbclient ", "ipmitool ", "ssh ", "-p", "-p ",
	"-vp ", "-e ", "-f ", "-u ", "-U ", "-P ", "--password", "--password=", "--password ",
	"--no-password ", "bob:", "bob%", "ab", "cd", "S3CR3T", "&", "&&", ";", "|", "||", "|&",
	"2>&1 ", ">&", " ", "  ", "\t", "\n", "\r", "\\\n", "'", `"`, `\"`, `'\''`, "\\", "db",
	"h", "x=1", "PASSWORD=", "#",
}

func randomCLILine(r *rand.Rand) string {
	var b strings.Builder
	for n := 1 + r.Intn(14); n > 0; n-- {
		b.WriteString(cliDiffTokens[r.Intn(len(cliDiffTokens))])
	}
	return b.String()
}

func checkCLIDiff(t *testing.T, in string) {
	t.Helper()
	got, old := redactOptions(in), oldRedactOptions(in)
	if got != old && !hidesMore(got, old) {
		t.Errorf("redactOptions(%q) shows something the old scanner redacts\n got %q\n old %q", in, got, old)
	}
}

func TestCLIHidesAtLeastOld(t *testing.T) {
	for _, in := range []string{
		"sshpass -p ab&cd x", "sshpass -p ab;sshpass -p cd ssh h", "mysql -pab&&mysql -pcd",
		"mysql -pab;grep -px", "curl -u a:b;curl -u c:d", "sshpass -p ab & sshpass -p cd x",
		"mysql -pab|mysql -pcd|mysql -pef", "sshpass -f /a;b -p cd ssh h",
		"cmd --password ab&cd y", "cmd --password ab;cd y", "cmd --password 'ab|cd' y",
	} {
		checkCLIDiff(t, in)
	}
	r := rand.New(rand.NewSource(2))
	n := 200000
	if testing.Short() || raceEnabled {
		n = 20000
	}
	for k := 0; k < n; k++ {
		checkCLIDiff(t, randomCLILine(r))
	}
}

// FuzzCLIHidesAtLeastOld is the fuzzing form of TestCLIHidesAtLeastOld, on
// arbitrary text and on lines made of cliDiffTokens.
//
//	go test ./redact -run '^$' -fuzz FuzzCLIHidesAtLeastOld -fuzztime 1m
func FuzzCLIHidesAtLeastOld(f *testing.F) {
	f.Add("sshpass -p ab&cd x", []byte{})
	f.Add("", []byte{0, 6, 21, 24, 22, 1, 4, 21, 25, 22})
	f.Fuzz(func(t *testing.T, s string, toks []byte) {
		var b strings.Builder
		b.WriteString(s)
		for _, k := range toks {
			b.WriteString(cliDiffTokens[int(k)%len(cliDiffTokens)])
		}
		if b.Len() > 4096 {
			return
		}
		checkCLIDiff(t, b.String())
	})
}
