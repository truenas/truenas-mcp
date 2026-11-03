package redact

import (
	"math/rand"
	"strings"
	"testing"
)

// Free text is redacted fail closed. A quoted value of a sensitive
// key=value / key: value pair ends at its closing quote, so the pairs after
// it stay visible. The end of an unquoted value cannot be proven
// ("password=ab cd=ef" may be the one secret "ab cd=ef"), so it is redacted
// to the end of the line, later pairs included.
func TestTextNarrowRedaction(t *testing.T) {
	cases := []struct{ name, in, want string }{
		// Quoted values end at their closing quote.
		{"double-quoted value with spaces", `nopool password="a b c" other=x`, `nopool password="` + Marker + `" other=x`},
		{"single-quoted value with spaces", `nopool password='a b c' other=x`, `nopool password='` + Marker + `' other=x`},
		{"escaped double quote inside double quotes", `x password="fa\"ke pw" other=x`, `x password="` + Marker + `" other=x`},
		{"escaped quote and backslash inside double quotes", `x password="fa\\\" ke" other=x`, `x password="` + Marker + `" other=x`},
		{"quoted value then an empty tail pair", `x password='a b' K= hotel`, `x password='` + Marker + `' K= hotel`},
		{"empty quoted value", `x password="" other=x`, `x password="" other=x`},
		{"multiple secrets, quoted then unquoted", `x passphrase='FAKE 1' api_key=FAKE2 user=bob`,
			"x passphrase='" + Marker + "' api_key=" + Marker},
		{"JSON-like pair", `x "password":"FAKE" other=x`, `x "password":"` + Marker + `" other=x`},
		{"JSON-like pair with space", `{"password": "FAKE pw", "other": "x"}`, `{"password": "` + Marker + `", "other": "x"}`},
		{"URL userinfo", "x https://user:FAKE@host/path other=x", "x https://user:" + Marker + "@host/path other=x"},
		{"URL userinfo with @ in the password", "x https://user:FA@KE@host other=x", "x https://user:" + Marker + "@host other=x"},
		{"query parameter", "GET /a?token=FAKE&page=2 other=x", "GET /a?token=" + Marker},
		{"--password value", "cmd --password FAKE other=x", "cmd --password " + Marker + " other=x"},
		{"empty value at the end of the line", "x password=\nnext", "x password=\nnext"},

		// Fail closed: an unquoted value runs to the end of the line.
		{"issue example", "nopool password=FAKEPW other=x", "nopool password=" + Marker},
		{"space then name=", "x password=ab cd=ef", "x password=" + Marker},
		{"several words", "x password=fake pw word other=x", "x password=" + Marker},
		{"no pair after the value", "x password=FAKE trailing words", "x password=" + Marker},
		{"backslash-escaped spaces", `x password=fake\ pw\ word other=x`, "x password=" + Marker},
		{"backslash-escaped space at the end of the value", `x password=fakepw\  other=x`, "x password=" + Marker},
		{"trailing backslash", `x password=fakepw\`, "x password=" + Marker},
		{"unterminated double quote", `x password="fake pw other=x`, "x password=" + Marker},
		{"unterminated single quote", `x password='fake pw other=x`, "x password=" + Marker},
		{"unterminated quote stops at the end of the line", "x password=\"fake pw other=x\nnext=1", "x password=" + Marker + "\nnext=1"},
		{"empty value", "x password= other=x", "x password= " + Marker},
		{"multiple secrets", "x password=FAKE1 token=FAKE2 secret=FAKE3 other=x", "x password=" + Marker},
		{"passphrase", "x passphrase=FAKE other=x", "x passphrase=" + Marker},
		{"secret", "x secret=FAKE other=x", "x secret=" + Marker},
		{"token", "x token=FAKE other=x", "x token=" + Marker},
		{"key", "x key=FAKE other=x", "x key=" + Marker},
		{"api_key", "x api_key=FAKE other=x", "x api_key=" + Marker},
		{"client_secret", "x client_secret=FAKE other=x", "x client_secret=" + Marker},
		{"passwd", "x passwd=FAKE other=x", "x passwd=" + Marker},
		{"upper case key", "x PASSWORD=FAKE other=x", "x PASSWORD=" + Marker},
		{"mixed case key", "x PassWord=FAKE Other=x", "x PassWord=" + Marker},
		{"prefixed key", "x MYSQL_ROOT_PASSWORD=FAKE other=x", "x MYSQL_ROOT_PASSWORD=" + Marker},
		{"prefixed key, tab", "x DB_PASSWORD = fa\tke=c", "x DB_PASSWORD = " + Marker},
		{"prefixed key, vertical tab", "x DB_PASSWORD = fa\vke=c x=1", "x DB_PASSWORD = " + Marker},
		{"vertical tab before the next pair", "x password=FAKE\v other=x", "x password=" + Marker},
		{"tab before the next pair", "x password=FAKE\tother=x", "x password=" + Marker},
		{"key: value", "error: password: FAKE other: x", "error: password: " + Marker},
		{"colon pair inside the value", "x TOKEN: my fake: rex", "x TOKEN: " + Marker},
		{"key: value then key=value", "x password: FAKE other=x", "x password: " + Marker},
		{"spaces around =", "x password = FAKE other = x", "x password = " + Marker},
		{"tail pair with an empty value", "x credentials = fake K= hotel", "x credentials = " + Marker},
		{"--password=value", "cmd --password=FAKE other=x", "cmd --password=" + Marker},
		{"--password=value then an option", "cmd --password=FAKE --verbose", "cmd --password=" + Marker},
		{"quote inside the value", `x password=fa"ke other=x`, "x password=" + Marker},
		{"quote inside the value, later quote", `x password=fa"ke pw" other=x`, "x password=" + Marker},
		{"escaped quote inside the value", `x password=fa\"ke other=x`, "x password=" + Marker},
		{"backtick inside the value", "x password=fa`ke other=x", "x password=" + Marker},
		{"delimiters stay in the value", "x password=fa,ke;pw)x other=x", "x password=" + Marker},
		{"comma before the next pair", "x password=FAKE, other=x", "x password=" + Marker},
		{"glued pair is part of the value", "x password=FAKE,other=x", "x password=" + Marker},
		{"user:pass@host is not a pair", "x password=FAKE user:pw@host", "x password=" + Marker},
		{"scheme is not a pair", "x password=FAKE http://host", "x password=" + Marker},
		{"auth scheme", "x authorization=Bearer FAKE-token x=1", "x authorization=Bearer " + Marker},
		{"auth scheme, key: value", "x password: Basic FAKE x: 1", "x password: Basic " + Marker},
		{"query string, then a pair after a space", "GET /a?token=FAKE&page=2 x=1&token=FAKE2 more=y",
			"GET /a?token=" + Marker},

		// Whole strings of the form NAME=VALUE (environment entries, env-file
		// lines) are redacted to their end.
		{"env entry", "PASSWORD=FAKE other=x", "PASSWORD=" + Marker},
		{"env entry, tab", "DB_PASSWORD = fa\tke=c", "DB_PASSWORD =" + Marker},
		{"env entry, vertical tab", "DB_PASSWORD = fa\vke=c", "DB_PASSWORD =" + Marker},
		{"env entry, colon pair", "TOKEN: my fake: rex", "TOKEN: " + Marker},
		{"env entry, tail pair with an empty value", "credentials = fake K= hotel", "credentials =" + Marker},
		{"env entry with spaces", "MYSQL_ROOT_PASSWORD=fake root pw", "MYSQL_ROOT_PASSWORD=" + Marker},
		{"env-file lines", "DB_PASSWORD=FAKE other=x\nTZ=UTC\nAPI_KEY=FAKE2 more=y",
			"DB_PASSWORD=" + Marker + "\nTZ=UTC\nAPI_KEY=" + Marker},
		{"export line", "export DB_PASSWORD=FAKE other=x", "export DB_PASSWORD=" + Marker},
	}
	for _, c := range cases {
		got := Text(c.in)
		if got != c.want {
			t.Errorf("%s: Text(%q)\n got %q\nwant %q", c.name, c.in, got, c.want)
		}
		if again := Text(got); again != got {
			t.Errorf("%s: not idempotent: %q", c.name, again)
		}
	}
}

// Secrets in the property test are made of secretChars only; the text around
// them has none of those letters or digits (and neither has Marker), so any
// of them in the output is a leak of part of the secret. '&' is included:
// "&name=" does not end an unquoted value, so a secret such as "ab&cd=ef"
// is redacted as a whole.
const (
	secretLetters = "QZXJKVY79"
	secretChars   = secretLetters + `QZXJ79 ,;:=@/#}){%+-.!"'\&` + "\t\v"
)

var narrowKeys = []string{
	"password", "PASSWORD", "Password", "passwd", "passphrase", "secret", "token",
	"api_key", "key", "client_secret", "private_key", "db_password", "--password",
	"DB_PASSWORD", "MYSQL_ROOT_PASSWORD", "TOKEN", "credentials",
}

var narrowSeps = []string{"=", " = ", ": ", ":\t", "\t=\t", " =\v"}

var narrowPrefixes = []string{"", "nopool ", "error: ", "a, ", "(", "{", "cmd -v ", "export ", "\t"}

var narrowSuffixes = []string{
	"", " other=x", " user: bob", "\tother=x", " other = x", ", other=x",
	" trailing words", " other", "\nnext=1", "\r\nnext: line", " --verbose",
	" other=x more=y", " user:pw@host", " http://host/", ")", "}", " ",
	" k= hotel", " my: rex", "\vother=x", "\tb=c", " cd=ef", "\nTZ=UTC",
	"&page=2", "&page=2 other=x", "&b=c",
}

// encodings write a secret as a value: each returns the encoded value, or
// false when the secret cannot be written that way.
var narrowEncodings = []struct {
	name   string
	quoted bool // the value ends at a closing quote
	encode func(string) (string, bool)
}{
	{"double-quoted", true, func(s string) (string, bool) {
		r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
		return `"` + r.Replace(s) + `"`, true
	}},
	{"single-quoted", true, func(s string) (string, bool) {
		return "'" + s + "'", !strings.Contains(s, "'")
	}},
	{"backslash-escaped", false, func(s string) (string, bool) {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			if strings.IndexByte(" \t\"'`\\", s[i]) >= 0 {
				b.WriteByte('\\')
			}
			b.WriteByte(s[i])
		}
		return b.String(), true
	}},
	{"bare word", false, func(s string) (string, bool) {
		return s, !strings.ContainsAny(s, " \t\"'`\\")
	}},
	{"unterminated double quote", false, func(s string) (string, bool) {
		return `"` + s, !strings.Contains(s, `"`)
	}},
	{"unterminated single quote", false, func(s string) (string, bool) {
		return "'" + s, !strings.Contains(s, "'")
	}},
}

// checkNoSecretLeak redacts prefix+key+sep+value+suffix and fails if any
// letter or digit of the secret is left before the end of that line. A value
// that is not quoted has no provable end, so it must also be redacted to the
// end of the line: nothing of the suffix may be left on it.
func checkNoSecretLeak(t *testing.T, prefix, key, sep, value, suffix, enc string, quoted bool) {
	t.Helper()
	in := prefix + key + sep + value + suffix
	out := Text(in)
	line := out
	if i := strings.IndexAny(out, "\r\n"); i >= 0 {
		line = out[:i]
	}
	// Keys such as TOKEN contain secret letters: look only after the key.
	rest, ok := strings.CutPrefix(line, prefix+key)
	if !ok {
		t.Errorf("%s: Text(%q) = %q, changed the text before the value", enc, in, out)
		return
	}
	if strings.ContainsAny(rest, secretLetters) {
		t.Errorf("%s: Text(%q) leaks part of the secret: %q", enc, in, out)
	}
	if !quoted && !strings.HasSuffix(strings.TrimRight(line, " \t"), Marker) {
		t.Errorf("%s: Text(%q) = %q, want the rest of the line redacted", enc, in, out)
	}
}

// TestTextNarrowNeverLeaks checks, over random secrets written in every way
// a value can be quoted or escaped, with every key, separator and text
// around them, that no part of the secret is ever left in the output.
func TestTextNarrowNeverLeaks(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	n := 0
	for iter := 0; iter < 300; iter++ {
		size := 1 + rng.Intn(12)
		var b strings.Builder
		b.WriteByte(secretLetters[rng.Intn(len(secretLetters))]) // at least one letter
		for b.Len() < size {
			b.WriteByte(secretChars[rng.Intn(len(secretChars))])
		}
		secret := b.String()
		for _, e := range narrowEncodings {
			value, ok := e.encode(secret)
			if !ok {
				continue
			}
			key := narrowKeys[rng.Intn(len(narrowKeys))]
			sep := narrowSeps[rng.Intn(len(narrowSeps))]
			if strings.HasPrefix(key, "--") {
				sep = "="
			}
			prefix := narrowPrefixes[rng.Intn(len(narrowPrefixes))]
			for _, suffix := range narrowSuffixes {
				checkNoSecretLeak(t, prefix, key, sep, value, suffix, e.name, e.quoted)
				n++
			}
		}
	}
	if n < 1000 {
		t.Fatalf("only %d lines checked", n)
	}
}

// FuzzTextNarrowNeverLeaks is the fuzzing form of TestTextNarrowNeverLeaks:
//
//	go test ./redact -run '^$' -fuzz FuzzTextNarrowNeverLeaks -fuzztime 60s
func FuzzTextNarrowNeverLeaks(f *testing.F) {
	f.Add([]byte("QZ X"), uint8(0), uint8(1), uint8(0), uint8(0))
	f.Add([]byte(`Q"Z\ X`), uint8(2), uint8(0), uint8(1), uint8(3))
	f.Add([]byte("Q,Z;X)=W"), uint8(3), uint8(2), uint8(2), uint8(5))
	f.Fuzz(func(t *testing.T, raw []byte, enc, key, sep, suffix uint8) {
		var b strings.Builder
		for _, c := range raw {
			b.WriteByte(secretChars[int(c)%len(secretChars)])
		}
		secret := b.String()
		if !strings.ContainsAny(secret, secretLetters) {
			return
		}
		e := narrowEncodings[int(enc)%len(narrowEncodings)]
		value, ok := e.encode(secret)
		if !ok {
			return
		}
		k := narrowKeys[int(key)%len(narrowKeys)]
		s := narrowSeps[int(sep)%len(narrowSeps)]
		if strings.HasPrefix(k, "--") {
			s = "="
		}
		checkNoSecretLeak(t, "x ", k, s, value, narrowSuffixes[int(suffix)%len(narrowSuffixes)], e.name, e.quoted)
	})
}
