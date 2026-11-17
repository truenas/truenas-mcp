package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/truenas"
)

// query_directory_services shows the stored credential and configuration
// with secrets (credential.password, credential.bindpw, idmap
// ldap_user_dn_password, ...) replaced by redact.Marker. A caller that
// sends that object back to configure_directory_service would otherwise
// store the marker as the real secret, so the stored value is put back
// where that is safe, and the update is refused where it is not.

// dsScopeFields are the payload fields that say which directory a stored
// secret belongs to. A kept secret is refused when any of them is sent
// with a value different from the stored one.
var dsScopeFields = []valuePath{
	{"service_type"},
	{"kerberos_realm"},
	{"configuration", "domain"},
	{"configuration", "target_server"},
	{"configuration", "server_urls"},
	// LDAP auxiliary_parameters lines are written into the same SSSD
	// domain section as the bind credential, so a line such as
	// ldap_backup_uri or ldap_uri can point the stored secret at another
	// server. When the parameters are themselves the kept secret, the
	// substituted value equals the stored one.
	{"configuration", "auxiliary_parameters"},
}

// dsTLSFields are boolean fields that protect a stored secret on the wire
// (LDAP and IPA certificate validation and STARTTLS, and an idmap
// backend's certificate validation), mapped to their middleware default.
// With a kept secret they may not be turned off relative to the stored
// config at the same path. directoryservices.update merges only top-level
// keys, so a field omitted from a sent object takes its default: an
// omitted field whose default is false (starttls) counts as turned off;
// validate_certificates defaults to true, so omitting it is allowed.
var dsTLSFields = map[string]bool{"validate_certificates": true, "starttls": false}

// restoreDirectoryServiceSecrets replaces every value in payload that is
// exactly redact.Marker with the value stored at the same path in the
// current directoryservices.config, when that is safe, and returns the new
// payload and the kept paths. A payload with no marker is returned as is
// without reading the stored config.
func restoreDirectoryServiceSecrets(ctx context.Context, client *truenas.Client, payload map[string]interface{}) (map[string]interface{}, []valuePath, error) {
	if _, found := findRedactionMarker(payload, nil); !found {
		return payload, nil, nil
	}
	raw, err := client.Call(ctx, "directoryservices.config")
	if err != nil {
		return nil, nil, fmt.Errorf("a value is the redaction marker %s, but the stored directory service config could not be read to keep it: %w", redact.Marker, err)
	}
	var stored map[string]interface{}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, nil, fmt.Errorf("a value is the redaction marker %s, but the stored directory service config could not be parsed: %w", redact.Marker, err)
	}
	out, kept, err := restoreRedactedValuesFrom(payload, stored, restoreLabels{stored: "the stored directory service config"})
	if err != nil {
		return nil, nil, err
	}
	if err := checkDirectoryServiceKept(kept, stored, out); err != nil {
		return nil, nil, err
	}
	return out, kept, nil
}

// checkDirectoryServiceKept refuses a kept path unless:
//   - query_directory_services redacts the stored value there, and the
//     outgoing payload still redacts it there;
//   - the object holding it, unless that is the configuration itself,
//     keeps every non-secret text field (for a credential: credential_type
//     and username, principal or bind DN; for an idmap backend: its name,
//     URL, bind DN and base DN), so the secret is not used for a different
//     account or server;
//   - the directory it belongs to is unchanged: service_type, and when
//     sent, kerberos_realm and the configuration's domain, target_server,
//     server_urls and auxiliary_parameters match the stored config;
//   - no certificate validation or STARTTLS flag (anywhere in the payload,
//     including idmap backends and trusted domains) is turned off where the
//     stored config has it on, either explicitly or, for a flag whose
//     middleware default is false, by omitting it from a sent object.
//     Turning one on is fine.
func checkDirectoryServiceKept(kept []valuePath, stored, next map[string]interface{}) error {
	if len(kept) == 0 {
		return nil
	}
	if err := checkKeptRedacted(kept, stored, next, "", "query_directory_services"); err != nil {
		return err
	}
	shownStored := redact.Value(deepCopyValue(stored))
	for _, p := range kept {
		parent := p[:len(p)-1]
		if len(parent) == 0 || (len(parent) == 1 && parent[0] == "configuration") {
			// A secret directly in the configuration (LDAP
			// auxiliary_parameters) belongs to the directory itself,
			// which the scope fields below pin.
			if err := checkDSScope(p, stored, next); err != nil {
				return err
			}
			continue
		}
		curEntry, _ := getValuePath(stored, parent)
		nextEntry, _ := getValuePath(next, parent)
		curMap, _ := curEntry.(map[string]interface{})
		nextMap, _ := nextEntry.(map[string]interface{})
		names := map[string]bool{}
		for k := range curMap {
			names[k] = true
		}
		for k := range nextMap {
			names[k] = true
		}
		for _, k := range sortedKeys(names) {
			if v, _ := getValuePath(shownStored, appendPath(parent, k)); v == redact.Marker {
				continue // another secret: kept or replaced on its own
			}
			cv, cok := curMap[k]
			nv, nok := nextMap[k]
			if !isDSIdentityValue(cv) && !isDSIdentityValue(nv) {
				continue
			}
			if cok != nok || !jsonValuesEqual(cv, nv) {
				return fmt.Errorf("%s is the redaction marker %s, but %s changes, so the stored secret is not kept for a different account or server; send the real value instead", p, redact.Marker, appendPath(parent, k))
			}
		}
		if err := checkDSScope(p, stored, next); err != nil {
			return err
		}
	}
	if err := checkDSTLSKept(kept[0], stored, next, nil); err != nil {
		return err
	}
	return checkDSTLSOmitted(kept[0], stored, next, stored, nil)
}

// checkDSTLSOmitted refuses, naming kept path p, when a dsTLSFields field
// whose middleware default is false is true in the stored config but
// omitted from next, while next sends the top-level object holding it (so
// the middleware resets the field to its default instead of keeping it).
func checkDSTLSOmitted(p valuePath, stored, next map[string]interface{}, cur interface{}, path valuePath) error {
	switch x := cur.(type) {
	case map[string]interface{}:
		for _, k := range sortedKeys(x) {
			child := appendPath(path, k)
			if def, isTLS := dsTLSFields[k]; isTLS && !def && x[k] == true && len(child) > 1 {
				if _, sent := getValuePath(next, child[:1]); sent {
					if _, ok := getValuePath(next, child); !ok {
						return fmt.Errorf("%s is the redaction marker %s, but %s is omitted while the stored config has it on; the middleware would reset it to false, so the stored secret is not kept. Send %s: true, or send the real value instead", p, redact.Marker, child, k)
					}
				}
			}
			if err := checkDSTLSOmitted(p, stored, next, x[k], child); err != nil {
				return err
			}
		}
	case []interface{}:
		for i, c := range x {
			if err := checkDSTLSOmitted(p, stored, next, c, appendPath(path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkDSTLSKept refuses, naming kept path p, when any dsTLSFields field in
// next is false where the stored config at the same path has it true.
func checkDSTLSKept(p valuePath, stored map[string]interface{}, next interface{}, path valuePath) error {
	switch x := next.(type) {
	case map[string]interface{}:
		for _, k := range sortedKeys(x) {
			child := appendPath(path, k)
			_, isTLS := dsTLSFields[k]
			if b, ok := x[k].(bool); ok && isTLS && !b {
				if cv, _ := getValuePath(stored, child); cv == true {
					return fmt.Errorf("%s is the redaction marker %s, but %s turns off a TLS protection the stored config has, so the stored secret is not kept; send the real value instead, or leave %s on", p, redact.Marker, child, k)
				}
			}
			if err := checkDSTLSKept(p, stored, x[k], child); err != nil {
				return err
			}
		}
	case []interface{}:
		for i, c := range x {
			if err := checkDSTLSKept(p, stored, c, appendPath(path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkDSScope refuses kept path p when a dsScopeFields field is sent with
// a value different from the stored config (service_type always counts).
func checkDSScope(p valuePath, stored, next map[string]interface{}) error {
	for _, f := range dsScopeFields {
		nv, nok := getValuePath(next, f)
		if !nok && f[0] != "service_type" {
			continue // omitted: TrueNAS keeps the stored value
		}
		cv, cok := getValuePath(stored, f)
		if cok != nok || !jsonValuesEqual(cv, nv) {
			return fmt.Errorf("%s is the redaction marker %s, but %s differs from the stored directory service config, so the stored secret is not sent to a different directory; send the real value instead", p, redact.Marker, f)
		}
	}
	return nil
}

// isDSIdentityValue reports whether v is a text value or a list of text
// values (a name, URL, DN or server list), as opposed to a flag or a
// number.
func isDSIdentityValue(v interface{}) bool {
	switch x := v.(type) {
	case string:
		return true
	case []interface{}:
		for _, e := range x {
			if _, ok := e.(string); !ok {
				return false
			}
		}
		return true
	}
	return false
}
