package redact

import (
	"math/rand"
	"strings"
	"testing"
)

// The differential tests below compare Text with mainText, a frozen copy of
// Text and redactBarePairs as they were before free-text values could be
// narrowed (the shared helpers are called as they are). The rule they
// enforce: Text may show a byte that mainText redacts only after a provable
// end of the value, which is the closing quote of a quoted value; mainText
// already ends quoted values there, so for every input the two outputs must
// be the same. In particular an unquoted value is redacted exactly as
// before, to the end of the line. A change that narrows a value further must
// extend diffAllowed with the provable end it relies on.

// mainText is Text before narrowing. Do not change it.
func mainText(s string) string {
	s = pemPrivateKey.ReplaceAllLiteralString(s, Marker)
	s = urlUserinfo.ReplaceAllString(s, "${1}"+Marker+"@")
	if m := envPair.FindStringSubmatch(s); m != nil {
		name := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(m[1]), "="))
		sens, meta := classifyKey(name)
		if sens && isRedactable(m[2]) && !scalarAt(m[2], 0, meta) {
			return m[1] + Marker
		}
	}
	s = authHeader.ReplaceAllString(s, "${1}${2} "+Marker)
	s = bearerToken.ReplaceAllString(s, "${1}"+Marker)
	// Before the quoted pairs, which would redact a sensitive-looking name
	// ('key': 'DB_PASS') that these passes need to see.
	s = redactOptions(s)
	s = redactYAML(s)
	s = redactFlowPairs(s)
	s = quotedPair.ReplaceAllStringFunc(s, func(m string) string {
		sub := quotedPair.FindStringSubmatch(m)
		key := quotedKey.FindStringSubmatch(sub[1])
		if key == nil {
			return m
		}
		sens, meta := classifyKey(key[1])
		if !sens {
			return m
		}
		if strings.HasPrefix(m[len(sub[1]):], `"`) {
			if !isRedactable(sub[2]) || meta.visible(sub[2]) {
				return m
			}
			return sub[1] + `"` + Marker + `"`
		}
		if !isRedactable(sub[3]) || meta.visible(sub[3]) {
			return m
		}
		return sub[1] + `'` + Marker + `'`
	})
	s = escapedPair.ReplaceAllStringFunc(s, func(m string) string {
		sub := escapedPair.FindStringSubmatch(m)
		if sens, meta := classifyKey(sub[2]); !sens || !isRedactable(sub[3]) || meta.visible(sub[3]) {
			return m
		}
		return sub[1] + `\"` + Marker + `\"`
	})
	return mainRedactBarePairs(s)
}

// mainRedactBarePairs is redactBarePairs before narrowing. Do not change it.
func mainRedactBarePairs(s string) string {
	if !strings.ContainsAny(s, "=:") {
		return s
	}
	var b strings.Builder
	pos := 0
	lineEnd := -1 // cached end of the line holding pos
	for pos < len(s) {
		m := barePair.FindStringSubmatchIndex(s[pos:])
		if m == nil {
			break
		}
		keyStart := pos + m[2]
		key := s[keyStart : pos+m[3]]
		valStart := pos + m[5] // end of the separator
		b.WriteString(s[pos:valStart])
		pos = valStart
		sens, meta := classifyKey(key)
		if strings.HasSuffix(s[:keyStart], "--") {
			sens, meta = optionSecret(key, true) // --password-file=/path
		}
		if !sens || strings.HasSuffix(s[:keyStart], "://") || scalarAt(s, valStart, meta) {
			continue
		}
		if lineEnd < valStart {
			lineEnd = len(s)
			if i := strings.IndexAny(s[valStart:], "\r\n"); i >= 0 {
				lineEnd = valStart + i
			}
		}
		if valStart < lineEnd && (s[valStart] == '"' || s[valStart] == '\'') {
			if close := closingQuote(s, valStart, lineEnd); close >= 0 {
				end := close + 1
				q := s[valStart : valStart+1]
				b.WriteString(redactQuoted(s[valStart:end], s[valStart+1:close], q))
				pos = end
				continue
			}
		}
		val := s[valStart:lineEnd]
		if loc := queryNext.FindStringIndex(val); loc != nil {
			val = val[:loc[0]]
		}
		val = strings.TrimRight(val, " \t")
		b.WriteString(redactUnquoted(val))
		pos = valStart + len(val)
	}
	b.WriteString(s[pos:])
	return b.String()
}

// diffAllowed reports whether Text(in) may differ from mainText(in). No
// difference is allowed yet.
func diffAllowed(in, got, old string) bool { return false }

func checkDiff(t *testing.T, in string) {
	t.Helper()
	got, old := Text(in), mainText(in)
	if got != old && !diffAllowed(in, got, old) {
		t.Errorf("Text(%q)\n got %q\nmain %q", in, got, old)
	}
}

// diffTokens are the pieces random lines are built from: sensitive keys
// (plain, upper case and prefixed), harmless keys, separators, the white
// space that may or may not end a value (tab and vertical tab included),
// quotes, escapes, query strings, env-file lines and words.
var diffTokens = []string{
	"password", "PASSWORD", "DB_PASSWORD", "MYSQL_ROOT_PASSWORD", "TOKEN", "token",
	"credentials", "secret", "api_key", "passphrase", "--password", "K", "cd", "page",
	"user", "other", "x", "my", "dog", "rex", "hotel", "ab", "ef", "a", "b", "c",
	"=", "= ", " = ", ":", ": ", ":\t", "\t", "\v", " ", "  ", "\n", "\r\n",
	`"`, "'", "`", `\`, `\ `, `\"`, "&", "&page=2", "?", "/a?token=", ",", ";", ")", "}", "]",
	"@", "://", "Bearer ", "export ", "TZ=UTC\n", "-p ", "--", "{", "[", "0", "1",
}

func randomDiffLine(rng *rand.Rand) string {
	var b strings.Builder
	n := 1 + rng.Intn(14)
	for i := 0; i < n; i++ {
		b.WriteString(diffTokens[rng.Intn(len(diffTokens))])
	}
	return b.String()
}

// TestTextMatchesMain checks the inputs that used to differ (several
// unquoted shapes followed by another pair) and a large sample of random
// lines made of diffTokens.
func TestTextMatchesMain(t *testing.T) {
	for _, in := range []string{
		"nopool password=FAKEPW other=x",
		"x password=ab cd=ef",
		"DB_PASSWORD = a\tb=c",
		"x DB_PASSWORD = a\tb=c",
		"x DB_PASSWORD = a\vb=c x=1",
		"TOKEN: my dog: rex",
		"x TOKEN: my dog: rex",
		"credentials = x K= hotel",
		"x credentials = x K= hotel",
		"MYSQL_ROOT_PASSWORD=fake root pw",
		"x MYSQL_ROOT_PASSWORD=FAKE other=x",
		"DB_PASSWORD=FAKE other=x\nTZ=UTC\nAPI_KEY=FAKE2 more=y",
		"GET /a?token=FAKE&page=2 other=x",
		"GET /a?token=FAKE&page=2 x=1&token=FAKE2 more=y",
		`x password=fake\ pw other=x`,
		`x password="a b" other=x`,
		`x password='a b' K= hotel`,
	} {
		checkDiff(t, in)
	}
	rng := rand.New(rand.NewSource(1))
	n := 200000
	if testing.Short() || raceEnabled {
		n = 20000
	}
	for i := 0; i < n; i++ {
		checkDiff(t, randomDiffLine(rng))
		if t.Failed() {
			return
		}
	}
}

// FuzzTextMatchesMain is the fuzzing form of TestTextMatchesMain, on
// arbitrary input and on lines built from diffTokens:
//
//	go test ./redact -run '^$' -fuzz FuzzTextMatchesMain -fuzztime 10m
func FuzzTextMatchesMain(f *testing.F) {
	f.Add("x password=ab cd=ef", []byte{})
	f.Add("credentials = x K= hotel", []byte{0, 26, 13})
	f.Add("DB_PASSWORD = a\tb=c", []byte{2, 28, 36, 30})
	f.Fuzz(func(t *testing.T, s string, toks []byte) {
		checkDiff(t, s)
		var b strings.Builder
		for _, c := range toks {
			b.WriteString(diffTokens[int(c)%len(diffTokens)])
		}
		checkDiff(t, s+b.String())
	})
}
