package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

// StorageVolume represents a storage volume configuration for app installation
type StorageVolume struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// ============================================================================
// Section 1: Search and Catalog Handlers
// ============================================================================

// handleSearchAppCatalog searches the TrueNAS app catalog
func handleSearchAppCatalog(client *truenas.Client, args map[string]interface{}) (string, error) {
	// Extract parameters
	query := ""
	if q, ok := args["query"].(string); ok {
		query = q
	}

	train := "stable"
	if t, ok := args["train"].(string); ok && t != "" {
		train = t
	}

	category := ""
	if c, ok := args["category"].(string); ok {
		category = c
	}

	limit := 20
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
	}

	// Build filters
	filters := []interface{}{}

	// Add query filter if provided
	if query != "" {
		filters = append(filters, []interface{}{"name", "~", query})
	}

	// Add category filter if provided
	if category != "" {
		filters = append(filters, []interface{}{"categories", "~", category})
	}

	// Build options
	options := map[string]interface{}{
		"limit": limit,
	}

	// Call app.available API
	result, err := client.Call("app.available", filters, options)
	if err != nil {
		return "", fmt.Errorf("failed to search app catalog: %w", err)
	}

	// Parse results
	var apps []interface{}
	if err := json.Unmarshal(result, &apps); err != nil {
		return "", fmt.Errorf("failed to parse search results: %w", err)
	}

	// Filter by train if not "all"
	if train != "all" {
		filtered := []interface{}{}
		for _, app := range apps {
			appMap, ok := app.(map[string]interface{})
			if !ok {
				continue
			}
			if appTrain, ok := appMap["train"].(string); ok && appTrain == train {
				filtered = append(filtered, app)
			}
		}
		apps = filtered
	}

	// Format results
	formatted := formatAppSearchResults(apps)

	return formatted, nil
}

// formatAppSearchResults formats app search results for display
func formatAppSearchResults(apps []interface{}) string {
	if len(apps) == 0 {
		return "No apps found matching the search criteria."
	}

	var results []map[string]interface{}
	for _, app := range apps {
		appMap, ok := app.(map[string]interface{})
		if !ok {
			continue
		}

		result := map[string]interface{}{
			"name":        appMap["name"],
			"title":       appMap["title"],
			"description": appMap["description"],
			"train":       appMap["train"],
			"version":     appMap["latest_version"],
			"installed":   appMap["installed"],
		}

		if categories, ok := appMap["categories"].([]interface{}); ok {
			result["categories"] = categories
		}

		results = append(results, result)
	}

	output, _ := json.MarshalIndent(map[string]interface{}{
		"count": len(results),
		"apps":  results,
		"note":  "Use get_app_catalog_details to view detailed information about a specific app",
	}, "", "  ")

	return string(output)
}

// handleGetAppCatalogDetails retrieves detailed information about a specific app
func handleGetAppCatalogDetails(client *truenas.Client, args map[string]interface{}) (string, error) {
	// Extract parameters
	appName, ok := args["app_name"].(string)
	if !ok || appName == "" {
		return "", fmt.Errorf("app_name is required")
	}

	train := "stable"
	if t, ok := args["train"].(string); ok && t != "" {
		train = t
	}

	// Call catalog.get_app_details API
	result, err := client.Call("catalog.get_app_details", appName, map[string]interface{}{
		"train": train,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get app details: %w", err)
	}

	// Parse result
	var appDetails map[string]interface{}
	if err := json.Unmarshal(result, &appDetails); err != nil {
		return "", fmt.Errorf("failed to parse app details: %w", err)
	}

	// Parse README for storage hints
	storageHints := []string{}
	if readme, ok := appDetails["app_readme"].(string); ok && readme != "" {
		storageHints = parseAppREADMEForStorageHints(readme)
	}

	// Extract full schema for wizard guidance
	schema := extractAppSchema(appDetails)

	// Format output
	formatted := formatAppDetails(appDetails, storageHints, schema)

	return formatted, nil
}

// parseAppREADMEForStorageHints extracts storage volume hints from app README
func parseAppREADMEForStorageHints(readme string) []string {
	hints := []string{}
	readmeLower := strings.ToLower(readme)

	// Common volume patterns to look for
	volumePatterns := []string{
		"config", "data", "media", "backups", "logs",
		"database", "postgres", "mysql", "redis",
		"cache", "temp", "uploads", "downloads",
	}

	for _, pattern := range volumePatterns {
		if strings.Contains(readmeLower, pattern) {
			// Check for context clues that this is storage-related
			if strings.Contains(readmeLower, pattern+" volume") ||
				strings.Contains(readmeLower, pattern+" storage") ||
				strings.Contains(readmeLower, pattern+" path") ||
				strings.Contains(readmeLower, pattern+" directory") {
				hints = append(hints, pattern)
			}
		}
	}

	// Remove duplicates
	seen := make(map[string]bool)
	unique := []string{}
	for _, hint := range hints {
		if !seen[hint] {
			seen[hint] = true
			unique = append(unique, hint)
		}
	}

	return unique
}

// formatAppDetails formats app details for display
func formatAppDetails(details map[string]interface{}, storageHints []string, schema map[string]interface{}) string {
	output := map[string]interface{}{
		"name":           details["name"],
		"title":          details["title"],
		"description":    details["description"],
		"latest_version": details["latest_version"],
		"categories":     details["categories"],
		"maintainers":    details["maintainers"],
	}

	if len(storageHints) > 0 {
		output["storage_hints"] = map[string]interface{}{
			"detected_volumes": storageHints,
			"recommendation":   "Create datasets following pattern: /mnt/<pool>/apps/<appname>/<volume_name>",
		}
	} else {
		output["storage_hints"] = map[string]interface{}{
			"detected_volumes": []string{},
			"recommendation":   "Default: Create /mnt/<pool>/apps/<appname>/data for general storage",
		}
	}

	// Add schema with wizard guidance
	if schema != nil {
		output["schema"] = formatSchemaForWizard(schema)
		output["wizard_guidance"] = generateWizardGuidance(schema)
	}

	// Add README if available (truncated for readability)
	if readme, ok := details["app_readme"].(string); ok && readme != "" {
		if len(readme) > 1000 {
			output["readme_preview"] = readme[:1000] + "... (truncated)"
		} else {
			output["readme_preview"] = readme
		}
	}

	formatted, _ := json.MarshalIndent(output, "", "  ")
	return string(formatted)
}

// extractAppSchema extracts schema from app details
func extractAppSchema(appDetails map[string]interface{}) map[string]interface{} {
	versions, ok := appDetails["versions"].(map[string]interface{})
	if !ok {
		return nil
	}

	latestVersion, ok := appDetails["latest_version"].(string)
	if !ok {
		return nil
	}

	versionData, ok := versions[latestVersion].(map[string]interface{})
	if !ok {
		return nil
	}

	schema, ok := versionData["schema"].(map[string]interface{})
	if !ok {
		return nil
	}

	return schema
}

// formatSchemaForWizard structures schema for LLM consumption
func formatSchemaForWizard(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}

	// Extract ALL groups dynamically (no hardcoding of group count)
	// Works for 6 groups (current) or N groups (future-proof)
	groups := []map[string]interface{}{}
	if groupsArray, ok := schema["groups"].([]interface{}); ok {
		for _, g := range groupsArray {
			if groupMap, ok := g.(map[string]interface{}); ok {
				groups = append(groups, groupMap)
			}
		}
	}

	// Extract questions and organize by group name
	// IMPORTANT: Summarize questions to avoid massive schemas (600+ timezone enums, etc.)
	groupedQuestions := make(map[string][]map[string]interface{})
	if questions, ok := schema["questions"].([]interface{}); ok {
		for _, q := range questions {
			if qMap, ok := q.(map[string]interface{}); ok {
				groupName, _ := qMap["group"].(string)
				// Summarize the question to essential fields only
				summarized := summarizeQuestion(qMap)
				groupedQuestions[groupName] = append(groupedQuestions[groupName], summarized)
			}
		}
	}

	return map[string]interface{}{
		"groups":             groups, // Array of ALL groups (dynamic count)
		"questions_by_group": groupedQuestions,
		"note":               "Configure each group section-by-section. Iterate through ALL groups. ALL storage MUST use type='host_path'.",
		"group_count":        len(groups), // Number of groups for this app
	}
}

// summarizeQuestion extracts essential fields from a question schema
// and summarizes large enums to avoid overwhelming output
func summarizeQuestion(question map[string]interface{}) map[string]interface{} {
	summarized := map[string]interface{}{}

	// Core fields
	if variable, ok := question["variable"].(string); ok {
		summarized["variable"] = variable
	}
	if label, ok := question["label"].(string); ok {
		summarized["label"] = label
	}
	if description, ok := question["description"].(string); ok {
		summarized["description"] = description
	}

	// Schema information (simplified)
	if schemaMap, ok := question["schema"].(map[string]interface{}); ok {
		schemaInfo := map[string]interface{}{}

		// Type
		if typeStr, ok := schemaMap["type"].(string); ok {
			schemaInfo["type"] = typeStr
		}

		// Required
		if required, ok := schemaMap["required"].(bool); ok {
			schemaInfo["required"] = required
		}

		// Default
		if defaultVal, ok := schemaMap["default"]; ok {
			schemaInfo["default"] = defaultVal
		}

		// Enum - CRITICAL: Summarize if large
		if enumArray, ok := schemaMap["enum"].([]interface{}); ok {
			if len(enumArray) > 10 {
				// For large enums (like timezones), just indicate count and show first few examples
				examples := []interface{}{}
				for i := 0; i < 3 && i < len(enumArray); i++ {
					examples = append(examples, enumArray[i])
				}
				schemaInfo["enum"] = map[string]interface{}{
					"count":    len(enumArray),
					"examples": examples,
					"note":     "Large enum - use default or common values",
				}
			} else {
				// For small enums, include all options
				schemaInfo["enum"] = enumArray
			}
		}

		// Min/Max for numbers
		if min, ok := schemaMap["min"]; ok {
			schemaInfo["min"] = min
		}
		if max, ok := schemaMap["max"]; ok {
			schemaInfo["max"] = max
		}

		// Attrs (nested schemas) - summarize recursively
		if attrs, ok := schemaMap["attrs"].([]interface{}); ok {
			summarizedAttrs := []map[string]interface{}{}
			for _, attr := range attrs {
				if attrMap, ok := attr.(map[string]interface{}); ok {
					summarizedAttrs = append(summarizedAttrs, summarizeQuestion(attrMap))
				}
			}
			if len(summarizedAttrs) > 0 {
				schemaInfo["attrs"] = summarizedAttrs
			}
		}

		// Subquestions - indicate presence without full recursion
		if subquestions, ok := schemaMap["subquestions"].([]interface{}); ok {
			schemaInfo["has_subquestions"] = len(subquestions)
			schemaInfo["subquestions_note"] = "Conditional fields - configure based on parent value"
		}

		summarized["schema"] = schemaInfo
	}

	return summarized
}

// generateWizardGuidance creates step-by-step instructions based on schema
func generateWizardGuidance(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}

	guidance := map[string]interface{}{
		"workflow": "section-by-section configuration",
		"steps": []string{
			"1. Review schema groups",
			"2. Query available pools (query_pools) and present options to user",
			"3. Create datasets for storage paths using create_dataset",
			"4. Configure storage (type='host_path', path=/mnt/<pool>/apps/<appname>/<purpose>)",
			"5. Configure network (ports and certificates)",
			"6. Configure user/group IDs (default: 568:568)",
			"7. Configure resources (CPU/memory, use defaults)",
			"8. Configure app-specific settings (timezone, env vars)",
			"9. Assemble complete values object from all groups",
			"10. Execute installation with values parameter",
		},
		"common_patterns": map[string]interface{}{
			"timezone":      "Use system timezone or user preference",
			"run_as":        "Default: user=568, group=568 (apps user)",
			"storage_type":  "ALWAYS use 'host_path', NEVER 'ix_volume'",
			"storage_paths": "Use query_pools to get available pools, then create datasets before installation",
			"port_bind_mode": "published (external access) or exposed (internal only)",
			"resources":     "Default: 2 CPUs, 4096 MB RAM",
		},
		"storage_workflow": map[string]interface{}{
			"step1": "Call query_pools to get available storage pools",
			"step2": "If multiple pools: use AskUserQuestion to let user choose. If one pool: use it automatically",
			"step3": "Create dataset at /mnt/<pool>/apps/<appname>/<purpose> using create_dataset",
			"step4": "Configure storage with type='host_path' and path to created dataset",
			"note":  "NEVER ask user to type pool name - always query and present options",
		},
	}

	return guidance
}

// ============================================================================
// Section 2: Installation Handler and Dry-Run
// ============================================================================

// handleInstallApp installs an app from the catalog
func handleInstallApp(client *truenas.Client, args map[string]interface{}, taskManager *tasks.Manager) (string, error) {
	// Extract parameters
	appName, ok := args["app_name"].(string)
	if !ok || appName == "" {
		return "", fmt.Errorf("app_name is required")
	}

	catalogApp, ok := args["catalog_app"].(string)
	if !ok || catalogApp == "" {
		return "", fmt.Errorf("catalog_app is required")
	}

	train := "stable"
	if t, ok := args["train"].(string); ok && t != "" {
		train = t
	}

	version := "latest"
	if v, ok := args["version"].(string); ok && v != "" {
		version = v
	}

	// Validate app name
	if err := validateAppName(appName); err != nil {
		return "", fmt.Errorf("invalid app_name: %v", err)
	}

	// Extract values parameter (required)
	values, ok := args["values"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("values parameter is required. Use get_app_catalog_details to see the schema and build the configuration")
	}

	// CRITICAL SECURITY: Enforce host-path-only storage
	if err := enforceHostPathStorage(values); err != nil {
		return "", fmt.Errorf("storage validation failed: %v", err)
	}

	// Extract storage paths for dataset verification
	storagePaths := extractStoragePathsFromValues(values)

	// Verify datasets exist
	if len(storagePaths) > 0 {
		missing, err := verifyDatasetPathsExist(client, storagePaths)
		if err != nil {
			return "", fmt.Errorf("failed to verify datasets: %v", err)
		}
		if len(missing) > 0 {
			return "", fmt.Errorf("datasets must exist before installation. Missing:\n%s\n\nUse create_dataset tool first.",
				strings.Join(missing, "\n  - "))
		}
	}

	// Call app.create API
	params := map[string]interface{}{
		"app_name":    appName,
		"catalog_app": catalogApp,
		"train":       train,
		"version":     version,
		"values":      values,
	}

	result, err := client.Call("app.create", params)
	if err != nil {
		return "", fmt.Errorf("failed to install app: %v", err)
	}

	// Parse job ID (app.create may return an array [job_id] or just job_id)
	var jobID int
	// First try to parse as an integer
	if err := json.Unmarshal(result, &jobID); err != nil {
		// If that fails, try parsing as an array and extract the first element
		var jobIDArray []int
		if err2 := json.Unmarshal(result, &jobIDArray); err2 != nil {
			return "", fmt.Errorf("failed to parse job ID as int or array: int error: %v, array error: %v", err, err2)
		}
		if len(jobIDArray) == 0 {
			return "", fmt.Errorf("app.create returned empty job ID array")
		}
		jobID = jobIDArray[0]
	}

	// Create task for tracking
	task, err := taskManager.CreateJobTask(
		"install_app",
		args,
		jobID,
		1*time.Hour, // 1 hour TTL
	)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	response := map[string]interface{}{
		"app_name":      appName,
		"catalog_app":   catalogApp,
		"train":         train,
		"version":       version,
		"task_id":       task.TaskID,
		"task_status":   task.Status,
		"poll_interval": task.PollInterval,
		"job_id":        jobID,
		"message":       fmt.Sprintf("Installation started. Track progress with tasks_get using task_id: %s", task.TaskID),
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// installAppDryRun implements dry-run for app installation
type installAppDryRun struct{}

func (d *installAppDryRun) ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	// Extract parameters
	appName := args["app_name"].(string)
	catalogApp := args["catalog_app"].(string)
	train := "stable"
	if t, ok := args["train"].(string); ok && t != "" {
		train = t
	}

	// Validate app name
	if err := validateAppName(appName); err != nil {
		return nil, fmt.Errorf("invalid app_name: %v", err)
	}

	// Extract values parameter (required)
	valuesParam, ok := args["values"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("values parameter is required. Use get_app_catalog_details to see the schema")
	}

	// Validate storage security
	if err := enforceHostPathStorage(valuesParam); err != nil {
		return nil, fmt.Errorf("storage validation failed: %v", err)
	}

	// Extract storage paths
	storagePaths := extractStoragePathsFromValues(valuesParam)
	datasetPaths := storagePaths

	// Verify datasets exist
	var missing []string
	var err error
	if len(storagePaths) > 0 {
		missing, err = verifyDatasetPathsExist(client, storagePaths)
		if err != nil {
			return nil, fmt.Errorf("failed to verify datasets: %v", err)
		}
	}

	// Check if app already exists
	existingApps, err := client.Call("app.query",
		[]interface{}{
			[]interface{}{"name", "=", appName},
		},
		map[string]interface{}{},
	)

	var apps []interface{}
	if err == nil {
		json.Unmarshal(existingApps, &apps)
	}

	appExists := len(apps) > 0

	// Get app details for version info
	appDetails, err := client.Call("catalog.get_app_details", catalogApp, map[string]interface{}{
		"train": train,
	})

	var detailsMap map[string]interface{}
	latestVersion := "unknown"
	if err == nil && json.Unmarshal(appDetails, &detailsMap) == nil {
		if v, ok := detailsMap["latest_version"].(string); ok {
			latestVersion = v
		}
	}

	// Build planned actions
	actions := []PlannedAction{}
	step := 1

	// Add warnings for missing datasets
	for _, dataset := range missing {
		actions = append(actions, PlannedAction{
			Step:        step,
			Description: fmt.Sprintf("WARNING: Dataset %s does not exist. Create it first with create_dataset.", dataset),
			Operation:   "verify",
			Target:      "pool.dataset.query",
		})
		step++
	}

	// Add installation action
	actions = append(actions, PlannedAction{
		Step:        step,
		Description: fmt.Sprintf("Install %s app version %s", catalogApp, latestVersion),
		Operation:   "create",
		Target:      "app.create",
		Details: map[string]interface{}{
			"app_name":      appName,
			"catalog_app":   catalogApp,
			"train":         train,
			"storage_paths": datasetPaths,
		},
	})

	// Build warnings
	warnings := []string{}
	if appExists {
		warnings = append(warnings, fmt.Sprintf("WARNING: App instance '%s' already exists. Installation will fail.", appName))
	}
	if len(missing) > 0 {
		warnings = append(warnings, "CRITICAL: The following datasets must exist before installation. Use create_dataset tool:")
		for _, ds := range missing {
			warnings = append(warnings, fmt.Sprintf("  - %s", ds))
		}
	}
	warnings = append(warnings, "App will use host-path volumes (not ix-volumes) as configured.")

	result := &DryRunResult{
		Tool: "install_app",
		CurrentState: map[string]interface{}{
			"app_exists":       appExists,
			"missing_datasets": missing,
			"storage_paths":    datasetPaths,
			"app_version":      latestVersion,
		},
		PlannedActions: actions,
		Warnings:       warnings,
		Requirements: &Requirements{
			Conditions: []string{
				"All datasets must exist and be mounted",
				"Sufficient disk space in target pool",
				"No existing app with the same instance name",
			},
		},
		EstimatedTime: &EstimatedTime{
			MinSeconds: 30,
			MaxSeconds: 300,
			Note:       "Time varies based on container image size and network speed",
		},
	}

	return result, nil
}

// handleInstallAppWithDryRun wraps handleInstallApp with dry-run support
func (r *Registry) handleInstallAppWithDryRun(client *truenas.Client, args map[string]interface{}) (string, error) {
	dryRun := &installAppDryRun{}
	return ExecuteWithDryRun(client, args, dryRun, func(c *truenas.Client, a map[string]interface{}) (string, error) {
		return handleInstallApp(c, a, r.taskManager)
	})
}

// ============================================================================
// Section 3: Deletion Handler and Dry-Run
// ============================================================================

// handleDeleteApp deletes an installed app
func handleDeleteApp(client *truenas.Client, args map[string]interface{}, taskManager *tasks.Manager) (string, error) {
	// Extract parameters
	appName, ok := args["app_name"].(string)
	if !ok || appName == "" {
		return "", fmt.Errorf("app_name is required")
	}

	removeImages := false
	if ri, ok := args["remove_images"].(bool); ok {
		removeImages = ri
	}

	// Call app.delete API
	params := map[string]interface{}{
		"remove_images": removeImages,
	}

	result, err := client.Call("app.delete", appName, params)
	if err != nil {
		return "", fmt.Errorf("failed to delete app: %v", err)
	}

	// Parse job ID (app.delete may return an array [job_id] or just job_id)
	var jobID int
	// First try to parse as an integer
	if err := json.Unmarshal(result, &jobID); err != nil {
		// If that fails, try parsing as an array and extract the first element
		var jobIDArray []int
		if err2 := json.Unmarshal(result, &jobIDArray); err2 != nil {
			return "", fmt.Errorf("failed to parse job ID as int or array: int error: %v, array error: %v", err, err2)
		}
		if len(jobIDArray) == 0 {
			return "", fmt.Errorf("app.delete returned empty job ID array")
		}
		jobID = jobIDArray[0]
	}

	// Create task for tracking
	task, err := taskManager.CreateJobTask(
		"delete_app",
		args,
		jobID,
		30*time.Minute, // 30 minute TTL
	)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	response := map[string]interface{}{
		"app_name":      appName,
		"remove_images": removeImages,
		"task_id":       task.TaskID,
		"task_status":   task.Status,
		"poll_interval": task.PollInterval,
		"job_id":        jobID,
		"message":       fmt.Sprintf("Deletion started. Track progress with tasks_get using task_id: %s", task.TaskID),
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// deleteAppDryRun implements dry-run for app deletion
type deleteAppDryRun struct{}

func (d *deleteAppDryRun) ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	// Extract parameters
	appName := args["app_name"].(string)

	// Query app details
	result, err := client.Call("app.query",
		[]interface{}{
			[]interface{}{"name", "=", appName},
		},
		map[string]interface{}{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query app: %v", err)
	}

	var apps []interface{}
	if err := json.Unmarshal(result, &apps); err != nil {
		return nil, fmt.Errorf("failed to parse app query: %v", err)
	}

	if len(apps) == 0 {
		return nil, fmt.Errorf("app not found: %s", appName)
	}

	app := apps[0].(map[string]interface{})

	// Extract storage paths if available
	storagePaths := []string{}
	if config, ok := app["config"].(map[string]interface{}); ok {
		if persistence, ok := config["persistence"].(map[string]interface{}); ok {
			for _, vol := range persistence {
				if volMap, ok := vol.(map[string]interface{}); ok {
					if hostPath, ok := volMap["hostPath"].(string); ok {
						storagePaths = append(storagePaths, hostPath)
					}
				}
			}
		}
	}

	// Build planned actions
	actions := []PlannedAction{
		{
			Step:        1,
			Description: "Stop app containers",
			Operation:   "stop",
			Target:      "app",
		},
		{
			Step:        2,
			Description: "Remove app configuration and containers",
			Operation:   "delete",
			Target:      "app.delete",
		},
	}

	// Build warnings
	warnings := []string{
		"IMPORTANT: Host-path datasets will NOT be deleted",
		"Data will be preserved in original locations",
		"To remove data, manually delete datasets after app removal",
	}

	if len(storagePaths) > 0 {
		warnings = append(warnings, "The following data paths will be preserved:")
		for _, path := range storagePaths {
			warnings = append(warnings, fmt.Sprintf("  - %s", path))
		}
	}

	result2 := &DryRunResult{
		Tool: "delete_app",
		CurrentState: map[string]interface{}{
			"app_name":      appName,
			"state":         app["state"],
			"version":       app["version"],
			"storage_paths": storagePaths,
		},
		PlannedActions: actions,
		Warnings:       warnings,
		EstimatedTime: &EstimatedTime{
			MinSeconds: 10,
			MaxSeconds: 60,
			Note:       "Time depends on app shutdown gracefully",
		},
	}

	return result2, nil
}

// handleDeleteAppWithDryRun wraps handleDeleteApp with dry-run support
func (r *Registry) handleDeleteAppWithDryRun(client *truenas.Client, args map[string]interface{}) (string, error) {
	dryRun := &deleteAppDryRun{}
	return ExecuteWithDryRun(client, args, dryRun, func(c *truenas.Client, a map[string]interface{}) (string, error) {
		return handleDeleteApp(c, a, r.taskManager)
	})
}

// ============================================================================
// Section 4: Helper Functions
// ============================================================================

// validateAppName validates app instance name follows TrueNAS requirements
func validateAppName(name string) error {
	if len(name) == 0 || len(name) > 40 {
		return fmt.Errorf("app name must be 1-40 characters, got %d", len(name))
	}

	pattern := regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)
	if !pattern.MatchString(name) {
		return fmt.Errorf("app name must be lowercase, start with letter, and contain only letters/numbers/hyphens (no leading/trailing hyphens)")
	}

	return nil
}

// validateStorageVolumes validates storage volumes follow host-path requirements
func validateStorageVolumes(volumes []StorageVolume) error {
	if len(volumes) == 0 {
		return fmt.Errorf("at least one storage volume required")
	}

	seen := make(map[string]bool)
	paths := make(map[string]bool)

	for _, vol := range volumes {
		// Check for empty name or path
		if vol.Name == "" {
			return fmt.Errorf("volume name cannot be empty")
		}
		if vol.Path == "" {
			return fmt.Errorf("volume path cannot be empty")
		}

		// Check for duplicate names
		if seen[vol.Name] {
			return fmt.Errorf("duplicate volume name: %s", vol.Name)
		}
		seen[vol.Name] = true

		// Check for duplicate paths
		if paths[vol.Path] {
			return fmt.Errorf("duplicate volume path: %s", vol.Path)
		}
		paths[vol.Path] = true

		// Validate path format
		if !strings.HasPrefix(vol.Path, "/mnt/") {
			return fmt.Errorf("volume path must start with /mnt/, got: %s", vol.Path)
		}
	}

	return nil
}

// buildPersistenceConfig converts storage volumes to TrueNAS persistence config
func buildPersistenceConfig(volumes []StorageVolume) map[string]interface{} {
	persistence := make(map[string]interface{})

	for _, vol := range volumes {
		persistence[vol.Name] = map[string]interface{}{
			"type":     "host-path", // ALWAYS host-path, NEVER ix-volume
			"hostPath": vol.Path,
		}
	}

	return persistence
}

// parseStoragePath extracts pool and dataset from /mnt/ path
func parseStoragePath(path string) (pool string, dataset string, err error) {
	if !strings.HasPrefix(path, "/mnt/") {
		return "", "", fmt.Errorf("path must start with /mnt/")
	}

	// Remove /mnt/ prefix
	trimmed := strings.TrimPrefix(path, "/mnt/")
	parts := strings.Split(trimmed, "/")

	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid path format: %s (expected /mnt/<pool>/<dataset>)", path)
	}

	pool = parts[0]
	dataset = strings.Join(parts, "/")

	return pool, dataset, nil
}

// verifyDatasetsExist checks if datasets exist for all storage volumes
func verifyDatasetsExist(client *truenas.Client, volumes []StorageVolume) ([]string, error) {
	var missing []string

	for _, vol := range volumes {
		_, dataset, err := parseStoragePath(vol.Path)
		if err != nil {
			return nil, err
		}

		// Query dataset
		result, err := client.Call("pool.dataset.query",
			[]interface{}{
				[]interface{}{"name", "=", dataset},
			},
			map[string]interface{}{},
		)

		if err != nil {
			// If query fails, consider it missing
			missing = append(missing, dataset)
			continue
		}

		var datasets []interface{}
		if err := json.Unmarshal(result, &datasets); err != nil {
			return nil, err
		}

		if len(datasets) == 0 {
			missing = append(missing, dataset)
		}
	}

	return missing, nil
}

// extractStorageVolumes extracts and parses storage volumes from args
func extractStorageVolumes(args map[string]interface{}) ([]StorageVolume, error) {
	volumesRaw, ok := args["storage_volumes"]
	if !ok {
		example := `[{"name": "config", "path": "/mnt/tank/apps/myapp/config"}]`
		return nil, fmt.Errorf("storage_volumes is required.\nExpected: an array of volume objects\nExample: %s", example)
	}

	volumesArray, ok := volumesRaw.([]interface{})
	if !ok {
		example := `[{"name": "config", "path": "/mnt/tank/apps/myapp/config"}, {"name": "data", "path": "/mnt/tank/apps/myapp/data"}]`
		return nil, fmt.Errorf("storage_volumes must be an array.\nExpected: an array of volume objects\nExample: %s", example)
	}

	if len(volumesArray) == 0 {
		example := `[{"name": "config", "path": "/mnt/tank/apps/myapp/config"}]`
		return nil, fmt.Errorf("at least one storage volume is required.\nExample: %s", example)
	}

	volumes := make([]StorageVolume, 0, len(volumesArray))
	for i, volRaw := range volumesArray {
		volMap, ok := volRaw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("storage volume at index %d is not an object", i)
		}

		// Get name field
		name, nameOk := volMap["name"].(string)
		if !nameOk || name == "" {
			// Build helpful error message showing what was provided
			providedFields := make([]string, 0, len(volMap))
			for k := range volMap {
				providedFields = append(providedFields, fmt.Sprintf("'%s'", k))
			}

			example := `{"name": "config", "path": "/mnt/tank/apps/myapp/config"}`
			if len(providedFields) > 0 {
				return nil, fmt.Errorf("storage volume at index %d missing required 'name' field.\nProvided fields: %s\nRequired fields: 'name', 'path'\nExample: %s",
					i, strings.Join(providedFields, ", "), example)
			}
			return nil, fmt.Errorf("storage volume at index %d missing required 'name' field.\nRequired fields: 'name', 'path'\nExample: %s", i, example)
		}

		// Get path field
		path, pathOk := volMap["path"].(string)
		if !pathOk || path == "" {
			// Build helpful error message showing what was provided
			providedFields := make([]string, 0, len(volMap))
			for k := range volMap {
				providedFields = append(providedFields, fmt.Sprintf("'%s'", k))
			}

			example := `{"name": "config", "path": "/mnt/tank/apps/myapp/config"}`
			suggestions := "\nNote: Use 'path' (not 'host_path', 'mount_path', or other variants)"

			// Check for common mistakes and provide specific guidance
			if _, hasHostPath := volMap["host_path"]; hasHostPath {
				suggestions += "\nFound 'host_path' - did you mean 'path'?"
			}
			if _, hasMountPath := volMap["mount_path"]; hasMountPath {
				suggestions += "\nFound 'mount_path' - this is not needed. Only use 'name' and 'path'"
			}
			if _, hasType := volMap["type"]; hasType {
				suggestions += "\nFound 'type' - this is not needed. TrueNAS MCP always uses host-path volumes"
			}

			return nil, fmt.Errorf("storage volume at index %d missing required 'path' field.\nProvided fields: %s\nRequired fields: 'name', 'path'%s\nExample: %s",
				i, strings.Join(providedFields, ", "), suggestions, example)
		}

		volumes = append(volumes, StorageVolume{
			Name: name,
			Path: path,
		})
	}

	return volumes, nil
}

// ============================================================================
// Section 5: Values-Based Storage Security Validation
// ============================================================================

// enforceHostPathStorage recursively validates storage configs use host_path
func enforceHostPathStorage(values map[string]interface{}) error {
	return validateStorageRecursive(values, "")
}

// validateStorageRecursive recursively validates storage configuration
func validateStorageRecursive(obj map[string]interface{}, path string) error {
	// Check type field FIRST (before iterating) to ensure consistent error messages
	if typeVal, ok := obj["type"]; ok {
		if typeStr, ok := typeVal.(string); ok {
			if typeStr == "ix_volume" {
				currentPath := "type"
				if path != "" {
					currentPath = path + ".type"
				}
				return fmt.Errorf("ix_volume not allowed at %s. Use type='host_path'", currentPath)
			}
		}
	}

	// Now iterate through all keys
	for key, value := range obj {
		currentPath := key
		if path != "" {
			currentPath = path + "." + key
		}

		// Check for ix_volume_config
		if key == "ix_volume_config" {
			return fmt.Errorf("ix_volume_config not allowed at %s. Use host_path_config only", currentPath)
		}

		// Validate host_path_config paths
		if key == "host_path_config" {
			if configMap, ok := value.(map[string]interface{}); ok {
				if pathVal, ok := configMap["path"].(string); ok {
					if !strings.HasPrefix(pathVal, "/mnt/") {
						return fmt.Errorf("invalid path at %s: must start with /mnt/", currentPath)
					}
				}
			}
		}

		// Recurse into nested objects and arrays
		if nestedObj, ok := value.(map[string]interface{}); ok {
			if err := validateStorageRecursive(nestedObj, currentPath); err != nil {
				return err
			}
		}

		if array, ok := value.([]interface{}); ok {
			for i, item := range array {
				if itemObj, ok := item.(map[string]interface{}); ok {
					itemPath := fmt.Sprintf("%s[%d]", currentPath, i)
					if err := validateStorageRecursive(itemObj, itemPath); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

// extractStoragePathsFromValues finds all host paths in values config
func extractStoragePathsFromValues(values map[string]interface{}) []string {
	paths := []string{}
	collectPaths(values, &paths)
	return paths
}

// collectPaths recursively collects all host_path_config paths
func collectPaths(obj map[string]interface{}, paths *[]string) {
	for key, value := range obj {
		if key == "host_path_config" {
			if configMap, ok := value.(map[string]interface{}); ok {
				if path, ok := configMap["path"].(string); ok {
					*paths = append(*paths, path)
				}
			}
		}

		if nestedObj, ok := value.(map[string]interface{}); ok {
			collectPaths(nestedObj, paths)
		}

		if array, ok := value.([]interface{}); ok {
			for _, item := range array {
				if itemObj, ok := item.(map[string]interface{}); ok {
					collectPaths(itemObj, paths)
				}
			}
		}
	}
}

// verifyDatasetPathsExist checks if datasets exist for all paths
func verifyDatasetPathsExist(client *truenas.Client, paths []string) ([]string, error) {
	missing := []string{}

	for _, path := range paths {
		_, dataset, err := parseStoragePath(path)
		if err != nil {
			return nil, err
		}

		result, err := client.Call("pool.dataset.query",
			[]interface{}{
				[]interface{}{"name", "=", dataset},
			},
			map[string]interface{}{},
		)

		if err != nil || result == nil {
			missing = append(missing, dataset)
			continue
		}

		var datasets []interface{}
		if err := json.Unmarshal(result, &datasets); err != nil {
			return nil, err
		}

		if len(datasets) == 0 {
			missing = append(missing, dataset)
		}
	}

	return missing, nil
}
