package redact

import (
	"strings"
	"testing"
)

// A quoted value after a secret key that has no closing quote (text cut
// short) is redacted to the end of the text instead of being left in place.
func TestUnclosedQuotedSecretRunsToEnd(t *testing.T) {
	for _, s := range []string{
		`{"config":{"bindpw":"s3cretVal`,
		`{"config":{"bindpw": "s3cretVal`,
		`{'bindpw': 's3cretVal`,
		`failed: {'password': 'abc', 'bindpw': 's3cretVal`,
		`{\"bindpw\":\"s3cretVal`,
		"{\"bindpw\":\"s3cretVal\nnext line s3cretMore",
	} {
		got := Text(s)
		if strings.Contains(got, "s3cret") {
			t.Errorf("Text(%q) = %q, leaks the value", s, got)
		}
		if !strings.Contains(got, Marker) {
			t.Errorf("Text(%q) = %q, no marker", s, got)
		}
	}
	// Closed values and non-secret keys are unchanged.
	for s, want := range map[string]string{
		`{"name":"plain`:                     `{"name":"plain`,
		`{"bindpw":"x","name":"y`:            `{"bindpw":"` + Marker + `","name":"y`,
		`{"token_ttl":"3600"}`:               `{"token_ttl":"3600"}`,
		`{"bindpw":"` + Marker + `"} rest "`: `{"bindpw":"` + Marker + `"} rest "`,
	} {
		if got := Text(s); got != want {
			t.Errorf("Text(%q) = %q, want %q", s, got, want)
		}
	}
}
