// Package redact removes secrets from data before it leaves the server: tool
// results returned to the MCP client, error messages, and debug logs.
//
// Redaction is key-based (values under credential-bearing keys such as
// password, bindpw, token, private_key or a key named "key") with a
// value-based backstop that removes PEM private key blocks wherever they
// appear. Redacted values are replaced with Marker; the key is kept so the
// model can still see that the field exists.
package redact

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

// Marker replaces every redacted value.
const Marker = "[REDACTED]"

// sensitiveKeyFragments match keys, after normalisation (lower case, with
// '_', '-', '.' and spaces removed), whose values must never be returned to
// the client or written to logs.
var sensitiveKeyFragments = []string{
	"password", "passwd", "passphrase", "secret", "token", "apikey",
	"privatekey", "credential", "bindpw", "keytab",
}

// sensitiveExactKeys match whole normalised keys that are too short or too
// generic to use as fragments.
var sensitiveExactKeys = map[string]bool{
	"key": true, "pass": true, "pw": true, "pwd": true, "psk": true,
}

// sensitiveKeySuffixes match normalised keys that end in a compound key name
// written without a separator (encryptionkey, datasetKey, sshkey, ...).
var sensitiveKeySuffixes = []string{
	"encryptionkey", "datasetkey", "sshkey", "accesskey", "authkey", "masterkey", "signingkey",
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

// endsWithKeyWord reports whether key ends in a separate word "key":
// "encryption_key", "dataset key", "SSH-KEY" or "privateKey", but not
// "monkey", "hotkey" or "keyboard".
func endsWithKeyWord(key string) bool {
	if len(key) <= 3 {
		return false
	}
	head, tail := key[:len(key)-3], key[len(key)-3:]
	switch {
	case strings.EqualFold(tail, "key") && strings.ContainsAny(head[len(head)-1:], "_-. "):
		return true
	case tail == "Key":
		r := rune(head[len(head)-1])
		return unicode.IsLower(r) || unicode.IsDigit(r)
	}
	return false
}

// IsSensitiveKey reports whether values under key must be redacted. Matching
// is case-insensitive and ignores '_', '-', '.' and spaces. Public keys are
// not secret, and "..._type" fields (credential_type, token_type) hold enum
// labels, so both are left alone.
func IsSensitiveKey(key string) bool {
	k := normalize(key)
	if k == "" || strings.Contains(k, "public") || strings.HasSuffix(k, "type") {
		return false
	}
	if sensitiveExactKeys[k] {
		return true
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
	return endsWithKeyWord(key)
}

// pemPrivateKey matches a PEM private key block (RSA, EC, OPENSSH, ENCRYPTED,
// PGP ...). A block without its END line is redacted to the end of the text.
var pemPrivateKey = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----(?s:.*?)(?:-----END [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----|\z)`)

// quotedPair matches "key": "value" (JSON) and 'key': 'value' (Python repr,
// as in middleware error messages) pairs embedded in free text.
var quotedPairs = []*regexp.Regexp{
	regexp.MustCompile(`"([^"\\\n]{1,64})"(\s*[:=]\s*)"((?:[^"\\]|\\.)*)"`),
	regexp.MustCompile(`'([^'\\\n]{1,64})'(\s*[:=]\s*)'((?:[^'\\]|\\.)*)'`),
}

// Text redacts secrets from free text such as error messages and traces:
// PEM private key blocks, and quoted key/value pairs whose key is sensitive.
func Text(s string) string {
	s = pemPrivateKey.ReplaceAllLiteralString(s, Marker)
	for i, re := range quotedPairs {
		q := `"`
		if i == 1 {
			q = `'`
		}
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			sub := re.FindStringSubmatch(m)
			if !IsSensitiveKey(sub[1]) || sub[3] == "" || sub[3] == Marker {
				return m
			}
			return q + sub[1] + q + sub[2] + q + Marker + q
		})
	}
	return s
}

// Value returns a deep copy of v (as decoded by encoding/json) with secrets
// redacted. The input is never modified.
func Value(v interface{}) interface{} {
	out, _ := walk(v, false)
	return out
}

// walk redacts v and reports whether anything changed. Under a sensitive key
// every non-empty string is redacted; numbers, booleans and null are kept
// (they are IDs and flags, e.g. a keychain credential ID or
// password_disabled), and nested objects are walked normally so their
// non-secret fields (name, type, ...) stay visible.
func walk(v interface{}, sensitive bool) (interface{}, bool) {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		changed := false
		pairSecret := isSensitiveNameValuePair(t)
		jobSecret := isSecretJob(t)
		for k, val := range t {
			if jobSecret && k == "result" {
				out[k] = redactAll(val)
				changed = true
				continue
			}
			s := IsSensitiveKey(k) || (pairSecret && k == "value")
			nv, c := walk(val, s)
			out[k] = nv
			changed = changed || c
		}
		return out, changed
	case []interface{}:
		out := make([]interface{}, len(t))
		changed := false
		for i, val := range t {
			nv, c := walk(val, sensitive)
			out[i] = nv
			changed = changed || c
		}
		return out, changed
	case string:
		if sensitive && t != "" && t != Marker {
			return Marker, true
		}
		r := pemPrivateKey.ReplaceAllLiteralString(t, Marker)
		return r, r != t
	default:
		return v, false
	}
}

// redactAll redacts every non-empty string in v, keeping the structure.
func redactAll(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, val := range t {
			out[k] = redactAll(val)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, val := range t {
			out[i] = redactAll(val)
		}
		return out
	case string:
		if t == "" {
			return t
		}
		return Marker
	default:
		return v
	}
}

// isSensitiveNameValuePair reports whether m is an environment-style entry
// such as {"name": "POSTGRES_PASSWORD", "value": "..."} (app configs) whose
// value must be redacted because of its name.
func isSensitiveNameValuePair(m map[string]interface{}) bool {
	if _, ok := m["value"]; !ok {
		return false
	}
	for _, field := range []string{"name", "key", "variable"} {
		if n, ok := m[field].(string); ok && IsSensitiveKey(n) {
			return true
		}
	}
	return false
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
	out, changed := walk(v, false)
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
	out, changed := walk(v, false)
	if !changed {
		return s
	}
	enc, err := encode(out, strings.Contains(s, "\n"))
	if err != nil {
		return Text(s)
	}
	return string(enc)
}
