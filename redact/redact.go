// Package redact removes secrets from data before it leaves the server: tool
// results returned to the MCP client, error messages, and debug logs.
//
// Redaction is key-based (values under credential-bearing keys such as
// password, DB_PASS, bindpw, token, private_key or a key named "key") with
// value-based backstops for free text: PEM private key blocks, quoted and
// unquoted key/value pairs, NAME=VALUE environment entries, Bearer tokens and
// passwords embedded in URLs. Redacted values are replaced with Marker; the
// key is kept so the model can still see that the field exists.
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
	"privatekey", "credential", "bindpw", "keytab", "cookie", "authorization",
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

// metadataSegments, as the final word of a multi-word key, describe a secret
// rather than hold it (pass_count, session_timeout, password_length,
// token_ttl), so the key is not sensitive.
var metadataSegments = map[string]bool{
	"count": true, "timeout": true, "length": true, "len": true, "ttl": true,
	"lifetime": true, "expiry": true, "expires": true, "expiration": true, "age": true,
	"interval": true, "size": true, "min": true, "max": true, "limit": true,
	"retries": true, "attempts": true, "enabled": true, "disabled": true,
	"required": true, "set": true, "configured": true, "changed": true, "algorithm": true,
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

// IsSensitiveKey reports whether values under key must be redacted. Matching
// is case-insensitive and ignores '_', '-', '.', spaces and camelCase.
// Public key fields and "<kind>_type" enum fields (credential_type,
// token_type) are not secret.
func IsSensitiveKey(key string) bool {
	k := normalize(key)
	if k == "" || publicKeyNames[k] {
		return false
	}
	segs := segments(key)
	if n := len(segs); n >= 2 && segs[n-1] == "type" && !typeExemptBlockers[segs[n-2]] {
		return false
	}
	if n := len(segs); n >= 2 && metadataSegments[segs[n-1]] {
		return false
	}
	if sensitiveExactKeys[k] {
		return true
	}
	for _, s := range segs {
		if sensitiveSegments[s] {
			return true
		}
	}
	if len(segs) > 1 && segs[len(segs)-1] == "key" {
		return true // encryption_key, privateKey, "dataset key"
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
	if strings.HasSuffix(k, "pass") {
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
	// urlUserinfo matches scheme://user:password@ in URLs and DSNs.
	urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^\s:/@'"]*:)([^\s@/'"]+)@`)
	// envPair matches a whole string of the form NAME=VALUE (compose
	// environment lists, env files).
	envPair = regexp.MustCompile(`^(\s*[A-Za-z_][A-Za-z0-9_.-]{0,127}\s*=)(?s:(.+))$`)
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
	// barePair matches key=value / key: value with an unquoted key; the
	// value may be quoted (Python kwargs: password='x') or bare up to the
	// next delimiter.
	barePair  = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_.-]{0,63})(\s*[=:]\s*)(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)'|([^\s,;&'"(){}\[\]<>]+))`)
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
		if IsSensitiveKey(name) && isRedactable(m[2]) {
			return m[1] + Marker
		}
	}
	s = authHeader.ReplaceAllString(s, "${1}${2} "+Marker)
	s = bearerToken.ReplaceAllString(s, "${1}"+Marker)
	s = quotedPair.ReplaceAllStringFunc(s, func(m string) string {
		sub := quotedPair.FindStringSubmatch(m)
		key := quotedKey.FindStringSubmatch(sub[1])
		if key == nil || !IsSensitiveKey(key[1]) {
			return m
		}
		if strings.HasPrefix(m[len(sub[1]):], `"`) {
			if !isRedactable(sub[2]) {
				return m
			}
			return sub[1] + `"` + Marker + `"`
		}
		if !isRedactable(sub[3]) {
			return m
		}
		return sub[1] + `'` + Marker + `'`
	})
	s = escapedPair.ReplaceAllStringFunc(s, func(m string) string {
		sub := escapedPair.FindStringSubmatch(m)
		if !IsSensitiveKey(sub[2]) || !isRedactable(sub[3]) {
			return m
		}
		return sub[1] + `\"` + Marker + `\"`
	})
	return redactBarePairs(s)
}

// redactBarePairs redacts the values of key=value / key: value pairs with a
// sensitive, unquoted key. Matches are scanned one at a time so that a
// non-sensitive key ("failed: password=x") does not swallow the next pair.
func redactBarePairs(s string) string {
	var b strings.Builder
	pos := 0
	for pos < len(s) {
		m := barePair.FindStringSubmatchIndex(s[pos:])
		if m == nil {
			break
		}
		key := s[pos+m[2] : pos+m[3]]
		valStart := pos + m[5] // end of the separator
		if !IsSensitiveKey(key) {
			b.WriteString(s[pos:valStart])
			pos = valStart
			continue
		}
		b.WriteString(s[pos:valStart])
		end := pos + m[1]
		var val, quote string
		switch {
		case m[6] >= 0:
			val, quote = s[pos+m[6]:pos+m[7]], `"`
		case m[8] >= 0:
			val, quote = s[pos+m[8]:pos+m[9]], `'`
		default:
			val = s[pos+m[10] : pos+m[11]]
		}
		if !isRedactable(val) || (quote == "" && authSchemes[strings.ToLower(val)]) {
			b.WriteString(s[valStart:end])
		} else {
			b.WriteString(quote + Marker + quote)
		}
		pos = end
	}
	b.WriteString(s[pos:])
	return b.String()
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
	sensitive   bool // under a sensitive key
	keepNumbers bool // numbers under this key are IDs (see keepsNumbers)
	kerberos    bool // inside a keytab/kerberos object
}

// walk redacts v and reports whether anything changed.
//
// Under a sensitive key, a non-empty string, a number (unless it is an ID)
// or a non-empty object/array is replaced with Marker wholesale. Booleans,
// null and empty strings stay visible (password_disabled, unset fields).
// Every other string is redacted as free text, and a string that holds JSON
// is redacted structurally.
func walk(v interface{}, c ctx) (interface{}, bool) {
	switch t := v.(type) {
	case nil, bool:
		return v, false
	case map[string]interface{}:
		if c.sensitive && len(t) > 0 {
			return Marker, true
		}
		return walkMap(t, c)
	case []interface{}:
		if c.sensitive && len(t) > 0 {
			return Marker, true
		}
		out := make([]interface{}, len(t))
		changed := false
		for i, val := range t {
			nv, ch := walk(val, ctx{kerberos: c.kerberos})
			out[i] = nv
			changed = changed || ch
		}
		return out, changed
	case string:
		if c.sensitive {
			if isRedactable(t) {
				return Marker, true
			}
			return t, false
		}
		r := redactString(t)
		return r, r != t
	case json.Number, float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		if c.sensitive && !c.keepNumbers {
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
	nameField, pairSecret := nameValuePair(t)
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
			child.sensitive = pairSecret
		default:
			child.sensitive = IsSensitiveKey(k) ||
				(jobSecret && k == "result") ||
				(c.kerberos && nk == "file") ||
				(userObj && nk == "hash")
			child.keepNumbers = keepsNumbers(k)
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
// such a pair) and whether that name is sensitive.
func nameValuePair(m map[string]interface{}) (string, bool) {
	if _, ok := m["value"]; !ok {
		return "", false
	}
	for _, field := range []string{"name", "key", "variable"} {
		if n, ok := m[field].(string); ok {
			return field, IsSensitiveKey(n)
		}
	}
	return "", false
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
