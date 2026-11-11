package tools

// App lifecycle tools, their remaining handlers, and registration of every
// app tool (query_apps, upgrade_app, start_app, stop_app, get_app_config,
// update_app, search_app_catalog, get_app_catalog_details, install_app,
// delete_app). Catalog, install and delete handlers live in apps.go;
// upgrade helpers live in upgrade_app.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerAppTools() {
	// Query installed apps
	r.tools["query_apps"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_apps",
			Description: "Query installed applications with their status, versions, and available updates. upgrade_available tells whether an upgrade exists; latest_version is the newest catalog version (always set, in catalog form, not comparable to human_version)",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"app_name": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Filter by specific app name",
					},
					"include_config": map[string]interface{}{
						"type":        "boolean",
						"description": "Include app configuration details (default: false)",
						"default":     false,
					},
				},
			},
		},
		Handler: handleQueryApps,
	}

	// Upgrade app
	r.tools["upgrade_app"] = Tool{
		Definition: mcp.Tool{
			Name:        "upgrade_app",
			Description: "Upgrade an application to a newer version. Supports dry-run mode to preview changes. Returns a task ID for tracking progress; the task completes only once the app is observed RUNNING after the upgrade job finishes (otherwise it fails with the app's state; if the app was merely still starting when health_timeout_seconds ran out, the message says the upgrade itself succeeded and the wait can be raised). For a version upgrade TrueNAS stops the app and snapshots its ix-volumes dataset (if it has one) for rollback; custom apps and image-only updates are instead redeployed with updated images, without a stop or any snapshot (the preview says which applies). A job state of SUCCESS only means the upgrade job finished, not that the app is healthy: use tasks_get or query_apps for the app state. Task IDs are kept in this server's memory only; after a server restart, list jobs with query_jobs (method app.upgrade narrows the list) and find the entry whose id matches the returned job_id. The changelog and version details shown come from the middleware's app.upgrade_summary and may be empty. This is a write operation that modifies the system.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"app_name": map[string]interface{}{
						"type":        "string",
						"description": "Name of the application to upgrade",
					},
					"version": map[string]interface{}{
						"type":        "string",
						"description": "Target version to upgrade to (default: 'latest')",
						"default":     "latest",
					},
					"snapshot_hostpaths": map[string]interface{}{
						"type":        "boolean",
						"description": "Also request snapshots of the app's host paths before the upgrade (default: true for safety). This only affects host paths: for a version upgrade TrueNAS stops the app and snapshots its ix-volumes dataset (if it has one) regardless. It is ignored for custom apps and image-only updates, which take no snapshot. It is a request, not a confirmation: only host paths that are ZFS datasets are snapshotted and the upgrade job does not report which snapshots it took.",
						"default":     true,
					},
					"health_timeout_seconds": map[string]interface{}{
						"type":        "integer",
						"description": "After the upgrade job finishes, how long to wait for the app to report RUNNING before the task fails (default: 60; 0 checks once without waiting). Must be an integer from 0 to 600, anything else is rejected. Raise it for slow-starting apps: running out of time while the app is still starting fails the task but does not mean the upgrade failed.",
						"default":     defaultUpgradeHealthTimeoutSeconds,
						"minimum":     0,
						"maximum":     maxUpgradeHealthTimeoutSeconds,
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview changes without executing (default: false)",
						"default":     false,
					},
				},
				"required": []string{"app_name"},
			},
		},
		Handler: r.handleUpgradeAppWithDryRun,
		Plan:    planUpgradeApp,
		Execute: r.executeUpgradeApp,
	}

	// Start app
	r.tools["start_app"] = Tool{
		Definition: mcp.Tool{
			Name:        "start_app",
			Description: "Start a stopped TrueNAS application. Job-based; use tasks_get with returned task_id to track progress. Supports dry_run to preview the action without executing it.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"app_name": map[string]interface{}{
						"type":        "string",
						"description": "Name of the application to start",
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview the action without executing it (default: false)",
						"default":     false,
					},
				},
				"required": []string{"app_name"},
			},
		},
		Handler: r.handleStartAppWithDryRun,
		Plan:    planStartApp,
	}

	// Stop app
	r.tools["stop_app"] = Tool{
		Definition: mcp.Tool{
			Name:        "stop_app",
			Description: "Stop a running TrueNAS application. Job-based; use tasks_get with returned task_id to track progress. Supports dry_run to preview the action without executing it.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"app_name": map[string]interface{}{
						"type":        "string",
						"description": "Name of the application to stop",
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview the action without executing it (default: false)",
						"default":     false,
					},
				},
				"required": []string{"app_name"},
			},
		},
		Handler: r.handleStopAppWithDryRun,
		Plan:    planStopApp,
		Execute: r.executeStopApp,
	}

	// Get app config
	r.tools["get_app_config"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_app_config",
			Description: "Retrieve the current user-specified configuration for an installed app. Returns the values object that can be modified and passed to update_app.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"app_name": map[string]interface{}{
						"type":        "string",
						"description": "Name of the installed application",
					},
				},
				"required": []string{"app_name"},
			},
		},
		Handler: handleGetAppConfig,
	}

	// Update app
	r.tools["update_app"] = Tool{
		Definition: mcp.Tool{
			Name:        "update_app",
			Description: "Update an installed app's configuration. Job-based; use tasks_get with returned task_id to track progress. Use get_app_config first to retrieve current config, then pass modified values. Storage may be omitted to leave it unchanged, including existing ix_volume entries. A partial storage object has omitted slots filled from the current config before it is sent, so they do not revert to schema defaults. When a provided slot keeps the same type and location, omitted top-level keys (such as read_only) and omitted fields of host_path_config, nfs_config, and cifs_config are also filled from the current slot. New storage entries, and type or location changes that are not a same-type NFS or CIFS server/path change, must use host_path with a cleaned path under /mnt/<pool>/ on an existing dataset. Changing the type or location of an existing slot (including an NFS or CIFS server or path) requires allow_storage_change=true; the preview and confirmation name each changed slot and state that data is not migrated. A change to read_only or ACL settings (acl_enable or acl) on a slot whose type and location are unchanged does not need the flag, but is listed as a plan warning with the old and new values. Supports dry_run to preview changes. This is a write operation that modifies the system.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"app_name": map[string]interface{}{
						"type":        "string",
						"description": "Name of the installed application to update",
					},
					"values": map[string]interface{}{
						"type":        "object",
						"description": "Updated configuration values. Use get_app_config to retrieve current values first. Storage may be omitted to leave current storage unchanged. If a partial storage object is sent, omitted slots are filled from the current config before the update is submitted. When a provided slot keeps the same type and location, omitted top-level keys and omitted fields of host_path_config, nfs_config, and cifs_config are also filled from the current slot. New entries, and type or location changes that are not a same-type NFS or CIFS server/path change, must be host_path; unchanged existing entries, including ix_volume, are left as they are.",
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview changes without executing (default: false)",
						"default":     false,
					},
					"allow_storage_change": map[string]interface{}{
						"type":        "boolean",
						"description": "Allow changing the type or location of an existing storage slot, including an NFS or CIFS server or path. Data is not migrated. Required when this update changes an existing slot's type or location. Not required for a read_only or ACL-only change (default: false).",
						"default":     false,
					},
				},
				"required": []string{"app_name", "values"},
			},
		},
		Handler: r.handleUpdateAppWithDryRun,
		Plan:    planUpdateApp,
		Execute: r.executeUpdateApp,
	}

	// Search app catalog
	r.tools["search_app_catalog"] = Tool{
		Definition: mcp.Tool{
			Name:        "search_app_catalog",
			Description: "Search TrueNAS app catalog by name, category, or keyword. Returns available applications from the catalog with their versions, categories, and installation status.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "Search query (case-insensitive literal substring match on the app name, not a regular expression)",
					},
					"train": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"stable", "enterprise", "community", "all"},
						"description": "Filter by catalog train (default: all trains). Each result includes its train.",
						"default":     "all",
					},
					"category": map[string]interface{}{
						"type":        "string",
						"description": "Filter by exact category name (the app's categories list must include this value, e.g. 'media', 'productivity', 'database')",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum results to return (default: 20)",
						"minimum":     1,
						"default":     20,
					},
				},
			},
		},
		Handler: handleSearchAppCatalog,
	}

	// Get app catalog details
	r.tools["get_app_catalog_details"] = Tool{
		Definition: mcp.Tool{
			Name: "get_app_catalog_details",
			Description: `Get detailed information about a specific app from the catalog, including README, screenshots, version info, storage volume hints, and the app schema (groups and questions, with each question's defaults). When the schema defines run-as user/group or resource limits, those defaults are included in a defaults object; they are omitted when the schema has none. The defaults object uses the schema's own nesting (for example defaults.resources.limits.cpus and defaults.resources.limits.memory); put those values under the same keys in install_app values. When the schema has run_as_custom_user, the run_as defaults apply only if that flag is true and are omitted from defaults when its default is false. When the schema has enable_resource_limits, the resources.limits defaults apply only if that flag is true and are omitted from defaults when its default is false.

Before calling install_app: review every schema group, call query_pools and ask the user which pool to use if more than one is available (use the only pool if there is just one), create datasets with create_dataset, and set every storage volume to type host_path under /mnt/<pool>/.... Use the schema's own defaults for run-as and resources when they are present. This result describes the app and its schema; it does not issue client-specific instructions.`,
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"app_name": map[string]interface{}{
						"type":        "string",
						"description": "App name from catalog (from search results)",
					},
					"train": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"stable", "enterprise", "community"},
						"description": "Catalog train (optional). When omitted, the train that has the app is looked up in the catalog; if several do, stable is preferred, then enterprise, then community. The response reports the train used.",
					},
				},
				"required": []string{"app_name"},
			},
		},
		Handler: handleGetAppCatalogDetails,
	}

	// Install app
	r.tools["install_app"] = Tool{
		Definition: mcp.Tool{
			Name: "install_app",
			Description: `Install a TrueNAS application using schema-driven configuration.

**IMPORTANT: ALL TRUENAS APPS ARE COMPLEX**
Every app requires configuration across multiple groups (currently 6, but may vary):
1. App Configuration (timezone, app-specific settings)
2. User and Group Configuration (run_as user/group IDs)
3. Network Configuration (ports and networking)
4. Storage Configuration (volumes and datasets)
5. Labels Configuration (metadata labels)
6. Resources Configuration (CPU, memory, GPU)

**UNIVERSAL WIZARD - SECTION-BY-SECTION CONFIGURATION:**

**STEP 1: Get App Schema**
1. Call get_app_catalog_details(app_name, train)
2. Review schema.groups array (iterate through ALL groups, don't assume count)
3. Check schema.group_count to know how many groups to configure
4. Review schema.questions_by_group (shows questions for each group)
5. Use the schema's own defaults for run-as and resources when they are present; omit them when the schema has none

**STEP 2: Understand Common Patterns**

All apps follow these patterns:

• **Timezone** (Group 1):
  - Variable: TZ
  - Type: enum with 600+ timezones
  - Recommendation: Use "Etc/UTC" or user's timezone

• **User/Group** (Group 2):
  - Variable: run_as
  - Structure: {user: <uid>, group: <gid>}
  - Defaults: take user and group from the app schema when present; omit them when the schema has none

• **Network** (Group 3):
  - Variable: network
  - Ports: {bind_mode: "published", port_number: <port>, host_ips: []}
  - Common ports: web_port, api_port, sync_port, etc.
  - bind_mode: "published" (external) or "exposed" (internal) or "" (none)

• **Storage** (Group 4) - CRITICAL:
  - Variable: storage
  - ALWAYS use: {"type": "host_path", "host_path_config": {"path": "/mnt/...", "acl_enable": false}}
  - NEVER use: {"type": "ix_volume", ...}
  - Common volumes: config, cache, data, transcodes
  - Pattern: /mnt/<pool>/apps/<appname>/<volume>

• **Labels** (Group 5):
  - Variable: labels
  - Structure: [{key: "name", value: "value"}]
  - Usually optional (empty array)

• **Resources** (Group 6):
  - Variable: resources
  - Structure: {limits: {cpus: <schema default>, memory: <schema default>}, gpus: {...}}
  - Defaults: take CPU and memory limits from the app schema when present; omit them when the schema has none

**STEP 3: Plan Storage (CRITICAL - Do This First)**

1. Identify storage volumes from schema:
   - Look in schema.questions_by_group["Storage Configuration"]
   - Find variables like: config, cache, data, transcodes, additional_storage
   - Each has type enum: ["host_path", "ix_volume", ...]

2. Call query_pools() to find available pools

3. Recommend dataset structure:
   - Format: <pool>/apps/<appname>/<volume>
   - Example: tank/apps/jellyfin/config

4. Present plan to user:
   "I'll create the following datasets for Jellyfin:
    - tank/apps/jellyfin/config (10GB)
    - tank/apps/jellyfin/cache (50GB)
    - tank/apps/jellyfin/transcodes (temporary, no dataset needed)"

**STEP 4: Create Datasets**

For each permanent storage volume (not temporary/tmpfs):
1. Call create_dataset with:
   - name: "<pool>/apps/<appname>/<volume>"
   - type: "FILESYSTEM"
   - share_type: "APPS"
   - compression: "LZ4"
   - quota: <size_in_bytes> (optional)
2. Confirm creation
3. Recommended quotas:
   - config: 10GB (10737418240)
   - cache: 50GB (53687091200)
   - data: 1TB+ (varies by app)

**STEP 5: Build Configuration by Group**

Go through each group and build configuration:

**Group 1 - App Configuration:**
{
  "TZ": "Etc/UTC",
  "<appname>": {
    // App-specific settings from schema
    "additional_envs": []
  }
}

**Group 2 - User/Group:**
{
  "run_as": {
    "user":  // from the app schema default, when present
    "group": // from the app schema default, when present
  }
}

**Group 3 - Network:**
{
  "network": {
    "web_port": {
      "bind_mode": "published",
      "port_number": 30013,
      "host_ips": []
    },
    "host_network": false
  }
}

**Group 4 - Storage (CRITICAL):**
{
  "storage": {
    "config": {
      "type": "host_path",
      "host_path_config": {
        "path": "/mnt/tank/apps/jellyfin/config",
        "acl_enable": false
      }
    },
    "cache": {
      "type": "host_path",
      "host_path_config": {
        "path": "/mnt/tank/apps/jellyfin/cache",
        "acl_enable": false
      }
    },
    "transcodes": {
      "type": "temporary"
    },
    "additional_storage": []
  }
}

**Group 5 - Labels:**
{
  "labels": []
}

**Group 6 - Resources:**
{
  "resources": {
    "limits": {
      "cpus":   // from the app schema default, when present
      "memory": // from the app schema default, when present
    },
    "gpus": {}
  }
}

**STEP 6: Assemble Complete Values Object**

Combine all groups into single values object:
{
  "TZ": "Etc/UTC",
  "jellyfin": {...},
  "run_as": {...},
  "network": {...},
  "storage": {...},
  "labels": [...],
  "resources": {...}
}

**STEP 7: Validate Configuration**

1. All storage volumes use type="host_path"
2. All paths start with /mnt/
3. All required groups present
4. Port numbers in valid range (1-65535)
5. User/group IDs are valid (>= 0)

**STEP 8: Dry-Run Preview**

Call install_app with dry_run=true:
install_app(
  app_name="jellyfin",
  catalog_app="jellyfin",
  train="community",
  values={...complete config...},
  dry_run=true
)

Review:
- Datasets exist?
- Configuration valid?
- Warnings or errors?

**STEP 9: Execute Installation**

If dry-run successful, call with dry_run=false:
install_app(
  app_name="jellyfin",
  catalog_app="jellyfin",
  train="community",
  values={...complete config...},
  dry_run=false
)

Returns task_id for tracking progress with tasks_get.

**CRITICAL SAFETY RULES:**
- ALWAYS use "type": "host_path" for storage
- NEVER use "type": "ix_volume"
- ALWAYS create datasets before installation
- ALWAYS validate paths start with /mnt/
- ALWAYS use dry-run before final installation

**ERROR RECOVERY:**
- Missing datasets: Create with create_dataset
- ix_volume detected: Convert to host_path format
- Invalid structure: Review schema and rebuild section
- Validation failed: Check error message for exact location`,
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"app_name": map[string]interface{}{
						"type":        "string",
						"description": "Unique app instance name (lowercase, alphanumeric, hyphens, 1-40 chars). Pattern: ^[a-z]([-a-z0-9]*[a-z0-9])?$",
						"pattern":     "^[a-z]([-a-z0-9]*[a-z0-9])?$",
					},
					"catalog_app": map[string]interface{}{
						"type":        "string",
						"description": "Catalog app name (from search results)",
					},
					"train": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"stable", "enterprise", "community"},
						"description": "Catalog train (optional). When omitted, the train that has the catalog app is looked up; if several do, stable is preferred, then enterprise, then community. The response reports the train used.",
					},
					"version": map[string]interface{}{
						"type":        "string",
						"description": "App version (default: latest)",
						"default":     "latest",
					},
					"values": map[string]interface{}{
						"type":        "object",
						"description": "Complete app configuration assembled from schema groups. Includes TZ, run_as, network, storage (host_path only), labels, and resources. Build this by iterating through schema groups from get_app_catalog_details.",
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview installation without executing (default: false)",
						"default":     false,
					},
				},
				"required": []string{"app_name", "catalog_app", "values"},
			},
		},
		Handler: r.handleInstallAppWithDryRun,
		Plan:    planInstallApp,
	}

	// Delete app
	r.tools["delete_app"] = Tool{
		Definition: mcp.Tool{
			Name:        "delete_app",
			Description: "Remove an installed application. IMPORTANT: Host-path datasets are NOT deleted and must be manually removed after app deletion. Data will be preserved in original locations. Use dry-run mode to preview what will be deleted.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"app_name": map[string]interface{}{
						"type":        "string",
						"description": "Installed app instance name to delete",
					},
					"remove_images": map[string]interface{}{
						"type":        "boolean",
						"description": "Remove container images (default: false)",
						"default":     false,
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview deletion without executing (default: false)",
						"default":     false,
					},
				},
				"required": []string{"app_name"},
			},
		},
		Handler: r.handleDeleteAppWithDryRun,
		Plan:    planDeleteApp,
		Execute: r.executeDeleteApp,
	}
}

func handleQueryApps(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	appName, _ := args["app_name"].(string)
	includeConfig, _ := args["include_config"].(bool)

	// Build query filters and options
	// Initialize as empty array, not nil (API expects [] not null)
	filters := []interface{}{}
	if appName != "" {
		filters = []interface{}{
			[]interface{}{"name", "=", appName},
		}
	}

	options := map[string]interface{}{
		"extra": map[string]interface{}{
			"retrieve_config": includeConfig,
		},
	}

	result, err := client.Call(ctx, "app.query", filters, options)
	if err != nil {
		return "", fmt.Errorf("failed to query apps: %w", err)
	}

	var apps []map[string]interface{}
	if err := json.Unmarshal(result, &apps); err != nil {
		return "", fmt.Errorf("failed to parse app list: %w", err)
	}

	// Simplify the response to show most relevant information
	simplified := make([]map[string]interface{}, 0, len(apps))
	for _, app := range apps {
		summary := map[string]interface{}{
			"name":              app["name"],
			"id":                app["id"],
			"state":             app["state"],
			"version":           app["human_version"],
			"human_version":     app["human_version"],
			"upgrade_available": app["upgrade_available"],
		}

		// latest_version is the newest catalog version of the app. app.query
		// always sets it, also when it equals the installed one, and in the
		// catalog form (e.g. "1.2.0"), not the human_version form
		// ("<app>_<chart>"), so the two cannot be compared directly.
		// upgrade_available is what tells whether an upgrade exists.
		summary["latest_version"] = app["latest_version"]

		// Include portals (web URLs) if available
		if portals, ok := app["portals"].([]interface{}); ok && len(portals) > 0 {
			summary["portals"] = portals
		}

		// Include active workload summary
		if workloads, ok := app["active_workloads"].(map[string]interface{}); ok {
			if containers, ok := workloads["containers"].(float64); ok {
				summary["active_containers"] = int(containers)
			}
		}

		// Include config if requested
		if includeConfig {
			if config, ok := app["config"]; ok {
				summary["config"] = config
			}
		}

		// Include metadata
		if metadata, ok := app["metadata"].(map[string]interface{}); ok {
			summary["app_metadata"] = map[string]interface{}{
				"train":       metadata["train"],
				"description": metadata["description"],
			}
		}

		simplified = append(simplified, summary)
	}

	formatted, err := json.MarshalIndent(simplified, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// planUpgradeApp builds the upgrade_app plan used by both the dry-run
// preview and the real call. The middleware write is a single app.upgrade;
// the preview still describes the stop/snapshot/upgrade/start/verify (or
// redeploy/verify) steps that call performs.
func planUpgradeApp(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	appName, ok := args["app_name"].(string)
	if !ok || appName == "" {
		return nil, fmt.Errorf("app_name is required")
	}

	healthTimeout, err := upgradeHealthTimeout(args)
	if err != nil {
		return nil, err
	}

	version := "latest"
	if v, ok := args["version"].(string); ok && v != "" {
		version = v
	}

	snapshotHostpaths := true
	if s, ok := args["snapshot_hostpaths"].(bool); ok {
		snapshotHostpaths = s
	}

	// Get current app state
	currentResult, err := client.Call(ctx, "app.query", []interface{}{
		[]interface{}{"name", "=", appName},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query app: %w", err)
	}

	var apps []map[string]interface{}
	if err := json.Unmarshal(currentResult, &apps); err != nil {
		return nil, fmt.Errorf("failed to parse app query: %w", err)
	}

	if len(apps) == 0 {
		return nil, fmt.Errorf("app %s not found", appName)
	}
	currentApp := apps[0]

	// Get upgrade summary
	summaryResult, err := client.Call(ctx, "app.upgrade_summary", appName, map[string]interface{}{
		"app_version": version,
	})
	if err != nil {
		if isNoUpgradeAvailable(err) {
			return nil, noUpgradeAvailableError(appName, version, currentApp)
		}
		return nil, fmt.Errorf("failed to get upgrade summary: %w", err)
	}

	// Parse summary - can be either object or array depending on TrueNAS version/app
	var summary interface{}
	if err := json.Unmarshal(summaryResult, &summary); err != nil {
		return nil, fmt.Errorf("failed to parse upgrade summary: %w", err)
	}
	summary = normalizeUpgradeSummary(summary)

	// Build current state
	currentState := map[string]interface{}{
		"name":    currentApp["name"],
		"version": currentApp["human_version"],
		"state":   currentApp["state"],
	}

	verify := PlannedAction{
		Description: fmt.Sprintf("Verify the app reports RUNNING after the job finishes (waits up to %ds); otherwise the task fails with the app's state", int(healthTimeout/time.Second)),
		Operation:   "verify",
		Target:      appName,
		Details: map[string]interface{}{
			"health_timeout_seconds": int(healthTimeout / time.Second),
		},
	}

	var actions []PlannedAction
	var warnings []string
	if imageOnly, reason := upgradeIsImageOnly(currentApp, summary); imageOnly {
		// Middleware pulls the images and redeploys, then returns: no
		// stop, no snapshot, snapshot_hostpaths is ignored, and
		// app.upgrade_summary reports the current version as the target.
		actions = []PlannedAction{
			{
				Description: fmt.Sprintf("Pull updated images and redeploy; no snapshot is taken (%s; the app stays at version %v)", reason, currentApp["human_version"]),
				Operation:   "redeploy",
				Target:      appName,
				Details:     summary,
			},
			verify,
		}
		warnings = append(warnings, fmt.Sprintf("WARNING: no snapshot is taken (%s): TrueNAS pulls updated images and redeploys the app without stopping it or snapshotting it, so there is no rollback point. snapshot_hostpaths has no effect here.", reason))
	} else {
		target := upgradeTargetDescription(version, summary)
		// Middleware stops the app, snapshots its ix-volumes dataset (if
		// it has one) and, when requested, host paths that are ZFS
		// datasets, and then upgrades it.
		actions = []PlannedAction{
			{
				Description: "Stop application containers",
				Operation:   "stop",
				Target:      appName,
			},
			{
				Description: upgradeSnapshotStep(snapshotHostpaths),
				Operation:   "snapshot",
				Target:      appName,
				Details: map[string]interface{}{
					"snapshot_hostpaths": snapshotHostpaths,
					"note":               snapshotNote(snapshotHostpaths),
				},
			},
			{
				Description: fmt.Sprintf("Upgrade from %v to %s", currentApp["human_version"], target),
				Operation:   "upgrade",
				Target:      appName,
				Details:     summary,
			},
			{
				Description: "Start application with new version",
				Operation:   "start",
				Target:      appName,
			},
			verify,
		}
		if !snapshotHostpaths {
			warnings = append(warnings, "WARNING: snapshot_hostpaths is disabled, so no host-path snapshot is requested. TrueNAS still stops the app and snapshots its ix-volumes dataset (if it has one) for rollback; data on host paths is not covered.")
		}
	}
	for i := range actions {
		actions[i].Step = i + 1
	}
	if state, _ := currentApp["state"].(string); state == "STOPPED" {
		warnings = append(warnings, "WARNING: the app is STOPPED and TrueNAS rejects upgrading a stopped app, so this upgrade will fail. Start the app first.")
	}

	upgradeOptions := map[string]interface{}{
		"app_version":        version,
		"snapshot_hostpaths": snapshotHostpaths,
	}

	fromVersion, _ := currentApp["version"].(string)
	if hv, _ := currentApp["human_version"].(string); hv != "" && hv != fromVersion {
		if fromVersion == "" {
			fromVersion = hv
		} else {
			fromVersion = fromVersion + " (" + hv + ")"
		}
	}
	toVersion := upgradeTargetDescription(version, summary)
	upgradeFacts := []string{
		confirmFact("app", appName),
		confirmFact("from version", fromVersion),
		confirmFact("to version", toVersion),
		confirmBoolFact("snapshot_hostpaths", snapshotHostpaths),
	}
	if imageOnly, reason := upgradeIsImageOnly(currentApp, summary); imageOnly {
		upgradeFacts = append(upgradeFacts, "no snapshot is taken ("+reason+")")
	}
	return &writePlan{
		Tool:          "upgrade_app",
		Method:        upgradeAppMethod,
		CallParams:    []interface{}{appName, upgradeOptions},
		JobMethod:     upgradeAppMethod,
		ParamNames:    []string{"app_name", "options"},
		CurrentState:  currentState,
		Actions:       actions,
		Warnings:      warnings,
		EstimatedTime: upgradeEstimatedTime(healthTimeout),
		TargetName:    appName,
		Result: map[string]interface{}{
			"summary":            summary,
			"health_timeout":     healthTimeout,
			"snapshot_hostpaths": snapshotHostpaths,
		},
		Bind:           upgradeBind(summary),
		ConfirmSummary: newConfirmSummary("Upgrade an application", upgradeFacts...),
	}, nil
}

func (r *Registry) handleUpgradeAppWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planUpgradeApp(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeUpgradeApp(ctx, client, args, plan)
}

// upgradeAppDryRun implements dry-run preview for app upgrades. Tests call
// this directly; it uses the same plan as the handler.
type upgradeAppDryRun struct{}

func (u *upgradeAppDryRun) ExecuteDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	plan, err := planUpgradeApp(ctx, client, args)
	if err != nil {
		return nil, err
	}
	return plan.dryRunResult()
}

func (r *Registry) executeUpgradeApp(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		if isNoUpgradeAvailable(err) {
			version := "latest"
			if v, ok := args["version"].(string); ok && v != "" {
				version = v
			}
			return "", noUpgradeAvailableError(plan.TargetName, version, queryAppQuietly(ctx, client, plan.TargetName))
		}
		return "", fmt.Errorf("failed to upgrade app: %w", err)
	}

	// Parse the job ID (app.upgrade may return an array [job_id] or just job_id)
	jobID, err := parseJobID(result)
	if err != nil {
		return "", fmt.Errorf("failed to parse job ID: %w", err)
	}

	healthTimeout, _ := plan.Result["health_timeout"].(time.Duration)
	snapshotHostpaths, _ := plan.Result["snapshot_hostpaths"].(bool)
	summary := plan.Result["summary"]

	opts := []tasks.TaskOption{tasks.WithAppHealthCheck(plan.TargetName, healthTimeout)}
	if plan.JobMethod != "" {
		opts = append(opts, tasks.WithExpectedMethod(plan.JobMethod))
	}

	// Create task to track upgrade progress. The task only completes once the
	// app is seen RUNNING after the job finishes.
	task, err := r.taskManager.CreateJobTask(ctx,
		"upgrade_app",
		args,
		jobID,
		1*time.Hour, // 1 hour TTL
		opts...,
	)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	response := map[string]interface{}{
		"app_name":        plan.TargetName,
		"upgrade_summary": summary,
		"task_id":         task.TaskID,
		"task_status":     task.Status,
		"poll_interval":   task.PollInterval,
		"job_id":          jobID,
		// What was asked for, not what happened: the upgrade job does not
		// report which snapshots, if any, it took.
		"snapshot_requested": snapshotHostpaths,
		"snapshot_note":      snapshotNote(snapshotHostpaths),
		"dry_run":            false,
		"executed":           true,
		"message": fmt.Sprintf("Upgrade started. Track progress with tasks_get using task_id: %s. "+
			"The task completes only after the app is RUNNING (waits up to %ds after the job finishes).",
			task.TaskID, int(healthTimeout/time.Second)),
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const startAppMethod = "app.start"
const stopAppMethod = "app.stop"
const upgradeAppMethod = "app.upgrade"
const updateAppMethod = "app.update"

func queryApp(ctx context.Context, client *truenas.Client, appName string) (map[string]interface{}, error) {
	currentResult, err := client.Call(ctx, "app.query", []interface{}{
		[]interface{}{"name", "=", appName},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query app: %w", err)
	}

	var apps []map[string]interface{}
	if err := json.Unmarshal(currentResult, &apps); err != nil {
		return nil, fmt.Errorf("failed to parse app query result: %w", err)
	}
	if len(apps) == 0 {
		return nil, fmt.Errorf("app not found: %s", appName)
	}
	return apps[0], nil
}

// planStartApp builds the start_app plan used by both the dry-run preview and
// the real call. A RUNNING app is refused, and so are DEPLOYING and STOPPING
// (retry when the app settles). CRASHED stays startable.
func planStartApp(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	appName, ok := args["app_name"].(string)
	if !ok || appName == "" {
		return nil, fmt.Errorf("app_name is required")
	}

	app, err := queryApp(ctx, client, appName)
	if err != nil {
		return nil, err
	}

	stateVal := app["state"]
	state, ok := stateVal.(string)
	if !ok || state == "" {
		return nil, fmt.Errorf("failed to parse app state: expected a string")
	}

	var blocked []string
	switch strings.ToUpper(state) {
	case "RUNNING":
		blocked = append(blocked, fmt.Sprintf("cannot start app %q: it is already RUNNING; nothing was executed", appName))
	case "DEPLOYING", "STOPPING":
		blocked = append(blocked, fmt.Sprintf("cannot start app %q: it is currently %s; retry when it settles; nothing was executed", appName, state))
	}

	var warnings []string
	if len(blocked) == 0 && !strings.EqualFold(state, "STOPPED") {
		warnings = append(warnings, fmt.Sprintf("App is currently in state: %s.", state))
	}

	return &writePlan{
		Tool:        "start_app",
		Method:      startAppMethod,
		CallParams:  []interface{}{appName},
		JobMethod:   startAppMethod,
		Description: fmt.Sprintf("Start application %q", appName),
		Operation:   "start",
		ParamNames:  []string{"app_name"},
		CurrentState: map[string]interface{}{
			"app_name": appName,
			"state":    state,
		},
		Warnings:      warnings,
		Blocked:       blocked,
		EstimatedTime: &EstimatedTime{MinSeconds: 5, MaxSeconds: 120, Note: "Depends on app startup time"},
		TargetName:    appName,
	}, nil
}

func (r *Registry) handleStartAppWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planStartApp(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeStartApp(ctx, client, args, plan)
}

func (r *Registry) executeStartApp(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to start app: %w", err)
	}

	jobID, err := parseJobID(result)
	if err != nil {
		return "", fmt.Errorf("failed to parse job ID: %w", err)
	}

	opts := []tasks.TaskOption{}
	if plan.JobMethod != "" {
		opts = append(opts, tasks.WithExpectedMethod(plan.JobMethod))
	}
	task, err := r.taskManager.CreateJobTask(ctx, "start_app", args, jobID, 10*time.Minute, opts...)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	response := map[string]interface{}{
		"app_name":      plan.TargetName,
		"task_id":       task.TaskID,
		"task_status":   task.Status,
		"poll_interval": task.PollInterval,
		"job_id":        jobID,
		"message":       fmt.Sprintf("App start initiated. Track progress with tasks_get using task_id: %s", task.TaskID),
	}
	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to format response: %w", err)
	}
	return string(formatted), nil
}

func planStopApp(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	appName, ok := args["app_name"].(string)
	if !ok || appName == "" {
		return nil, fmt.Errorf("app_name is required")
	}

	app, err := queryApp(ctx, client, appName)
	if err != nil {
		return nil, err
	}

	stateVal := app["state"]
	state, ok := stateVal.(string)
	if !ok || state == "" {
		return nil, fmt.Errorf("failed to parse app state: expected a string")
	}

	return &writePlan{
		Tool:        "stop_app",
		Method:      stopAppMethod,
		CallParams:  []interface{}{appName},
		JobMethod:   stopAppMethod,
		Description: "Stop application containers",
		Operation:   "stop",
		ParamNames:  []string{"app_name"},
		CurrentState: map[string]interface{}{
			"app_name": appName,
			"state":    state,
		},
		Warnings: []string{
			fmt.Sprintf("App '%s' (currently %s) will become unavailable after stopping.", appName, state),
		},
		EstimatedTime: &EstimatedTime{MinSeconds: 5, MaxSeconds: 60, Note: "Depends on app shutdown time"},
		TargetName:    appName,
		ConfirmSummary: newConfirmSummary("Stop an application",
			confirmFact("app", appName),
			confirmFact("state", state),
		),
	}, nil
}

func (r *Registry) handleStopAppWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planStopApp(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeStopApp(ctx, client, args, plan)
}

func (r *Registry) executeStopApp(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to stop app: %w", err)
	}

	jobID, err := parseJobID(result)
	if err != nil {
		return "", fmt.Errorf("failed to parse job ID: %w", err)
	}

	opts := []tasks.TaskOption{}
	if plan.JobMethod != "" {
		opts = append(opts, tasks.WithExpectedMethod(plan.JobMethod))
	}
	task, err := r.taskManager.CreateJobTask(ctx, "stop_app", args, jobID, 5*time.Minute, opts...)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	response := map[string]interface{}{
		"app_name":      plan.TargetName,
		"task_id":       task.TaskID,
		"task_status":   task.Status,
		"poll_interval": task.PollInterval,
		"job_id":        jobID,
		"message":       fmt.Sprintf("App stop initiated. Track progress with tasks_get using task_id: %s", task.TaskID),
	}
	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to format response: %w", err)
	}
	return string(formatted), nil
}

// handleGetAppConfig retrieves the user-specified config for an app
func handleGetAppConfig(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	appName, ok := args["app_name"].(string)
	if !ok || appName == "" {
		return "", fmt.Errorf("app_name is required")
	}

	result, err := client.Call(ctx, "app.config", appName)
	if err != nil {
		return "", fmt.Errorf("failed to get app config: %w", err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(result, &config); err != nil {
		return "", fmt.Errorf("failed to parse app config: %w", err)
	}

	response := map[string]interface{}{
		"app_name": appName,
		"config":   config,
	}
	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}
	return string(formatted), nil
}

// planUpdateApp builds the update_app plan used by both the dry-run
// preview and the real call. Missing host-path datasets are a preview
// warning; the real call still refuses until they exist.
func planUpdateApp(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	appName, ok := args["app_name"].(string)
	if !ok || appName == "" {
		return nil, fmt.Errorf("app_name is required")
	}

	values, ok := args["values"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("values is required and must be an object")
	}

	allowStorageChange, err := parseAllowStorageChange(args)
	if err != nil {
		return nil, err
	}

	app, err := queryInstalledApp(ctx, client, appName)
	if err != nil {
		return nil, err
	}

	currentConfig, err := fetchAppConfig(ctx, client, appName)
	if err != nil {
		return nil, err
	}

	valuesOut := values
	var datasetPaths, missing, storageWarnings []string
	if !isCustomApp(app) {
		schema, err := schemaFromInstalledApp(ctx, client, app)
		if err != nil {
			return nil, err
		}
		valuesOut, datasetPaths, missing, storageWarnings, err = prepareUpdateAppStorage(ctx, client, schema, values, currentConfig, allowStorageChange)
		if err != nil {
			return nil, fmt.Errorf("storage validation failed: %v", err)
		}
	}

	warnings := []string{
		"App containers will be restarted to apply configuration changes.",
	}
	warnings = append(warnings, storageWarnings...)
	if len(missing) > 0 {
		warnings = append(warnings, "CRITICAL: The following datasets must exist before updating. Use create_dataset tool:")
		for _, ds := range missing {
			warnings = append(warnings, fmt.Sprintf("  - %s", ds))
		}
	}

	return &writePlan{
		Tool:       "update_app",
		Method:     updateAppMethod,
		CallParams: []interface{}{appName, map[string]interface{}{"values": valuesOut}},
		JobMethod:  updateAppMethod,
		ParamNames: []string{"app_name", "options"},
		CurrentState: map[string]interface{}{
			"app_name":         appName,
			"current_config":   currentConfig,
			"missing_datasets": missing,
			"storage_paths":    datasetPaths,
		},
		Actions: []PlannedAction{
			{
				Step:        1,
				Description: "Apply new configuration values to app",
				Operation:   "update",
				Target:      "app.update",
				Details: map[string]interface{}{
					"app_name":      appName,
					"new_values":    valuesOut,
					"storage_paths": datasetPaths,
				},
			},
		},
		Warnings:      warnings,
		EstimatedTime: &EstimatedTime{MinSeconds: 10, MaxSeconds: 300, Note: "Depends on app restart time"},
		TargetName:    appName,
		Result:        map[string]interface{}{"missing": missing},
		ConfirmSummary: newConfirmSummary("Update an application configuration",
			append([]string{confirmFact("app", appName)}, updateAppConfirmFacts(currentConfig, valuesOut)...)...,
		),
	}, nil
}

func (r *Registry) handleUpdateAppWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planUpdateApp(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeUpdateApp(ctx, client, args, plan)
}

func (r *Registry) executeUpdateApp(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if missing, ok := plan.Result["missing"].([]string); ok && len(missing) > 0 {
		return "", datasetsMustExistError(missing, "update")
	}

	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to update app: %w", err)
	}

	jobID, err := parseJobID(result)
	if err != nil {
		return "", fmt.Errorf("failed to parse job ID: %w", err)
	}

	opts := []tasks.TaskOption{}
	if plan.JobMethod != "" {
		opts = append(opts, tasks.WithExpectedMethod(plan.JobMethod))
	}
	task, err := r.taskManager.CreateJobTask(ctx, "update_app", args, jobID, 30*time.Minute, opts...)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	response := map[string]interface{}{
		"app_name":      plan.TargetName,
		"task_id":       task.TaskID,
		"task_status":   task.Status,
		"poll_interval": task.PollInterval,
		"job_id":        jobID,
		"message":       fmt.Sprintf("App update initiated. Track progress with tasks_get using task_id: %s", task.TaskID),
	}
	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to format response: %w", err)
	}
	return string(formatted), nil
}
