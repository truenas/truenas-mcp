package tools

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/tasks"
)

type obj = map[string]interface{}

// fullDSExamples returns, per service type, arguments that set every
// argument the schema offers for that service type.
func fullDSExamples() map[string]obj {
	return map[string]obj{
		"ACTIVEDIRECTORY": {
			"service_type": "ACTIVEDIRECTORY", "enable": true,
			"credential": obj{"credential_type": "KERBEROS_USER", "username": "admin", "password": "s3cret"},
			"configuration": obj{
				"hostname": "truenas", "domain": "corp.example.com",
				"idmap":               obj{"idmap_domain": obj{"idmap_backend": "RID", "range_low": 100000001.0, "range_high": 200000000.0}},
				"site":                "Default-First-Site-Name",
				"computer_account_ou": "TRUENAS_SERVERS",
				"use_default_domain":  true, "enable_trusted_domains": true,
				"trusted_domains": []interface{}{obj{"name": "BROOK", "idmap_backend": "RID", "range_low": 200000001.0, "range_high": 300000000.0}},
			},
			"enable_account_cache": false, "enable_dns_updates": false, "timeout": 30.0,
			"kerberos_realm": "CORP.EXAMPLE.COM", "force": true,
		},
		"IPA": {
			"service_type": "IPA", "enable": true,
			"credential": obj{"credential_type": "KERBEROS_PRINCIPAL", "principal": "host/truenas@IPA.EXAMPLE.COM"},
			"configuration": obj{
				"target_server": "ipa.example.com", "hostname": "truenas", "domain": "ipa.example.com",
				"basedn": "dc=ipa,dc=example,dc=com", "smb_domain": obj{"name": "IPA"}, "validate_certificates": false,
			},
		},
		"LDAP": {
			"service_type": "LDAP", "enable": false,
			"credential": obj{"credential_type": "LDAP_PLAIN", "binddn": "cn=admin,dc=example,dc=com", "bindpw": "s3cret"},
			"configuration": obj{
				"server_urls": []interface{}{"ldaps://ldap.example.com"}, "basedn": "dc=example,dc=com",
				"starttls": true, "validate_certificates": false, "schema": "RFC2307BIS",
				"search_bases":         obj{"base_user": "ou=people,dc=example,dc=com"},
				"attribute_maps":       obj{"passwd": obj{"user_name": "uid"}},
				"auxiliary_parameters": "ldap_referrals = false",
			},
		},
	}
}

func dsSchema(t *testing.T) obj {
	t.Helper()
	tool, ok := NewRegistry(nil, nil).tools["configure_directory_service"]
	if !ok {
		t.Fatal("configure_directory_service is not registered")
	}
	return tool.Definition.InputSchema
}

func propsOf(t *testing.T, schema obj) obj {
	t.Helper()
	props, ok := schema["properties"].(obj)
	if !ok {
		t.Fatalf("schema has no properties: %v", schema)
	}
	return props
}

func keysOf(m obj) []string {
	keys := sortedKeys(m)
	sort.Strings(keys)
	return keys
}

func schemaType(k dsFieldKind) string {
	return map[dsFieldKind]string{dsString: "string", dsBool: "boolean", dsStringList: "array", dsObject: "object", dsObjectList: "array", dsInt: "integer"}[k]
}

func TestConfigureDirectoryServiceSchemaMatchesHandler(t *testing.T) {
	schema := dsSchema(t)
	props := propsOf(t, schema)

	// Top level: exactly the fields the handler accepts, plus the tool-level
	// dry_run and confirm.
	want := []string{"service_type", "credential", "configuration"}
	for k := range dsTopLevelFields {
		want = append(want, k)
	}
	for k := range dsControlArgs {
		want = append(want, k)
	}
	sort.Strings(want)
	if got := keysOf(props); !reflect.DeepEqual(got, want) {
		t.Errorf("schema properties = %v\nhandler fields   = %v", got, want)
	}
	for k, f := range dsTopLevelFields {
		p, ok := props[k].(obj)
		if !ok {
			continue // reported by the property-set comparison above
		}
		if p["type"] != schemaType(f.kind) {
			t.Errorf("%s: schema type %v, handler reads %s", k, p["type"], schemaType(f.kind))
		}
		if f.kind == dsInt && (p["minimum"] != f.min || p["maximum"] != f.max) {
			t.Errorf("%s: schema range %v..%v, handler enforces %d..%d", k, p["minimum"], p["maximum"], f.min, f.max)
		}
	}

	// Required: what the handler insists on for every call. service_type,
	// credential and configuration are required only together (see
	// TestConfigureDirectoryServiceBareDisable), so the schema leaves them out.
	var wantReq []string
	for k, f := range dsTopLevelFields {
		if f.required {
			wantReq = append(wantReq, k)
		}
	}
	gotReq, _ := schema["required"].([]string)
	gotReq = append([]string(nil), gotReq...)
	sort.Strings(gotReq)
	sort.Strings(wantReq)
	if !reflect.DeepEqual(gotReq, wantReq) {
		t.Errorf("schema required = %v, handler requires %v", gotReq, wantReq)
	}

	// service_type values.
	st := props["service_type"].(obj)
	gotTypes := append([]string(nil), st["enum"].([]string)...)
	wantTypes := append([]string(nil), dsServiceTypes...)
	sort.Strings(gotTypes)
	sort.Strings(wantTypes)
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Errorf("service_type enum = %v, handler accepts %v", gotTypes, wantTypes)
	}
	if got, want := keysOf(obj{"ACTIVEDIRECTORY": 1, "IPA": 1, "LDAP": 1}), wantTypes; !reflect.DeepEqual(got, want) {
		t.Errorf("handler has configuration fields for %v, want %v", got, want)
	}
	for _, s := range dsServiceTypes {
		if _, ok := dsConfigurationFields[s]; !ok {
			t.Errorf("no configuration fields for %s", s)
		}
	}

	// credential: credential_type plus the union of every type's fields.
	cred := props["credential"].(obj)
	credProps := propsOf(t, cred)
	wantCred := map[string]bool{"credential_type": true}
	for _, fields := range dsCredentialTypes {
		for _, f := range fields {
			wantCred[f] = true
		}
	}
	for k := range wantCred {
		if _, ok := credProps[k]; !ok {
			t.Errorf("handler reads credential.%s but the schema does not list it", k)
		}
	}
	for k := range credProps {
		if !wantCred[k] {
			t.Errorf("schema lists credential.%s but the handler never reads it", k)
		}
	}
	gotCredTypes := append([]string(nil), credProps["credential_type"].(obj)["enum"].([]string)...)
	sort.Strings(gotCredTypes)
	if want := sortedKeys(dsCredentialTypes); !reflect.DeepEqual(gotCredTypes, want) {
		t.Errorf("credential_type enum = %v, handler accepts %v", gotCredTypes, want)
	}
	if cred["additionalProperties"] != false {
		t.Error("credential must not allow additional properties (TrueNAS rejects them)")
	}

	// configuration: the union of every service type's fields, same types.
	conf := props["configuration"].(obj)
	confProps := propsOf(t, conf)
	wantConf := map[string]dsFieldKind{}
	for _, fields := range dsConfigurationFields {
		for name, f := range fields {
			if prev, ok := wantConf[name]; ok && prev != f.kind {
				t.Errorf("configuration.%s has different kinds in different service types", name)
			}
			wantConf[name] = f.kind
		}
	}
	for name, kind := range wantConf {
		p, ok := confProps[name].(obj)
		if !ok {
			t.Errorf("handler reads configuration.%s but the schema does not list it", name)
			continue
		}
		if p["type"] != schemaType(kind) {
			t.Errorf("configuration.%s: schema type %v, handler reads %s", name, p["type"], schemaType(kind))
		}
	}
	for name := range confProps {
		if _, ok := wantConf[name]; !ok {
			t.Errorf("schema lists configuration.%s but the handler never reads it", name)
		}
	}
	if conf["additionalProperties"] != false {
		t.Error("configuration must not allow additional properties (TrueNAS rejects them)")
	}
	if e := confProps["schema"].(obj)["enum"].([]string); !reflect.DeepEqual(e, dsConfigurationFields["LDAP"]["schema"].enum) {
		t.Errorf("configuration.schema enum = %v, handler accepts %v", e, dsConfigurationFields["LDAP"]["schema"].enum)
	}
}

// Behavioural cross-check of the same property: the arguments the schema
// offers are all consumed, and nothing outside it is.
func TestConfigureDirectoryServiceConsumesEveryAdvertisedField(t *testing.T) {
	props := propsOf(t, dsSchema(t))
	seenTop := map[string]bool{}
	seenCred := map[string]bool{}
	seenConf := map[string]bool{}

	for name, args := range fullDSExamples() {
		payload, err := buildDirectoryServiceUpdatePayload(args)
		if err != nil {
			t.Fatalf("%s: full example rejected: %v", name, err)
		}
		// The payload carries every argument supplied, unchanged.
		for k, v := range args {
			got, ok := payload[k]
			if !ok {
				t.Errorf("%s: argument %s was dropped from the payload", name, k)
				continue
			}
			if !reflect.DeepEqual(got, normalizeDS(v)) {
				t.Errorf("%s: %s = %#v in payload, want %#v", name, k, got, v)
			}
			seenTop[k] = true
		}
		for k := range payload {
			if _, ok := args[k]; !ok {
				t.Errorf("%s: payload has %s which was not an argument", name, k)
			}
		}
		for k := range args["credential"].(obj) {
			seenCred[k] = true
		}
		for k := range args["configuration"].(obj) {
			seenConf[k] = true
		}
	}
	// Every credential type is built too.
	for credType, fields := range dsCredentialTypes {
		cred := obj{"credential_type": credType}
		for _, f := range fields {
			cred[f] = "v"
			seenCred[f] = true
		}
		svc := "LDAP"
		args := fullDSExamples()["LDAP"]
		args["credential"] = cred
		args["kerberos_realm"] = "EXAMPLE.COM"
		if payload, err := buildDirectoryServiceUpdatePayload(args); err != nil {
			t.Errorf("%s credential %v rejected for %s: %v", credType, cred, svc, err)
		} else if !reflect.DeepEqual(payload["credential"], cred) {
			t.Errorf("%s credential = %v, want %v", credType, payload["credential"], cred)
		}
	}

	seenTop["confirm"], seenTop["dry_run"] = true, true // consumed by the safety layer and dry-run wrapper
	for k := range props {
		if !seenTop[k] {
			t.Errorf("top-level schema property %s is never consumed in the examples", k)
		}
	}
	for k := range propsOf(t, props["credential"].(obj)) {
		if !seenCred[k] {
			t.Errorf("credential.%s is advertised but no example consumes it", k)
		}
	}
	for k := range propsOf(t, props["configuration"].(obj)) {
		if !seenConf[k] {
			t.Errorf("configuration.%s is advertised but no example consumes it", k)
		}
	}
}

// normalizeDS maps the float64 numbers of decoded JSON to the ints the
// payload carries for integer fields.
func normalizeDS(v interface{}) interface{} {
	if f, ok := v.(float64); ok && f == float64(int(f)) && f < 1000 {
		return int(f)
	}
	return v
}

func TestConfigureDirectoryServiceBuildsPayload(t *testing.T) {
	tests := []struct {
		name string
		args obj
		want obj
	}{
		{
			name: "active directory, only the required fields",
			args: obj{
				"service_type": "ACTIVEDIRECTORY", "enable": true,
				"credential":    obj{"credential_type": "KERBEROS_USER", "username": "admin", "password": "pw"},
				"configuration": obj{"hostname": "truenas", "domain": "corp.example.com"},
			},
			want: obj{
				"service_type": "ACTIVEDIRECTORY", "enable": true,
				"credential":    obj{"credential_type": "KERBEROS_USER", "username": "admin", "password": "pw"},
				"configuration": obj{"hostname": "truenas", "domain": "corp.example.com"},
			},
		},
		{
			name: "ldap with plain bind, optional fields and control args",
			args: obj{
				"service_type": "LDAP", "enable": true, "dry_run": true, "confirm": true,
				"credential":    obj{"credential_type": "LDAP_PLAIN", "binddn": "cn=a,dc=x", "bindpw": "pw"},
				"configuration": obj{"server_urls": []interface{}{"ldaps://ldap.example.com"}, "basedn": "dc=example,dc=com", "starttls": false},
				"timeout":       30.0, "enable_account_cache": false,
			},
			want: obj{
				"service_type": "LDAP", "enable": true,
				"credential":    obj{"credential_type": "LDAP_PLAIN", "binddn": "cn=a,dc=x", "bindpw": "pw"},
				"configuration": obj{"server_urls": []interface{}{"ldaps://ldap.example.com"}, "basedn": "dc=example,dc=com", "starttls": false},
				"timeout":       30, "enable_account_cache": false,
			},
		},
		{
			name: "ldap anonymous",
			args: obj{
				"service_type": "LDAP", "enable": false,
				"credential":    obj{"credential_type": "LDAP_ANONYMOUS"},
				"configuration": obj{"server_urls": []interface{}{"ldap://ldap.example.com"}, "basedn": "dc=example,dc=com"},
			},
			want: obj{
				"service_type": "LDAP", "enable": false,
				"credential":    obj{"credential_type": "LDAP_ANONYMOUS"},
				"configuration": obj{"server_urls": []interface{}{"ldap://ldap.example.com"}, "basedn": "dc=example,dc=com"},
			},
		},
		{
			name: "ldap with kerberos principal and realm",
			args: obj{
				"service_type": "LDAP", "enable": true, "kerberos_realm": "EXAMPLE.COM",
				"credential":    obj{"credential_type": "KERBEROS_PRINCIPAL", "principal": "nfs/truenas@EXAMPLE.COM"},
				"configuration": obj{"server_urls": []interface{}{"ldaps://ldap.example.com"}, "basedn": "dc=example,dc=com"},
			},
			want: obj{
				"service_type": "LDAP", "enable": true, "kerberos_realm": "EXAMPLE.COM",
				"credential":    obj{"credential_type": "KERBEROS_PRINCIPAL", "principal": "nfs/truenas@EXAMPLE.COM"},
				"configuration": obj{"server_urls": []interface{}{"ldaps://ldap.example.com"}, "basedn": "dc=example,dc=com"},
			},
		},
		{
			name: "ldap with mutual TLS",
			args: obj{
				"service_type": "LDAP", "enable": true,
				"credential":    obj{"credential_type": "LDAP_MTLS", "client_certificate": "ldap_client"},
				"configuration": obj{"server_urls": []interface{}{"ldaps://ldap.example.com"}, "basedn": "dc=example,dc=com"},
			},
			want: obj{
				"service_type": "LDAP", "enable": true,
				"credential":    obj{"credential_type": "LDAP_MTLS", "client_certificate": "ldap_client"},
				"configuration": obj{"server_urls": []interface{}{"ldaps://ldap.example.com"}, "basedn": "dc=example,dc=com"},
			},
		},
		{
			name: "ipa",
			args: obj{
				"service_type": "IPA", "enable": true, "force": false,
				"credential":    obj{"credential_type": "KERBEROS_USER", "username": "admin", "password": "pw"},
				"configuration": obj{"target_server": "ipa.example.com", "hostname": "truenas", "domain": "example.com", "basedn": "dc=example,dc=com"},
			},
			want: obj{
				"service_type": "IPA", "enable": true, "force": false,
				"credential":    obj{"credential_type": "KERBEROS_USER", "username": "admin", "password": "pw"},
				"configuration": obj{"target_server": "ipa.example.com", "hostname": "truenas", "domain": "example.com", "basedn": "dc=example,dc=com"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildDirectoryServiceUpdatePayload(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("payload\n got %s\nwant %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestConfigureDirectoryServiceValidation(t *testing.T) {
	ad := func() obj { return fullDSExamples()["ACTIVEDIRECTORY"] }
	ldap := func() obj { return fullDSExamples()["LDAP"] }
	ipa := func() obj { return fullDSExamples()["IPA"] }
	with := func(base obj, mutate func(obj)) obj {
		out := obj{}
		b, _ := json.Marshal(base)
		_ = json.Unmarshal(b, &out)
		mutate(out)
		return out
	}
	cfg := func(a obj) obj { return a["configuration"].(obj) }
	cred := func(a obj) obj { return a["credential"].(obj) }

	tests := []struct {
		name    string
		args    obj
		wantErr string
	}{
		{"missing service_type", with(ad(), func(a obj) { delete(a, "service_type") }), "service_type is required"},
		{"unknown service_type", with(ad(), func(a obj) { a["service_type"] = "NIS" }), "service_type must be one of"},
		{"lower case service_type", with(ad(), func(a obj) { a["service_type"] = "ldap" }), "service_type must be one of"},
		{"missing enable", with(ad(), func(a obj) { delete(a, "enable") }), "enable is required"},
		{"enable not a boolean", with(ad(), func(a obj) { a["enable"] = "yes" }), "enable must be a boolean"},
		{"missing credential", with(ad(), func(a obj) { delete(a, "credential") }), "credential is required"},
		{"missing configuration", with(ad(), func(a obj) { delete(a, "configuration") }), "configuration is required"},
		{"credential not an object", with(ad(), func(a obj) { a["credential"] = "admin" }), "credential must be an object"},
		{"missing credential_type", with(ad(), func(a obj) { delete(cred(a), "credential_type") }), "credential.credential_type is required"},
		{"unknown credential_type", with(ad(), func(a obj) { cred(a)["credential_type"] = "NTLM" }), "credential.credential_type is required"},
		{"kerberos user without password", with(ad(), func(a obj) { delete(cred(a), "password") }), "password is required in credential (KERBEROS_USER)"},
		{"kerberos user without username", with(ad(), func(a obj) { delete(cred(a), "username") }), "username is required in credential (KERBEROS_USER)"},
		{"old credential field names", with(ad(), func(a obj) {
			a["credential"] = obj{"type": "KERBEROS_USER", "username": "a", "password": "b"}
		}), "credential.credential_type is required"},
		{"credential field of another type", with(ad(), func(a obj) { cred(a)["bindpw"] = "x" }), `unknown field "bindpw" in credential (KERBEROS_USER)`},
		{"empty password", with(ad(), func(a obj) { cred(a)["password"] = "" }), "credential (KERBEROS_USER).password must be a non-empty string"},
		{"ldap_plain without binddn", with(ldap(), func(a obj) { delete(cred(a), "binddn") }), "binddn is required in credential (LDAP_PLAIN)"},
		{"ldap_plain without bindpw", with(ldap(), func(a obj) { delete(cred(a), "bindpw") }), "bindpw is required in credential (LDAP_PLAIN)"},
		{"ldap_mtls without certificate", with(ldap(), func(a obj) { a["credential"] = obj{"credential_type": "LDAP_MTLS"} }), "client_certificate is required"},
		{"principal credential without principal", with(ad(), func(a obj) { a["credential"] = obj{"credential_type": "KERBEROS_PRINCIPAL"} }), "principal is required"},
		{"ldap credential for active directory", with(ad(), func(a obj) { a["credential"] = obj{"credential_type": "LDAP_ANONYMOUS"} }), "only supported for service_type LDAP"},
		{"ldap credential for ipa", with(ipa(), func(a obj) { a["credential"] = obj{"credential_type": "LDAP_PLAIN", "binddn": "x", "bindpw": "y"} }), "only supported for service_type LDAP"},
		{"kerberos on ldap needs a realm", with(ldap(), func(a obj) {
			a["credential"] = obj{"credential_type": "KERBEROS_PRINCIPAL", "principal": "p@R"}
		}), "kerberos_realm is required"},
		{"ad without hostname", with(ad(), func(a obj) { delete(cfg(a), "hostname") }), "hostname is required in configuration (ACTIVEDIRECTORY)"},
		{"ad without domain", with(ad(), func(a obj) { delete(cfg(a), "domain") }), "domain is required in configuration (ACTIVEDIRECTORY)"},
		{"ad with the old domainname field", with(ad(), func(a obj) { cfg(a)["domainname"] = "x" }), `unknown field "domainname"`},
		{"ad with an ldap field", with(ad(), func(a obj) { cfg(a)["basedn"] = "dc=x" }), `unknown field "basedn" in configuration (ACTIVEDIRECTORY)`},
		{"configuration repeats service_type", with(ad(), func(a obj) { cfg(a)["service_type"] = "ACTIVEDIRECTORY" }), `unknown field "service_type" in configuration`},
		{"ad boolean option of the wrong type", with(ad(), func(a obj) { cfg(a)["use_default_domain"] = "true" }), "use_default_domain must be a boolean"},
		{"ad trusted_domains entry not an object", with(ad(), func(a obj) { cfg(a)["trusted_domains"] = []interface{}{"BROOK"} }), "trusted_domains[0] must be an object"},
		{"ldap without server_urls", with(ldap(), func(a obj) { delete(cfg(a), "server_urls") }), "server_urls is required in configuration (LDAP)"},
		{"ldap with empty server_urls", with(ldap(), func(a obj) { cfg(a)["server_urls"] = []interface{}{} }), "server_urls must be a non-empty array"},
		{"ldap server_urls as a string", with(ldap(), func(a obj) { cfg(a)["server_urls"] = "ldaps://x" }), "server_urls must be a non-empty array"},
		{"ldap with the old hostname field", with(ldap(), func(a obj) { cfg(a)["hostname"] = []interface{}{"ldap"} }), `unknown field "hostname" in configuration (LDAP)`},
		{"ldap without basedn", with(ldap(), func(a obj) { delete(cfg(a), "basedn") }), "basedn is required in configuration (LDAP)"},
		{"ldap bad schema", with(ldap(), func(a obj) { cfg(a)["schema"] = "RFC2307BIZ" }), "schema must be one of RFC2307, RFC2307BIS"},
		{"ipa without target_server", with(ipa(), func(a obj) { delete(cfg(a), "target_server") }), "target_server is required in configuration (IPA)"},
		{"ipa without basedn", with(ipa(), func(a obj) { delete(cfg(a), "basedn") }), "basedn is required in configuration (IPA)"},
		{"timeout too low", with(ad(), func(a obj) { a["timeout"] = 4.0 }), "timeout must be an integer from 5 to 60"},
		{"timeout too high", with(ad(), func(a obj) { a["timeout"] = 61.0 }), "timeout must be an integer from 5 to 60"},
		{"timeout fractional", with(ad(), func(a obj) { a["timeout"] = 10.5 }), "timeout must be an integer from 5 to 60"},
		{"timeout a string", with(ad(), func(a obj) { a["timeout"] = "10" }), "timeout must be an integer from 5 to 60"},
		{"empty kerberos_realm", with(ad(), func(a obj) { a["kerberos_realm"] = "" }), "kerberos_realm must be a non-empty string"},
		{"force not a boolean", with(ad(), func(a obj) { a["force"] = 1.0 }), "force must be a boolean"},
		{"old top-level names", with(ad(), func(a obj) { a["bindname"] = "a"; a["domain"] = "example.com" }), `unknown field "bindname" in arguments`},
		{"old names only", obj{"type": "activedirectory", "domain": "example.com", "enable": true}, `unknown field "domain" in arguments`},
		{"enable without service_type", obj{"enable": true}, "service_type is required to enable"},
		{"credential without service_type", obj{"enable": false, "credential": obj{"credential_type": "LDAP_ANONYMOUS"}}, "credential needs service_type"},
		{"configuration without service_type", obj{"enable": false, "configuration": obj{"hostname": "x"}}, "configuration needs service_type"},
		{"service_type without credential and configuration", obj{"enable": false, "service_type": "LDAP"}, "credential is required in arguments when service_type is given"},
		{"disabled kerberos user on active directory", with(ad(), func(a obj) { a["enable"] = false }), "KERBEROS_USER credentials may not be stored for a disabled ACTIVEDIRECTORY service"},
		{"disabled kerberos user on ipa", with(ipa(), func(a obj) {
			a["enable"] = false
			a["credential"] = obj{"credential_type": "KERBEROS_USER", "username": "a", "password": "b"}
		}), "KERBEROS_USER credentials may not be stored for a disabled IPA service"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := buildDirectoryServiceUpdatePayload(tc.args)
			if err == nil {
				t.Fatalf("accepted; payload %v", payload)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}

	// The same errors reach the caller of the tool, with nothing sent.
	client, fake := newFakeClient(t)
	r := NewRegistry(client, nil)
	bad := with(ad(), func(a obj) { delete(cfg(a), "domain") })
	for _, extra := range []obj{{"confirm": true}, {"dry_run": true}} {
		args := with(bad, func(obj) {})
		for k, v := range extra {
			args[k] = v
		}
		if _, err := r.CallTool("configure_directory_service", args); err == nil || !strings.Contains(err.Error(), "domain is required") {
			t.Errorf("CallTool(%v) error = %v", extra, err)
		}
	}
	if calls := fake.called("directoryservices.update"); len(calls) != 0 {
		t.Errorf("invalid arguments reached directoryservices.update: %v", calls)
	}
}

// dsFake answers the middleware calls the tool makes.
func dsFake(t *testing.T) (*Registry, *fakeMiddleware, *tasks.Manager) {
	t.Helper()
	fake := &fakeMiddleware{results: map[string]interface{}{
		"directoryservices.status": obj{"type": "LDAP", "status": "HEALTHY", "status_msg": nil},
		"directoryservices.update": 4242,
	}}
	client, fake := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	return NewRegistry(client, mgr), fake, mgr
}

func TestConfigureDirectoryServiceDryRunShowsExactPayload(t *testing.T) {
	args := fullDSExamples()["ACTIVEDIRECTORY"]
	wantPayload, err := buildDirectoryServiceUpdatePayload(args)
	if err != nil {
		t.Fatal(err)
	}
	wantMasked, _ := json.Marshal(maskDirectoryServicePayload(wantPayload))

	check := func(t *testing.T, out string, preview bool) {
		t.Helper()
		var reply struct {
			Status         string          `json:"status"`
			Preview        json.RawMessage `json:"preview"`
			Tool           string          `json:"tool"`
			PlannedActions []struct {
				Operation string                 `json:"operation"`
				Details   map[string]interface{} `json:"details"`
			} `json:"planned_actions"`
			Warnings     []string               `json:"warnings"`
			CurrentState map[string]interface{} `json:"current_state"`
		}
		if err := json.Unmarshal([]byte(out), &reply); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		if preview {
			if reply.Status != "confirmation_required" {
				t.Fatalf("status = %q", reply.Status)
			}
			var inner = reply
			if err := json.Unmarshal(reply.Preview, &inner); err != nil {
				t.Fatal(err)
			}
			reply = inner
		}
		if reply.Tool != "configure_directory_service" || len(reply.PlannedActions) == 0 {
			t.Fatalf("unexpected dry run: %s", out)
		}
		first := reply.PlannedActions[0]
		if first.Operation != "configure" || first.Details["method"] != "directoryservices.update" {
			t.Errorf("first action = %+v", first)
		}
		gotPayload, _ := json.Marshal(first.Details["payload"])
		if !jsonEqual(t, gotPayload, wantMasked) {
			t.Errorf("payload in dry run\n got %s\nwant %s", gotPayload, wantMasked)
		}
		if strings.Contains(out, "s3cret") {
			t.Errorf("dry run leaked the password:\n%s", out)
		}
		if !strings.Contains(out, redact.Marker) {
			t.Errorf("dry run does not show masked secrets:\n%s", out)
		}
		if reply.CurrentState["current_service"] != "LDAP" {
			t.Errorf("current_state = %v", reply.CurrentState)
		}
		var conflict bool
		for _, w := range reply.Warnings {
			if strings.Contains(w, "CONFLICT: LDAP is currently configured and will be replaced by ACTIVEDIRECTORY") {
				conflict = true
			}
		}
		if !conflict {
			t.Errorf("no replace warning in %v", reply.Warnings)
		}
	}

	t.Run("explicit dry_run", func(t *testing.T) {
		r, fake, _ := dsFake(t)
		call := withArgs(args, obj{"dry_run": true})
		out, err := r.CallTool("configure_directory_service", call)
		if err != nil {
			t.Fatal(err)
		}
		check(t, out, false)
		if len(fake.called("directoryservices.update")) != 0 {
			t.Error("dry run called directoryservices.update")
		}
	})
	t.Run("without confirm", func(t *testing.T) {
		r, fake, _ := dsFake(t)
		out, err := r.CallTool("configure_directory_service", withArgs(args, nil))
		if err != nil {
			t.Fatal(err)
		}
		check(t, out, true)
		if len(fake.called("directoryservices.update")) != 0 {
			t.Error("unconfirmed call called directoryservices.update")
		}
	})
	t.Run("confirm with dry_run still previews only", func(t *testing.T) {
		r, fake, _ := dsFake(t)
		out, err := r.CallTool("configure_directory_service", withArgs(args, obj{"confirm": true, "dry_run": true}))
		if err != nil {
			t.Fatal(err)
		}
		check(t, out, false)
		if len(fake.called("directoryservices.update")) != 0 {
			t.Error("dry run called directoryservices.update")
		}
	})
}

func withArgs(base, extra obj) obj {
	out := obj{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y interface{}
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}

func TestConfigureDirectoryServiceExecutesWithConfirm(t *testing.T) {
	r, fake, mgr := dsFake(t)
	args := fullDSExamples()["ACTIVEDIRECTORY"]
	wantPayload, err := buildDirectoryServiceUpdatePayload(args)
	if err != nil {
		t.Fatal(err)
	}

	out, err := r.CallTool("configure_directory_service", withArgs(args, obj{"confirm": true}))
	if err != nil {
		t.Fatal(err)
	}
	calls := fake.called("directoryservices.update")
	if len(calls) != 1 {
		t.Fatalf("directoryservices.update called %d times", len(calls))
	}
	// One positional argument: exactly the payload the dry run showed.
	var params []json.RawMessage
	if err := json.Unmarshal(calls[0].Params, &params); err != nil || len(params) != 1 {
		t.Fatalf("params = %s (%v)", calls[0].Params, err)
	}
	wantJSON, _ := json.Marshal(wantPayload)
	if !jsonEqual(t, params[0], wantJSON) {
		t.Errorf("sent\n %s\nwant\n %s", params[0], wantJSON)
	}

	var reply struct {
		Success       bool   `json:"success"`
		DirectoryType string `json:"directory_type"`
		Enabled       bool   `json:"enabled"`
		JobID         int    `json:"job_id"`
		TaskID        string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatal(err)
	}
	if !reply.Success || reply.DirectoryType != "ACTIVEDIRECTORY" || !reply.Enabled || reply.JobID != 4242 || reply.TaskID == "" {
		t.Errorf("reply = %+v\n%s", reply, out)
	}
	if strings.Contains(out, "s3cret") {
		t.Errorf("reply leaked the password: %s", out)
	}

	task, err := mgr.Get(reply.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.JobID == nil || *task.JobID != 4242 {
		t.Errorf("task job = %v", task.JobID)
	}
	if task.TTL != int64((15 * time.Minute).Seconds()) {
		t.Errorf("task TTL = %ds, want 900", task.TTL)
	}
	stored, _ := json.Marshal(task.Arguments)
	if strings.Contains(string(stored), "s3cret") {
		t.Errorf("task arguments keep the password: %s", stored)
	}
}

func TestConfigureDirectoryServiceReportsMiddlewareError(t *testing.T) {
	fake := &fakeMiddleware{
		results: map[string]interface{}{"directoryservices.status": obj{"type": nil, "status": nil}},
		handle: func(method string, _ json.RawMessage) (interface{}, obj, bool) {
			if method == "directoryservices.update" {
				return nil, obj{"code": -32001, "message": "Method call error", "data": obj{"reason": "[EINVAL] bad"}}, true
			}
			return nil, nil, false
		},
	}
	client, _ := newFakeClientFrom(t, fake)
	r := NewRegistry(client, nil)
	_, err := r.CallTool("configure_directory_service", withArgs(fullDSExamples()["IPA"], obj{"confirm": true}))
	if err == nil || !strings.Contains(err.Error(), "failed to configure IPA") {
		t.Errorf("err = %v", err)
	}
}

func dsStatus(dsType, status interface{}) obj {
	return obj{"directoryservices.status": obj{"type": dsType, "status": status, "status_msg": nil}}
}

func leaveFake(t *testing.T, results obj) (*Registry, *fakeMiddleware, *tasks.Manager) {
	t.Helper()
	fake := &fakeMiddleware{results: results}
	client, fake := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	return NewRegistry(client, mgr), fake, mgr
}

// onlyMethods fails the test if the fake middleware saw any method outside
// the allowed list.
func onlyMethods(t *testing.T, fake *fakeMiddleware, allowed ...string) {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, c := range fake.calls {
		ok := false
		for _, a := range allowed {
			ok = ok || c.Method == a
		}
		if !ok {
			t.Errorf("unexpected middleware call %s %s", c.Method, c.Params)
		}
	}
}

func leaveArgs(extra obj) obj {
	return withArgs(obj{"credential": obj{"credential_type": "KERBEROS_USER", "username": "admin", "password": "s3cret"}}, extra)
}

func TestLeaveDirectoryServiceSchemaMatchesHandler(t *testing.T) {
	tool, ok := NewRegistry(nil, nil).tools["leave_directory_service"]
	if !ok {
		t.Fatal("leave_directory_service is not registered")
	}
	schema := tool.Definition.InputSchema
	props := propsOf(t, schema)

	// Top level: the credential plus the tool-level dry_run and confirm.
	want := []string{"credential"}
	for k := range dsControlArgs {
		want = append(want, k)
	}
	sort.Strings(want)
	if got := keysOf(props); !reflect.DeepEqual(got, want) {
		t.Errorf("schema properties = %v, handler reads %v", got, want)
	}
	if req, _ := schema["required"].([]string); !reflect.DeepEqual(req, []string{"credential"}) {
		t.Errorf("schema required = %v, handler requires [credential]", req)
	}

	// credential: KERBEROS_USER only, with the fields the handler reads.
	cred := props["credential"].(obj)
	credProps := propsOf(t, cred)
	wantCred := append([]string{"credential_type"}, dsCredentialTypes[dsLeaveCredentialType]...)
	sort.Strings(wantCred)
	if got := keysOf(credProps); !reflect.DeepEqual(got, wantCred) {
		t.Errorf("credential properties = %v, handler reads %v", got, wantCred)
	}
	if e := credProps["credential_type"].(obj)["enum"].([]string); !reflect.DeepEqual(e, []string{dsLeaveCredentialType}) {
		t.Errorf("credential_type enum = %v", e)
	}
	gotCredReq, _ := cred["required"].([]string)
	gotCredReq = append([]string(nil), gotCredReq...)
	sort.Strings(gotCredReq)
	if !reflect.DeepEqual(gotCredReq, wantCred) {
		t.Errorf("credential required = %v, handler requires %v", gotCredReq, wantCred)
	}
	if cred["additionalProperties"] != false {
		t.Error("credential must not allow additional properties")
	}
	for name, p := range credProps {
		if p.(obj)["type"] != "string" {
			t.Errorf("credential.%s type = %v, handler reads a string", name, p.(obj)["type"])
		}
	}

	// Behaviour: everything the schema offers reaches the payload, and only that.
	payload, err := buildDirectoryServiceLeavePayload(leaveArgs(obj{"dry_run": true, "confirm": true}))
	if err != nil {
		t.Fatal(err)
	}
	wantPayload := obj{"credential": obj{"credential_type": "KERBEROS_USER", "username": "admin", "password": "s3cret"}}
	if !reflect.DeepEqual(payload, wantPayload) {
		t.Errorf("payload = %v, want %v", payload, wantPayload)
	}

	// The service types the handler allows are the ones the middleware supports.
	if !reflect.DeepEqual(dsLeaveServiceTypes, []string{"ACTIVEDIRECTORY", "IPA"}) {
		t.Errorf("leave service types = %v", dsLeaveServiceTypes)
	}
}

func TestLeaveDirectoryServiceValidation(t *testing.T) {
	with := func(mutate func(obj)) obj {
		a := leaveArgs(nil)
		mutate(a)
		return a
	}
	cred := func(a obj) obj { return a["credential"].(obj) }
	tests := []struct {
		name    string
		args    obj
		wantErr string
	}{
		{"no arguments", obj{}, "credential is required in arguments"},
		{"credential not an object", obj{"credential": "admin"}, "credential must be an object"},
		{"missing credential_type", with(func(a obj) { delete(cred(a), "credential_type") }), "credential.credential_type is required and must be KERBEROS_USER"},
		{"principal credential", obj{"credential": obj{"credential_type": "KERBEROS_PRINCIPAL", "principal": "p@R"}}, "must be KERBEROS_USER"},
		{"ldap credential", obj{"credential": obj{"credential_type": "LDAP_PLAIN", "binddn": "x", "bindpw": "y"}}, "must be KERBEROS_USER"},
		{"missing username", with(func(a obj) { delete(cred(a), "username") }), "username is required in credential (KERBEROS_USER)"},
		{"missing password", with(func(a obj) { delete(cred(a), "password") }), "password is required in credential (KERBEROS_USER)"},
		{"empty password", with(func(a obj) { cred(a)["password"] = "" }), "credential (KERBEROS_USER).password must be a non-empty string"},
		{"password not a string", with(func(a obj) { cred(a)["password"] = 5.0 }), "password must be a non-empty string"},
		{"unknown credential field", with(func(a obj) { cred(a)["principal"] = "p" }), `unknown field "principal" in credential (KERBEROS_USER)`},
		{"unknown top-level field", with(func(a obj) { a["force"] = true }), `unknown field "force" in arguments`},
		{"credentials at the top level", obj{"username": "admin", "password": "s3cret", "credential": leaveArgs(nil)["credential"]}, `unknown field "password" in arguments`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := buildDirectoryServiceLeavePayload(tc.args)
			if err == nil {
				t.Fatalf("accepted; payload %v", payload)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}

	// Invalid arguments are reported before any middleware call, in every mode.
	r, fake, _ := leaveFake(t, dsStatus("ACTIVEDIRECTORY", "HEALTHY"))
	for _, extra := range []obj{{}, {"dry_run": true}, {"confirm": true}} {
		if _, err := r.CallTool("leave_directory_service", withArgs(obj{"credential": obj{"credential_type": "KERBEROS_USER", "username": "a"}}, extra)); err == nil ||
			!strings.Contains(err.Error(), "password is required") {
			t.Errorf("%v: err = %v", extra, err)
		}
	}
	if n := len(fake.called("directoryservices.status")) + len(fake.called("directoryservices.leave")); n != 0 {
		t.Errorf("invalid arguments reached the middleware (%d calls)", n)
	}
}

func TestLeaveDirectoryServiceDryRunPastPreChecks(t *testing.T) {
	for _, dsType := range []string{"ACTIVEDIRECTORY", "IPA"} {
		t.Run(dsType, func(t *testing.T) {
			r, fake, _ := leaveFake(t, dsStatus(dsType, "HEALTHY"))
			out, err := r.CallTool("leave_directory_service", leaveArgs(obj{"dry_run": true}))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, "s3cret") {
				t.Errorf("dry run leaked the password:\n%s", out)
			}

			var got DryRunResult
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, out)
			}
			want := DryRunResult{
				Tool:         "leave_directory_service",
				CurrentState: obj{"directory_type": dsType, "enabled": true, "healthy": true},
				PlannedActions: []PlannedAction{
					{
						Step: 1, Description: "Call directoryservices.leave to leave the " + dsType + " domain", Operation: "leave", Target: dsType,
						Details: obj{
							"method": "directoryservices.leave",
							"payload": obj{"credential": obj{
								"credential_type": "KERBEROS_USER", "username": "admin", "password": redact.Marker,
							}},
						},
					},
					{Step: 2, Description: "TrueNAS then resets its local " + dsType + " configuration and disables the service", Operation: "disable", Target: dsType},
				},
				Warnings: []string{
					"System will leave the " + dsType + " domain/server",
					"Domain users will no longer be able to authenticate",
					"Shares with domain ACLs may become inaccessible",
					"This operation cannot be undone automatically - rejoin will be required",
					`The KERBEROS_USER credential for "admin" is sent to directoryservices.leave; the password is masked in this preview`,
				},
				Requirements: &Requirements{Conditions: []string{
					"Connectivity to " + dsType + " servers",
					"Proper DNS configuration",
					"A KERBEROS_USER credential with the administrative privileges needed to remove the computer account",
				}},
				EstimatedTime: &EstimatedTime{MinSeconds: 5, MaxSeconds: 60, Note: "Time varies based on network and domain controller response"},
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if !jsonEqual(t, gotJSON, wantJSON) {
				t.Errorf("dry run\n got %s\nwant %s", gotJSON, wantJSON)
			}

			// Only status queries; nothing that leaves or changes the configuration.
			onlyMethods(t, fake, "auth.login_ex", "directoryservices.status")
			if len(fake.called("directoryservices.status")) == 0 {
				t.Error("dry run did not query directoryservices.status")
			}
		})
	}

	// Without confirm, the preview is the same and nothing is sent.
	r, fake, _ := leaveFake(t, dsStatus("ACTIVEDIRECTORY", "HEALTHY"))
	out, err := r.CallTool("leave_directory_service", leaveArgs(nil))
	if err != nil || !strings.Contains(out, "confirmation_required") || !strings.Contains(out, `"method": "directoryservices.leave"`) {
		t.Errorf("unconfirmed call: err=%v\n%s", err, out)
	}
	if strings.Contains(out, "s3cret") {
		t.Errorf("preview leaked the password:\n%s", out)
	}
	onlyMethods(t, fake, "auth.login_ex", "directoryservices.status")
}

func TestLeaveDirectoryServicePreChecks(t *testing.T) {
	tests := []struct {
		name    string
		status  obj
		wantErr string
	}{
		{"nothing configured", dsStatus(nil, nil), "no directory service is currently configured"},
		{"configured but disabled", dsStatus("IPA", "DISABLED"), "IPA is configured but not enabled"},
		{"ldap cannot be left", dsStatus("LDAP", "HEALTHY"), "LDAP does not support leave operations (only ACTIVEDIRECTORY and IPA do)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, fake, _ := leaveFake(t, tc.status)
			for _, extra := range []obj{{"dry_run": true}, {"confirm": true}} {
				if _, err := r.CallTool("leave_directory_service", leaveArgs(extra)); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("%v: err = %v, want %q", extra, err, tc.wantErr)
				}
			}
			onlyMethods(t, fake, "auth.login_ex", "directoryservices.status")
		})
	}
}

func TestLeaveDirectoryServiceExecutesWithConfirm(t *testing.T) {
	results := dsStatus("ACTIVEDIRECTORY", "HEALTHY")
	results["directoryservices.leave"] = 77
	r, fake, mgr := leaveFake(t, results)

	out, err := r.CallTool("leave_directory_service", leaveArgs(obj{"confirm": true}))
	if err != nil {
		t.Fatal(err)
	}
	calls := fake.called("directoryservices.leave")
	if len(calls) != 1 {
		t.Fatalf("directoryservices.leave called %d times", len(calls))
	}
	// One positional argument: exactly the payload the dry run shows, unmasked.
	wantParams := `[{"credential": {"credential_type": "KERBEROS_USER", "username": "admin", "password": "s3cret"}}]`
	if !jsonEqual(t, calls[0].Params, []byte(wantParams)) {
		t.Errorf("params = %s, want %s", calls[0].Params, wantParams)
	}
	onlyMethods(t, fake, "auth.login_ex", "directoryservices.status", "directoryservices.leave")

	var reply struct {
		Success       bool   `json:"success"`
		DirectoryType string `json:"directory_type"`
		JobID         int    `json:"job_id"`
		TaskID        string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatal(err)
	}
	if !reply.Success || reply.DirectoryType != "ACTIVEDIRECTORY" || reply.JobID != 77 || reply.TaskID == "" {
		t.Fatalf("reply = %s", out)
	}
	if strings.Contains(out, "s3cret") {
		t.Errorf("reply leaked the password: %s", out)
	}

	task, err := mgr.Get(reply.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.JobID == nil || *task.JobID != 77 {
		t.Errorf("task job = %v", task.JobID)
	}
	if want := int64((15 * time.Minute).Seconds()); task.TTL != want {
		t.Errorf("task TTL = %ds, want %ds", task.TTL, want)
	}
	stored, _ := json.Marshal(task.Arguments)
	if strings.Contains(string(stored), "s3cret") || !strings.Contains(string(stored), redact.Marker) {
		t.Errorf("task arguments = %s, want the password masked", stored)
	}
}

func TestLeaveDirectoryServiceJobIDForms(t *testing.T) {
	tests := []struct {
		name    string
		result  interface{}
		wantJob int
		warning bool
	}{
		{"list of job ids", []int{9}, 9, false},
		{"no job id", nil, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			results := dsStatus("IPA", "HEALTHY")
			results["directoryservices.leave"] = tc.result
			r, _, _ := leaveFake(t, results)
			out, err := r.CallTool("leave_directory_service", leaveArgs(obj{"confirm": true}))
			if err != nil {
				t.Fatal(err)
			}
			var reply struct {
				JobID   int    `json:"job_id"`
				Warning string `json:"warning"`
			}
			if err := json.Unmarshal([]byte(out), &reply); err != nil {
				t.Fatal(err)
			}
			if reply.JobID != tc.wantJob || (reply.Warning != "") != tc.warning {
				t.Errorf("reply = %s", out)
			}
		})
	}
}

func TestLeaveDirectoryServiceReportsMiddlewareError(t *testing.T) {
	fake := &fakeMiddleware{
		results: dsStatus("ACTIVEDIRECTORY", "HEALTHY"),
		handle: func(method string, _ json.RawMessage) (interface{}, obj, bool) {
			if method == "directoryservices.leave" {
				return nil, obj{"code": -32001, "message": "Method call error", "data": obj{"reason": "[EFAULT] bad"}}, true
			}
			return nil, nil, false
		},
	}
	client, _ := newFakeClientFrom(t, fake)
	r := NewRegistry(client, nil)
	_, err := r.CallTool("leave_directory_service", leaveArgs(obj{"confirm": true}))
	if err == nil || !strings.Contains(err.Error(), "failed to leave ACTIVEDIRECTORY") {
		t.Errorf("err = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaked the password: %v", err)
	}
}
