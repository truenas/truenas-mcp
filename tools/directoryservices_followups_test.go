package tools

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/truenas/truenas-mcp/redact"
)

// adWithIdmapSecrets returns an Active Directory configuration whose idmap
// and trusted_domains carry ldap_user_dn_password, a secret in the middleware
// model (LDAP_Idmap and RFC2307_Idmap).
func adWithIdmapSecrets() obj {
	args := fullDSExamples()["ACTIVEDIRECTORY"]
	conf := args["configuration"].(obj)
	conf["idmap"] = obj{"idmap_domain": obj{
		"name": "CORP", "idmap_backend": "LDAP", "range_low": 100000001.0, "range_high": 200000000.0,
		"ldap_base_dn": "dc=idmap,dc=example,dc=com", "ldap_user_dn": "cn=idmap,dc=example,dc=com",
		"ldap_user_dn_password": "idmap-s3cret", "ldap_url": "ldaps://idmap.example.com", "readonly": false,
	}}
	conf["trusted_domains"] = []interface{}{
		obj{"name": "BROOK", "idmap_backend": "RFC2307", "range_low": 200000001.0, "range_high": 300000000.0,
			"ldap_url": "ldaps://brook.example.com", "ldap_user_dn": "cn=u,dc=brook", "ldap_user_dn_password": "trusted-s3cret",
			"bind_path_user": "ou=u", "bind_path_group": "ou=g"},
		obj{"name": "RIVER", "idmap_backend": "RID", "range_low": 300000001.0, "range_high": 400000000.0},
	}
	return args
}

var nestedSecrets = []string{"idmap-s3cret", "trusted-s3cret", "s3cret"}

func assertNoSecrets(t *testing.T, what, text string) {
	t.Helper()
	for _, s := range nestedSecrets {
		if strings.Contains(text, s) {
			t.Errorf("%s leaked %q:\n%s", what, s, text)
		}
	}
}

func TestConfigureDirectoryServiceMasksNestedSecrets(t *testing.T) {
	args := adWithIdmapSecrets()
	payload, err := buildDirectoryServiceUpdatePayload(args)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("mask helper", func(t *testing.T) {
		masked := maskDirectoryServicePayload(payload)
		b, _ := json.Marshal(masked)
		assertNoSecrets(t, "masked payload", string(b))
		conf := masked["configuration"].(map[string]interface{})
		dom := conf["idmap"].(map[string]interface{})["idmap_domain"].(map[string]interface{})
		if dom["ldap_user_dn_password"] != redact.Marker {
			t.Errorf("idmap_domain.ldap_user_dn_password = %v", dom["ldap_user_dn_password"])
		}
		trusted := conf["trusted_domains"].([]interface{})
		if trusted[0].(map[string]interface{})["ldap_user_dn_password"] != redact.Marker {
			t.Errorf("trusted_domains[0].ldap_user_dn_password = %v", trusted[0])
		}
		// Everything that is not a secret is unchanged.
		if dom["ldap_user_dn"] != "cn=idmap,dc=example,dc=com" || dom["idmap_backend"] != "LDAP" || dom["range_low"] != 100000001.0 {
			t.Errorf("non-secret idmap fields changed: %v", dom)
		}
		if trusted[1].(map[string]interface{})["name"] != "RIVER" {
			t.Errorf("trusted_domains[1] = %v", trusted[1])
		}
		cred := masked["credential"].(map[string]interface{})
		if cred["username"] != "admin" || cred["credential_type"] != "KERBEROS_USER" || cred["password"] != redact.Marker {
			t.Errorf("credential = %v", cred)
		}
		// The input is not modified.
		if payload["configuration"].(map[string]interface{})["idmap"].(map[string]interface{})["idmap_domain"].(map[string]interface{})["ldap_user_dn_password"] != "idmap-s3cret" {
			t.Error("masking modified the payload that is sent to TrueNAS")
		}
	})

	t.Run("preview", func(t *testing.T) {
		for _, extra := range []obj{{"dry_run": true}, {}} {
			r, fake, _ := dsFake(t)
			out, err := r.CallTool("configure_directory_service", withArgs(args, extra))
			if err != nil {
				t.Fatal(err)
			}
			assertNoSecrets(t, "preview", out)
			if strings.Count(out, redact.Marker) < 3 {
				t.Errorf("preview does not show the masked secrets:\n%s", out)
			}
			if len(fake.called("directoryservices.update")) != 0 {
				t.Error("preview called directoryservices.update")
			}
		}
	})

	t.Run("confirm", func(t *testing.T) {
		r, fake, mgr := dsFake(t)
		out, err := r.CallTool("configure_directory_service", withArgs(args, obj{"confirm": true}))
		if err != nil {
			t.Fatal(err)
		}
		assertNoSecrets(t, "response", out)
		// TrueNAS itself still gets the real values.
		calls := fake.called("directoryservices.update")
		if len(calls) != 1 || !strings.Contains(string(calls[0].Params), "idmap-s3cret") || !strings.Contains(string(calls[0].Params), "trusted-s3cret") {
			t.Fatalf("params sent to TrueNAS = %v", calls)
		}
		var reply struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal([]byte(out), &reply); err != nil {
			t.Fatal(err)
		}
		task, err := mgr.Get(reply.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		stored, _ := json.Marshal(task.Arguments)
		assertNoSecrets(t, "task arguments", string(stored))
		if !strings.Contains(string(stored), redact.Marker) || !strings.Contains(string(stored), "RIVER") {
			t.Errorf("task arguments = %s", stored)
		}
	})
}

// The handler's masking and the output redactor agree: the preview, run
// through the redactor the server applies to every tool result, keeps
// credential_type and username visible and every secret masked.
func TestDirectoryServiceDryRunThroughOutputRedactor(t *testing.T) {
	check := func(t *testing.T, out string) {
		t.Helper()
		redacted := redact.ToolOutput(out)
		assertNoSecrets(t, "redacted preview", redacted)
		for _, want := range []string{`"credential_type": "KERBEROS_USER"`, `"username": "admin"`, `"password": "[REDACTED]"`} {
			if !strings.Contains(redacted, want) {
				t.Errorf("redacted preview lacks %s:\n%s", want, redacted)
			}
		}
	}
	t.Run("configure", func(t *testing.T) {
		r, _, _ := dsFake(t)
		out, err := r.CallTool("configure_directory_service", withArgs(adWithIdmapSecrets(), obj{"dry_run": true}))
		if err != nil {
			t.Fatal(err)
		}
		check(t, out)
		if redacted := redact.ToolOutput(out); !strings.Contains(redacted, `"ldap_user_dn_password": "[REDACTED]"`) {
			t.Errorf("nested secret not shown as masked:\n%s", redacted)
		}
	})
	t.Run("leave", func(t *testing.T) {
		r, _, _ := leaveFake(t, dsStatus("ACTIVEDIRECTORY", "HEALTHY"))
		out, err := r.CallTool("leave_directory_service", leaveArgs(obj{"dry_run": true}))
		if err != nil {
			t.Fatal(err)
		}
		check(t, out)
	})
	// What the redactor does to an unmasked payload (the task arguments a
	// client could read): same result.
	t.Run("unmasked payload", func(t *testing.T) {
		payload, err := buildDirectoryServiceUpdatePayload(adWithIdmapSecrets())
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(obj{"payload": payload})
		redacted := redact.ToolOutput(string(b))
		assertNoSecrets(t, "redacted payload", redacted)
		if !strings.Contains(redacted, `"username":"admin"`) && !strings.Contains(redacted, `"username": "admin"`) {
			t.Errorf("username hidden:\n%s", redacted)
		}
	})
}

func TestConfigureDirectoryServiceBareDisable(t *testing.T) {
	args := obj{"enable": false}

	payload, err := buildDirectoryServiceUpdatePayload(withArgs(args, obj{"confirm": true, "dry_run": false, "timeout": 20.0}))
	if err != nil {
		t.Fatal(err)
	}
	if want := (obj{"enable": false, "timeout": 20}); !reflect.DeepEqual(map[string]interface{}(payload), map[string]interface{}(want)) {
		t.Errorf("payload = %v, want %v", payload, want)
	}

	t.Run("preview warns that domain authentication stops", func(t *testing.T) {
		r, fake, _ := leaveFake(t, dsStatus("ACTIVEDIRECTORY", "HEALTHY"))
		out, err := r.CallTool("configure_directory_service", withArgs(args, obj{"dry_run": true}))
		if err != nil {
			t.Fatal(err)
		}
		var got DryRunResult
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		a := got.PlannedActions[0]
		if a.Operation != "disable" || a.Target != "ACTIVEDIRECTORY" || a.Description != "Call directoryservices.update to disable ACTIVEDIRECTORY" {
			t.Errorf("first action = %+v", a)
		}
		if p, _ := json.Marshal(a.Details.(map[string]interface{})["payload"]); string(p) != `{"enable":false}` {
			t.Errorf("payload = %s", p)
		}
		if len(got.PlannedActions) != 1 {
			t.Errorf("a disable plans no join step: %+v", got.PlannedActions)
		}
		joined := strings.Join(got.Warnings, "\n")
		for _, want := range []string{"Disabling ACTIVEDIRECTORY stops domain/directory authentication", "use leave_directory_service"} {
			if !strings.Contains(joined, want) {
				t.Errorf("warnings lack %q: %v", want, got.Warnings)
			}
		}
		if strings.Contains(joined, "will attempt to join") {
			t.Errorf("disable preview talks about joining: %v", got.Warnings)
		}
		onlyMethods(t, fake, "auth.login_ex", "directoryservices.status")
	})

	t.Run("nothing configured", func(t *testing.T) {
		r, _, _ := leaveFake(t, dsStatus("none", nil))
		out, err := r.CallTool("configure_directory_service", withArgs(args, obj{"dry_run": true}))
		if err != nil {
			t.Fatal(err)
		}
		var got DryRunResult
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, out)
		}
		if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "nothing to disable") {
			t.Errorf("warnings = %v, want only the nothing-to-disable notice", got.Warnings)
		}
		for _, s := range append(append([]string{}, got.Warnings...), got.PlannedActions[0].Description, got.PlannedActions[0].Target) {
			if strings.Contains(s, "none") || strings.Contains(s, "stops") {
				t.Errorf("preview mentions %q with nothing configured", s)
			}
		}
	})

	t.Run("confirm sends exactly enable=false", func(t *testing.T) {
		r, fake, mgr := dsFake(t)
		out, err := r.CallTool("configure_directory_service", withArgs(args, obj{"confirm": true}))
		if err != nil {
			t.Fatal(err)
		}
		calls := fake.called("directoryservices.update")
		if len(calls) != 1 || string(calls[0].Params) != `[{"enable":false}]` {
			t.Fatalf("params = %v", calls)
		}
		var reply struct {
			Success       bool   `json:"success"`
			Enabled       bool   `json:"enabled"`
			DirectoryType string `json:"directory_type"`
			JobID         int    `json:"job_id"`
			TaskID        string `json:"task_id"`
			Message       string `json:"message"`
		}
		if err := json.Unmarshal([]byte(out), &reply); err != nil {
			t.Fatal(err)
		}
		if !reply.Success || reply.Enabled || reply.DirectoryType != "" || reply.JobID != 4242 || reply.TaskID == "" ||
			!strings.Contains(reply.Message, "Disabling the configured directory service") {
			t.Errorf("reply = %+v", reply)
		}
		if _, err := mgr.Get(reply.TaskID); err != nil {
			t.Error(err)
		}
	})

	t.Run("disable with a stored configuration warns too", func(t *testing.T) {
		r, _, _ := dsFake(t)
		out, err := r.CallTool("configure_directory_service", withArgs(fullDSExamples()["LDAP"], obj{"dry_run": true}))
		if err != nil || !strings.Contains(out, "Disabling LDAP stops domain/directory authentication") {
			t.Errorf("err=%v\n%s", err, out)
		}
	})
}

func TestConfigureDirectoryServiceKerberosUserRuleOnlyForADAndIPA(t *testing.T) {
	// LDAP may store KERBEROS_USER while disabled (the rule is for AD/IPA).
	args := fullDSExamples()["LDAP"]
	args["credential"] = obj{"credential_type": "KERBEROS_USER", "username": "a", "password": "b"}
	args["kerberos_realm"] = "EXAMPLE.COM"
	if _, err := buildDirectoryServiceUpdatePayload(args); err != nil {
		t.Errorf("LDAP + KERBEROS_USER + enable=false rejected: %v", err)
	}
	// AD/IPA may store a principal while disabled, and KERBEROS_USER while enabled.
	for _, name := range []string{"ACTIVEDIRECTORY", "IPA"} {
		a := fullDSExamples()[name]
		a["credential"] = obj{"credential_type": "KERBEROS_PRINCIPAL", "principal": "host/x@R"}
		a["enable"] = false
		if _, err := buildDirectoryServiceUpdatePayload(a); err != nil {
			t.Errorf("%s + KERBEROS_PRINCIPAL + enable=false rejected: %v", name, err)
		}
		a["credential"] = obj{"credential_type": "KERBEROS_USER", "username": "a", "password": "b"}
		a["enable"] = true
		if _, err := buildDirectoryServiceUpdatePayload(a); err != nil {
			t.Errorf("%s + KERBEROS_USER + enable=true rejected: %v", name, err)
		}
	}
}

func TestConfigureDirectoryServiceFlatArgumentsHint(t *testing.T) {
	flat := []obj{
		{"type": "activedirectory", "domain": "example.com", "enable": true},
		{"type": "ldap", "server": "ldap.example.com", "bindname": "cn=a", "bindpw": "pw", "enable": true},
		{"enable": true, "netbiosname": "NAS", "ssl": "ON", "bindname": "a", "domain": "x"},
		{"enable": true, "service_type": "LDAP", "hostname": "x"},
	}
	for _, args := range flat {
		_, err := buildDirectoryServiceUpdatePayload(args)
		if err == nil {
			t.Fatalf("%v accepted", args)
		}
		for _, want := range []string{"the arguments of configure_directory_service changed", "Use service_type + credential + configuration (see the tool description)"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%v: error %q lacks %q", args, err, want)
			}
		}
	}
	// The hint names the offending arguments, and reaches the tool caller.
	client, fake := newFakeClient(t)
	r := NewRegistry(client, nil)
	_, err := r.CallTool("configure_directory_service", obj{"type": "activedirectory", "domain": "example.com", "bindname": "a", "enable": true, "confirm": true})
	if err == nil || !strings.Contains(err.Error(), "bindname, domain, type are no longer top-level arguments") {
		t.Errorf("err = %v", err)
	}
	if len(fake.called("directoryservices.update")) != 0 {
		t.Error("flat arguments reached directoryservices.update")
	}
	// The hint does not depend on which check fails first.
	for name, args := range map[string]obj{
		"flat key, missing credential and configuration": {"enable": true, "service_type": "LDAP", "hostname": "x"},
		"flat keys, valid service_type, no credential":   {"enable": true, "service_type": "ACTIVEDIRECTORY", "domain": "example.com", "bindname": "a"},
		"flat key, bad service_type":                     {"enable": true, "service_type": "activedirectory", "domain": "example.com"},
		"flat key, bad timeout":                          {"enable": true, "service_type": "LDAP", "server": "x", "timeout": 1.0},
		"flat key with valid credential/configuration": func() obj {
			a := fullDSExamples()["LDAP"]
			a["server"] = "ldap.example.com"
			return a
		}(),
	} {
		_, err := buildDirectoryServiceUpdatePayload(args)
		if err == nil || !strings.Contains(err.Error(), "are no longer top-level arguments") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// Legacy keys are listed alphabetically, whatever the input order.
	_, err = buildDirectoryServiceUpdatePayload(obj{"enable": true, "service_type": "LDAP", "ssl": "ON", "domain": "x", "bindname": "a", "type": "ldap"})
	if err == nil || !strings.Contains(err.Error(), "bindname, domain, ssl, type are no longer top-level arguments") {
		t.Errorf("err = %v", err)
	}
	// An unknown argument that is not from the old shape gets no hint.
	if _, err := buildDirectoryServiceUpdatePayload(obj{"enable": true, "bogus": 1}); err == nil || strings.Contains(err.Error(), "changed") {
		t.Errorf("err = %v", err)
	}
	// Valid arguments are untouched.
	if _, err := buildDirectoryServiceUpdatePayload(fullDSExamples()["IPA"]); err != nil {
		t.Error(err)
	}
}

func TestDirectoryServiceIntegerForms(t *testing.T) {
	args := func(timeout interface{}) obj { return obj{"enable": false, "timeout": timeout} }
	for _, ok := range []interface{}{float64(30), 30, int64(30), int32(30), uint(30), json.Number("30"), json.Number("3e1"), float32(30)} {
		payload, err := buildDirectoryServiceUpdatePayload(args(ok))
		if err != nil {
			t.Errorf("timeout %T(%v) rejected: %v", ok, ok, err)
			continue
		}
		if payload["timeout"] != 30 {
			t.Errorf("timeout %T(%v) = %#v, want int 30", ok, ok, payload["timeout"])
		}
	}
	huge := float64(1 << 62)
	for _, bad := range []interface{}{10.5, json.Number("10.5"), json.Number("x"), "10", true, nil, float64(4), 61, json.Number("1e400"), huge, int64(1) << 40, uint64(1) << 40} {
		if p, err := buildDirectoryServiceUpdatePayload(args(bad)); err == nil || !strings.Contains(err.Error(), "timeout must be an integer from 5 to 60") {
			t.Errorf("timeout %T(%v): payload %v, err %v", bad, bad, p, err)
		}
	}
}

func TestParseJobID(t *testing.T) {
	for in, want := range map[string]int{"42": 42, "[42]": 42, "[7,8]": 7, "[]": 0, "null": 0, `"x"`: 0, `{"id":1}`: 0, "": 0, "4.5": 0} {
		if got := parseJobID(json.RawMessage(in)); got != want {
			t.Errorf("parseJobID(%q) = %d, want %d", in, got, want)
		}
	}
}

// ---- schema pinned to the middleware model ----

type middlewareFields struct {
	Update struct {
		Fields          []string                                     `json:"fields"`
		ServiceTypes    []string                                     `json:"service_types"`
		Timeout         struct{ Ge, Le int }                         `json:"timeout"`
		CredentialTypes map[string]struct{ Fields, Secret []string } `json:"credential_types"`
		Configuration   map[string]struct {
			Fields       []string `json:"fields"`
			Required     []string `json:"required"`
			SchemaValues []string `json:"schema_values"`
		} `json:"configuration"`
		IdmapSecretFields map[string][]string `json:"idmap_secret_fields"`
	} `json:"update"`
	Leave struct {
		Fields           []string `json:"fields"`
		CredentialType   string   `json:"credential_type"`
		CredentialFields []string `json:"credential_fields"`
		ServiceTypes     []string `json:"service_types"`
	} `json:"leave"`
}

func loadMiddlewareFields(t *testing.T) middlewareFields {
	t.Helper()
	b, err := os.ReadFile("testdata/directoryservices_middleware_fields.json")
	if err != nil {
		t.Fatal(err)
	}
	var f middlewareFields
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func sorted(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func requiredOf(fields map[string]dsField) []string {
	out := []string{}
	for k, f := range fields {
		if f.required {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func stringsOf(t *testing.T, v interface{}) []string {
	t.Helper()
	switch e := v.(type) {
	case []string:
		return sorted(e)
	case nil:
		return nil
	}
	t.Fatalf("not a string list: %#v", v)
	return nil
}

// TestDirectoryServiceSchemaPinnedToMiddlewareModel compares the tool schemas
// and the handler's tables with the field names of the middleware model
// (testdata/directoryservices_middleware_fields.json), not only with each
// other, so a wrong name copied into both is caught.
func TestDirectoryServiceSchemaPinnedToMiddlewareModel(t *testing.T) {
	mw := loadMiddlewareFields(t)
	schema := dsSchema(t)
	props := propsOf(t, schema)

	// Top level: every update field, plus only the tool's own dry_run/confirm.
	wantTop := append(sorted(mw.Update.Fields), "confirm", "dry_run")
	sort.Strings(wantTop)
	if got := keysOf(props); !reflect.DeepEqual(got, wantTop) {
		t.Errorf("schema properties = %v\nmiddleware fields = %v", got, wantTop)
	}
	handlerTop := []string{"service_type", "credential", "configuration"}
	for k := range dsTopLevelFields {
		handlerTop = append(handlerTop, k)
	}
	if got := sorted(handlerTop); !reflect.DeepEqual(got, sorted(mw.Update.Fields)) {
		t.Errorf("handler top-level fields = %v\nmiddleware fields = %v", got, sorted(mw.Update.Fields))
	}
	if !reflect.DeepEqual(sorted(dsServiceTypes), sorted(mw.Update.ServiceTypes)) {
		t.Errorf("service types = %v, middleware %v", dsServiceTypes, mw.Update.ServiceTypes)
	}
	if got := stringsOf(t, props["service_type"].(obj)["enum"]); !reflect.DeepEqual(got, sorted(mw.Update.ServiceTypes)) {
		t.Errorf("schema service_type enum = %v, middleware %v", got, mw.Update.ServiceTypes)
	}
	if to := dsTopLevelFields["timeout"]; to.min != mw.Update.Timeout.Ge || to.max != mw.Update.Timeout.Le {
		t.Errorf("handler timeout range %d..%d, middleware %d..%d", to.min, to.max, mw.Update.Timeout.Ge, mw.Update.Timeout.Le)
	}
	tp := props["timeout"].(obj)
	if tp["minimum"] != mw.Update.Timeout.Ge || tp["maximum"] != mw.Update.Timeout.Le {
		t.Errorf("schema timeout range %v..%v, middleware %d..%d", tp["minimum"], tp["maximum"], mw.Update.Timeout.Ge, mw.Update.Timeout.Le)
	}

	// Credential types and their fields.
	wantCredProps := map[string]bool{"credential_type": true}
	var wantCredTypes []string
	for ct, def := range mw.Update.CredentialTypes {
		wantCredTypes = append(wantCredTypes, ct)
		if got := sorted(dsCredentialTypes[ct]); !reflect.DeepEqual(got, sorted(def.Fields)) && !(len(got) == 0 && len(def.Fields) == 0) {
			t.Errorf("handler credential fields for %s = %v, middleware %v", ct, got, def.Fields)
		}
		for _, f := range def.Fields {
			wantCredProps[f] = true
		}
	}
	sort.Strings(wantCredTypes)
	if got := sortedKeys(dsCredentialTypes); !reflect.DeepEqual(got, wantCredTypes) {
		t.Errorf("handler credential types = %v, middleware %v", got, wantCredTypes)
	}
	credProps := propsOf(t, props["credential"].(obj))
	for k := range wantCredProps {
		if _, ok := credProps[k]; !ok {
			t.Errorf("schema credential lacks middleware field %s", k)
		}
	}
	for k := range credProps {
		if !wantCredProps[k] {
			t.Errorf("schema credential has %s, not a middleware field", k)
		}
	}
	if got := stringsOf(t, credProps["credential_type"].(obj)["enum"]); !reflect.DeepEqual(got, wantCredTypes) {
		t.Errorf("schema credential_type enum = %v, middleware %v", got, wantCredTypes)
	}

	// Configuration fields and required fields per service type.
	confProps := propsOf(t, props["configuration"].(obj))
	wantConf := map[string]bool{}
	for st, def := range mw.Update.Configuration {
		fields := dsConfigurationFields[st]
		gotNames := []string{}
		for k := range fields {
			gotNames = append(gotNames, k)
		}
		if !reflect.DeepEqual(sorted(gotNames), sorted(def.Fields)) {
			t.Errorf("handler configuration fields for %s = %v, middleware %v", st, sorted(gotNames), sorted(def.Fields))
		}
		if got := requiredOf(fields); !reflect.DeepEqual(got, sorted(def.Required)) {
			t.Errorf("handler requires %v in %s configuration, middleware requires %v", got, st, sorted(def.Required))
		}
		for _, f := range def.Fields {
			wantConf[f] = true
		}
		if def.SchemaValues != nil {
			if got := sorted(fields["schema"].enum); !reflect.DeepEqual(got, sorted(def.SchemaValues)) {
				t.Errorf("handler schema values = %v, middleware %v", got, def.SchemaValues)
			}
		}
	}
	for k := range wantConf {
		if _, ok := confProps[k]; !ok {
			t.Errorf("schema configuration lacks middleware field %s", k)
		}
	}
	for k := range confProps {
		if !wantConf[k] {
			t.Errorf("schema configuration has %s, not a middleware field", k)
		}
	}
	if len(mw.Update.Configuration) != len(dsConfigurationFields) {
		t.Errorf("handler has configuration for %d service types, middleware %d", len(dsConfigurationFields), len(mw.Update.Configuration))
	}

	// Every secret the middleware marks is masked by the handler, at any depth.
	probe := obj{}
	for ct, def := range mw.Update.CredentialTypes {
		for _, s := range def.Secret {
			probe["cred_"+ct+"_"+s] = obj{s: "SECRET-VALUE"}
		}
	}
	for backend, names := range mw.Update.IdmapSecretFields {
		for _, s := range names {
			probe["idmap_"+backend] = obj{"idmap_domain": obj{"idmap_backend": backend, s: "SECRET-VALUE"},
				"trusted_domains": []interface{}{obj{"idmap_backend": backend, s: "SECRET-VALUE"}}}
		}
	}
	if b, _ := json.Marshal(maskDirectoryServicePayload(probe)); strings.Contains(string(b), "SECRET-VALUE") {
		t.Errorf("a secret field of the middleware model is not masked: %s", b)
	}
}

func TestLeaveDirectoryServicePinnedToMiddlewareModel(t *testing.T) {
	mw := loadMiddlewareFields(t).Leave
	tool := NewRegistry(nil, nil).tools["leave_directory_service"]
	props := propsOf(t, tool.Definition.InputSchema)
	want := append(sorted(mw.Fields), "confirm", "dry_run")
	sort.Strings(want)
	if got := keysOf(props); !reflect.DeepEqual(got, want) {
		t.Errorf("leave schema properties = %v, middleware %v", got, want)
	}
	credProps := propsOf(t, props["credential"].(obj))
	wantCred := sorted(append([]string{"credential_type"}, mw.CredentialFields...))
	if got := keysOf(credProps); !reflect.DeepEqual(got, wantCred) {
		t.Errorf("leave credential properties = %v, middleware %v", got, wantCred)
	}
	if got := stringsOf(t, credProps["credential_type"].(obj)["enum"]); !reflect.DeepEqual(got, []string{mw.CredentialType}) {
		t.Errorf("leave credential_type enum = %v, middleware %s", got, mw.CredentialType)
	}
	if dsLeaveCredentialType != mw.CredentialType || !reflect.DeepEqual(sorted(dsLeaveServiceTypes), sorted(mw.ServiceTypes)) {
		t.Errorf("handler leave = %s %v, middleware %s %v", dsLeaveCredentialType, dsLeaveServiceTypes, mw.CredentialType, mw.ServiceTypes)
	}
	if got := sorted(dsCredentialTypes[dsLeaveCredentialType]); !reflect.DeepEqual(got, sorted(mw.CredentialFields)) {
		t.Errorf("handler leave credential fields = %v, middleware %v", got, mw.CredentialFields)
	}
}

// attribute_maps.passwd only holds LDAP attribute names, so it stays visible
// in the preview, the response and the task arguments, while the real
// secrets next to it (the bind password, nested credential secrets and
// ldap_user_dn_password) stay masked.
func TestConfigureDirectoryServiceAttributeMapsVisibleSecretsMasked(t *testing.T) {
	ldap := fullDSExamples()["LDAP"]
	conf := ldap["configuration"].(obj)
	conf["attribute_maps"] = obj{
		"passwd": obj{"user_name": "uid", "user_uid": "uidNumber", "user_shell": "loginShell"},
		"group":  obj{"group_member": "memberUid"},
	}

	visible := []string{`"passwd"`, `"user_name": "uid"`, `"user_uid": "uidNumber"`, `"user_shell": "loginShell"`, `"group_member": "memberUid"`}
	checkText := func(t *testing.T, what, text string) {
		t.Helper()
		assertNoSecrets(t, what, text)
		for _, want := range visible {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %s:\n%s", what, want, text)
			}
		}
		if strings.Contains(text, `"passwd": "[REDACTED]"`) {
			t.Errorf("%s masks attribute_maps.passwd:\n%s", what, text)
		}
	}

	t.Run("mask helper", func(t *testing.T) {
		payload, err := buildDirectoryServiceUpdatePayload(ldap)
		if err != nil {
			t.Fatal(err)
		}
		masked := maskDirectoryServicePayload(payload)
		maps := masked["configuration"].(map[string]interface{})["attribute_maps"].(map[string]interface{})
		passwd, ok := maps["passwd"].(map[string]interface{})
		if !ok || passwd["user_name"] != "uid" || passwd["user_uid"] != "uidNumber" || passwd["user_shell"] != "loginShell" {
			t.Errorf("attribute_maps.passwd = %v", maps["passwd"])
		}
		cred := masked["credential"].(map[string]interface{})
		if cred["bindpw"] != redact.Marker || cred["binddn"] != "cn=admin,dc=example,dc=com" {
			t.Errorf("credential = %v", cred)
		}
	})

	t.Run("LDAP preview, response and task arguments", func(t *testing.T) {
		r, _, _ := dsFake(t)
		out, err := r.CallTool("configure_directory_service", withArgs(ldap, obj{"dry_run": true}))
		if err != nil {
			t.Fatal(err)
		}
		checkText(t, "preview", out)
		checkText(t, "redacted preview", redact.ToolOutput(out))
		if !strings.Contains(out, `"bindpw": "[REDACTED]"`) {
			t.Errorf("bindpw not shown as masked:\n%s", out)
		}

		r, fake, mgr := dsFake(t)
		out, err = r.CallTool("configure_directory_service", withArgs(ldap, obj{"confirm": true}))
		if err != nil {
			t.Fatal(err)
		}
		// The response does not echo the payload; it must not leak anything.
		assertNoSecrets(t, "response", redact.ToolOutput(out))
		calls := fake.called("directoryservices.update")
		if len(calls) != 1 || !strings.Contains(string(calls[0].Params), "s3cret") || !strings.Contains(string(calls[0].Params), "uidNumber") {
			t.Fatalf("params sent to TrueNAS = %v", calls)
		}
		var reply struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal([]byte(out), &reply); err != nil {
			t.Fatal(err)
		}
		task, err := mgr.Get(reply.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		stored, _ := json.MarshalIndent(task.Arguments, "", "  ")
		checkText(t, "task arguments", redact.ToolOutput(string(stored)))
		checkText(t, "raw task arguments", string(stored))
	})
}
