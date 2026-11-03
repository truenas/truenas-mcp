package redact

import (
	"math/rand"
	"strings"
	"testing"
)

// The value of a sensitive key=value / key: value pair in free text is
// redacted, and the pairs after it on the same line stay visible, when the
// value can be shown to end: a quoted value at its closing quote, an unquoted
// value at the white space before the next pair. When the end is unclear
// (several words, quotes, no pair after it) the rest of the line is
// redacted.
func TestTextNarrowRedaction(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"issue example", "nopool password=FAKEPW other=x", "nopool password=" + Marker + " other=x"},
		{"double-quoted value with spaces", `nopool password="a b c" other=x`, `nopool password="` + Marker + `" other=x`},
		{"single-quoted value with spaces", `nopool password='a b c' other=x`, `nopool password='` + Marker + `' other=x`},
		{"escaped double quote inside double quotes", `x password="fa\"ke pw" other=x`, `x password="` + Marker + `" other=x`},
		{"escaped quote and backslash inside double quotes", `x password="fa\\\" ke" other=x`, `x password="` + Marker + `" other=x`},
		{"backslash-escaped spaces", `x password=fake\ pw\ word other=x`, "x password=" + Marker + " other=x"},
		{"backslash-escaped space at the end of the value", `x password=fakepw\  other=x`, "x password=" + Marker + "  other=x"},
		{"trailing backslash", `x password=fakepw\`, "x password=" + Marker},
		{"unterminated double quote", `x password="fake pw other=x`, "x password=" + Marker},
		{"unterminated single quote", `x password='fake pw other=x`, "x password=" + Marker},
		{"unterminated quote stops at the end of the line", "x password=\"fake pw other=x\nnext=1", "x password=" + Marker + "\nnext=1"},
		{"empty value", "x password= other=x", "x password= " + Marker},
		{"empty value at the end of the line", "x password=\nnext", "x password=\nnext"},
		{"empty quoted value", `x password="" other=x`, `x password="" other=x`},
		{"multiple secrets", "x password=FAKE1 token=FAKE2 secret=FAKE3 other=x",
			"x password=" + Marker + " token=" + Marker + " secret=" + Marker + " other=x"},
		{"multiple secrets, quoted and unquoted", `x passphrase='FAKE 1' api_key=FAKE2 user=bob`,
			"x passphrase='" + Marker + "' api_key=" + Marker + " user=bob"},
		{"passphrase", "x passphrase=FAKE other=x", "x passphrase=" + Marker + " other=x"},
		{"secret", "x secret=FAKE other=x", "x secret=" + Marker + " other=x"},
		{"token", "x token=FAKE other=x", "x token=" + Marker + " other=x"},
		{"key", "x key=FAKE other=x", "x key=" + Marker + " other=x"},
		{"api_key", "x api_key=FAKE other=x", "x api_key=" + Marker + " other=x"},
		{"client_secret", "x client_secret=FAKE other=x", "x client_secret=" + Marker + " other=x"},
		{"passwd", "x passwd=FAKE other=x", "x passwd=" + Marker + " other=x"},
		{"upper case key", "x PASSWORD=FAKE other=x", "x PASSWORD=" + Marker + " other=x"},
		{"mixed case key", "x PassWord=FAKE Other=x", "x PassWord=" + Marker + " Other=x"},
		{"key: value", "error: password: FAKE other: x", "error: password: " + Marker + " other: x"},
		{"key: value then key=value", "x password: FAKE other=x", "x password: " + Marker + " other=x"},
		{"spaces around =", "x password = FAKE other = x", "x password = " + Marker + " other = x"},
		{"tab before the next pair", "x password=FAKE\tother=x", "x password=" + Marker + "\tother=x"},
		{"--password=value", "cmd --password=FAKE other=x", "cmd --password=" + Marker + " other=x"},
		{"--password value", "cmd --password FAKE other=x", "cmd --password " + Marker + " other=x"},
		{"--password=value then an option", "cmd --password=FAKE --verbose", "cmd --password=" + Marker},
		{"JSON-like pair", `x "password":"FAKE" other=x`, `x "password":"` + Marker + `" other=x`},
		{"JSON-like pair with space", `{"password": "FAKE pw", "other": "x"}`, `{"password": "` + Marker + `", "other": "x"}`},
		{"URL userinfo", "x https://user:FAKE@host/path other=x", "x https://user:" + Marker + "@host/path other=x"},
		{"URL userinfo with @ in the password", "x https://user:FA@KE@host other=x", "x https://user:" + Marker + "@host other=x"},
		{"query parameter", "GET /a?token=FAKE&page=2 other=x", "GET /a?token=" + Marker + "&page=2 other=x"},

		// Fail closed: the end of the value is not clear.
		{"several words", "x password=fake pw word other=x", "x password=" + Marker},
		{"no pair after the value", "x password=FAKE trailing words", "x password=" + Marker},
		{"quote inside the value", `x password=fa"ke other=x`, "x password=" + Marker},
		{"quote inside the value, later quote", `x password=fa"ke pw" other=x`, "x password=" + Marker},
		{"escaped quote inside the value", `x password=fa\"ke other=x`, "x password=" + Marker},
		{"backtick inside the value", "x password=fa`ke other=x", "x password=" + Marker},
		{"delimiters stay in the value", "x password=fa,ke;pw)x other=x", "x password=" + Marker + " other=x"},
		{"comma before the next pair", "x password=FAKE, other=x", "x password=" + Marker + " other=x"},
		{"glued pair is part of the value", "x password=FAKE,other=x", "x password=" + Marker},
		{"user:pass@host is not a pair", "x password=FAKE user:pw@host", "x password=" + Marker},
		{"scheme is not a pair", "x password=FAKE http://host", "x password=" + Marker},
		{"auth scheme", "x authorization=Bearer FAKE-token x=1", "x authorization=Bearer " + Marker},
		{"auth scheme, key: value", "x password: Basic FAKE x: 1", "x password: Basic " + Marker},
		{"whole string NAME=VALUE (env entry)", "PASSWORD=FAKE other=x", "PASSWORD=" + Marker},
	}
	for _, c := range cases {
		got := Text(c.in)
		if got != c.want {
			t.Errorf("%s: Text(%q)\n got %q\nwant %q", c.name, c.in, got, c.want)
		}
		if again := Text(got); again != got {
			t.Errorf("%s: not idempotent: %q", c.name, again)
		}
		for _, secret := range []string{"FAKE", "fake", "pw"} {
			if strings.Contains(c.in, secret) && strings.Contains(got, secret) {
				t.Errorf("%s: %q leaks %q", c.name, got, secret)
			}
		}
	}
}

// Secrets in the property test are made of secretChars only; the text around
// them has none of those letters or digits (and neither has Marker), so any
// of them in the output is a leak of part of the secret. '&' is left out: an
// unquoted value ends at "&name=" so that query strings
// (?token=S3CR3T&page=2) keep their other parameters, and a secret written
// unquoted with "&name=" in it is not covered by this change.
const (
	secretLetters = "QZXJKVY79"
	secretChars   = secretLetters + `QZXJ79 ,;:=@/#}){%+-.!"'\` + "\t"
)

var narrowKeys = []string{
	"password", "PASSWORD", "Password", "passwd", "passphrase", "secret", "token",
	"api_key", "key", "client_secret", "private_key", "db_password", "--password",
}

var narrowSeps = []string{"=", " = ", ": ", ":\t"}

var narrowPrefixes = []string{"", "nopool ", "error: ", "a, ", "(", "{", "cmd -v "}

var narrowSuffixes = []string{
	"", " other=x", " user: bob", "\tother=x", " other = x", ", other=x",
	" trailing words", " other", "\nnext=1", "\r\nnext: line", " --verbose",
	" other=x more=y", " user:pw@host", " http://host/", ")", "}", " ",
}

// encodings write a secret as a value: each returns the encoded value, or
// false when the secret cannot be written that way.
var narrowEncodings = []struct {
	name   string
	encode func(string) (string, bool)
}{
	{"double-quoted", func(s string) (string, bool) {
		r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
		return `"` + r.Replace(s) + `"`, true
	}},
	{"single-quoted", func(s string) (string, bool) {
		return "'" + s + "'", !strings.Contains(s, "'")
	}},
	{"backslash-escaped", func(s string) (string, bool) {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			if strings.IndexByte(" \t\"'`\\", s[i]) >= 0 {
				b.WriteByte('\\')
			}
			b.WriteByte(s[i])
		}
		return b.String(), true
	}},
	{"bare word", func(s string) (string, bool) {
		return s, !strings.ContainsAny(s, " \t\"'`\\")
	}},
	{"unterminated double quote", func(s string) (string, bool) {
		return `"` + s, !strings.Contains(s, `"`)
	}},
	{"unterminated single quote", func(s string) (string, bool) {
		return "'" + s, !strings.Contains(s, "'")
	}},
}

// checkNoSecretLeak redacts prefix+key+sep+value+suffix and fails if any
// letter or digit of the secret is left before the end of that line.
func checkNoSecretLeak(t *testing.T, prefix, key, sep, value, suffix, enc string) {
	t.Helper()
	in := prefix + key + sep + value + suffix
	out := Text(in)
	line := out
	if i := strings.IndexAny(out, "\r\n"); i >= 0 {
		line = out[:i]
	}
	if strings.ContainsAny(line, secretLetters) {
		t.Errorf("%s: Text(%q) leaks part of the secret: %q", enc, in, out)
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
				checkNoSecretLeak(t, prefix, key, sep, value, suffix, e.name)
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
		checkNoSecretLeak(t, "x ", k, s, value, narrowSuffixes[int(suffix)%len(narrowSuffixes)], e.name)
	})
}
