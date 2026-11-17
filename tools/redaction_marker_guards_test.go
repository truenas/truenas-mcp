package tools

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/tasks"
)

const (
	fakeStoredBindPW  = "fake-stored-bindpw-0000"
	fakeStoredIdmapPW = "fake-stored-idmap-pw-0000"
)

// storedLDAPConfig is a directoryservices.config result for an LDAP
// service bound with a plain bind DN and password.
func storedLDAPConfig() obj {
	return obj{
		"id":           1.0,
		"service_type": "LDAP",
		"enable":       true,
		"credential": obj{
			"credential_type": "LDAP_PLAIN",
			"binddn":          "cn=reader,dc=example,dc=com",
			"bindpw":          fakeStoredBindPW,
		},
		"enable_account_cache": true,
		"enable_dns_updates":   false,
		"timeout":              10.0,
		"kerberos_realm":       nil,
		"configuration": obj{
			"server_urls":           []interface{}{"ldaps://ldap.example.com"},
			"basedn":                "dc=example,dc=com",
			"starttls":              false,
			"validate_certificates": true,
			"schema":                "RFC2307",
		},
	}
}

// storedADConfig is a directoryservices.config result for Active Directory
// with an LDAP idmap backend whose bind password is stored.
func storedADConfig() obj {
	return obj{
		"id":           1.0,
		"service_type": "ACTIVEDIRECTORY",
		"enable":       true,
		"credential": obj{
			"credential_type": "KERBEROS_PRINCIPAL",
			"principal":       "TRUENAS$@CORP.EXAMPLE.COM",
		},
		"kerberos_realm": "CORP.EXAMPLE.COM",
		"configuration": obj{
			"hostname": "truenas",
			"domain":   "corp.example.com",
			"idmap": obj{
				"idmap_domain": obj{
					"name":                  "CORP",
					"idmap_backend":         "LDAP",
					"range_low":             100000001.0,
					"range_high":            200000000.0,
					"ldap_url":              "ldap://idmap.example.com",
					"ldap_user_dn":          "cn=idmap,dc=example,dc=com",
					"ldap_base_dn":          "dc=example,dc=com",
					"ldap_user_dn_password": fakeStoredIdmapPW,
					"readonly":              true,
				},
			},
		},
	}
}

func dsStoredFake(t *testing.T, stored obj) (*Registry, *fakeMiddleware) {
	t.Helper()
	fake := &fakeMiddleware{results: map[string]interface{}{
		"directoryservices.status": obj{"type": stored["service_type"], "status": "HEALTHY", "status_msg": nil},
		"directoryservices.config": stored,
		"directoryservices.update": 4242,
		"core.get_jobs":            []interface{}{obj{"id": 4242.0, "method": "directoryservices.update", "state": "RUNNING"}},
	}}
	client, fake := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	t.Cleanup(mgr.Shutdown)
	return NewRegistry(client, mgr), fake
}

// dsArgsFromQuery is what a caller sends after query_directory_services:
// the stored service_type, credential and configuration with secrets
// redacted, plus enable.
func dsArgsFromQuery(t *testing.T, stored obj) obj {
	t.Helper()
	shown, ok := redact.Value(cloneValues(stored)).(map[string]interface{})
	if !ok {
		t.Fatal("redact.Value did not return an object")
	}
	args := obj{
		"service_type":  shown["service_type"],
		"enable":        true,
		"credential":    shown["credential"],
		"configuration": shown["configuration"],
	}
	if realm, ok := shown["kerberos_realm"].(string); ok {
		args["kerberos_realm"] = realm
	}
	return args
}

func dsUpdateSent(t *testing.T, fake *fakeMiddleware) map[string]interface{} {
	t.Helper()
	calls := fake.called("directoryservices.update")
	if len(calls) != 1 {
		t.Fatalf("directoryservices.update called %d times, want 1", len(calls))
	}
	var params []map[string]interface{}
	if err := json.Unmarshal(calls[0].Params, &params); err != nil || len(params) != 1 {
		t.Fatalf("params %s: %v", calls[0].Params, err)
	}
	return params[0]
}

// Regression: configure_directory_service sent the marker copied from
// query_directory_services as the real bind password. The stored value is
// kept now, and nothing returned to the caller contains it.
func TestConfigureDirectoryServiceKeepsRedactedBindPassword(t *testing.T) {
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	stored := storedLDAPConfig()
	r, fake := dsStoredFake(t, stored)
	args := dsArgsFromQuery(t, stored)
	if got := args["credential"].(map[string]interface{})["bindpw"]; got != redact.Marker {
		t.Fatalf("test setup: query_directory_services shows bindpw as %v, want the marker", got)
	}
	args["configuration"].(map[string]interface{})["validate_certificates"] = false

	check := func(label, out string) {
		t.Helper()
		if strings.Contains(out, fakeStoredBindPW) {
			t.Errorf("%s contains the stored secret:\n%s", label, out)
		}
	}
	preview, err := r.CallTool(t.Context(), "configure_directory_service", withArgs(args, obj{"dry_run": true}))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	check("dry-run preview", preview)
	if !strings.Contains(preview, "credential.bindpw") {
		t.Errorf("preview does not name the kept path credential.bindpw:\n%s", preview)
	}
	prompt, err := r.CallTool(t.Context(), "configure_directory_service", cloneValues(args))
	if err != nil {
		t.Fatalf("confirmation prompt: %v", err)
	}
	check("confirmation prompt", prompt)
	if !strings.Contains(prompt, "credential.bindpw") {
		t.Errorf("confirmation prompt does not name the kept path:\n%s", prompt)
	}
	tok := replyToken(t, prompt)
	if tok == "" {
		t.Fatalf("no confirmation_token in prompt:\n%s", prompt)
	}
	out, err := r.CallTool(t.Context(), "configure_directory_service", withArgs(args, obj{"confirmation_token": tok}))
	if err != nil {
		t.Fatalf("confirmed: %v", err)
	}
	check("execute reply", out)
	var reply map[string]interface{}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("reply: %v\n%s", err, out)
	}
	taskID, _ := reply["task_id"].(string)
	if taskID == "" {
		t.Fatalf("no task_id in reply:\n%s", out)
	}
	taskOut, err := r.CallTool(t.Context(), "tasks_get", obj{"task_id": taskID})
	if err != nil {
		t.Fatalf("tasks_get: %v", err)
	}
	check("tasks_get", taskOut)
	check("logs", logs.String())

	sent := dsUpdateSent(t, fake)
	if got := sent["credential"].(map[string]interface{})["bindpw"]; got != fakeStoredBindPW {
		t.Errorf("directoryservices.update credential.bindpw = %v, want the stored value (the marker must not be stored)", got)
	}
	if got := sent["configuration"].(map[string]interface{})["validate_certificates"]; got != false {
		t.Errorf("configuration.validate_certificates = %v, want the caller's change", got)
	}
}

// A kept idmap backend password is put back when its backend is unchanged.
func TestConfigureDirectoryServiceKeepsRedactedIdmapPassword(t *testing.T) {
	stored := storedADConfig()
	r, fake := dsStoredFake(t, stored)
	args := dsArgsFromQuery(t, stored)
	args["configuration"].(map[string]interface{})["site"] = "Branch"
	if _, err := r.CallTool(t.Context(), "configure_directory_service", approve(t, r, "configure_directory_service", args)); err != nil {
		t.Fatalf("configure_directory_service: %v", err)
	}
	dom := dsUpdateSent(t, fake)["configuration"].(map[string]interface{})["idmap"].(map[string]interface{})["idmap_domain"].(map[string]interface{})
	if dom["ldap_user_dn_password"] != fakeStoredIdmapPW {
		t.Errorf("idmap ldap_user_dn_password = %v, want the stored value", dom["ldap_user_dn_password"])
	}
}

// The stored secret is never sent for a different account, backend or
// directory, and the marker is never stored: each case is refused, naming
// the path, in the dry run and the confirm path.
func TestConfigureDirectoryServiceRedactedMarkerRefused(t *testing.T) {
	cases := map[string]struct {
		stored func() obj
		edit   func(args obj)
		path   string
	}{
		"bind DN changed": {storedLDAPConfig, func(a obj) {
			a["credential"].(map[string]interface{})["binddn"] = "cn=admin,dc=example,dc=com"
		}, "credential.bindpw"},
		"server changed": {storedLDAPConfig, func(a obj) {
			a["configuration"].(map[string]interface{})["server_urls"] = []interface{}{"ldaps://other.example.com"}
		}, "credential.bindpw"},
		"kerberos realm changed": {storedLDAPConfig, func(a obj) {
			a["kerberos_realm"] = "OTHER.EXAMPLE.COM"
		}, "credential.bindpw"},
		"credential type changed": {storedLDAPConfig, func(a obj) {
			a["credential"] = obj{"credential_type": "KERBEROS_USER", "username": "admin", "password": redact.Marker}
			a["kerberos_realm"] = "EXAMPLE.COM"
		}, "credential.password"},
		"marker on a field that is not secret": {storedLDAPConfig, func(a obj) {
			a["configuration"].(map[string]interface{})["basedn"] = redact.Marker
		}, "configuration.basedn"},
		"embedded marker": {storedLDAPConfig, func(a obj) {
			a["configuration"].(map[string]interface{})["auxiliary_parameters"] = "x=" + redact.Marker
		}, "configuration.auxiliary_parameters"},
		"idmap URL changed": {storedADConfig, func(a obj) {
			dom := a["configuration"].(map[string]interface{})["idmap"].(map[string]interface{})["idmap_domain"].(map[string]interface{})
			dom["ldap_url"] = "ldap://other.example.com"
		}, "configuration.idmap.idmap_domain.ldap_user_dn_password"},
		"domain changed": {storedADConfig, func(a obj) {
			a["configuration"].(map[string]interface{})["domain"] = "other.example.com"
		}, "configuration.idmap.idmap_domain.ldap_user_dn_password"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stored := tc.stored()
			r, fake := dsStoredFake(t, stored)
			args := dsArgsFromQuery(t, stored)
			tc.edit(args)
			for _, extra := range []obj{{"dry_run": true}, {}} {
				out, err := r.CallTool(t.Context(), "configure_directory_service", withArgs(cloneValues(args), extra))
				if err == nil {
					t.Fatalf("configure_directory_service (%v) accepted the marker: %s", extra, out)
				}
				if !strings.Contains(err.Error(), tc.path) {
					t.Errorf("error %q, want the path %s", err, tc.path)
				}
				for _, secret := range []string{fakeStoredBindPW, fakeStoredIdmapPW} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("error leaks the stored secret: %v", err)
					}
				}
			}
			if n := len(fake.called("directoryservices.update")); n != 0 {
				t.Errorf("directoryservices.update called %d times", n)
			}
		})
	}
}

// LDAP auxiliary_parameters (redacted as a whole) is kept when other
// configuration fields change but the directory is the same, and refused
// when the server changes.
func TestConfigureDirectoryServiceKeepsRedactedAuxiliaryParameters(t *testing.T) {
	const aux = "ldap_default_authtok = fake-aux-secret-0000"
	stored := storedLDAPConfig()
	stored["configuration"].(map[string]interface{})["auxiliary_parameters"] = aux
	r, fake := dsStoredFake(t, stored)
	args := dsArgsFromQuery(t, stored)
	conf := args["configuration"].(map[string]interface{})
	if conf["auxiliary_parameters"] != redact.Marker {
		t.Fatalf("test setup: auxiliary_parameters shown as %v", conf["auxiliary_parameters"])
	}
	conf["basedn"] = "ou=people,dc=example,dc=com"
	if _, err := r.CallTool(t.Context(), "configure_directory_service", approve(t, r, "configure_directory_service", args)); err != nil {
		t.Fatalf("configure_directory_service: %v", err)
	}
	sent := dsUpdateSent(t, fake)["configuration"].(map[string]interface{})
	if sent["auxiliary_parameters"] != aux || sent["basedn"] != "ou=people,dc=example,dc=com" {
		t.Errorf("configuration = %v, want the stored auxiliary_parameters and the new basedn", sent)
	}

	conf["server_urls"] = []interface{}{"ldaps://other.example.com"}
	_, err := r.CallTool(t.Context(), "configure_directory_service", withArgs(args, obj{"dry_run": true}))
	if err == nil || !strings.Contains(err.Error(), "configuration.auxiliary_parameters") || strings.Contains(err.Error(), "fake-aux-secret") {
		t.Errorf("err = %v, want a refusal naming configuration.auxiliary_parameters", err)
	}
}

// A payload without the marker does not read the stored config.
func TestConfigureDirectoryServiceWithoutMarkerSkipsConfigRead(t *testing.T) {
	r, fake := dsStoredFake(t, storedLDAPConfig())
	args := fullDSExamples()["LDAP"]
	if _, err := r.CallTool(t.Context(), "configure_directory_service", withArgs(args, obj{"dry_run": true})); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if n := len(fake.called("directoryservices.config")); n != 0 {
		t.Errorf("directoryservices.config called %d times, want 0", n)
	}
}

// install_app has no stored value to keep, so a marker anywhere in values
// is refused before anything is created.
func TestInstallAppRefusesRedactionMarker(t *testing.T) {
	for name, set := range map[string]func(v map[string]interface{}){
		"exact": func(v map[string]interface{}) {
			v["network"] = map[string]interface{}{"auth_key": redact.Marker}
		},
		"embedded": func(v map[string]interface{}) {
			v["env"] = []interface{}{"DB_PASSWORD=" + redact.Marker}
		},
		"env entry": func(v map[string]interface{}) {
			v["env"] = []interface{}{map[string]interface{}{"name": "DB_PASSWORD", "value": redact.Marker}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, fake := hostPathRegistry(t)
			values := validHostPathValues()
			set(values)
			for _, dry := range []bool{true, false} {
				args := map[string]interface{}{"app_name": "media", "catalog_app": "jellyfin", "train": "community", "values": cloneValues(values)}
				if dry {
					args["dry_run"] = true
				}
				out, err := r.CallTool(t.Context(), "install_app", args)
				if err == nil {
					t.Fatalf("install_app (dry_run=%v) accepted the marker: %s", dry, out)
				}
				if !strings.Contains(err.Error(), "values.") || !strings.Contains(err.Error(), redact.Marker) {
					t.Errorf("error %q, want the path and the marker named", err)
				}
			}
			if n := len(fake.called("app.create")); n != 0 {
				t.Errorf("app.create called %d times", n)
			}
		})
	}
}

// update_app: a secret is kept only for the same entry. Renaming
// DB_PASSWORD to API_TOKEN keeps the value masked, but the app would get
// the secret under a different variable.
func TestUpdateAppRedactedMarkerRefusedOnMaskedRename(t *testing.T) {
	current := envAppConfig()
	r, fake := issue157Registry(t, current, false)
	values := cloneValues(current)
	values["env"] = []interface{}{envEntry("API_TOKEN", redact.Marker), envEntry("LOG_LEVEL", "info")}
	if v := redact.Value(cloneValues(values)).(map[string]interface{})["env"].([]interface{})[0].(map[string]interface{})["value"]; v != redact.Marker {
		t.Fatalf("test setup: API_TOKEN is not masked (%v)", v)
	}
	for _, extra := range []map[string]interface{}{{"dry_run": true}, nil} {
		out, err := callUpdateApp(t, r, cloneValues(values), extra)
		if err == nil {
			t.Fatalf("update_app (%v) moved the secret to another variable: %s", extra, out)
		}
		if !strings.Contains(err.Error(), "env[0].value") || !strings.Contains(err.Error(), "name") {
			t.Errorf("error %q, want the path env[0].value and the name field", err)
		}
		if strings.Contains(err.Error(), fakeEnvSecret) {
			t.Errorf("error leaks the stored value: %v", err)
		}
	}
	if n := len(fake.called("app.update")); n != 0 {
		t.Errorf("app.update called %d times", n)
	}
}

// update_app: the storage-location check also applies when only the stored
// entry has the storage shape (the outgoing entry dropped its type).
func TestCheckKeptValuesStoredStorageShape(t *testing.T) {
	current := cifsAppConfig("files.example.invalid")
	next := cloneValues(current)
	entry := next["storage"].(map[string]interface{})["additional_storage"].([]interface{})[0].(map[string]interface{})
	delete(entry, "type")
	kept := []valuePath{{"storage", "additional_storage", 0, "cifs_config", "password"}}
	err := checkKeptValues(kept, current, next)
	if err == nil || !strings.Contains(err.Error(), "changes type or location") {
		t.Fatalf("checkKeptValues = %v, want a refusal for the changed storage entry", err)
	}
	if strings.Contains(err.Error(), issue157CIFSPassword) {
		t.Errorf("error leaks the stored password: %v", err)
	}
}

// update_app confirmation facts list only the changed leaf fields of a
// resent nested object, so a kept secret does not look like part of the
// change; a changed secret is reported without its value.
func TestUpdateAppConfirmFactsAreFieldLevel(t *testing.T) {
	current := map[string]interface{}{
		"network": map[string]interface{}{"auth_key": redact.Marker, "hostname": "node-a", "routes": []interface{}{"192.0.2.0/24"}},
		"env":     []interface{}{envEntry("DB_PASSWORD", "fake-old-0000"), envEntry("LOG_LEVEL", "info")},
	}
	next := map[string]interface{}{
		"network": map[string]interface{}{"auth_key": redact.Marker, "hostname": "node-b", "routes": []interface{}{"192.0.2.0/24"}},
		"env":     []interface{}{envEntry("DB_PASSWORD", "fake-new-0000"), envEntry("LOG_LEVEL", "info")},
	}
	facts := strings.Join(updateAppSummaryFacts("app", current, next, []string{"network.auth_key"}), "\n")
	for _, want := range []string{
		"changing keys: env, network",
		`network.hostname: "node-a" -> "node-b"`,
		"env[0].value: changed (secret, not shown)",
		"kept at stored value (not shown) for: network.auth_key",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	for _, refuse := range []string{"auth_key\":", "routes", "LOG_LEVEL", "fake-old-0000", "fake-new-0000", "->  {"} {
		if strings.Contains(facts, refuse) {
			t.Errorf("facts contain %q:\n%s", refuse, facts)
		}
	}
}
