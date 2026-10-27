// Package redact removes secrets from data before it leaves the server: tool
// results returned to the MCP client, error messages, and debug logs.
//
// Redaction is key-based (values under credential-bearing keys such as
// password, DB_PASS, bindpw, token, private_key or a key named "key") with
// value-based backstops for free text: PEM private key blocks, quoted and
// unquoted key/value pairs, NAME=VALUE environment entries, Bearer tokens,
// passwords embedded in URLs, command-line options and YAML. Redacted values
// are replaced with Marker; the key is kept so the model can still see that
// the field exists.
package redact

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"unicode"
)

// Marker replaces every redacted value.
const Marker = "[REDACTED]"

// sensitiveKeyFragments match keys, after normalisation (lower case, with
// '_', '-', '.' and spaces removed), anywhere in the name.
var sensitiveKeyFragments = []string{
	"password", "passwd", "passphrase", "secret", "token", "apikey",
	"privatekey", "privkey", "credential", "bindpw", "keytab", "cookie", "authorization",
	"preshared",
}

// sensitiveSegments match a whole word of the key, after splitting it on
// '_', '-', '.', spaces and camelCase boundaries. They are too short or too
// common to match inside other words ("bypass", "spinning", "pwm").
var sensitiveSegments = map[string]bool{
	"pass": true, "pw": true, "pwd": true, "passwd": true, "password": true,
	"psk": true, "otp": true, "totp": true, "pin": true, "session": true,
	"community": true,
}

// sensitiveKeySuffixes match normalised keys that end in a compound name
// written without a separator (encryptionkey, monpwd, rootpw, ...).
var sensitiveKeySuffixes = []string{
	"encryptionkey", "datasetkey", "sshkey", "accesskey", "authkey", "masterkey", "signingkey",
	"pwd", "pw",
}

// passPrefixes are the prefixes for which a run-together "...pass" name is a
// password (dbpass, adminpass); a bare "pass" suffix would also catch
// "bypass" and "compass".
var passPrefixes = []string{
	"db", "mon", "admin", "root", "user", "mail", "smtp", "imap", "ftp", "redis",
	"mysql", "sql", "pg", "ldap", "vnc", "proxy", "rpc", "auth", "api", "web",
}

// sensitiveExactKeys match whole normalised keys: password hashes and the
// bare name "key" (API key records, encryption key entries).
var sensitiveExactKeys = map[string]bool{
	"key": true, "unixhash": true, "smbhash": true, "pwhash": true, "nthash": true,
}

// publicKeyNames are exact normalised names of public key fields, which are
// not secret. Anything else that merely contains "public"
// (public_key_secret) is matched normally.
var publicKeyNames = map[string]bool{
	"publickey": true, "pubkey": true, "sshpublickey": true, "sshpubkey": true,
	"peerpublickey": true, "clientpublickey": true, "serverpublickey": true,
	"hostpublickey": true,
}

// typeExemptBlockers are words that, directly before a final "type" segment,
// still name a secret value rather than a kind of credential
// (secret_type, password_type).
var typeExemptBlockers = map[string]bool{
	"secret": true, "password": true, "passwd": true, "passphrase": true,
	"pass": true, "pw": true, "pwd": true, "pin": true, "otp": true,
}

// metaKind says which values a metadata key (one that describes a secret
// rather than holds it, such as token_ttl) may show.
type metaKind uint8

const (
	metaNone   metaKind = iota // not a metadata key
	metaScalar                 // booleans, numbers, null, dates and durations
	metaIdent                  // also identifiers (algorithm names: HS256)
	// metaFlag is not a metadata key: it holds a secret (peer_private) but
	// may also be a flag, so boolean-like text (true, no, null) stays
	// visible; numbers and other strings are redacted.
	metaFlag
)

// isMeta reports whether m is a metadata kind: the key describes a secret
// rather than holds it.
func (m metaKind) isMeta() bool { return m == metaScalar || m == metaIdent }

// visible reports whether v, a value written as text (unquoted), may be
// shown under a key of this kind.
func (m metaKind) visible(v string) bool {
	switch m {
	case metaNone:
		return false
	case metaFlag:
		return isFlagText(v)
	case metaIdent:
		if isIdentText(v) {
			return true
		}
	}
	return isScalarText(v)
}

// metadataSegments, as the final word of a multi-word key, describe a secret
// rather than hold it (pass_count, session_timeout, password_length,
// token_ttl, jwt_secret_algorithm), so the key is not sensitive.
var metadataSegments = map[string]metaKind{
	"count": metaScalar, "timeout": metaScalar, "length": metaScalar, "len": metaScalar,
	"ttl": metaScalar, "lifetime": metaScalar, "expiry": metaScalar, "expires": metaScalar,
	"expiration": metaScalar, "age": metaScalar, "interval": metaScalar, "size": metaScalar,
	"min": metaScalar, "max": metaScalar, "limit": metaScalar, "retries": metaScalar,
	"attempts": metaScalar, "enabled": metaScalar, "disabled": metaScalar,
	"required": metaScalar, "set": metaScalar, "configured": metaScalar, "changed": metaScalar,
	"algorithm": metaIdent, "algo": metaIdent, "alg": metaIdent,
}

func normalize(key string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', '.', ' ':
			return -1
		}
		return unicode.ToLower(r)
	}, key)
}

// segments splits key into lower-case words on '_', '-', '.', spaces and
// camelCase boundaries: "dbPass" -> [db pass], "APIKey" -> [api key],
// "MYSQL_ROOT_PASSWORD" -> [mysql root password].
func segments(key string) []string {
	var out []string
	var cur []rune
	rs := []rune(key)
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for i, r := range rs {
		switch {
		case r == '_' || r == '-' || r == '.' || unicode.IsSpace(r):
			flush()
			continue
		case unicode.IsUpper(r) && i > 0:
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

// privateSegments, as the final word of a multi-word key, name private key
// material (wg_private, peer_private, ssh_priv), unless the previous word
// is one of nonSecretPrivateContexts, where "private" is a flag or a
// network name (is_private, network_private, dhcp_private). Boolean values
// stay visible under these keys (see metaFlag).
var privateSegments = map[string]bool{"private": true, "priv": true}

var nonSecretPrivateContexts = map[string]bool{
	"is": true, "network": true, "net": true, "dhcp": true, "subnet": true, "vlan": true,
	"ip": true, "ipv4": true, "ipv6": true, "addr": true, "address": true, "range": true,
	"cidr": true, "lan": true, "vpc": true, "zone": true, "dns": true, "interface": true,
	"iface": true, "make": true, "set": true, "mark": true, "keep": true, "only": true,
	"repo": true, "repository": true, "registry": true, "image": true, "share": true,
	"visibility": true, "browse": true, "mode": true,
}

// IsSensitiveKey reports whether values under key must be redacted. Matching
// is case-insensitive and ignores '_', '-', '.', spaces and camelCase.
// Public key fields and "<kind>_type" enum fields (credential_type,
// token_type) are not secret. Keys that describe a secret rather than hold
// it (token_ttl, password_set) are not sensitive here; when redacting a
// value, such keys still hide objects, arrays and most strings, and only
// show booleans, numbers, null, dates and durations (and identifiers for
// *_algorithm; see classifyKey).
func IsSensitiveKey(key string) bool {
	sensitive, metadata := classifyKey(key)
	return sensitive && !metadata.isMeta()
}

// classifyKey reports whether key names a secret, ignoring metadata words,
// and, for a sensitive key, whether its final word is a metadata word
// (token_ttl, password_set, secret_expires, jwt_secret_algorithm) and so
// which values stay visible.
func classifyKey(key string) (sensitive bool, metadata metaKind) {
	k := normalize(key)
	if k == "" || publicKeyNames[k] {
		return false, metaNone
	}
	segs := segments(key)
	n := len(segs)
	if n >= 2 && segs[n-1] == "type" && !typeExemptBlockers[segs[n-2]] {
		return false, metaNone
	}
	if !sensitiveName(k, segs) {
		return false, metaNone
	}
	if n >= 2 {
		if privateSegments[segs[n-1]] {
			return true, metaFlag
		}
		return true, metadataSegments[segs[n-1]]
	}
	return true, metaNone
}

// trimDigits removes trailing digits from a key word, so that numbered
// fields (pass2, pw1) match like the unnumbered ones.
func trimDigits(s string) string {
	return strings.TrimRight(s, "0123456789")
}

func sensitiveName(k string, segs []string) bool {
	if sensitiveExactKeys[k] {
		return true
	}
	for _, s := range segs {
		if sensitiveSegments[s] || sensitiveSegments[trimDigits(s)] {
			return true
		}
	}
	if n := len(segs); n > 1 && (segs[n-1] == "key" || (privateSegments[segs[n-1]] && !nonSecretPrivateContexts[segs[n-2]])) {
		return true // encryption_key, privateKey, "dataset key", wg_private
	}
	for _, f := range sensitiveKeyFragments {
		if strings.Contains(k, f) {
			return true
		}
	}
	for _, s := range sensitiveKeySuffixes {
		if strings.HasSuffix(k, s) {
			return true
		}
	}
	if k = trimDigits(k); strings.HasSuffix(k, "pass") {
		for _, p := range passPrefixes {
			if k == p+"pass" {
				return true
			}
		}
	}
	return false
}

// keepsNumbers reports whether a number under key is an ID rather than a
// secret: "id", "*_id", and private_key/privatekey, which in keychain-based
// objects (replication, SSH connections) is the ID of a keychain entry, not
// key material.
func keepsNumbers(key string) bool {
	k := normalize(key)
	if k == "privatekey" {
		return true
	}
	segs := segments(key)
	return len(segs) > 0 && segs[len(segs)-1] == "id"
}

// pemPrivateKey matches a PEM private key block (RSA, EC, OPENSSH, ENCRYPTED,
// PGP ...). A block without its END line is redacted to the end of the text.
var pemPrivateKey = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----(?s:.*?)(?:-----END [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----|\z)`)

var (
	// urlUserinfo matches scheme://user:password@ in URLs and DSNs. The
	// password is greedy up to the last '@' before the host, so a raw '@'
	// in the password ("u:p@ss@host") is redacted with the rest of it; it
	// cannot contain whitespace, quotes, '/', '?' or '#', so it never runs
	// into the path, query or a following URL. The user name cannot contain
	// ':' or '@', so forms without a password (ssh://root@host) never match.
	// The "//" may be JSON-escaped ("postgres:\/\/u:p@host").
	urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*:\\?/\\?/[^\s:/@'"\\]*:)([^\s/?#'"]+)@`)
	// envPair matches a whole string of the form NAME=VALUE (compose
	// environment lists, env files). It is limited to a single line and
	// to unquoted values; quoted and multi-line text is left to barePair.
	envPair = regexp.MustCompile(`^([ \t]*[A-Za-z_][A-Za-z0-9_.-]{0,127}[ \t]*=)([^"'\r\n][^\r\n]*)$`)
	// authHeader matches an Authorization header value with its scheme.
	authHeader = regexp.MustCompile(`(?i)((?:proxy-)?authorization["']?\s*[:=]\s*["']?)(bearer|basic|digest|token|negotiate)\s+([^\s'",;]+)`)
	// bearerToken matches "Bearer <token>" anywhere.
	bearerToken = regexp.MustCompile(`(?i)\b(bearer\s+)([A-Za-z0-9._~+/-]{8,}=*)`)
	// quotedPair matches "key": "value" (JSON) and 'key': 'value' (Python
	// repr, as in middleware errors), including mixed quotes. Group 3 is a
	// double-quoted value, group 4 a single-quoted one.
	quotedPair = regexp.MustCompile(`(["'][^"'\\\n]{1,64}["']\s*[:=]\s*)(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)')`)
	// escapedPair matches \"key\": \"value\", JSON that was itself
	// embedded in a JSON string.
	escapedPair = regexp.MustCompile(`(\\"([^"\\\n]{1,64})\\"\s*:\s*)\\"((?:[^"\\]|\\[^"])*)\\"`)
	// barePair matches the key and separator of key=value / key: value with
	// an unquoted key on one line. Where the value ends is worked out in
	// redactBarePairs, and only for sensitive keys, so that a long line of
	// non-sensitive pairs is scanned once. The separator may not cross a
	// newline.
	barePair = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_.-]{0,63})([ \t]*[=:][ \t]*)`)
	// nextPair matches the start of the next key=value / key: value pair on
	// a line (whitespace, ',', ';' or '(' before the key). A quoted value
	// ends at the last suitable closing quote before it.
	nextPair = regexp.MustCompile(`[\s,;(]+[A-Za-z_][A-Za-z0-9_.-]{0,63}[ \t]*[=:]`)
	// queryNext matches the start of the next query-string parameter
	// (&name=), where an unquoted value inside a URL ends.
	queryNext = regexp.MustCompile(`&[A-Za-z_][A-Za-z0-9_.-]*=`)
	quotedKey = regexp.MustCompile(`^["']([^"'\\\n]{1,64})["']`)
)

// authSchemes are left in place when they are the first word of a value
// ("Authorization: Bearer [REDACTED]").
var authSchemes = map[string]bool{"bearer": true, "basic": true, "digest": true, "token": true, "negotiate": true}

func isRedactable(v string) bool { return v != "" && v != Marker }

// Text redacts secrets from free text such as error messages, traces and
// string values inside JSON results.
func Text(s string) string {
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
	return redactBarePairs(s)
}

// redactBarePairs redacts the values of key=value / key: value pairs with a
// sensitive, unquoted key. Matches are scanned one at a time so that a
// non-sensitive key ("failed: password=x") does not swallow the next pair.
//
// Value extent:
//   - A value that opens with ' or " ends at the last matching, unescaped
//     quote before the next key=value / key: value pair on the line (or
//     before the end of the line) that is followed by whitespace, ',', ';',
//     ')', '}', ']' or the end of the line. A quote inside the value
//     ("it's") therefore does not end it early. If there is no such quote,
//     the value runs to the end of the line.
//   - Any other value runs to the end of the line, trailing whitespace
//     excluded. ',', ';', '}' and ')' do not end it: they also occur inside
//     secrets and error text ("Invalid value, got 'X'"), so the rest of the
//     line is redacted instead. This can over-redact later pairs on the same
//     line, which is preferred to leaking.
//   - The one exception is a URL query string: the value ends before
//     "&name=", so later parameters are checked on their own.
//   - A value never crosses a newline, so following lines stay visible.
//
// An auth scheme word at the start of the value is kept
// ("Authorization: Bearer [REDACTED]"), and a key directly after "://" (the
// user name of a URL) is skipped so the host stays visible.
//
// The work is linear in len(s): a non-sensitive key costs nothing past its
// separator, the end of the line is found once per line, and the text
// scanned for a quoted value's extent is either consumed or scanned at most
// once more.
func redactBarePairs(s string) string {
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

// scalarAt reports whether the value starting at s[i], up to the next
// delimiter (or the closing quote of a quoted value), may be shown under a
// metadata key of kind meta (token_ttl=3600, password_expires: 2026-12-01,
// token_lifetime: "1h"). It looks at no more than maxScalarText+2 bytes.
func scalarAt(s string, i int, meta metaKind) bool {
	if meta == metaNone {
		return false
	}
	if i < len(s) && (s[i] == '"' || s[i] == '\'') {
		q := s[i]
		for end := i + 1; end < len(s) && end-i <= maxScalarText+1; end++ {
			switch s[end] {
			case q:
				// The value must end at the quote (not token_ttl="5"x).
				return valueEndsAt(s, end+1) && meta.visible(s[i+1:end])
			case '\\', '\n', '\r':
				return false
			}
		}
		return false
	}
	end := i
	for end < len(s) && end-i <= maxScalarText && !strings.ContainsRune(" \t\r\n,;)}]&\"'", rune(s[end])) {
		end++
	}
	if end-i > maxScalarText || !valueEndsAt(s, end) {
		return false
	}
	return meta.visible(s[i:end])
}

// valueEndsAt reports whether a value may end at s[i]: the end of s,
// whitespace or a separator, but not a quote (token_ttl=5'x).
func valueEndsAt(s string, i int) bool {
	return i >= len(s) || strings.IndexByte(" \t\r\n,;)}]&", s[i]) >= 0
}

// closingQuote returns the index of the quote that closes the value opening
// with the quote at s[open], or -1 if there is none on the line (which ends
// at lineEnd). It is the last unescaped matching quote before the next
// key=value pair (or the end of the line) that is followed by whitespace,
// ',', ';', ')', '}', ']' or the end of the line.
func closingQuote(s string, open, lineEnd int) int {
	q := s[open]
	limit := lineEnd
	if loc := nextPair.FindStringIndex(s[open+1 : lineEnd]); loc != nil {
		limit = open + 1 + loc[0]
	}
	for i := limit - 1; i > open; i-- {
		if s[i] != q || !quoteEnds(s, i+1, lineEnd) {
			continue
		}
		bs := 0
		for j := i - 1; j > open && s[j] == '\\'; j-- {
			bs++
		}
		if bs%2 == 0 {
			return i
		}
	}
	return -1
}

// quoteEnds reports whether a closing quote just before s[i] can end a
// value: it is followed by the end of the line or a delimiter.
func quoteEnds(s string, i, lineEnd int) bool {
	if i >= lineEnd {
		return true
	}
	switch s[i] {
	case ' ', '\t', ',', ';', ')', '}', ']':
		return true
	}
	return false
}

func redactQuoted(orig, val, quote string) string {
	if !isRedactable(val) {
		return orig
	}
	return quote + Marker + quote
}

func redactUnquoted(val string) string {
	scheme, rest, _ := strings.Cut(val, " ")
	if authSchemes[strings.ToLower(scheme)] {
		rest = strings.TrimLeft(rest, " ")
		if !isRedactable(rest) {
			return val
		}
		return scheme + " " + Marker
	}
	if !isRedactable(val) {
		return val
	}
	return Marker
}

// Value returns a deep copy of v with secrets redacted. The input is never
// modified. Generic JSON values (as decoded by encoding/json) are walked
// directly; typed values (structs, map[string]string, slices of maps ...)
// are round-tripped through JSON first.
func Value(v interface{}) interface{} {
	out, _ := walk(v, ctx{})
	return out
}

// ctx describes where a value sits.
type ctx struct {
	sensitive   bool     // under a sensitive key
	metadata    metaKind // the sensitive key describes a secret (token_ttl): scalars stay visible
	keepNumbers bool     // numbers under this key are IDs (see keepsNumbers)
	kerberos    bool     // inside a keytab/kerberos object
	credential  bool     // the value of a key named "credential" (see walkTypedCredential)
	attrMaps    bool     // the object under a key named "attribute_maps" (see walkMap)
}

// walk redacts v and reports whether anything changed.
//
// Under a sensitive key, a non-empty string (unless the key is a metadata
// key such as token_ttl and the string is a date, duration or number), a
// number (unless it is an ID or the key is a metadata key) or a non-empty
// object/array is replaced with Marker wholesale. Booleans, null and empty
// strings stay visible (password_disabled, unset fields). An object that
// refers to a stored credential (it has an "id" or "name") is not replaced
// wholesale: its id, name and type fields stay visible and its other fields
// are redacted (see walkReference). Every other string is redacted as free
// text, and a string that holds JSON is redacted structurally.
func walk(v interface{}, c ctx) (interface{}, bool) {
	switch t := v.(type) {
	case nil, bool:
		return v, false
	case map[string]interface{}:
		if c.sensitive && len(t) > 0 {
			if c.credential && !c.metadata.isMeta() && hasCredentialType(t) {
				return walkTypedCredential(t, c)
			}
			if !c.metadata.isMeta() && isReference(t) {
				return walkReference(t, c)
			}
			return Marker, true
		}
		return walkMap(t, c)
	case []interface{}:
		if c.sensitive && len(t) > 0 {
			return Marker, true
		}
		return walkArray(t, c)
	case string:
		if c.sensitive {
			if isRedactable(t) && !c.metadata.visible(t) {
				return Marker, true
			}
			return t, false
		}
		r := redactString(t)
		return r, r != t
	case json.Number, float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		if c.sensitive && !c.keepNumbers && !c.metadata.isMeta() {
			return Marker, true
		}
		return v, false
	}
	// Typed value: convert to generic JSON and walk that.
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Interface:
		if rv.IsNil() {
			return v, false
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		if c.sensitive {
			return Marker, true
		}
		return v, false
	}
	g, ok := decode(raw)
	if !ok {
		return v, false
	}
	out, _ := walk(g, c)
	return out, true // the type changed (typed -> generic)
}

func walkMap(t map[string]interface{}, c ctx) (interface{}, bool) {
	out := make(map[string]interface{}, len(t))
	changed := false
	nameField, pairSecret, pairMeta := nameValuePair(t)
	jobSecret := isSecretJob(t)
	userObj := isUserObject(t)
	method, _ := t["method"].(string)
	keytabJob := strings.HasPrefix(method, "kerberos.keytab")
	for k, val := range t {
		nk := normalize(k)
		child := ctx{kerberos: c.kerberos || keytabJob || strings.Contains(nk, "keytab") || strings.Contains(nk, "kerberos")}
		switch {
		case k == nameField:
			// The name of a {name|key|variable, value} pair is visible.
		case k == "value" && nameField != "":
			child.sensitive, child.metadata = pairSecret, pairMeta
		default:
			sens, meta := classifyKey(k)
			child.sensitive = sens || (jobSecret && k == "result") ||
				(c.kerberos && nk == "file") ||
				(userObj && nk == "hash")
			child.metadata = meta
			child.keepNumbers = keepsNumbers(k)
			child.credential = nk == "credential"
			// LDAP attribute_maps.passwd maps passwd fields to LDAP attribute
			// names (user_name: uid); it holds no secret although its name
			// contains "passwd". Only that object directly under
			// attribute_maps is exempt: its fields are still matched by
			// name and every other key keeps the normal rules.
			if _, isMap := val.(map[string]interface{}); isMap {
				child.attrMaps = nk == "attributemaps"
				if c.attrMaps && nk == "passwd" {
					child.sensitive, child.metadata = false, metaNone
				}
			}
		}
		nv, ch := walk(val, child)
		out[k] = nv
		changed = changed || ch
	}
	return out, changed
}

// redactString redacts a string that is not under a sensitive key. A string
// holding a JSON object or array is redacted structurally and re-encoded
// compactly; anything else is redacted as text.
func redactString(s string) string {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) > 1 && (trimmed[0] == '{' || trimmed[0] == '[') {
		if v, ok := decode([]byte(trimmed)); ok {
			out, changed := walk(v, ctx{})
			if !changed {
				return s
			}
			if enc, err := encode(out, false); err == nil {
				return string(enc)
			}
		}
	}
	return Text(s)
}

// nameValuePair reports whether m is an environment-style entry such as
// {"name": "POSTGRES_PASSWORD", "value": "..."} or {"key": "DB_PASS",
// "value": "..."}. It returns the field holding the name (empty if m is not
// such a pair), whether that name is sensitive and whether it is a metadata
// name (REDIS_PASSWORD_SET), whose boolean or numeric value stays visible.
func nameValuePair(m map[string]interface{}) (field string, sensitive bool, metadata metaKind) {
	if _, ok := m["value"]; !ok {
		return "", false, metaNone
	}
	for _, f := range []string{"name", "key", "variable"} {
		if n, ok := m[f].(string); ok {
			sens, meta := classifyKey(n)
			return f, sens, meta
		}
	}
	return "", false, metaNone
}

// referenceTypeFields are the normalised names of the fields that give the
// kind of a stored credential.
var referenceTypeFields = map[string]bool{
	"type": true, "providertype": true, "credentialtype": true, "credentialstype": true,
	"authtype": true, "authenticationtype": true,
}

// enumValue matches a short identifier-like value (S3, SSH_CREDENTIALS,
// oauth2), as held by a type field.
var enumValue = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// isReferenceField reports whether key, holding val, identifies a stored
// credential rather than holding its secret: a scalar id or name, or a
// known type field (type, provider_type, credential_type, auth_type) whose
// value looks like an enum. Other *_type fields (key_type) are redacted.
func isReferenceField(key string, val interface{}) bool {
	if !isScalar(val) {
		return false
	}
	k := normalize(key)
	switch {
	case k == "id" || k == "name":
		return true
	case referenceTypeFields[k]:
		s, isString := val.(string)
		return !isString || enumValue.MatchString(s)
	}
	return false
}

// isReference reports whether m, found under a sensitive key, refers to a
// stored credential (cloudsync "credentials", replication
// "ssh_credentials"): it has a scalar "id" or "name".
func isReference(m map[string]interface{}) bool {
	for _, f := range []string{"id", "name"} {
		if isScalar(m[f]) {
			return true
		}
	}
	return false
}

func isScalar(v interface{}) bool {
	switch v.(type) {
	case string, bool, json.Number, float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return true
	}
	return false
}

// walkReference redacts an object under a sensitive key that refers to a
// stored credential. Reference fields (see isReferenceField) stay
// visible, nested objects that have a reference field are handled the same
// way (provider: {"type": "S3", ...}), and every other field is redacted
// as if it were directly under the sensitive key.
func walkReference(t map[string]interface{}, c ctx) (interface{}, bool) {
	out := make(map[string]interface{}, len(t))
	for k, val := range t {
		var nv interface{}
		switch m, isMap := val.(map[string]interface{}); {
		case isReferenceField(k, val):
			nv, _ = walk(val, ctx{kerberos: c.kerberos})
		case isMap && hasReferenceField(m):
			nv, _ = walkReference(m, c)
		default:
			nv, _ = walk(val, ctx{sensitive: true, keepNumbers: keepsNumbers(k), kerberos: c.kerberos})
		}
		out[k] = nv
	}
	return out, true
}

// typedCredentialVisible are the fields of a typed credential object (one
// with a credential_type, such as the directoryservices.update credential)
// that name who authenticates rather than hold a secret. Every other field
// is redacted, so a field this list does not know about stays hidden.
// client_certificate is an id or name (see walkTypedCredential).
var typedCredentialVisible = map[string]bool{
	"credentialtype": true, "username": true, "principal": true, "binddn": true,
}

// hasCredentialType reports whether m has a credential_type field.
func hasCredentialType(m map[string]interface{}) bool {
	_, ok := m["credential_type"]
	return ok
}

// walkTypedCredential redacts the object under a "credential" key that has
// a credential_type field. Only the allowlisted fields
// (typedCredentialVisible), a client_certificate given as an id or name, and
// an id or name stay visible; every other field (password, bindpw, anything
// unknown) is redacted as if it were directly under a sensitive key.
// Visible values are still redacted as free text, and a visible field that
// holds an object or array, or a credential_type that is not an enum value,
// is redacted. An object under client_certificate keeps only its scalar id
// and name.
func walkTypedCredential(t map[string]interface{}, c ctx) (interface{}, bool) {
	out := make(map[string]interface{}, len(t))
	changed := false
	for k, val := range t {
		nk := normalize(k)
		var nv interface{}
		var ch bool
		switch m, isMap := val.(map[string]interface{}); {
		case nk == "clientcertificate" && isMap && len(m) > 0:
			nv, ch = walkCertificateReference(m)
		case nk == "credentialtype" && isScalar(val) && !isReferenceField(k, val):
			nv, ch = Marker, true // not an enum value: could hold anything
		case isScalar(val) && (typedCredentialVisible[nk] || nk == "clientcertificate" || nk == "id" || nk == "name"):
			nv, ch = walk(val, ctx{kerberos: c.kerberos})
		default:
			nv, ch = walk(val, ctx{sensitive: true, kerberos: c.kerberos})
		}
		out[k] = nv
		changed = changed || ch
	}
	return out, changed
}

// walkCertificateReference keeps the scalar id and name of a certificate
// object and redacts everything else in it.
func walkCertificateReference(m map[string]interface{}) (interface{}, bool) {
	out := make(map[string]interface{}, len(m))
	changed := false
	for k, val := range m {
		var nv interface{}
		var ch bool
		if nk := normalize(k); (nk == "id" || nk == "name") && isScalar(val) {
			nv, ch = walk(val, ctx{})
		} else {
			nv, ch = walk(val, ctx{sensitive: true})
		}
		out[k] = nv
		changed = changed || ch
	}
	return out, changed
}

func hasReferenceField(m map[string]interface{}) bool {
	for k, v := range m {
		if isReferenceField(k, v) {
			return true
		}
	}
	return false
}

// isUserObject reports whether m looks like a user account, whose "hash"
// field is a password hash.
func isUserObject(m map[string]interface{}) bool {
	_, u := m["username"]
	_, uid := m["uid"]
	return u || uid
}

// secretResultMethods return a secret as their job result (e.g. a dataset
// encryption key as a bare hex string), so key-based matching cannot see it.
var secretResultMethods = map[string]bool{
	"pool.dataset.export_key":                  true,
	"pool.dataset.export_keys":                 true,
	"pool.dataset.export_keys_for_replication": true,
}

// isSecretJob reports whether m is a job (core.get_jobs entry) whose result
// is a secret.
func isSecretJob(m map[string]interface{}) bool {
	method, _ := m["method"].(string)
	_, hasResult := m["result"]
	return hasResult && secretResultMethods[method]
}

// decode parses raw as a single JSON value, keeping numbers exact.
func decode(raw []byte) (interface{}, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err == nil {
		return nil, false // trailing data: not a single JSON value
	}
	return v, true
}

func encode(v interface{}, indent bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// JSON returns raw with secrets redacted, re-encoded compactly. Input that
// is not valid JSON is redacted as text. Intended for debug logs.
func JSON(raw []byte) []byte {
	v, ok := decode(raw)
	if !ok {
		return []byte(Text(string(raw)))
	}
	out, changed := walk(v, ctx{})
	if !changed {
		return raw
	}
	enc, err := encode(out, false)
	if err != nil {
		return []byte(Text(string(raw)))
	}
	return enc
}

// ToolOutput redacts a tool result before it is returned to the client. A
// JSON result is redacted structurally and returned unchanged when it holds
// no secrets; when something is redacted it is re-encoded, indented if the
// original was. Any other text is redacted with Text.
func ToolOutput(s string) string {
	v, ok := decode([]byte(s))
	if !ok {
		return Text(s)
	}
	out, changed := walk(v, ctx{})
	if !changed {
		return s
	}
	enc, err := encode(out, strings.Contains(s, "\n"))
	if err != nil {
		return Text(s)
	}
	return string(enc)
}

// maxScalarText is the length of the longest value isScalarText accepts.
const maxScalarText = 40

var (
	// isoDate matches an ISO 8601 / RFC 3339 date or timestamp.
	isoDate = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}(?:[T ][0-9]{2}:[0-9]{2}(?::[0-9]{2}(?:\.[0-9]{1,9})?)?(?:Z|[+-][0-9]{2}:?[0-9]{2})?)?$`)
	// duration matches a Go-style or simple duration (1h, 1h30m, 500ms,
	// 7d, 2w) or an ISO 8601 one (PT1H, P30D).
	duration = regexp.MustCompile(`^(?:(?:[0-9]+(?:\.[0-9]+)?(?:ns|us|µs|ms|s|m|h|d|w))+|P(?:[0-9]+[YMWD])*(?:T(?:[0-9]+(?:\.[0-9]+)?[HMS])+)?)$`)
)

// isFlagText reports whether v is boolean-like text (true, no, null, or
// empty), which a metaFlag key may show.
func isFlagText(v string) bool {
	switch strings.ToLower(v) {
	case "", "true", "false", "null", "none", "nil", "yes", "no", "on", "off":
		return true
	}
	return false
}

// isScalarText reports whether a value written as text is a boolean, a
// number, null, empty, a date or a duration, which a metadata key
// (token_ttl, password_expires) may show.
func isScalarText(v string) bool {
	if isFlagText(v) {
		return true
	}
	if len(v) > maxScalarText {
		return false
	}
	if v != "P" && (isoDate.MatchString(v) || duration.MatchString(v)) {
		return true
	}
	digits := 0
	for i, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case (r == '-' || r == '+') && i == 0, r == '.':
		default:
			return false
		}
	}
	return digits > 0
}

// isIdentText reports whether v is a short identifier such as an algorithm
// name (HS256, aes-256-gcm, sha512), which a *_algorithm key may show.
func isIdentText(v string) bool {
	if v == "" || len(v) > maxScalarText {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '_' || c == '-' || c == '.' || c == '+' || c == '/') {
			return false
		}
	}
	return true
}
