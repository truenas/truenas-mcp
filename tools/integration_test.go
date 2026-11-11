//go:build integration

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/truenas/truenas-mcp/truenas"
)

func envTruthy(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func integrationClient(t *testing.T) *truenas.Client {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("TRUENAS_URL"))
	key := os.Getenv("TRUENAS_API_KEY")
	user := strings.TrimSpace(os.Getenv("TRUENAS_API_USER"))
	if url == "" || key == "" || user == "" {
		t.Skip("integration tests require TRUENAS_URL, TRUENAS_API_KEY, and TRUENAS_API_USER")
	}
	tlsCfg, err := truenas.NewTLSConfig(envTruthy("TRUENAS_INSECURE"), os.Getenv("TRUENAS_TLS_CA"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := truenas.NewClientWithOptions(url, key, tlsCfg, truenas.Options{
		APIUser: user,
		Proxy:   os.Getenv("TRUENAS_PROXY"),
	})
	if err != nil {
		t.Fatalf("connect to TrueNAS: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func integrationRegistry(t *testing.T) (*Registry, *truenas.Client) {
	t.Helper()
	client := integrationClient(t)
	return NewRegistry(client, nil), client
}

func dumpToolResult(t *testing.T, name, out string, callErr error) {
	t.Helper()
	dir := strings.TrimSpace(os.Getenv("TRUENAS_INTEGRATION_DUMP"))
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("dump dir: %v", err)
	}
	payload := map[string]interface{}{"tool": name, "output": out}
	if callErr != nil {
		payload["error"] = callErr.Error()
	}
	// The raw output may itself be JSON; keep it as a string so a parse
	// failure does not hide the text.
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("dump %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), append(data, '\n'), 0o600); err != nil {
		t.Fatalf("dump %s: %v", name, err)
	}
}

func requiredFields(schema map[string]interface{}) []string {
	switch req := schema["required"].(type) {
	case []string:
		return req
	case []interface{}:
		out := make([]string, 0, len(req))
		for _, v := range req {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

type integrationInventory struct {
	pool        string
	app         string
	catalogApp  string
	alertUUID   string
	bootID      string
	scrubID     float64
	hasScrubID  bool
	taskID      string
	datasetPath string
}

func discoverInventory(t *testing.T, r *Registry) integrationInventory {
	t.Helper()
	inv := integrationInventory{
		pool:       integrationPoolName(),
		app:        integrationAppName(),
		catalogApp: strings.TrimSpace(os.Getenv("TRUENAS_INTEGRATION_CATALOG_APP")),
	}
	if out, err := r.CallTool(t.Context(), "query_pools", map[string]interface{}{}); err == nil {
		var pools []map[string]interface{}
		// Read and dry-run tools may use the first pool when the env is
		// unset. The scratch write cycle never does; it requires the
		// pool to be named explicitly.
		if json.Unmarshal([]byte(out), &pools) == nil && inv.pool == "" && !integrationWritesEnabled() {
			for _, p := range pools {
				if name, _ := p["name"].(string); name != "" {
					inv.pool = name
					break
				}
			}
		}
	}
	if out, err := r.CallTool(t.Context(), "query_apps", map[string]interface{}{}); err == nil {
		resp := map[string]interface{}{}
		if json.Unmarshal([]byte(out), &resp) == nil {
			for _, key := range []string{"apps", "installed"} {
				list, _ := resp[key].([]interface{})
				for _, raw := range list {
					app, _ := raw.(map[string]interface{})
					if name, _ := app["name"].(string); name != "" && inv.app == "" && os.Getenv("TRUENAS_INTEGRATION_APP") == "" {
						// Do not auto-pick an installed app for write/delete.
						// Read tools may still use it when the env is unset:
						inv.app = name
						break
					}
				}
			}
		}
		// query_apps may return a bare list.
		if inv.app == "" {
			var list []map[string]interface{}
			if json.Unmarshal([]byte(out), &list) == nil {
				for _, app := range list {
					if name, _ := app["name"].(string); name != "" {
						inv.app = name
						break
					}
				}
			}
		}
	}
	if out, err := r.CallTool(t.Context(), "search_app_catalog", map[string]interface{}{}); err == nil && inv.catalogApp == "" {
		resp := map[string]interface{}{}
		_ = json.Unmarshal([]byte(out), &resp)
		for _, key := range []string{"apps", "results"} {
			list, _ := resp[key].([]interface{})
			for _, raw := range list {
				app, _ := raw.(map[string]interface{})
				if name, _ := app["name"].(string); name != "" {
					inv.catalogApp = name
					break
				}
			}
			if inv.catalogApp != "" {
				break
			}
		}
	}
	if inv.catalogApp == "" {
		inv.catalogApp = "syncthing"
	}
	if out, err := r.CallTool(t.Context(), "list_alerts", map[string]interface{}{}); err == nil {
		resp := map[string]interface{}{}
		if json.Unmarshal([]byte(out), &resp) == nil {
			list, _ := resp["alerts"].([]interface{})
			for _, raw := range list {
				a, _ := raw.(map[string]interface{})
				if id, _ := a["uuid"].(string); id != "" {
					inv.alertUUID = id
					break
				}
				if id, _ := a["id"].(string); id != "" {
					inv.alertUUID = id
					break
				}
			}
		}
	}
	if out, err := r.CallTool(t.Context(), "query_boot_environments", map[string]interface{}{}); err == nil {
		resp := map[string]interface{}{}
		if json.Unmarshal([]byte(out), &resp) == nil {
			list, _ := resp["boot_environments"].([]interface{})
			for _, raw := range list {
				env, _ := raw.(map[string]interface{})
				if id, _ := env["id"].(string); id != "" {
					inv.bootID = id
					break
				}
			}
		}
	}
	if out, err := r.CallTool(t.Context(), "query_scrub_schedules", map[string]interface{}{}); err == nil {
		var list []map[string]interface{}
		resp := map[string]interface{}{}
		if json.Unmarshal([]byte(out), &list) == nil && len(list) > 0 {
			if id, ok := list[0]["id"].(float64); ok {
				inv.scrubID, inv.hasScrubID = id, true
			}
		} else if json.Unmarshal([]byte(out), &resp) == nil {
			for _, key := range []string{"schedules", "scrub_schedules"} {
				rows, _ := resp[key].([]interface{})
				for _, raw := range rows {
					row, _ := raw.(map[string]interface{})
					if id, ok := row["id"].(float64); ok {
						inv.scrubID, inv.hasScrubID = id, true
						break
					}
				}
			}
		}
	}
	if out, err := r.CallTool(t.Context(), "tasks_list", map[string]interface{}{}); err == nil {
		resp := map[string]interface{}{}
		if json.Unmarshal([]byte(out), &resp) == nil {
			list, _ := resp["tasks"].([]interface{})
			for _, raw := range list {
				task, _ := raw.(map[string]interface{})
				if id, _ := task["id"].(string); id != "" {
					inv.taskID = id
					break
				}
			}
		}
	}
	if inv.pool != "" {
		inv.datasetPath = inv.pool + "/mcp-it-" + time.Now().UTC().Format("20060102-150405")
	}
	return inv
}

func argsForTool(name string, required []string, inv integrationInventory, dryRun bool) (map[string]interface{}, string) {
	args := map[string]interface{}{}
	if dryRun {
		args["dry_run"] = true
	}
	var missing []string
	for _, field := range required {
		switch field {
		case "app_name":
			if inv.app != "" {
				args["app_name"] = inv.app
			} else if name == "install_app" || name == "get_app_catalog_details" {
				args["app_name"] = "mcp-it-app"
			} else {
				missing = append(missing, field)
			}
		case "catalog_app":
			args["catalog_app"] = inv.catalogApp
		case "values":
			args["values"] = map[string]interface{}{}
		case "name":
			if name == "create_dataset" && inv.datasetPath != "" {
				args["name"] = inv.datasetPath
			} else if name == "create_smb_share" {
				args["name"] = "mcp-it-share"
			} else {
				missing = append(missing, field)
			}
		case "path":
			if inv.datasetPath != "" {
				args["path"] = "/mnt/" + inv.datasetPath
			} else if inv.pool != "" {
				args["path"] = "/mnt/" + inv.pool + "/mcp-it-share"
			} else {
				missing = append(missing, field)
			}
		case "pool":
			if inv.pool != "" {
				args["pool"] = inv.pool
			} else {
				missing = append(missing, field)
			}
		case "schedule":
			args["schedule"] = map[string]interface{}{"minute": "0", "hour": "0", "dom": "*", "month": "*", "dow": "*"}
		case "uuid":
			if inv.alertUUID != "" {
				args["uuid"] = inv.alertUUID
			} else {
				args["uuid"] = "00000000-0000-0000-0000-000000000000"
			}
		case "id":
			switch name {
			case "delete_boot_environment":
				if inv.bootID != "" {
					args["id"] = inv.bootID
				} else {
					args["id"] = "mcp-it-missing-bootenv"
				}
			case "delete_scrub_schedule":
				if inv.hasScrubID {
					args["id"] = inv.scrubID
				} else {
					args["id"] = 0
				}
			default:
				missing = append(missing, field)
			}
		case "task_id":
			if inv.taskID != "" {
				args["task_id"] = inv.taskID
			} else {
				missing = append(missing, field)
			}
		case "enable":
			args["enable"] = false
		case "credential":
			args["credential"] = map[string]interface{}{
				"credential_type": "KERBEROS_USER",
				"username":        "mcp-it-user",
				"password":        "mcp-it-not-a-real-password",
			}
		default:
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		return args, "missing " + strings.Join(missing, ", ")
	}
	return args, ""
}

func TestIntegrationReadTools(t *testing.T) {
	r, _ := integrationRegistry(t)
	inv := discoverInventory(t, r)
	var missing []string
	for _, def := range r.ListTools() {
		if safetyOf(def.Name).Access != Read {
			continue
		}
		args, reason := argsForTool(def.Name, requiredFields(def.InputSchema), inv, false)
		t.Run(def.Name, func(t *testing.T) {
			if reason != "" {
				t.Skip(reason)
			}
			out, err := r.CallTool(t.Context(), def.Name, args)
			dumpToolResult(t, def.Name, out, err)
			if err != nil {
				// A handler error (no data, unknown id) still means the
				// tool ran against the live middleware.
				t.Logf("%s: %v", def.Name, err)
			}
		})
		if reason != "" {
			missing = append(missing, def.Name+": "+reason)
		}
	}
	if len(missing) > 0 {
		t.Logf("skipped read tools: %s", strings.Join(missing, "; "))
	}
}

func TestIntegrationWriteToolDryRuns(t *testing.T) {
	r, _ := integrationRegistry(t)
	inv := discoverInventory(t, r)
	var missing []string
	for _, def := range r.ListTools() {
		if safetyOf(def.Name).Access == Read {
			continue
		}
		args, reason := argsForTool(def.Name, requiredFields(def.InputSchema), inv, true)
		t.Run(def.Name, func(t *testing.T) {
			if reason != "" {
				t.Skip(reason)
			}
			if args["dry_run"] != true {
				t.Fatal("dry-run pass must set dry_run=true")
			}
			out, err := r.CallTool(t.Context(), def.Name, args)
			dumpToolResult(t, def.Name+"-dry-run", out, err)
			body := toolResultBody(out, err)
			if body == "" {
				if err != nil {
					// A handler error (unknown id, cannot proceed) still
					// means the tool ran; there is no structured preview
					// to check.
					t.Logf("%s dry-run: %v", def.Name, err)
					return
				}
				t.Fatal("dry-run returned no result")
			}
			if err := dryRunResultError(body); err != nil {
				t.Errorf("%s: %v\n%s", def.Name, err, body)
			}
		})
		if reason != "" {
			missing = append(missing, def.Name+": "+reason)
		}
	}
	if len(missing) > 0 {
		t.Logf("skipped write dry-runs: %s", strings.Join(missing, "; "))
	}
}

func TestIntegrationScratchWriteCycle(t *testing.T) {
	if !integrationWritesEnabled() {
		t.Skip("scratch write cycle requires TRUENAS_INTEGRATION_WRITES=1")
	}
	r, client := integrationRegistry(t)
	pool, skip := scratchWritePool()
	if skip != "" {
		t.Skip(skip)
	}

	name := pool + "/mcp-it-" + time.Now().UTC().Format("20060102-150405")
	t.Logf("scratch dataset %s", name)
	out, err := r.CallTool(t.Context(), "create_dataset", map[string]interface{}{"name": name, "dry_run": true})
	dumpToolResult(t, "create_dataset-scratch-dry-run", out, err)
	if err != nil {
		t.Fatalf("create_dataset dry-run: %v\n%s", err, out)
	}
	if derr := dryRunResultError(toolResultBody(out, err)); derr != nil {
		t.Fatalf("create_dataset dry-run: %v\n%s", derr, out)
	}
	out, err = r.CallTool(t.Context(), "create_dataset", map[string]interface{}{"name": name})
	dumpToolResult(t, "create_dataset-scratch", out, err)
	if err != nil {
		t.Fatalf("create_dataset: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		// t.Context() is already canceled when Cleanup runs.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := client.Call(ctx, "pool.dataset.delete", name, map[string]interface{}{"recursive": true}); err != nil {
			t.Errorf("cleanup pool.dataset.delete %s: %v", name, err)
		}
	})

	envApp := integrationAppName()
	if envApp == "" {
		t.Log("skipping app cycle; set TRUENAS_INTEGRATION_APP to stop and start a disposable app")
		return
	}

	stopOut, err := r.CallTool(t.Context(), "stop_app", map[string]interface{}{"app_name": envApp})
	dumpToolResult(t, "stop_app-preview", stopOut, err)
	if err != nil {
		t.Fatalf("stop_app preview: %v\n%s", err, stopOut)
	}
	tok := replyToken(t, stopOut)
	if tok == "" {
		t.Fatalf("stop_app preview did not return a confirmation_token:\n%s", stopOut)
	}
	stopOut, err = r.CallTool(t.Context(), "stop_app", map[string]interface{}{"app_name": envApp, "confirmation_token": tok})
	dumpToolResult(t, "stop_app", stopOut, err)
	if err != nil {
		t.Fatalf("stop_app: %v\n%s", err, stopOut)
	}

	startOut, err := r.CallTool(t.Context(), "start_app", map[string]interface{}{"app_name": envApp})
	dumpToolResult(t, "start_app", startOut, err)
	if err != nil {
		t.Fatalf("start_app: %v\n%s", err, startOut)
	}
}
