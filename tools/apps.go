package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
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

	// Without a train every train is searched, so apps that are only in
	// the community (or enterprise) train are found too.
	train := "all"
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
	if limit <= 0 {
		return "", fmt.Errorf("limit must be at least 1; nothing was executed")
	}

	// Build filters
	filters := []interface{}{}

	// Crin is a case-insensitive literal substring match (operator.contains
	// after casefold). The 25.10 "~" operator is Python re.match, which
	// anchors at the start, so an escaped "cloud" would miss "nextcloud".
	if query != "" {
		filters = append(filters, []interface{}{"name", "Crin", query})
	}

	// categories is a list of strings; rin is membership (the list contains
	// this exact category). "=" would compare the whole list to the string
	// and never match.
	if category != "" {
		filters = append(filters, []interface{}{"categories", "rin", category})
	}

	// Filter by train in the middleware too, so limit counts only apps in
	// the requested train.
	if train != "all" {
		filters = append(filters, []interface{}{"train", "=", train})
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

	// Without a train, look up which train has the app instead of assuming
	// stable: community apps are not in the stable train.
	train := ""
	if t, ok := args["train"].(string); ok && t != "" {
		train = t
	}
	var trains []string
	if train == "" {
		var err error
		train, trains, err = resolveCatalogTrain(client, appName)
		if err != nil {
			return "", err
		}
	}

	// Call catalog.get_app_details API
	result, err := client.Call("catalog.get_app_details", appName, map[string]interface{}{
		"train": train,
	})
	if err != nil {
		var apiErr *truenas.APICallError
		if errors.As(err, &apiErr) && strings.Contains(apiErr.Message, "[ENOENT]") {
			return "", fmt.Errorf("app %q was not found in the %s catalog train; use search_app_catalog to find the app and its train, then pass that train", appName, train)
		}
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
	formatted := formatAppDetails(appDetails, train, trains, storageHints, schema)

	return formatted, nil
}

// catalogTrainPreference orders the trains tried when an app is in more
// than one: stable (the former default) first. Trains not listed here come
// after these, in alphabetical order.
var catalogTrainPreference = []string{"stable", "enterprise", "community"}

// resolveCatalogTrain looks up which catalog trains have appName and returns
// the preferred one plus all of them (sorted by preference). If the lookup
// itself fails it falls back to stable, the previous default.
func resolveCatalogTrain(client *truenas.Client, appName string) (string, []string, error) {
	result, err := client.Call("app.available",
		[]interface{}{[]interface{}{"name", "=", appName}},
		map[string]interface{}{},
	)
	if err != nil {
		return "stable", nil, nil
	}
	var apps []map[string]interface{}
	if err := json.Unmarshal(result, &apps); err != nil {
		return "stable", nil, nil
	}
	seen := map[string]bool{}
	var trains []string
	for _, app := range apps {
		if t, ok := app["train"].(string); ok && t != "" && !seen[t] {
			seen[t] = true
			trains = append(trains, t)
		}
	}
	if len(trains) == 0 {
		return "", nil, fmt.Errorf("app %q was not found in any catalog train; use search_app_catalog to find available apps", appName)
	}
	rank := func(t string) int {
		for i, p := range catalogTrainPreference {
			if t == p {
				return i
			}
		}
		return len(catalogTrainPreference)
	}
	sort.Slice(trains, func(i, j int) bool {
		ri, rj := rank(trains[i]), rank(trains[j])
		if ri != rj {
			return ri < rj
		}
		return trains[i] < trains[j]
	})
	return trains[0], trains, nil
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
func formatAppDetails(details map[string]interface{}, train string, trains []string, storageHints []string, schema map[string]interface{}) string {
	output := map[string]interface{}{
		"name":           details["name"],
		"title":          details["title"],
		"description":    details["description"],
		"train":          train,
		"latest_version": details["latest_version"],
		"categories":     details["categories"],
		"maintainers":    details["maintainers"],
	}
	if len(trains) > 1 {
		output["available_trains"] = trains
		output["train_note"] = fmt.Sprintf("App is in several catalog trains; showing the %s train. Pass train to see another.", train)
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

// extractAppSchema extracts the latest version's schema from app details.
func extractAppSchema(appDetails map[string]interface{}) map[string]interface{} {
	schema, _ := extractAppSchemaVersion(appDetails, "latest")
	return schema
}

// extractAppSchemaVersion returns the schema for version, or the latest
// version when version is empty or "latest". A specific version that is
// not in the catalog is an error; it does not fall back to latest.
func extractAppSchemaVersion(appDetails map[string]interface{}, version string) (map[string]interface{}, error) {
	if version == "" || version == "latest" {
		if v, ok := appDetails["latest_version"].(string); ok {
			version = v
		}
	}
	versions, ok := appDetails["versions"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("app details have no versions/schema")
	}
	versionData, ok := versions[version].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("app schema for version %q not found", version)
	}
	schema, ok := versionData["schema"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("app version has no schema")
	}
	return schema, nil
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
			"timezone":       "Use system timezone or user preference",
			"run_as":         "Default: user=568, group=568 (apps user)",
			"storage_type":   "ALWAYS use 'host_path', NEVER 'ix_volume'",
			"storage_paths":  "Use query_pools to get available pools, then create datasets before installation",
			"port_bind_mode": "published (external access) or exposed (internal only)",
			"resources":      "Default: 2 CPUs, 4096 MB RAM",
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

	train, trainNote, err := catalogTrainForInstall(client, catalogApp, args)
	if err != nil {
		return "", err
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

	// CRITICAL SECURITY: every schema storage entry must be a host_path
	// under /mnt/<pool>/, and those datasets must already exist.
	schema, err := fetchCatalogAppSchema(client, catalogApp, train, version)
	if err != nil {
		return "", err
	}
	if _, missing, err := checkAppHostPathStorage(client, schema, values); err != nil {
		return "", fmt.Errorf("storage validation failed: %v", err)
	} else if len(missing) > 0 {
		return "", datasetsMustExistError(missing, "installation")
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
	if trainNote != "" {
		response["train_note"] = trainNote
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
	appName, ok := args["app_name"].(string)
	if !ok || appName == "" {
		return nil, fmt.Errorf("app_name is required")
	}
	catalogApp, ok := args["catalog_app"].(string)
	if !ok || catalogApp == "" {
		return nil, fmt.Errorf("catalog_app is required")
	}
	train, trainNote, err := catalogTrainForInstall(client, catalogApp, args)
	if err != nil {
		return nil, err
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

	version := "latest"
	if v, ok := args["version"].(string); ok && v != "" {
		version = v
	}

	schema, err := fetchCatalogAppSchema(client, catalogApp, train, version)
	if err != nil {
		return nil, err
	}
	datasetPaths, missing, err := checkAppHostPathStorage(client, schema, valuesParam)
	if err != nil {
		return nil, fmt.Errorf("storage validation failed: %v", err)
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
	resolvedVersion := version
	if err == nil && json.Unmarshal(appDetails, &detailsMap) == nil {
		if version == "" || version == "latest" {
			if v, ok := detailsMap["latest_version"].(string); ok {
				resolvedVersion = v
			}
		}
	}
	if resolvedVersion == "" {
		resolvedVersion = "unknown"
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
		Description: fmt.Sprintf("Install %s app version %s", catalogApp, resolvedVersion),
		Operation:   "create",
		Target:      "app.create",
		Details: map[string]interface{}{
			"app_name":      appName,
			"catalog_app":   catalogApp,
			"train":         train,
			"storage_paths": datasetPaths,
			"version":       resolvedVersion,
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
			"app_version":      resolvedVersion,
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
	if trainNote != "" {
		result.CurrentState.(map[string]interface{})["train_note"] = trainNote
		if len(actions) > 0 && actions[len(actions)-1].Details != nil {
			if details, ok := actions[len(actions)-1].Details.(map[string]interface{}); ok {
				details["train_note"] = trainNote
			}
		}
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
	appName, ok := args["app_name"].(string)
	if !ok || appName == "" {
		return nil, fmt.Errorf("app_name is required")
	}

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

	app, ok := apps[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected app.query result for %s", appName)
	}

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

// catalogTrainForInstall returns the catalog train for an install: the
// caller's train when given, otherwise the preferred train that has the
// catalog app (same lookup as get_app_catalog_details). If the lookup
// itself fails, it falls back to stable and returns a note saying so.
func catalogTrainForInstall(client *truenas.Client, catalogApp string, args map[string]interface{}) (string, string, error) {
	if t, ok := args["train"].(string); ok && t != "" {
		return t, "", nil
	}
	train, trains, err := resolveCatalogTrain(client, catalogApp)
	if err != nil {
		return "", "", err
	}
	if trains == nil && train == "stable" {
		return train, "Catalog train lookup failed; assuming the stable train.", nil
	}
	return train, "", nil
}

// appNamePattern is the TrueNAS app instance name rule.
var appNamePattern = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// validateAppName validates app instance name follows TrueNAS requirements
func validateAppName(name string) error {
	if len(name) == 0 || len(name) > 40 {
		return fmt.Errorf("app name must be 1-40 characters, got %d", len(name))
	}

	if !appNamePattern.MatchString(name) {
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
	return validateStorageRecursive(values, "", nil)
}

// validateStorageRecursive recursively validates storage configuration
func validateStorageRecursive(obj map[string]interface{}, path string, pools map[string]bool) error {
	// Check type field FIRST (before iterating) to ensure consistent error messages
	if typeVal, ok := obj["type"]; ok {
		if typeStr, ok := typeVal.(string); ok {
			if isStorageLikeType(typeStr) && typeStr != "host_path" {
				loc := path
				if loc == "" {
					loc = "values"
				}
				if typeStr == "ix_volume" {
					currentPath := "type"
					if path != "" {
						currentPath = path + ".type"
					}
					return fmt.Errorf("ix_volume not allowed at %s. Use type='host_path'", currentPath)
				}
				return fmt.Errorf("storage at %s must use type='host_path' (got %s)", loc, typeStr)
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

		// Validate host_path_config paths, including acl.path when present.
		if key == "host_path_config" {
			if configMap, ok := value.(map[string]interface{}); ok {
				if pathVal, ok := configMap["path"].(string); ok && pathVal != "" {
					if err := validateHostPathLocation(pathVal, currentPath+".path", pools); err != nil {
						return err
					}
				}
				if acl, ok := configMap["acl"].(map[string]interface{}); ok {
					if aclPath, ok := acl["path"].(string); ok && aclPath != "" {
						if err := validateHostPathLocation(aclPath, currentPath+".acl.path", pools); err != nil {
							return err
						}
					}
				}
			}
		}

		// Recurse into nested objects and arrays
		if nestedObj, ok := value.(map[string]interface{}); ok {
			if err := validateStorageRecursive(nestedObj, currentPath, pools); err != nil {
				return err
			}
		}

		if array, ok := value.([]interface{}); ok {
			for i, item := range array {
				if itemObj, ok := item.(map[string]interface{}); ok {
					itemPath := fmt.Sprintf("%s[%d]", currentPath, i)
					if err := validateStorageRecursive(itemObj, itemPath, pools); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

// storageSlot is one schema storage entry or hostpath-typed field.
// kind is "storage", "hostpath", or "hostpathfile". path may contain
// "[]" markers for list items (for example storage.shares[].share_entry.host_path).
type storageSlot struct {
	path string
	list bool
	kind string
}

func datasetsMustExistError(missing []string, action string) error {
	if action == "" {
		action = "installation"
	}
	return fmt.Errorf("datasets must exist before %s. Missing:\n%s\n\nUse create_dataset tool first.",
		action, strings.Join(missing, "\n  - "))
}

// isStorageLikeType reports whether typeStr is a volume/storage type used
// in the TrueNAS app catalog (or an obvious alias). Comparison is
// case-insensitive so off-schema values such as "NFS" are still caught.
// The only type this tool accepts is the exact string "host_path".
func isStorageLikeType(typeStr string) bool {
	switch strings.ToLower(typeStr) {
	case "host_path", "ix_volume", "nfs", "cifs", "tmpfs", "temporary",
		"anonymous", "volume", "smb", "smb_share":
		return true
	default:
		return false
	}
}

func fetchCatalogAppSchema(client *truenas.Client, catalogApp, train, version string) (map[string]interface{}, error) {
	if train == "" {
		train = "stable"
	}
	result, err := client.Call("catalog.get_app_details", catalogApp, map[string]interface{}{
		"train": train,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get app schema: %w", err)
	}
	var details map[string]interface{}
	if err := json.Unmarshal(result, &details); err != nil {
		return nil, fmt.Errorf("failed to parse app details: %w", err)
	}
	return extractAppSchemaVersion(details, version)
}

func fetchInstalledAppSchema(client *truenas.Client, appName string) (map[string]interface{}, error) {
	result, err := client.Call("app.query",
		[]interface{}{[]interface{}{"name", "=", appName}},
		map[string]interface{}{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query installed app: %w", err)
	}
	var apps []map[string]interface{}
	if err := json.Unmarshal(result, &apps); err != nil {
		return nil, fmt.Errorf("failed to parse app query: %w", err)
	}
	if len(apps) == 0 {
		return nil, fmt.Errorf("app %q was not found", appName)
	}
	app := apps[0]
	catalogApp := appName
	train := ""
	version, _ := app["version"].(string)
	if md, ok := app["metadata"].(map[string]interface{}); ok {
		if n, ok := md["name"].(string); ok && n != "" {
			catalogApp = n
		} else if n, ok := md["app"].(string); ok && n != "" {
			catalogApp = n
		}
		if t, ok := md["train"].(string); ok {
			train = t
		}
	}
	if train == "" {
		resolved, _, err := resolveCatalogTrain(client, catalogApp)
		if err != nil {
			return nil, err
		}
		train = resolved
	}
	return fetchCatalogAppSchema(client, catalogApp, train, version)
}

func listPoolNames(client *truenas.Client) (map[string]bool, error) {
	result, err := client.Call("pool.query", []interface{}{})
	if err != nil {
		return nil, fmt.Errorf("failed to query pools: %v", err)
	}
	var raw []interface{}
	if err := json.Unmarshal(result, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse pools: %v", err)
	}
	names := map[string]bool{}
	for _, p := range raw {
		if m, ok := p.(map[string]interface{}); ok {
			if n, ok := m["name"].(string); ok && n != "" {
				names[n] = true
			}
		}
	}
	return names, nil
}

// checkAppHostPathStorage is the shared storage check for install_app and
// update_app (and their dry runs). It requires every schema storage entry to
// be present as type host_path with a cleaned path under /mnt/<pool>/, then
// reports which of those datasets are missing.
func checkAppHostPathStorage(client *truenas.Client, schema, values map[string]interface{}) ([]string, []string, error) {
	if values == nil {
		return nil, nil, fmt.Errorf("values parameter is required")
	}
	if schema == nil {
		return nil, nil, fmt.Errorf("app schema is required to enforce host_path storage")
	}
	pools, err := listPoolNames(client)
	if err != nil {
		return nil, nil, err
	}
	slots, err := collectStorageSlots(schema)
	if err != nil {
		return nil, nil, err
	}
	var collected []string
	for _, slot := range slots {
		if err := collectSlotPaths(values, slot, pools, &collected); err != nil {
			return nil, nil, err
		}
	}
	if err := validateStorageRecursive(values, "", pools); err != nil {
		return nil, nil, err
	}
	missing, err := verifyDatasetPathsExist(client, collected)
	if err != nil {
		return nil, nil, err
	}
	return collected, missing, nil
}

func collectSlotPaths(values map[string]interface{}, slot storageSlot, pools map[string]bool, collected *[]string) error {
	if slot.kind == "hostpath" || slot.kind == "hostpathfile" {
		dotted := slot.path
		if slot.list && !strings.Contains(dotted, "[]") {
			dotted += "[]"
		}
		located, err := iterValuesAt(values, dotted, "")
		if err != nil {
			return err
		}
		for _, lv := range located {
			if lv.val == nil {
				continue
			}
			p, ok := lv.val.(string)
			if !ok {
				return fmt.Errorf("host path at %s must be a string", lv.loc)
			}
			if p == "" {
				return fmt.Errorf("host path at %s must be a non-empty string", lv.loc)
			}
			target, err := hostPathDatasetTarget(p, slot.kind, lv.loc, pools)
			if err != nil {
				return err
			}
			*collected = append(*collected, target)
		}
		return nil
	}

	if slot.list {
		raw, ok := lookupValuesPath(values, slot.path)
		if !ok || raw == nil {
			return nil
		}
		arr, ok := raw.([]interface{})
		if !ok {
			return fmt.Errorf("storage at %s must be a list of host_path entries", slot.path)
		}
		for i, item := range arr {
			entry, ok := item.(map[string]interface{})
			if !ok {
				return fmt.Errorf("storage at %s[%d] must be a host_path entry", slot.path, i)
			}
			p, err := validateHostPathEntry(entry, fmt.Sprintf("%s[%d]", slot.path, i), pools)
			if err != nil {
				return err
			}
			*collected = append(*collected, p)
		}
		return nil
	}
	raw, ok := lookupValuesPath(values, slot.path)
	if !ok {
		return fmt.Errorf("storage at %s is required and must use type='host_path' with host_path_config.path", slot.path)
	}
	entry, ok := raw.(map[string]interface{})
	if !ok {
		return fmt.Errorf("storage at %s must use type='host_path' with host_path_config.path", slot.path)
	}
	p, err := validateHostPathEntry(entry, slot.path, pools)
	if err != nil {
		return err
	}
	*collected = append(*collected, p)
	return nil
}

func collectStorageSlots(schema map[string]interface{}) ([]storageSlot, error) {
	raw, ok := schema["questions"]
	if !ok {
		return nil, fmt.Errorf("app schema has no questions list; refusing to validate storage")
	}
	questions, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("app schema questions is not a list; refusing to validate storage")
	}
	if len(questions) == 0 {
		return nil, fmt.Errorf("app schema has an empty questions list; refusing to validate storage")
	}
	var slots []storageSlot
	for i, q := range questions {
		qm, ok := q.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("app schema questions[%d] cannot be parsed; refusing to validate storage", i)
		}
		if err := walkSchemaQuestion(qm, "", &slots); err != nil {
			return nil, err
		}
	}
	return slots, nil
}

func walkSchemaQuestion(q map[string]interface{}, prefix string, slots *[]storageSlot) error {
	variable, _ := q["variable"].(string)
	sch, _ := q["schema"].(map[string]interface{})
	if sch == nil {
		return nil
	}
	return walkSchemaNode(sch, joinValuePath(prefix, variable), slots)
}

func joinValuePath(prefix, name string) string {
	if name == "" {
		return prefix
	}
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func walkSchemaNode(schema map[string]interface{}, valuePath string, slots *[]storageSlot) error {
	typ := appSchemaType(schema)
	if typ == "list" {
		item := listItemSchema(schema)
		if item == nil {
			loc := valuePath
			if loc == "" {
				loc = "schema"
			}
			return fmt.Errorf("app schema list at %s has items that cannot be parsed; refusing to validate storage", loc)
		}
		if isStorageDictSchema(item) {
			*slots = append(*slots, storageSlot{path: valuePath, list: true, kind: "storage"})
			return nil
		}
		if kind := hostPathSchemaKind(item); kind != "" {
			*slots = append(*slots, storageSlot{path: valuePath, list: true, kind: kind})
			return nil
		}
		// Descend into list items with a [] marker so lookup expands each element.
		return walkSchemaNode(item, valuePath+"[]", slots)
	}
	if kind := hostPathSchemaKind(schema); kind != "" {
		*slots = append(*slots, storageSlot{path: valuePath, list: false, kind: kind})
		return nil
	}
	if isStorageDictSchema(schema) {
		// Stop here so host_path_config.path is handled by validateHostPathEntry
		// (which also covers acl.path) rather than as a bare hostpath field.
		*slots = append(*slots, storageSlot{path: valuePath, list: false, kind: "storage"})
		return nil
	}
	for _, attr := range schemaAttrs(schema) {
		if err := walkSchemaQuestion(attr, valuePath, slots); err != nil {
			return err
		}
	}
	return nil
}

func hostPathSchemaKind(schema map[string]interface{}) string {
	switch appSchemaType(schema) {
	case "hostpathfile":
		return "hostpathfile"
	case "hostpath", "hostpathdirectory":
		return "hostpath"
	default:
		return ""
	}
}

func appSchemaType(schema map[string]interface{}) string {
	t, _ := schema["type"].(string)
	return t
}

func schemaAttrs(schema map[string]interface{}) []map[string]interface{} {
	raw, _ := schema["attrs"].([]interface{})
	var out []map[string]interface{}
	for _, a := range raw {
		if m, ok := a.(map[string]interface{}); ok {
			out = append(out, m)
		}
	}
	return out
}

func listItemSchema(schema map[string]interface{}) map[string]interface{} {
	switch v := schema["items"].(type) {
	case []interface{}:
		if len(v) == 0 {
			return nil
		}
		if m, ok := v[0].(map[string]interface{}); ok {
			if s, ok := m["schema"].(map[string]interface{}); ok {
				return s
			}
			return m
		}
	case map[string]interface{}:
		if s, ok := v["schema"].(map[string]interface{}); ok {
			return s
		}
		return v
	}
	return nil
}

func schemaEnumValues(schema map[string]interface{}) []string {
	enum, ok := schema["enum"].([]interface{})
	if !ok {
		return nil
	}
	var vals []string
	for _, e := range enum {
		switch v := e.(type) {
		case string:
			vals = append(vals, v)
		case map[string]interface{}:
			if s, ok := v["value"].(string); ok {
				vals = append(vals, s)
			}
		}
	}
	return vals
}

func isStorageTypeAttr(schema map[string]interface{}) bool {
	if def, ok := schema["default"].(string); ok {
		if def == "host_path" || def == "ix_volume" {
			return true
		}
	}
	for _, v := range schemaEnumValues(schema) {
		if v == "host_path" || v == "ix_volume" {
			return true
		}
	}
	return false
}

func isStorageDictSchema(schema map[string]interface{}) bool {
	typ := appSchemaType(schema)
	if typ != "" && typ != "dict" && typ != "object" {
		return false
	}
	for _, attr := range schemaAttrs(schema) {
		if attr["variable"] == "type" {
			sub, _ := attr["schema"].(map[string]interface{})
			if sub != nil && isStorageTypeAttr(sub) {
				return true
			}
		}
	}
	return false
}

func lookupValuesPath(values map[string]interface{}, dotted string) (interface{}, bool) {
	if dotted == "" {
		return values, true
	}
	cur := interface{}(values)
	for _, part := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		next, ok := m[part]
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

func validateHostPathEntry(entry map[string]interface{}, loc string, pools map[string]bool) (string, error) {
	typeStr, _ := entry["type"].(string)
	if typeStr != "host_path" {
		got := typeStr
		if got == "" {
			got = "missing"
		}
		return "", fmt.Errorf("storage at %s must use type='host_path' (got %s)", loc, got)
	}
	if _, ok := entry["ix_volume_config"]; ok {
		return "", fmt.Errorf("ix_volume_config not allowed at %s. Use host_path_config only", loc)
	}
	cfg, ok := entry["host_path_config"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("storage at %s must have host_path_config.path", loc)
	}

	pathVal, pathOK := cfg["path"].(string)
	if pathOK && pathVal != "" {
		if err := validateHostPathLocation(pathVal, loc+".host_path_config.path", pools); err != nil {
			return "", err
		}
	}

	aclPath := ""
	if acl, ok := cfg["acl"].(map[string]interface{}); ok {
		if p, ok := acl["path"].(string); ok && p != "" {
			aclPath = p
			if err := validateHostPathLocation(aclPath, loc+".host_path_config.acl.path", pools); err != nil {
				return "", err
			}
		}
	}

	// acl_enable must be a real boolean: a string "true" would otherwise
	// be read as false here while TrueNAS may treat it differently.
	aclEnable := false
	if v, present := cfg["acl_enable"]; present {
		b, ok := v.(bool)
		if !ok {
			return "", fmt.Errorf("storage at %s: host_path_config.acl_enable must be a boolean (true or false)", loc)
		}
		aclEnable = b
	}
	if aclEnable {
		if aclPath == "" {
			return "", fmt.Errorf("storage at %s has host_path_config.acl_enable=true but no host_path_config.acl.path", loc)
		}
		// The apps library bind-mounts acl.path and ignores host_path_config.path.
		return aclPath, nil
	}
	if !pathOK || pathVal == "" {
		return "", fmt.Errorf("storage at %s must have host_path_config.path", loc)
	}
	return pathVal, nil
}

// validateHostPathLocation requires a cleaned path under /mnt/<pool>/ for an
// existing pool, with at least one component below the pool.
func validateHostPathLocation(p, loc string, pools map[string]bool) error {
	cleaned := path.Clean(p)
	if cleaned != p {
		return fmt.Errorf("invalid path %q at %s: must be a cleaned path under /mnt/<pool>/", p, loc)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("invalid path %q at %s: must not contain '..'", p, loc)
		}
	}
	if !strings.HasPrefix(p, "/mnt/") || p == "/mnt/" {
		return fmt.Errorf("invalid path %q at %s: must start with /mnt/", p, loc)
	}
	parts := strings.Split(strings.TrimPrefix(p, "/mnt/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid path %q at %s: must be under /mnt/<pool>/ for an existing pool (at least one component below the pool)", p, loc)
	}
	if pools != nil && !pools[parts[0]] {
		return fmt.Errorf("invalid path %q at %s: pool %q does not exist", p, loc, parts[0])
	}
	if parts[1] == ".system" || parts[1] == "ix-applications" {
		return fmt.Errorf("invalid path %q at %s: %s is a reserved system dataset", p, loc, parts[1])
	}
	return nil
}

func hostPathDatasetTarget(p, kind, loc string, pools map[string]bool) (string, error) {
	if err := validateHostPathLocation(p, loc, pools); err != nil {
		return "", err
	}
	// hostpathfile is explicit. hostpath is typed as a directory in the
	// catalog, but some templates (nextcloud custom_confs) bind-mount a
	// file; treat a last component with a "." as a file so valid configs work.
	if kind == "hostpathfile" || (kind == "hostpath" && hostPathLooksLikeFile(p)) {
		parent := path.Dir(p)
		if err := validateHostPathLocation(parent, loc, pools); err != nil {
			return "", fmt.Errorf("invalid path %q at %s: file must be under a dataset below the pool (not the pool root)", p, loc)
		}
		return parent, nil
	}
	return p, nil
}

func hostPathLooksLikeFile(p string) bool {
	base := path.Base(p)
	if base == "" || base == "." || base == ".." {
		return false
	}
	return strings.Contains(base, ".")
}

type locatedValue struct {
	loc string
	val interface{}
}

// iterValuesAt walks a dotted path that may contain "[]" list markers and
// returns every matching value with a concrete location (indexes filled in).
func iterValuesAt(root interface{}, dotted, locPrefix string) ([]locatedValue, error) {
	if dotted == "" {
		return []locatedValue{{loc: strings.TrimSuffix(locPrefix, "."), val: root}}, nil
	}
	i := strings.Index(dotted, "[]")
	if i < 0 {
		v, ok := lookupFrom(root, dotted)
		if !ok {
			return nil, nil
		}
		return []locatedValue{{loc: locPrefix + dotted, val: v}}, nil
	}
	head := dotted[:i]
	tail := strings.TrimPrefix(dotted[i+2:], ".")
	var arrSrc interface{}
	if head == "" {
		arrSrc = root
	} else {
		var ok bool
		arrSrc, ok = lookupFrom(root, head)
		if !ok {
			return nil, nil
		}
	}
	arr, ok := arrSrc.([]interface{})
	if !ok {
		loc := locPrefix + strings.TrimSuffix(head, ".")
		if loc == "" {
			loc = "values"
		}
		return nil, fmt.Errorf("value at %s must be a list", loc)
	}
	var out []locatedValue
	for idx, item := range arr {
		itemLoc := locPrefix + head + fmt.Sprintf("[%d]", idx)
		if tail == "" {
			out = append(out, locatedValue{loc: itemLoc, val: item})
			continue
		}
		more, err := iterValuesAt(item, tail, itemLoc+".")
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	return out, nil
}

func lookupFrom(root interface{}, dotted string) (interface{}, bool) {
	if dotted == "" {
		return root, true
	}
	cur := root
	for _, part := range strings.Split(dotted, ".") {
		if part == "" {
			continue
		}
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		next, ok := m[part]
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
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
		if err != nil {
			return nil, fmt.Errorf("failed to look up dataset %s: %w", dataset, err)
		}
		if result == nil {
			return nil, fmt.Errorf("failed to look up dataset %s: empty response", dataset)
		}

		var datasets []interface{}
		if err := json.Unmarshal(result, &datasets); err != nil {
			return nil, fmt.Errorf("failed to look up dataset %s: %w", dataset, err)
		}

		if len(datasets) == 0 {
			missing = append(missing, dataset)
		}
	}

	return missing, nil
}
