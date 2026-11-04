package redact

import (
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// The differential tests below compare Text with oldText, a frozen copy of
// Text and redactBarePairs as they were before this branch (the shared
// helpers are called as they are), when an unquoted value still ended at
// "&name=" (oldQueryNext). The rule they enforce: Text never shows a byte
// that oldText redacts unless it comes after a provable end of the value.
// oldText already ends quoted values at their closing quote, and no further
// provable end is relied on yet, so Text must redact everything oldText
// redacts and may only hide more (an unquoted value now also runs past
// "&name="). A change that narrows a value further must extend diffAllowed
// with the provable end it relies on.

// oldQueryNext is the regexp oldText ended unquoted values at. Do not
// change it.
var oldQueryNext = regexp.MustCompile(`&[A-Za-z_][A-Za-z0-9_.-]*=`)

// oldText is Text before this branch. Do not change it.
func oldText(s string) string {
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
	return oldRedactBarePairs(s)
}

// oldRedactBarePairs is redactBarePairs before this branch. Do not change
// it.
func oldRedactBarePairs(s string) string {
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
		if loc := oldQueryNext.FindStringIndex(val); loc != nil {
			val = val[:loc[0]]
		}
		val = strings.TrimRight(val, " \t")
		b.WriteString(redactUnquoted(val))
		pos = valStart + len(val)
	}
	b.WriteString(s[pos:])
	return b.String()
}

// diffAllowed reports whether Text(in) = got may differ from
// oldText(in) = old: got must hide everything old hides (see hidesMore).
// No provable end beyond those oldText knows is accepted yet.
func diffAllowed(in, got, old string) bool { return hidesMore(got, old) }

// markerItems splits an output into items: one per visible byte, and -1 for
// each Marker.
func markerItems(s string) []int {
	var items []int
	for len(s) > 0 {
		if strings.HasPrefix(s, Marker) {
			items = append(items, -1)
			s = s[len(Marker):]
			continue
		}
		items = append(items, int(s[0]))
		s = s[1:]
	}
	return items
}

// hidesMore reports whether got can be made from old by replacing runs of
// zero or more items of old (visible bytes or markers) with a single Marker.
// Then every byte old redacts is redacted in got as well, and every byte
// got shows is shown by old at the same place. It is wildcard matching:
// each Marker of got matches any run of items of old, and each visible
// byte of got matches the same visible byte of old. A Marker that hides
// nothing (an empty value redacted, as in an unterminated quote at the end
// of a line) shows nothing either, so it is accepted.
func hidesMore(got, old string) bool {
	var p []int // pattern: a Marker of got becomes "any run" (-3)
	for _, it := range markerItems(got) {
		if it == -1 {
			p = append(p, -3)
		} else {
			p = append(p, it)
		}
	}
	t := markerItems(old)
	i, j, star, mark := 0, 0, -1, 0
	for i < len(t) {
		switch {
		case j < len(p) && p[j] == t[i]:
			i++
			j++
		case j < len(p) && p[j] == -3:
			star, mark = j, i
			j++
		case star >= 0:
			mark++
			i, j = mark, star+1
		default:
			return false
		}
	}
	for j < len(p) && p[j] == -3 {
		j++
	}
	return j == len(p)
}

func checkDiff(t *testing.T, in string) {
	t.Helper()
	got, old := Text(in), oldText(in)
	if got != old && !diffAllowed(in, got, old) {
		t.Errorf("Text(%q) shows something the old code redacts\n got %q\n old %q", in, got, old)
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

// TestHidesMore checks the comparison itself: hiding more is accepted,
// showing a byte that old redacts (or a different byte) is not.
func TestHidesMore(t *testing.T) {
	M := Marker
	cases := []struct {
		got, old string
		want     bool
	}{
		{"a=" + M, "a=" + M, true},
		{"a=" + M, "a=" + M + "&b=c", true},
		{"a=" + M, "a=" + M + "&b=" + M, true},
		{M, "x " + M + " y", true},
		{"x " + M + " y", "x " + M + " y", true},
		{"a=" + M + " b=" + M, "a=" + M + " b=c", true},
		{"a=" + M + " b=c", "a=" + M, false},      // shows bytes old redacts
		{"a=" + M + "&b=c", "a=" + M, false},      // the old narrowing at &name=
		{"a=x", "a=" + M, false},                  // shows the secret
		{"a=" + M + M, "a=" + M, true},            // an extra marker hides nothing
		{"a=" + M + " y", "a=" + M + " z", false}, // different visible byte
		{"a=" + M, "a=", true},                    // nor does a marker on an empty value
		{M + "x", "x" + M, false},                 // a visible byte moved
		{"", "", true},
	}
	for _, c := range cases {
		if got := hidesMore(c.got, c.old); got != c.want {
			t.Errorf("hidesMore(%q, %q) = %v, want %v", c.got, c.old, got, c.want)
		}
	}
}

// TestTextNeverShowsOldRedacted checks the inputs that differed while
// unquoted values were narrowed (several unquoted shapes followed by
// another pair), query strings, and a large sample of random lines made of
// diffTokens.
func TestTextNeverShowsOldRedacted(t *testing.T) {
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

// FuzzTextNeverShowsOldRedacted is the fuzzing form of TestTextNeverShowsOldRedacted, on
// arbitrary input and on lines built from diffTokens:
//
//	go test ./redact -run '^$' -fuzz FuzzTextNeverShowsOldRedacted -fuzztime 10m
func FuzzTextNeverShowsOldRedacted(f *testing.F) {
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
