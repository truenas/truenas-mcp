package redact

import (
	"math/rand"
	"strings"
	"testing"
)

// An unquoted value is not ended by "&name=": a secret can contain it, so
// the value is redacted to the end of the line, query strings included.
func TestTextAmpersandInValue(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"value is only &name=", "password=&J=", "password=" + Marker},
		{"value starts with &name=", "x password=&J= other=x", "x password=" + Marker},
		{"&name= inside the value", "x password=ab&cd=ef", "x password=" + Marker},
		{"&name= inside a colon value", "x secret: fake&pw=1", "x secret: " + Marker},
		{"--password=value with &name=", "cmd --password=fake&pw=1 -v", "cmd --password=" + Marker},
		{"query string", "GET /api?token=fake-oauth-token&page=2 HTTP/1.1", "GET /api?token=" + Marker},
		{"query string with a second token", "https://h/?token=fake1&token=fake2", "https://h/?token=" + Marker},
		{"sensitive parameter after another", "https://h/?page=2&token=fake&x=1", "https://h/?page=2&token=" + Marker},
		{"next line stays visible", "x token=fake&page=2\nnext=1", "x token=" + Marker + "\nnext=1"},
		{"quoted value still ends at its quote", `x password="fake&pw=1" other=x`, `x password="` + Marker + `" other=x`},
		{"single-quoted value", `x 'password': 'fake&pw=1'`, `x 'password': '` + Marker + `'`},
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

// Secrets in the property test below are made of ampChars, which include
// '&', '=' and '?'. The text around them has none of ampLetters (and
// neither has Marker), so any of them left in the output is part of a
// secret.
const (
	ampLetters = "QZXJKVY79"
	ampChars   = ampLetters + "QZX79&&&==?_-.%+/;,@#"
)

var (
	ampKeys     = []string{"password", "token", "api_key", "secret", "DB_PASSWORD", "access_token", "--password"}
	ampSeps     = []string{"=", " = ", ": "}
	ampPrefixes = []string{"", "x ", "GET /a?", "https://h/p?page=2&", "url=https://h/?"}
	ampSuffixes = []string{"", "&page=2", "&page=2 HTTP/1.1", " HTTP/1.1", "&b=c other=x", "&", "&=", "\nnext=1"}
)

// checkAmpLine redacts prefix+key+sep+secret+suffix (the secret unquoted)
// and fails if any character of the secret is left on its line, or if
// anything after the secret is left on it.
func checkAmpLine(t *testing.T, prefix, key, sep, secret, suffix string) {
	t.Helper()
	in := prefix + key + sep + secret + suffix
	out := Text(in)
	line := out
	if i := strings.IndexAny(out, "\r\n"); i >= 0 {
		line = out[:i]
	}
	// Keys such as TOKEN contain secret letters: look only after the key.
	rest, ok := strings.CutPrefix(line, prefix+key)
	if !ok {
		t.Errorf("Text(%q) = %q changed the text before the value", in, out)
		return
	}
	if strings.ContainsAny(rest, ampLetters) {
		t.Errorf("Text(%q) leaks part of the secret: %q", in, out)
	}
	if !strings.HasSuffix(strings.TrimRight(line, " \t"), Marker) {
		t.Errorf("Text(%q) = %q, want the rest of the line redacted", in, out)
	}
}

func randomAmpSecret(rng *rand.Rand) string {
	size := 1 + rng.Intn(12)
	var b strings.Builder
	for b.Len() < size {
		b.WriteByte(ampChars[rng.Intn(len(ampChars))])
	}
	s := b.String()
	if !strings.ContainsAny(s, ampLetters) {
		s += string(ampLetters[rng.Intn(len(ampLetters))])
	}
	return s
}

// TestTextAmpersandNeverLeaks checks random unquoted secrets containing '&',
// '=' and '?', in and out of query strings, with every key, separator and
// text after them.
func TestTextAmpersandNeverLeaks(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	n := 0
	for iter := 0; iter < 2000; iter++ {
		secret := randomAmpSecret(rng)
		key := ampKeys[rng.Intn(len(ampKeys))]
		sep := ampSeps[rng.Intn(len(ampSeps))]
		if strings.HasPrefix(key, "--") {
			sep = "="
		}
		prefix := ampPrefixes[rng.Intn(len(ampPrefixes))]
		for _, suffix := range ampSuffixes {
			checkAmpLine(t, prefix, key, sep, secret, suffix)
			n++
		}
	}
	if n < 10000 {
		t.Fatalf("only %d lines checked", n)
	}
}

// FuzzTextAmpersandNeverLeaks is the fuzzing form of
// TestTextAmpersandNeverLeaks:
//
//	go test ./redact -run '^$' -fuzz FuzzTextAmpersandNeverLeaks -fuzztime 60s
func FuzzTextAmpersandNeverLeaks(f *testing.F) {
	f.Add([]byte("Q&J="), uint8(0), uint8(0), uint8(0), uint8(0))
	f.Add([]byte("&J="), uint8(1), uint8(2), uint8(3), uint8(4))
	f.Fuzz(func(t *testing.T, raw []byte, key, sep, prefix, suffix uint8) {
		var b strings.Builder
		for _, c := range raw {
			b.WriteByte(ampChars[int(c)%len(ampChars)])
		}
		secret := b.String()
		if !strings.ContainsAny(secret, ampLetters) {
			return
		}
		k := ampKeys[int(key)%len(ampKeys)]
		s := ampSeps[int(sep)%len(ampSeps)]
		if strings.HasPrefix(k, "--") {
			s = "="
		}
		checkAmpLine(t, ampPrefixes[int(prefix)%len(ampPrefixes)], k, s, secret, ampSuffixes[int(suffix)%len(ampSuffixes)])
	})
}
