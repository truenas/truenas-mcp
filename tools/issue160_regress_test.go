package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationTestsAreOptIn is the default-suite check that the live
// TrueNAS suite is behind the integration build tag, is documented, and
// is not a required make test / CI step.
func TestIntegrationTestsAreOptIn(t *testing.T) {
	src, err := os.ReadFile("integration_test.go")
	if err != nil {
		t.Fatalf("integration_test.go: %v (the live suite must exist behind the integration build tag)", err)
	}
	if !bytes.Contains(src, []byte("//go:build integration")) {
		t.Fatal("integration_test.go must start with //go:build integration so make test does not need a live NAS")
	}

	docs, err := os.ReadFile(filepath.Join("..", "docs", "integration.md"))
	if err != nil {
		t.Fatalf("docs/integration.md: %v", err)
	}
	for _, want := range []string{"TRUENAS_URL", "TRUENAS_API_KEY", "TRUENAS_API_USER", "-tags integration", "make test"} {
		if !bytes.Contains(docs, []byte(want)) {
			t.Errorf("docs/integration.md should mention %q", want)
		}
	}

	mf, err := os.ReadFile(filepath.Join("..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(mf, []byte("\nintegration:")) && !bytes.Contains(mf, []byte("\nintegration :")) {
		t.Fatal("Makefile must have an opt-in integration target")
	}
	if bytes.Contains(mf, []byte("test: integration")) || bytes.Contains(mf, []byte("test: lint integration")) {
		t.Fatal("make test must not depend on the integration target")
	}

	wf, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "build.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wf, []byte("-tags integration")) || bytes.Contains(wf, []byte("make integration")) {
		t.Fatal("CI must not run the live integration suite as a required check")
	}
}

func TestGetCurrentBootEnvironment(t *testing.T) {
	envs := []interface{}{
		map[string]interface{}{
			"id": "25.04.0", "dataset": "boot-pool/ROOT/25.04.0", "active": false, "activated": false,
			"created": map[string]interface{}{"$date": float64(1736038861000)}, "used_bytes": float64(1 << 30),
			"used": "1 GiB", "keep": false, "can_activate": true,
		},
		map[string]interface{}{
			"id": "27.0.0-RC.1", "dataset": "boot-pool/ROOT/27.0.0-RC.1", "active": true, "activated": true,
			"created": map[string]interface{}{"$date": float64(1767225600000)}, "used_bytes": float64(2 << 30),
			"used": "2 GiB", "keep": false, "can_activate": true,
		},
	}
	client, _ := newFakeClientWith(t, map[string]interface{}{"boot.environment.query": envs}, nil)
	out, err := handleGetCurrentBootEnvironment(context.Background(), client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	resp := decode(t, out)
	active, _ := resp["active"].(map[string]interface{})
	activated, _ := resp["activated"].(map[string]interface{})
	if active["id"] != "27.0.0-RC.1" {
		t.Errorf("active = %v, want 27.0.0-RC.1; body=%s", active, out)
	}
	if activated["id"] != "27.0.0-RC.1" {
		t.Errorf("activated = %v, want 27.0.0-RC.1; body=%s", activated, out)
	}
	msg, _ := resp["message"].(string)
	if !strings.Contains(msg, "Active") || !strings.Contains(msg, "Activated") {
		t.Errorf("message = %q, want it to explain active vs activated; body=%s", msg, out)
	}
}

func TestGetDirectoryServiceStatus(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		client, _ := newFakeClientWith(t, map[string]interface{}{
			"directoryservices.status": map[string]interface{}{"type": nil, "status": "DISABLED", "status_msg": nil},
		}, nil)
		out, err := handleGetDirectoryServiceStatus(context.Background(), client, map[string]interface{}{})
		if err != nil {
			t.Fatal(err)
		}
		resp := decode(t, out)
		ds, _ := resp["directory_service"].(map[string]interface{})
		if ds["type"] != "none" {
			t.Errorf("type = %v, want none; body=%s", ds["type"], out)
		}
		msg, _ := resp["message"].(string)
		if !strings.Contains(strings.ToLower(msg), "no directory service") {
			t.Errorf("message = %q, want 'no directory service'; body=%s", msg, out)
		}
	})
	t.Run("healthy AD", func(t *testing.T) {
		client, _ := newFakeClientWith(t, map[string]interface{}{
			"directoryservices.status": map[string]interface{}{"type": "ACTIVEDIRECTORY", "status": "HEALTHY", "status_msg": nil},
		}, nil)
		out, err := handleGetDirectoryServiceStatus(context.Background(), client, map[string]interface{}{})
		if err != nil {
			t.Fatal(err)
		}
		resp := decode(t, out)
		ds, _ := resp["directory_service"].(map[string]interface{})
		if ds["type"] != "ACTIVEDIRECTORY" || ds["enabled"] != true || ds["healthy"] != true {
			t.Errorf("directory_service = %v, want ACTIVEDIRECTORY enabled and healthy; body=%s", ds, out)
		}
		msg, _ := resp["message"].(string)
		if !strings.Contains(msg, "ACTIVEDIRECTORY") || !strings.Contains(msg, "healthy") {
			t.Errorf("message = %q; body=%s", msg, out)
		}
	})
}

func TestListDirectoryCertificatesOmitsPrivateKey(t *testing.T) {
	certs := []interface{}{
		map[string]interface{}{
			"id": 1.0, "name": "truenas_default", "from": "2025-01-01 00:00:00",
			"until": "2026-01-01 00:00:00", "issuer": "CN=truenas",
			"privatekey": "-----BEGIN PRIVATE KEY-----\nINTEGRATION-SECRET-KEY\n-----END PRIVATE KEY-----",
			"key_length": 4096.0,
		},
	}
	client, _ := newFakeClientWith(t, map[string]interface{}{"certificate.query": certs}, nil)
	out, err := handleListDirectoryCertificates(context.Background(), client, map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "INTEGRATION-SECRET-KEY") || strings.Contains(out, "privatekey") {
		t.Errorf("certificate listing leaked a private key: %s", out)
	}
	resp := decode(t, out)
	if resp["count"] != 1.0 {
		t.Errorf("count = %v, want 1; body=%s", resp["count"], out)
	}
	list, _ := resp["certificates"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("certificates = %s", out)
	}
	cert, _ := list[0].(map[string]interface{})
	if cert["name"] != "truenas_default" || cert["id"] != 1.0 {
		t.Errorf("cert = %v, want id=1 name=truenas_default; body=%s", cert, out)
	}
}

func TestGetAppConfig(t *testing.T) {
	if _, err := handleGetAppConfig(context.Background(), nil, map[string]interface{}{}); err == nil || !strings.Contains(err.Error(), "app_name") {
		t.Errorf("missing app_name: err = %v, want an error naming app_name", err)
	}
	cfg := map[string]interface{}{"TZ": "UTC", "storage": map[string]interface{}{"data": map[string]interface{}{"type": "host_path"}}}
	client, _ := newFakeClientWith(t, map[string]interface{}{"app.config": cfg}, nil)
	out, err := handleGetAppConfig(context.Background(), client, map[string]interface{}{"app_name": "plex"})
	if err != nil {
		t.Fatal(err)
	}
	resp := decode(t, out)
	if resp["app_name"] != "plex" {
		t.Errorf("app_name = %v, want plex; body=%s", resp["app_name"], out)
	}
	got, _ := resp["config"].(map[string]interface{})
	if got["TZ"] != "UTC" {
		t.Errorf("config = %v, want TZ=UTC; body=%s", got, out)
	}
}

func TestGetUpsMetrics(t *testing.T) {
	fake := &fakeMiddleware{
		results: map[string]interface{}{
			"reporting.graphs": []interface{}{
				map[string]interface{}{"name": "upsvoltage", "identifiers": []interface{}{"battery", "input"}},
			},
		},
		handle: func(method string, params json.RawMessage) (interface{}, map[string]interface{}, bool) {
			if method != "reporting.get_data" {
				return nil, nil, false
			}
			name := reportingGraphName(t, params)
			switch name {
			case "upscharge":
				return []interface{}{reportingGraph("upscharge", "charge", 80, 81, 82)}, nil, true
			case "upsvoltage":
				return []interface{}{reportingGraph("upsvoltage", "voltage", 12, 12.1, 12.2)}, nil, true
			default:
				return nil, middlewareError("ENOENT", "'"+name+"' is not a valid graph plugin."), true
			}
		},
	}
	client, _ := newFakeClientFrom(t, fake)
	out, err := handleGetUpsMetrics(context.Background(), client, map[string]interface{}{
		"graphs": []interface{}{"upscharge", "upsvoltage"},
		"unit":   "HOUR",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := decode(t, out)
	charge, _ := resp["upscharge"].(map[string]interface{})
	if charge["data_points_total"] != 3.0 {
		t.Errorf("upscharge = %v, want 3 data points; body=%s", charge, out)
	}
	voltage, _ := resp["upsvoltage"].(map[string]interface{})
	if voltage["battery"] == nil || voltage["input"] == nil {
		t.Errorf("upsvoltage = %v, want per-identifier battery and input; body=%s", voltage, out)
	}
}

func TestAnalyzeCapacityCPU(t *testing.T) {
	fake := reportingFake(t, map[string]map[string]interface{}{
		"cpu": reportingGraph("cpu", "cpu", 10, 20, 15, 25, 30),
	}, nil, nil)
	client, _ := newFakeClientFrom(t, fake)
	out, err := handleAnalyzeCapacity(context.Background(), client, map[string]interface{}{
		"metrics":    []interface{}{"cpu"},
		"time_range": "HOUR",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := decode(t, out)
	cpu, _ := resp["cpu"].(map[string]interface{})
	if cpu["metric"] != "CPU" {
		t.Errorf("cpu = %v, want metric=CPU; body=%s", cpu, out)
	}
	if cpu["time_range"] != "HOUR" {
		t.Errorf("time_range = %v, want HOUR; body=%s", cpu["time_range"], out)
	}
	if cpu["capacity_status"] == nil || cpu["current_utilization_pct"] == nil {
		t.Errorf("cpu missing status or current utilization; body=%s", out)
	}
	if _, ok := resp["summary"]; !ok {
		t.Errorf("missing summary; body=%s", out)
	}
}

// get_pool_capacity_details handler tests live with #158 in
// issue158_regress_test.go (tank/tank2 fixture, top-level used/available,
// bounded top-N, and visible query/parse errors). They are not repeated here.
