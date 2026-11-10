package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- 1. validateSharePath rejects .. and paths that escape /mnt/<pool> ----

func TestNitsValidateSharePathRejectsDotDotAndPoolEscape(t *testing.T) {
	for _, path := range []string{
		"/mnt/tank/../tank2/share",
		"/mnt/../etc/passwd",
		"/mnt/tank/../../etc",
		"/mnt/tank/foo/../../../outside",
	} {
		if err := validateSharePath(path); err == nil {
			t.Errorf("validateSharePath(%q) = nil, want an error", path)
		}
	}
	// After cleaning, this stays under /mnt/tank.
	if err := validateSharePath("/mnt/tank/foo/../bar"); err != nil {
		t.Errorf("validateSharePath(cleaned stay-in-pool) = %v, want nil", err)
	}

	// Both share tools use the same helper.
	if _, err := buildCreateSMBSharePayload(map[string]interface{}{"name": "media", "path": "/mnt/tank/../other/share"}); err == nil {
		t.Error("create_smb_share accepted a path that escapes /mnt/tank")
	}
	if _, err := buildCreateNFSSharePayload(map[string]interface{}{"path": "/mnt/tank/../other/share"}); err == nil {
		t.Error("create_nfs_share accepted a path that escapes /mnt/tank")
	}
}

// ---- 2. catalog defaults: nested resources, numeric only, run_as_custom_user ----

func TestNitsCatalogDefaultsNestResourcesAndHonorRunAsCondition(t *testing.T) {
	out := callGetAppCatalogDetails(t, catalogAppDetails2510("sampleapp", "1.2.0", catalogRunAsResourcesSchema(1000, 2000, 8, 2048)))
	defaults := defaultsMap(t, out)
	resources, _ := defaults["resources"].(map[string]interface{})
	if resources == nil {
		t.Fatalf("defaults.resources missing: %v", defaults)
	}
	if _, ok := resources["cpus"]; ok {
		t.Errorf("resources is flattened; cpus should live under limits: %v", resources)
	}
	limits, _ := resources["limits"].(map[string]interface{})
	if limits == nil {
		t.Fatalf("defaults.resources.limits missing: %v", resources)
	}
	wantNum(t, limits, "cpus", 8)
	wantNum(t, limits, "memory", 2048)

	desc := NewRegistry(nil, nil).tools["get_app_catalog_details"].Definition.Description
	for _, want := range []string{"resources.limits", "install_app"} {
		if !strings.Contains(desc, want) {
			t.Errorf("get_app_catalog_details description should say where defaults go (want %q): %s", want, desc)
		}
	}

	// Numeric defaults only: a string default is dropped.
	schema := map[string]interface{}{
		"questions": []interface{}{
			catalogSchemaQuestion("run_as", "User and Group Configuration", []interface{}{
				map[string]interface{}{
					"variable": "user",
					"label":    "User ID",
					"schema":   map[string]interface{}{"type": "string", "default": "1000"},
				},
				catalogIntAttr("group", "Group ID", 2000),
			}),
		},
	}
	got := extractSchemaDefaults(schema)
	runAs, _ := got["run_as"].(map[string]interface{})
	if runAs == nil {
		t.Fatal("run_as missing when group has a numeric default")
	}
	if _, ok := runAs["user"]; ok {
		t.Errorf("string user default was passed through: %v", runAs)
	}
	if runAs["group"] != 2000 {
		t.Errorf("group = %v, want 2000", runAs["group"])
	}

	// ix-app: run_as applies only when run_as_custom_user is true.
	withFlag := catalogRunAsResourcesSchema(1000, 2000, 8, 2048)
	qs := withFlag["questions"].([]interface{})
	withFlag["questions"] = append([]interface{}{
		map[string]interface{}{
			"variable": "run_as_custom_user",
			"label":    "Custom User",
			"schema":   map[string]interface{}{"type": "boolean", "default": false},
		},
	}, qs...)
	got = extractSchemaDefaults(withFlag)
	if _, ok := got["run_as"]; ok {
		t.Errorf("run_as present when run_as_custom_user defaults to false: %v", got)
	}
	if got["resources"] == nil {
		t.Errorf("resources missing when only run_as should be omitted: %v", got)
	}

	withFlagTrue := catalogRunAsResourcesSchema(1000, 2000, 8, 2048)
	qs = withFlagTrue["questions"].([]interface{})
	withFlagTrue["questions"] = append([]interface{}{
		map[string]interface{}{
			"variable": "run_as_custom_user",
			"label":    "Custom User",
			"schema":   map[string]interface{}{"type": "boolean", "default": true},
		},
	}, qs...)
	got = extractSchemaDefaults(withFlagTrue)
	if _, ok := got["run_as"]; !ok {
		t.Errorf("run_as omitted when run_as_custom_user defaults to true: %v", got)
	}

	if !strings.Contains(desc, "run_as_custom_user") {
		t.Errorf("description should mention the run_as_custom_user condition: %s", desc)
	}
}

// ---- 3. zfsPropValue: scalars pass through, composites are dropped ----

func TestNitsZfsPropValueDropsComposites(t *testing.T) {
	if got := zfsPropValue("off"); got != "off" {
		t.Errorf("scalar string = %#v, want \"off\"", got)
	}
	if got := zfsPropValue(float64(1)); got != float64(1) {
		t.Errorf("scalar number = %#v, want 1", got)
	}
	if got := zfsPropValue([]interface{}{"a"}); got != nil {
		t.Errorf("array = %#v, want nil", got)
	}
	if got := zfsPropValue(map[string]interface{}{"parsed": []interface{}{"a"}}); got != nil {
		t.Errorf("prop object with array parsed = %#v, want nil", got)
	}
	if got := zfsPropValue(map[string]interface{}{"parsed": "off"}); got != "off" {
		t.Errorf("prop object with scalar parsed = %#v, want \"off\"", got)
	}
}

// ---- 4. install_app job-id error does not interpolate the raw result ----

func TestNitsInstallAppJobIDErrorIsGeneric(t *testing.T) {
	const secret = "nits-secret-payload"
	r, _ := issue111iInstallRegistry(t, map[string]interface{}{
		"app.create": map[string]interface{}{"secret": secret},
	}, false, nil)
	_, err := r.CallTool(t.Context(), "install_app", issue111iInstallArgs())
	if err == nil {
		t.Fatal("install_app succeeded with a non-integer job id")
	}
	if err.Error() != "missing or invalid job id" {
		t.Errorf("error = %q, want %q", err.Error(), "missing or invalid job id")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "app.create returned") {
		t.Errorf("error interpolates the raw result: %v", err)
	}
}

// ---- 5. README wording: bare int; tools also accept an array ----

func TestNitsReadmeJobIDWording(t *testing.T) {
	text := readmeFile(t)
	if !strings.Contains(text, "bare") || !strings.Contains(strings.ToLower(text), "integer") {
		t.Error("README should say job methods return a bare integer")
	}
	if !strings.Contains(strings.ToLower(text), "also accept") && !strings.Contains(strings.ToLower(text), "also accepts") {
		t.Error("README should say the tools also accept an array")
	}
}

func readmeFile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
