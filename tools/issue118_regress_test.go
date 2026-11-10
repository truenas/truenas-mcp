package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
)

// catalogAppDetails2510 is the 25.10 catalog.get_app_details shape:
// CatalogAppInfo fields plus versions[latest].schema and values, as
// returned by get_app_details in middleware plugins/catalog/apps_util.py
// on origin/r2510 (TS-25.10.8). The API model is CatalogGetAppDetailsResult
// / CatalogAppInfo in api/v25_10_0/catalog.py; that model sets extra=allow
// so the versions map is included. Schema questions follow the official
// apps questions.yaml shape (groups plus questions with nested schema.attrs),
// matching middleware plugins/apps construct_schema tests.
func catalogAppDetails2510(name, version string, schema map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"app_readme":           "",
		"categories":           []interface{}{"media"},
		"description":          "A catalog app",
		"healthy":              true,
		"healthy_error":        nil,
		"home":                 "https://example.invalid/app",
		"location":             "/mnt/.ix-apps/truenas_catalog/trains/community/" + name,
		"latest_version":       version,
		"latest_app_version":   version,
		"latest_human_version": version,
		"last_update":          "2026-01-01 00:00:00",
		"name":                 name,
		"recommended":          false,
		"title":                "Example App",
		"maintainers": []interface{}{
			map[string]interface{}{"name": "truenas", "email": "dev@example.invalid", "url": nil},
		},
		"tags":        []interface{}{},
		"screenshots": []interface{}{},
		"sources":     []interface{}{},
		"icon_url":    nil,
		"versions": map[string]interface{}{
			version: map[string]interface{}{
				"name":              name,
				"categories":        []interface{}{"media"},
				"app_readme":        nil,
				"location":          "/mnt/.ix-apps/truenas_catalog/trains/community/" + name + "/" + version,
				"healthy":           true,
				"supported":         true,
				"healthy_error":     nil,
				"required_features": []interface{}{},
				"version":           version,
				"human_version":     version,
				"home":              nil,
				"readme":            nil,
				"changelog":         nil,
				"last_update":       "2026-01-01 00:00:00",
				"app_metadata": map[string]interface{}{
					"name":        name,
					"train":       "community",
					"version":     version,
					"app_version": version,
					"title":       "Example App",
					"description": "A catalog app",
					"home":        "https://example.invalid/app",
				},
				"schema": schema,
				"values": map[string]interface{}{},
			},
		},
	}
}

// catalogSchemaQuestion is one questions.yaml entry: a top-level variable
// whose schema is a dict of attrs, each with a typed default, as in the
// official apps run_as and resources groups.
func catalogSchemaQuestion(variable, group string, attrs []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"variable": variable,
		"label":    "",
		"group":    group,
		"schema": map[string]interface{}{
			"type":  "dict",
			"attrs": attrs,
		},
	}
}

func catalogIntAttr(variable, label string, defaultVal int) map[string]interface{} {
	return map[string]interface{}{
		"variable": variable,
		"label":    label,
		"schema": map[string]interface{}{
			"type":     "int",
			"default":  defaultVal,
			"required": true,
			"min":      0,
		},
	}
}

// catalogRunAsResourcesSchema is the 25.10 questions.yaml shape for the
// User and Group and Resources groups, with caller-supplied defaults.
func catalogRunAsResourcesSchema(user, group, cpus, memory int) map[string]interface{} {
	return map[string]interface{}{
		"groups": []interface{}{
			map[string]interface{}{"name": "User and Group Configuration", "description": "Configure User and Group"},
			map[string]interface{}{"name": "Resources Configuration", "description": "Configure Resources"},
		},
		"questions": []interface{}{
			catalogSchemaQuestion("run_as", "User and Group Configuration", []interface{}{
				catalogIntAttr("user", "User ID", user),
				catalogIntAttr("group", "Group ID", group),
			}),
			catalogSchemaQuestion("resources", "Resources Configuration", []interface{}{
				map[string]interface{}{
					"variable": "limits",
					"label":    "Limits",
					"schema": map[string]interface{}{
						"type": "dict",
						"attrs": []interface{}{
							catalogIntAttr("cpus", "CPUs", cpus),
							catalogIntAttr("memory", "Memory (in MB)", memory),
						},
					},
				},
			}),
		},
	}
}

func catalogSchemaWithoutRunAsOrResources() map[string]interface{} {
	return map[string]interface{}{
		"groups": []interface{}{
			map[string]interface{}{"name": "App Configuration", "description": "Configure the app"},
		},
		"questions": []interface{}{
			map[string]interface{}{
				"variable": "TZ",
				"label":    "Timezone",
				"group":    "App Configuration",
				"schema": map[string]interface{}{
					"type":     "string",
					"default":  "Etc/UTC",
					"required": true,
				},
			},
		},
	}
}

func callGetAppCatalogDetails(t *testing.T, details map[string]interface{}) string {
	t.Helper()
	fake := &fakeMiddleware{results: map[string]interface{}{
		"catalog.get_app_details": details,
	}}
	client, _ := newFakeClientFrom(t, fake)
	mgr := tasks.NewManager(client, tasks.PollerConfig{PollInterval: time.Second, CleanupInterval: time.Minute})
	defer mgr.Shutdown()
	name, _ := details["name"].(string)
	out, err := NewRegistry(client, mgr).CallTool(t.Context(), "get_app_catalog_details", map[string]interface{}{
		"app_name": name,
		"train":    "community",
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertNoClientWizardOrFixedDefaults(t *testing.T, out string) {
	t.Helper()
	for _, bad := range []string{"AskUserQuestion", "568", "4096", "2 CPUs"} {
		if strings.Contains(out, bad) {
			t.Errorf("get_app_catalog_details output contains %q", bad)
		}
	}
}

func defaultsMap(t *testing.T, out string) map[string]interface{} {
	t.Helper()
	resp := decode(t, out)
	defaults, ok := resp["defaults"].(map[string]interface{})
	if !ok {
		t.Fatalf("defaults missing or not an object: %s", out)
	}
	return defaults
}

func wantNum(t *testing.T, m map[string]interface{}, key string, want float64) {
	t.Helper()
	got, ok := m[key].(float64)
	if !ok || got != want {
		t.Errorf("%s = %v (%T), want %v", key, m[key], m[key], want)
	}
}

// A schema with no run-as or resource defaults must not grow client-specific
// instructions or the old fixed 568 / 2 CPUs / 4096 MB values.
func TestGetAppCatalogDetailsOmitsClientWizardAndFixedDefaults(t *testing.T) {
	out := callGetAppCatalogDetails(t, catalogAppDetails2510("sampleapp", "1.2.0", catalogSchemaWithoutRunAsOrResources()))
	assertNoClientWizardOrFixedDefaults(t, out)
	resp := decode(t, out)
	if _, ok := resp["wizard_guidance"]; ok {
		t.Errorf("wizard_guidance still present: %v", resp["wizard_guidance"])
	}
	if _, ok := resp["defaults"]; ok {
		t.Errorf("defaults present for a schema with no run-as or resource defaults: %v", resp["defaults"])
	}
}

// Defaults taken from the fixture schema appear; the old fixed values do not.
func TestGetAppCatalogDetailsUsesSchemaDefaults(t *testing.T) {
	out := callGetAppCatalogDetails(t, catalogAppDetails2510("sampleapp", "1.2.0", catalogRunAsResourcesSchema(1000, 2000, 8, 2048)))
	assertNoClientWizardOrFixedDefaults(t, out)
	if _, ok := decode(t, out)["wizard_guidance"]; ok {
		t.Error("wizard_guidance still present")
	}
	defaults := defaultsMap(t, out)
	runAs, _ := defaults["run_as"].(map[string]interface{})
	if runAs == nil {
		t.Fatalf("defaults.run_as missing: %v", defaults)
	}
	wantNum(t, runAs, "user", 1000)
	wantNum(t, runAs, "group", 2000)
	resources, _ := defaults["resources"].(map[string]interface{})
	if resources == nil {
		t.Fatalf("defaults.resources missing: %v", defaults)
	}
	limits, _ := resources["limits"].(map[string]interface{})
	if limits == nil {
		t.Fatalf("defaults.resources.limits missing: %v", resources)
	}
	wantNum(t, limits, "cpus", 8)
	wantNum(t, limits, "memory", 2048)
}

// When the schema uses the common official-app values (568 and 4096), those
// numbers may appear because they came from the fixture, but the output still
// must not name a client-specific tool or the "2 CPUs" phrase.
func TestGetAppCatalogDetailsReportsSchemaDefaultsWhenTheyMatchOldFixedValues(t *testing.T) {
	out := callGetAppCatalogDetails(t, catalogAppDetails2510("sampleapp", "1.2.0", catalogRunAsResourcesSchema(568, 568, 2, 4096)))
	if strings.Contains(out, "AskUserQuestion") || strings.Contains(out, "2 CPUs") {
		t.Errorf("output still has client-specific or hard-coded wording: %s", out)
	}
	if _, ok := decode(t, out)["wizard_guidance"]; ok {
		t.Error("wizard_guidance still present")
	}
	defaults := defaultsMap(t, out)
	runAs, _ := defaults["run_as"].(map[string]interface{})
	resources, _ := defaults["resources"].(map[string]interface{})
	if runAs == nil || resources == nil {
		t.Fatalf("defaults incomplete: %v", defaults)
	}
	limits, _ := resources["limits"].(map[string]interface{})
	if limits == nil {
		t.Fatalf("defaults.resources.limits missing: %v", resources)
	}
	wantNum(t, runAs, "user", 568)
	wantNum(t, runAs, "group", 568)
	wantNum(t, limits, "cpus", 2)
	wantNum(t, limits, "memory", 4096)
}

// Guidance lives in the tool descriptions, worded so any MCP client can
// follow it, with no client-specific tool names and no fixed run-as or
// resource defaults.
func TestAppCatalogToolDescriptionsCarryClientNeutralGuidance(t *testing.T) {
	r := NewRegistry(nil, nil)
	for _, name := range []string{"get_app_catalog_details", "install_app"} {
		tool, ok := r.tools[name]
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		desc := tool.Definition.Description
		if desc == "" {
			t.Errorf("%s has an empty description", name)
			continue
		}
		for _, bad := range []string{"AskUserQuestion", "wizard_guidance", "568", "4096", "2 CPUs"} {
			if strings.Contains(desc, bad) {
				t.Errorf("%s description contains %q", name, bad)
			}
		}
	}
	details := r.tools["get_app_catalog_details"].Definition.Description
	install := r.tools["install_app"].Definition.Description
	for _, want := range []string{"query_pools", "create_dataset", "host_path"} {
		if !strings.Contains(details, want) && !strings.Contains(install, want) {
			t.Errorf("neither tool description mentions %q", want)
		}
	}
	if !strings.Contains(details, "schema") || !strings.Contains(install, "schema") {
		t.Error("tool descriptions should point at the app schema for configuration")
	}
	if !strings.Contains(strings.ToLower(details+install), "default") {
		t.Error("tool descriptions should say to use schema defaults when present")
	}
}

// A camelCase runAs question and a schema that only sets run-as still
// produce defaults under the schema's own variable name, with resources
// omitted.
func TestExtractSchemaDefaultsUsesSchemaVariableNamesAndOmitsMissing(t *testing.T) {
	schema := map[string]interface{}{
		"questions": []interface{}{
			catalogSchemaQuestion("runAs", "User and Group Configuration", []interface{}{
				catalogIntAttr("user", "User ID", 1000),
			}),
		},
	}
	got := extractSchemaDefaults(schema)
	if got == nil {
		t.Fatal("extractSchemaDefaults returned nil")
	}
	if _, ok := got["resources"]; ok {
		t.Errorf("resources present when the schema has none: %v", got)
	}
	runAs, _ := got["runAs"].(map[string]interface{})
	if runAs == nil {
		t.Fatalf("runAs missing: %v", got)
	}
	if runAs["user"] != 1000 {
		t.Errorf("runAs.user = %v, want 1000", runAs["user"])
	}
	if _, ok := runAs["group"]; ok {
		t.Errorf("group present when the schema has no group default: %v", runAs)
	}

	if extractSchemaDefaults(catalogSchemaWithoutRunAsOrResources()) != nil {
		t.Error("schema without run-as or resources should yield no defaults")
	}
	if extractSchemaDefaults(nil) != nil {
		t.Error("nil schema should yield no defaults")
	}
}
