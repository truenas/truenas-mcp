package redact

import (
	"encoding/json"
	"testing"
)

// LDAP attribute_maps.passwd maps passwd fields to LDAP attribute names; it
// is not a secret. The exemption covers only that object directly under
// attribute_maps: its fields are still matched by name, and the same names
// anywhere else are redacted as usual.
func TestAttributeMapsPasswdVisibleOtherSecretsMasked(t *testing.T) {
	runChecks(t, `{"configuration":{
		"attribute_maps":{"passwd":{"user_name":"uid","user_uid":"uidNumber","user_shell":"loginShell"},
			"shadow":{"shadow_last_change":"shadowLastChange"}},
		"search_bases":{"base_user":"ou=people,dc=example,dc=com"},
		"bindpw":"fake-bind-pw",
		"idmap":{"idmap_domain":{"ldap_user_dn_password":"fake-idmap-pw"}}},
		"credential":{"credential_type":"LDAP_PLAIN","binddn":"cn=admin,dc=example,dc=com","bindpw":"fake-cred-pw"}}`,
		[]check{
			kept("uid", "configuration", "attribute_maps", "passwd", "user_name"),
			kept("uidNumber", "configuration", "attribute_maps", "passwd", "user_uid"),
			kept("loginShell", "configuration", "attribute_maps", "passwd", "user_shell"),
			kept("shadowLastChange", "configuration", "attribute_maps", "shadow", "shadow_last_change"),
			kept("ou=people,dc=example,dc=com", "configuration", "search_bases", "base_user"),
			redacted("configuration", "bindpw"),
			redacted("configuration", "idmap", "idmap_domain", "ldap_user_dn_password"),
			redacted("credential", "bindpw"),
			kept("cn=admin,dc=example,dc=com", "credential", "binddn"),
		})
}

// The exemption does not widen: a secret-named field inside passwd, a
// "passwd" key anywhere else, and a scalar or array under attribute_maps are
// still redacted, and free text inside the visible object is still scanned.
func TestAttributeMapsExemptionIsTight(t *testing.T) {
	runChecks(t, `{
		"attribute_maps":{"passwd":{"user_name":"uid","password":"fake-user-pw","bindpw":"fake-bind-pw",
			"user_gecos":"password=fake-inline-pw"}},
		"other":{"passwd":{"user_name":"fake-other-pw"}},
		"nested":{"attribute_maps":{"deeper":{"passwd":{"user_name":"fake-deep-pw"}}}},
		"passwd":{"user_name":"fake-top-pw"}}`,
		[]check{
			kept("uid", "attribute_maps", "passwd", "user_name"),
			redacted("attribute_maps", "passwd", "password"),
			redacted("attribute_maps", "passwd", "bindpw"),
			kept("password="+Marker, "attribute_maps", "passwd", "user_gecos"),
			redacted("other", "passwd"),
			redacted("nested", "attribute_maps", "deeper", "passwd"),
			redacted("passwd"),
		})
	for _, in := range []string{
		`{"attribute_maps":{"passwd":"fake-scalar-pw"}}`,
		`{"attribute_maps":{"passwd":["fake-array-pw"]}}`,
	} {
		runChecks(t, in, []check{redacted("attribute_maps", "passwd")})
	}
	// Value (used by the tools to mask previews) agrees with ToolOutput.
	var v map[string]interface{}
	if err := json.Unmarshal([]byte(`{"attribute_maps":{"passwd":{"user_name":"uid"}}}`), &v); err != nil {
		t.Fatal(err)
	}
	got := Value(v).(map[string]interface{})["attribute_maps"].(map[string]interface{})["passwd"].(map[string]interface{})
	if got["user_name"] != "uid" {
		t.Errorf("Value masked attribute_maps.passwd: %v", got)
	}
}
