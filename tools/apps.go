package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

// ============================================================================
// Section 1: Search and Catalog Handlers
// ============================================================================

// handleSearchAppCatalog searches the TrueNAS app catalog
func handleSearchAppCatalog(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
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
	result, err := client.Call(ctx, "app.available", filters, options)
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
func handleGetAppCatalogDetails(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
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
		train, trains, err = resolveCatalogTrain(ctx, client, appName)
		if err != nil {
			return "", err
		}
	}

	// Call catalog.get_app_details API
	result, err := client.Call(ctx, "catalog.get_app_details", appName, map[string]interface{}{
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

	// Extract the latest version's schema (defaults are taken from it).
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
func resolveCatalogTrain(ctx context.Context, client *truenas.Client, appName string) (string, []string, error) {
	result, err := client.Call(ctx, "app.available",
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

	// Add the schema and any run-as / resource defaults taken from it.
	if schema != nil {
		output["schema"] = formatSchemaForWizard(schema)
		if defaults := extractSchemaDefaults(schema); defaults != nil {
			output["defaults"] = defaults
		}
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

// extractSchemaDefaults returns run-as user/group and resource-limit
// defaults from the app schema in catalog.get_app_details. Keys use the
// schema's own variable names (run_as or runAs). Resource defaults keep
// the schema nesting (for example resources.limits.cpus). Only numeric
// defaults are passed through. In ix-app, run_as applies only when
// run_as_custom_user is true, so run_as is omitted when that flag's
// default is false. Likewise, resources.limits is omitted when
// enable_resource_limits defaults to false. Fields and the whole object
// are omitted when the schema has no such default.
func extractSchemaDefaults(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	out := map[string]interface{}{}
	includeRunAs := true
	if q, _ := schemaQuestionByVariable(schema, "run_as_custom_user", "runAsCustomUser"); q != nil {
		if d, ok := schemaQuestionBoolDefault(q); ok && !d {
			includeRunAs = false
		}
	}
	if includeRunAs {
		if q, name := schemaQuestionByVariable(schema, "run_as", "runAs"); q != nil {
			runAs := map[string]interface{}{}
			if v, ok := schemaNumericAttrDefault(q, "user"); ok {
				runAs["user"] = v
			}
			if v, ok := schemaNumericAttrDefault(q, "group"); ok {
				runAs["group"] = v
			}
			if len(runAs) > 0 {
				out[name] = runAs
			}
		}
	}
	includeResourceLimits := schemaFlagEnabled(schema, "enable_resource_limits", "enableResourceLimits")
	if q, _ := schemaQuestionByVariable(schema, "resources"); q != nil {
		if resources := extractNestedNumericDefaults(q); len(resources) > 0 {
			if !includeResourceLimits {
				delete(resources, "limits")
			}
			if len(resources) > 0 {
				out["resources"] = resources
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// schemaFlagEnabled reports whether a boolean schema flag is enabled.
// A missing flag, or one with no boolean default, is treated as enabled
// so catalog apps that do not define the flag keep their defaults. A
// default of false (top-level or nested under resources, as in ix-app)
// disables the flagged section.
func schemaFlagEnabled(schema map[string]interface{}, names ...string) bool {
	if q, _ := schemaQuestionByVariable(schema, names...); q != nil {
		if d, ok := schemaQuestionBoolDefault(q); ok {
			return d
		}
	}
	if q, _ := schemaQuestionByVariable(schema, "resources"); q != nil {
		if d, ok := schemaBoolAttrDefault(q, names...); ok {
			return d
		}
	}
	return true
}

func schemaBoolAttrDefault(question map[string]interface{}, names ...string) (bool, bool) {
	for _, name := range names {
		v, ok := findSchemaAttrDefault(question, name)
		if !ok {
			continue
		}
		b, ok := v.(bool)
		if ok {
			return b, true
		}
	}
	return false, false
}

func schemaQuestionBoolDefault(question map[string]interface{}) (bool, bool) {
	schemaMap, _ := question["schema"].(map[string]interface{})
	if schemaMap == nil {
		return false, false
	}
	d, ok := schemaMap["default"]
	if !ok {
		return false, false
	}
	b, ok := d.(bool)
	return b, ok
}

func schemaNumericAttrDefault(question map[string]interface{}, variable string) (interface{}, bool) {
	v, ok := findSchemaAttrDefault(question, variable)
	if !ok {
		return nil, false
	}
	return asNumericDefault(v)
}

// extractNestedNumericDefaults walks a question's nested schema.attrs and
// returns only numeric defaults, preserving the schema nesting.
func extractNestedNumericDefaults(node map[string]interface{}) map[string]interface{} {
	if node == nil {
		return nil
	}
	schemaMap, _ := node["schema"].(map[string]interface{})
	if schemaMap == nil {
		return nil
	}
	attrs, ok := schemaMap["attrs"].([]interface{})
	if !ok {
		return nil
	}
	out := map[string]interface{}{}
	for _, a := range attrs {
		am, ok := a.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := am["variable"].(string)
		if name == "" {
			continue
		}
		if nested := extractNestedNumericDefaults(am); len(nested) > 0 {
			out[name] = nested
			continue
		}
		if v, ok := attrDirectNumericDefault(am); ok {
			out[name] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func attrDirectNumericDefault(node map[string]interface{}) (interface{}, bool) {
	schemaMap, _ := node["schema"].(map[string]interface{})
	if schemaMap == nil {
		return nil, false
	}
	d, ok := schemaMap["default"]
	if !ok {
		return nil, false
	}
	return asNumericDefault(d)
}

func asNumericDefault(v interface{}) (interface{}, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int8:
		return int(n), true
	case int16:
		return int(n), true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case uint:
		return int(n), true
	case uint8:
		return int(n), true
	case uint16:
		return int(n), true
	case uint32:
		return int(n), true
	case uint64:
		return int(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
		f, err := n.Float64()
		if err != nil {
			return nil, false
		}
		return f, true
	default:
		return nil, false
	}
}

func schemaQuestionByVariable(schema map[string]interface{}, names ...string) (map[string]interface{}, string) {
	questions, ok := schema["questions"].([]interface{})
	if !ok {
		return nil, ""
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	for _, q := range questions {
		qm, ok := q.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := qm["variable"].(string)
		if want[name] {
			return qm, name
		}
	}
	return nil, ""
}

func findSchemaAttrDefault(node map[string]interface{}, variable string) (interface{}, bool) {
	if node == nil {
		return nil, false
	}
	schemaMap, _ := node["schema"].(map[string]interface{})
	if name, _ := node["variable"].(string); name == variable && schemaMap != nil {
		if d, ok := schemaMap["default"]; ok && d != nil {
			return d, true
		}
	}
	if schemaMap == nil {
		return nil, false
	}
	attrs, ok := schemaMap["attrs"].([]interface{})
	if !ok {
		return nil, false
	}
	for _, a := range attrs {
		am, ok := a.(map[string]interface{})
		if !ok {
			continue
		}
		if d, ok := findSchemaAttrDefault(am, variable); ok {
			return d, true
		}
	}
	return nil, false
}

// ============================================================================
// Section 2: Installation Handler and Dry-Run
// ============================================================================

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const appCreateMethod = "app.create"

// maskInstallAppArgs returns a deep copy of install_app arguments with
// secret values (passwords, tokens, and similar, including nested ones)
// replaced by redact.Marker. Use this for stored task arguments so a
// later tasks_get cannot recover the secrets the install used.
func maskInstallAppArgs(args map[string]interface{}) map[string]interface{} {
	if args == nil {
		return nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return map[string]interface{}{}
	}
	var copied map[string]interface{}
	if err := json.Unmarshal(raw, &copied); err != nil {
		return map[string]interface{}{}
	}
	masked := redact.Value(copied)
	if m, ok := masked.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

// planInstallApp builds the install_app plan used by both the dry-run
// preview and the real call. The app.create payload is unchanged from
// main: version is sent as "latest" when the caller omitted it, even
// when the preview also resolves the catalog version for display.
// Missing host-path datasets and an existing instance name refuse both
// the preview and the real call.
func planInstallApp(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	appName, ok := args["app_name"].(string)
	if !ok || appName == "" {
		return nil, fmt.Errorf("app_name is required")
	}
	catalogApp, ok := args["catalog_app"].(string)
	if !ok || catalogApp == "" {
		return nil, fmt.Errorf("catalog_app is required")
	}
	train, trainNote, err := catalogTrainForInstall(ctx, client, catalogApp, args)
	if err != nil {
		return nil, err
	}
	if err := validateAppName(appName); err != nil {
		return nil, fmt.Errorf("invalid app_name: %v", err)
	}
	values, ok := args["values"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("values parameter is required. Use get_app_catalog_details to see the schema and build the configuration")
	}

	version := "latest"
	if v, ok := args["version"].(string); ok && v != "" {
		version = v
	}

	schema, err := fetchCatalogAppSchema(ctx, client, catalogApp, train, version)
	if err != nil {
		return nil, err
	}
	datasetPaths, missing, err := checkAppHostPathStorage(ctx, client, schema, values)
	if err != nil {
		return nil, fmt.Errorf("storage validation failed: %v", err)
	}
	if len(missing) > 0 {
		return nil, datasetsMustExistError(missing, "installation")
	}

	existing, err := client.Call(ctx, "app.query",
		[]interface{}{[]interface{}{"name", "=", appName}},
		map[string]interface{}{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to check existing app instance: %w", err)
	}
	var apps []interface{}
	if err := json.Unmarshal(existing, &apps); err != nil {
		return nil, fmt.Errorf("failed to parse installed apps: %w", err)
	}
	for _, raw := range apps {
		m, _ := raw.(map[string]interface{})
		if m == nil {
			continue
		}
		if m["name"] == appName || m["id"] == appName {
			return nil, fmt.Errorf("app instance %q already exists", appName)
		}
	}

	params := map[string]interface{}{
		"app_name":    appName,
		"catalog_app": catalogApp,
		"train":       train,
		"version":     version,
		"values":      values,
	}

	resolvedVersion := version
	detailsResult, detailsErr := client.Call(ctx, "catalog.get_app_details", catalogApp, map[string]interface{}{
		"train": train,
	})
	var detailsMap map[string]interface{}
	if detailsErr == nil && json.Unmarshal(detailsResult, &detailsMap) == nil {
		if version == "" || version == "latest" {
			if v, ok := detailsMap["latest_version"].(string); ok {
				resolvedVersion = v
			}
		}
	}
	if resolvedVersion == "" {
		resolvedVersion = "unknown"
	}

	current := map[string]interface{}{
		"app_exists":       false,
		"storage_paths":    datasetPaths,
		"version":          version,
		"resolved_version": resolvedVersion,
		"values":           values,
	}
	if trainNote != "" {
		current["train_note"] = trainNote
	}

	details := map[string]interface{}{
		"app_name":      appName,
		"catalog_app":   catalogApp,
		"train":         train,
		"version":       version,
		"values":        values,
		"storage_paths": datasetPaths,
	}
	if trainNote != "" {
		details["train_note"] = trainNote
	}

	return &writePlan{
		Tool:         "install_app",
		Method:       appCreateMethod,
		JobMethod:    appCreateMethod,
		CallParams:   []interface{}{params},
		ParamNames:   []string{"data"},
		TargetName:   appName,
		CurrentState: current,
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Install %s app version %s", catalogApp, resolvedVersion),
			Operation:   "create",
			Target:      appCreateMethod,
			Details:     details,
		}},
		Warnings: []string{
			"App will use host-path volumes (not ix-volumes) as configured.",
		},
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
		Result: map[string]interface{}{
			"app_name":    appName,
			"catalog_app": catalogApp,
			"train":       train,
			"version":     version,
			"train_note":  trainNote,
		},
	}, nil
}

func (r *Registry) handleInstallAppWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planInstallApp(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeInstallApp(ctx, client, args, plan)
}

func (r *Registry) executeInstallApp(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to install app: %v", err)
	}

	jobID, err := parseJobID(result)
	if err != nil {
		return "", err
	}

	opts := []tasks.TaskOption{}
	if plan.JobMethod != "" {
		opts = append(opts, tasks.WithExpectedMethod(plan.JobMethod))
	}
	task, err := r.taskManager.CreateJobTask(ctx,
		"install_app",
		maskInstallAppArgs(args),
		jobID,
		1*time.Hour,
		opts...,
	)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	appName, _ := plan.Result["app_name"].(string)
	catalogApp, _ := plan.Result["catalog_app"].(string)
	train, _ := plan.Result["train"].(string)
	version, _ := plan.Result["version"].(string)
	trainNote, _ := plan.Result["train_note"].(string)

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

// ============================================================================
// Section 3: Deletion Handler and Dry-Run
// ============================================================================

const deleteAppMethod = "app.delete"

// planDeleteApp builds the delete_app plan used by both the dry-run preview
// and the real call. Host-path datasets are not deleted; that stays a warning.
func planDeleteApp(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
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

	removeImages := false
	if ri, ok := args["remove_images"].(bool); ok {
		removeImages = ri
	}

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

	return &writePlan{
		Tool:        "delete_app",
		Method:      deleteAppMethod,
		CallParams:  []interface{}{appName, map[string]interface{}{"remove_images": removeImages}},
		JobMethod:   deleteAppMethod,
		Description: fmt.Sprintf("Remove application %q", appName),
		Operation:   "delete",
		ParamNames:  []string{"app_name", "options"},
		CurrentState: map[string]interface{}{
			"app_name":      appName,
			"state":         state,
			"version":       app["version"],
			"storage_paths": storagePaths,
		},
		Warnings:      warnings,
		EstimatedTime: &EstimatedTime{MinSeconds: 10, MaxSeconds: 60, Note: "Time depends on app shutdown gracefully"},
		TargetName:    appName,
	}, nil
}

func (r *Registry) handleDeleteAppWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planDeleteApp(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeDeleteApp(ctx, client, args, plan)
}

func (r *Registry) executeDeleteApp(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to delete app: %w", err)
	}

	jobID, err := parseJobID(result)
	if err != nil {
		return "", fmt.Errorf("failed to parse job ID: %w", err)
	}

	opts := []tasks.TaskOption{}
	if plan.JobMethod != "" {
		opts = append(opts, tasks.WithExpectedMethod(plan.JobMethod))
	}
	task, err := r.taskManager.CreateJobTask(ctx, "delete_app", args, jobID, 30*time.Minute, opts...)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	removeImages := false
	if len(plan.CallParams) > 1 {
		if params, ok := plan.CallParams[1].(map[string]interface{}); ok {
			if ri, ok := params["remove_images"].(bool); ok {
				removeImages = ri
			}
		}
	}

	response := map[string]interface{}{
		"app_name":      plan.TargetName,
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

// ============================================================================
// Section 4: Helper Functions
// ============================================================================

// catalogTrainForInstall returns the catalog train for an install: the
// caller's train when given, otherwise the preferred train that has the
// catalog app (same lookup as get_app_catalog_details). If the lookup
// itself fails, it falls back to stable and returns a note saying so.
func catalogTrainForInstall(ctx context.Context, client *truenas.Client, catalogApp string, args map[string]interface{}) (string, string, error) {
	if t, ok := args["train"].(string); ok && t != "" {
		return t, "", nil
	}
	train, trains, err := resolveCatalogTrain(ctx, client, catalogApp)
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
			if isStorageLikeObject(obj, typeStr) && typeStr != "host_path" {
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
// kind is "storage", "hostpath", "hostpathdirectory", or "hostpathfile".
// path may contain "[]" markers for list items (for example
// storage.shares[].share_entry.host_path).
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

// volumeConfigKeys are the per-type config objects used on a storage entry
// in the TrueNAS apps library. An unknown type that carries one of these
// is still treated as storage, so new volume types are refused.
var volumeConfigKeys = []string{
	"host_path_config", "ix_volume_config", "nfs_config", "cifs_config",
	"smb_config", "tmpfs_config", "volume_config", "anonymous_config",
}

func hasVolumeConfigKey(obj map[string]interface{}) bool {
	for _, k := range volumeConfigKeys {
		if _, ok := obj[k]; ok {
			return true
		}
	}
	return false
}

// isStorageLikeObject reports whether obj should be held to the host_path
// rule. Known storage type strings are always caught. An unrecognized type
// is also caught when the object carries a volume config key, so unknown
// off-schema types fail closed instead of passing.
func isStorageLikeObject(obj map[string]interface{}, typeStr string) bool {
	return isStorageLikeType(typeStr) || hasVolumeConfigKey(obj)
}

func fetchCatalogAppSchema(ctx context.Context, client *truenas.Client, catalogApp, train, version string) (map[string]interface{}, error) {
	if train == "" {
		train = "stable"
	}
	result, err := client.Call(ctx, "catalog.get_app_details", catalogApp, map[string]interface{}{
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

func queryInstalledApp(ctx context.Context, client *truenas.Client, appName string) (map[string]interface{}, error) {
	result, err := client.Call(ctx, "app.query",
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
	return apps[0], nil
}

func isCustomApp(app map[string]interface{}) bool {
	v, ok := app["custom_app"].(bool)
	return ok && v
}

func schemaFromInstalledApp(ctx context.Context, client *truenas.Client, app map[string]interface{}) (map[string]interface{}, error) {
	appName, _ := app["name"].(string)
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
	if catalogApp == "" {
		catalogApp = appName
	}
	if train == "" {
		resolved, _, err := resolveCatalogTrain(ctx, client, catalogApp)
		if err != nil {
			return nil, err
		}
		train = resolved
	}
	return fetchCatalogAppSchema(ctx, client, catalogApp, train, version)
}

func listPoolNames(ctx context.Context, client *truenas.Client) (map[string]bool, error) {
	result, err := client.Call(ctx, "pool.query", []interface{}{})
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

// checkAppHostPathStorage is the storage check for install_app (and its dry
// run). It requires every schema storage entry to be present as type
// host_path with a cleaned path under /mnt/<pool>/, then reports which of
// those datasets are missing. update_app uses prepareUpdateAppStorage.
func checkAppHostPathStorage(ctx context.Context, client *truenas.Client, schema, values map[string]interface{}) ([]string, []string, error) {
	if values == nil {
		return nil, nil, fmt.Errorf("values parameter is required")
	}
	if schema == nil {
		return nil, nil, fmt.Errorf("app schema is required to enforce host_path storage")
	}
	pools, err := listPoolNames(ctx, client)
	if err != nil {
		return nil, nil, err
	}
	slots, err := collectStorageSlots(schema)
	if err != nil {
		return nil, nil, err
	}
	var collected []string
	for _, slot := range slots {
		if err := collectSlotPaths(ctx, client, values, slot, pools, &collected); err != nil {
			return nil, nil, err
		}
	}
	if err := validateStorageRecursive(values, "", pools); err != nil {
		return nil, nil, err
	}
	missing, err := verifyDatasetPathsExist(ctx, client, collected)
	if err != nil {
		return nil, nil, err
	}
	return collected, missing, nil
}

type storageLocationChange struct {
	slot   string
	oldLoc string
	newLoc string
}

func cloneJSONMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}

func cloneJSON(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

func fetchAppConfig(ctx context.Context, client *truenas.Client, appName string) (map[string]interface{}, error) {
	result, err := client.Call(ctx, "app.config", appName)
	if err != nil {
		return nil, fmt.Errorf("failed to get current config: %w", err)
	}
	if len(result) == 0 || string(result) == "null" {
		return map[string]interface{}{}, nil
	}
	var current map[string]interface{}
	if err := json.Unmarshal(result, &current); err != nil {
		return nil, fmt.Errorf("failed to parse current config: %w", err)
	}
	if current == nil {
		current = map[string]interface{}{}
	}
	return current, nil
}

func parseAllowStorageChange(args map[string]interface{}) (bool, error) {
	v, ok := args["allow_storage_change"]
	if !ok || v == nil {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("allow_storage_change must be a boolean (true or false)")
	}
	return b, nil
}

func isStorageSectionPath(p string) bool {
	return p == "storage" || strings.HasPrefix(p, "storage.") || strings.HasPrefix(p, "storage[")
}

func fillStorageFromCurrent(values, current map[string]interface{}) (map[string]interface{}, error) {
	out := cloneJSONMap(values)
	if out == nil {
		out = map[string]interface{}{}
	}
	raw, ok := out["storage"]
	if !ok {
		return out, nil
	}
	newStorage, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("storage must be an object")
	}
	currentStorage, _ := current["storage"].(map[string]interface{})
	for key, val := range currentStorage {
		if _, present := newStorage[key]; !present {
			newStorage[key] = cloneJSON(val)
		}
	}
	fillSameLocationProvidedSlots(newStorage, currentStorage)
	out["storage"] = newStorage
	return out, nil
}

// slotConfigKeys are the per-type config objects that are deep-filled from
// the current slot when type and location are unchanged. Other nested
// objects are not merged.
var slotConfigKeys = []string{"host_path_config", "nfs_config", "cifs_config"}

func fillSameLocationProvidedSlots(newStorage, currentStorage map[string]interface{}) {
	if newStorage == nil || currentStorage == nil {
		return
	}
	for key, val := range newStorage {
		cur, ok := currentStorage[key]
		if !ok {
			continue
		}
		switch provided := val.(type) {
		case map[string]interface{}:
			currentSlot, ok := cur.(map[string]interface{})
			if ok {
				fillSameLocationSlot(provided, currentSlot)
			}
		case []interface{}:
			currentList, ok := cur.([]interface{})
			if !ok {
				continue
			}
			for i, item := range provided {
				entry, ok := item.(map[string]interface{})
				if !ok || i >= len(currentList) {
					continue
				}
				currentEntry, ok := currentList[i].(map[string]interface{})
				if ok {
					fillSameLocationSlot(entry, currentEntry)
				}
			}
		}
	}
}

func fillSameLocationSlot(provided, current map[string]interface{}) {
	if provided == nil || current == nil {
		return
	}
	if !sameTypeAndProvidedLocation(current, provided) {
		return
	}
	for key, val := range current {
		if _, ok := provided[key]; !ok {
			provided[key] = cloneJSON(val)
		}
	}
	for _, key := range slotConfigKeys {
		providedCfg, pok := provided[key].(map[string]interface{})
		currentCfg, cok := current[key].(map[string]interface{})
		if !pok || !cok {
			continue
		}
		for ck, cv := range currentCfg {
			if _, ok := providedCfg[ck]; !ok {
				providedCfg[ck] = cloneJSON(cv)
			}
		}
	}
}

func sameTypeAndProvidedLocation(current, provided map[string]interface{}) bool {
	if !providedHasLocationFields(provided, entryTypeOr(provided, current)) {
		return false
	}
	cmp := provided
	if storageEntryType(provided) == "" && storageEntryType(current) != "" {
		tmp := cloneJSONMap(provided)
		tmp["type"] = storageEntryType(current)
		cmp = tmp
	}
	return storageLocationUnchanged(current, cmp)
}

func entryTypeOr(provided, current map[string]interface{}) string {
	if typ := storageEntryType(provided); typ != "" {
		return typ
	}
	return storageEntryType(current)
}

func providedHasLocationFields(entry map[string]interface{}, typ string) bool {
	switch typ {
	case "host_path":
		cfg, _ := entry["host_path_config"].(map[string]interface{})
		if cfg == nil {
			return false
		}
		if p, ok := cfg["path"].(string); ok && p != "" {
			return true
		}
		if aclEnable, _ := cfg["acl_enable"].(bool); aclEnable {
			if acl, ok := cfg["acl"].(map[string]interface{}); ok {
				if p, ok := acl["path"].(string); ok && p != "" {
					return true
				}
			}
		}
		return false
	case "nfs":
		return shareHasServerAndPath(entry["nfs_config"])
	case "cifs":
		return shareHasServerAndPath(entry["cifs_config"])
	case "ix_volume":
		cfg, _ := entry["ix_volume_config"].(map[string]interface{})
		name, _ := cfg["dataset_name"].(string)
		return name != ""
	default:
		return false
	}
}

func shareHasServerAndPath(raw interface{}) bool {
	cfg, ok := raw.(map[string]interface{})
	if !ok {
		return false
	}
	server, _ := cfg["server"].(string)
	path, _ := cfg["path"].(string)
	return server != "" && path != ""
}

func shareConfigLocation(typ string, raw interface{}) string {
	cfg, _ := raw.(map[string]interface{})
	server, _ := cfg["server"].(string)
	path, _ := cfg["path"].(string)
	if server == "" && path == "" {
		return typ
	}
	return typ + " " + server + ":" + path
}

func isShareStorageType(typ string) bool {
	return typ == "nfs" || typ == "cifs"
}

func validateShareConfig(entry map[string]interface{}, typ, loc string) error {
	key := typ + "_config"
	if !shareHasServerAndPath(entry[key]) {
		return fmt.Errorf("storage at %s must have %s.server and %s.path", loc, key, key)
	}
	return nil
}

func boolField(entry map[string]interface{}, key string) (bool, bool) {
	if entry == nil {
		return false, false
	}
	v, ok := entry[key]
	if !ok || v == nil {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func formatJSONValue(v interface{}) string {
	if v == nil {
		return "null"
	}
	if b, ok := v.(bool); ok {
		return fmt.Sprintf("%t", b)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func accessChangeWarnings(slot string, current, next map[string]interface{}) []string {
	if current == nil || next == nil {
		return nil
	}
	var warnings []string
	curRO, curROSet := boolField(current, "read_only")
	nextRO, nextROSet := boolField(next, "read_only")
	if curROSet || nextROSet {
		if !curROSet {
			curRO = false
		}
		if !nextROSet {
			nextRO = false
		}
		if curRO != nextRO {
			warnings = append(warnings, fmt.Sprintf("storage slot %s: changing read_only from %t to %t", slot, curRO, nextRO))
		}
	}
	warnings = append(warnings, aclChangeWarnings(slot, "host_path_config", current, next)...)
	warnings = append(warnings, aclChangeWarnings(slot, "ix_volume_config", current, next)...)
	return warnings
}

func aclChangeWarnings(slot, configKey string, current, next map[string]interface{}) []string {
	curCfg, _ := current[configKey].(map[string]interface{})
	nextCfg, _ := next[configKey].(map[string]interface{})
	if curCfg == nil && nextCfg == nil {
		return nil
	}
	var warnings []string
	curEnable, curSet := boolField(curCfg, "acl_enable")
	nextEnable, nextSet := boolField(nextCfg, "acl_enable")
	if curSet || nextSet {
		if !curSet {
			curEnable = false
		}
		if !nextSet {
			nextEnable = false
		}
		if curEnable != nextEnable {
			warnings = append(warnings, fmt.Sprintf("storage slot %s: changing %s.acl_enable from %t to %t", slot, configKey, curEnable, nextEnable))
		}
	}
	aclKey := "acl"
	if configKey == "ix_volume_config" {
		_, curHas := curCfg["acl_entries"]
		_, nextHas := nextCfg["acl_entries"]
		if curHas || nextHas {
			aclKey = "acl_entries"
		}
	}
	var curACL, nextACL interface{}
	if curCfg != nil {
		curACL = curCfg[aclKey]
	}
	if nextCfg != nil {
		nextACL = nextCfg[aclKey]
	}
	if !jsonValuesEqual(curACL, nextACL) && (curACL != nil || nextACL != nil) {
		warnings = append(warnings, fmt.Sprintf("storage slot %s: changing %s.%s from %s to %s", slot, configKey, aclKey, formatJSONValue(curACL), formatJSONValue(nextACL)))
	}
	return warnings
}

func jsonValuesEqual(a, b interface{}) bool {
	if a == nil && b == nil {
		return true
	}
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

func storageEntryType(entry map[string]interface{}) string {
	if entry == nil {
		return ""
	}
	typ, _ := entry["type"].(string)
	return typ
}

// storageEntryLocation is the data location of a 25.10 catalog storage
// slot: host_path_config.path (or acl.path when acl_enable is true),
// ix_volume plus the ix_volume_config.dataset_name, or a share's
// nfs_config/cifs_config server and path. Credential fields are omitted.
func storageEntryLocation(entry map[string]interface{}) string {
	if entry == nil {
		return ""
	}
	typ := storageEntryType(entry)
	switch typ {
	case "host_path":
		if cfg, ok := entry["host_path_config"].(map[string]interface{}); ok {
			aclEnable := false
			if v, ok := cfg["acl_enable"].(bool); ok {
				aclEnable = v
			}
			if aclEnable {
				if acl, ok := cfg["acl"].(map[string]interface{}); ok {
					if p, ok := acl["path"].(string); ok && p != "" {
						return p
					}
				}
			}
			if p, ok := cfg["path"].(string); ok && p != "" {
				return p
			}
		}
		return "host_path"
	case "ix_volume":
		if cfg, ok := entry["ix_volume_config"].(map[string]interface{}); ok {
			if name, ok := cfg["dataset_name"].(string); ok && name != "" {
				return fmt.Sprintf("ix_volume %q", name)
			}
		}
		return "ix_volume"
	case "nfs":
		return shareConfigLocation("nfs", entry["nfs_config"])
	case "cifs":
		return shareConfigLocation("cifs", entry["cifs_config"])
	default:
		if typ == "" {
			return "unknown"
		}
		return typ
	}
}

func storageLocationUnchanged(current, next map[string]interface{}) bool {
	if current == nil || next == nil {
		return false
	}
	return storageEntryType(current) == storageEntryType(next) &&
		storageEntryLocation(current) == storageEntryLocation(next)
}

func formatStorageChangeLine(ch storageLocationChange) string {
	return fmt.Sprintf("storage slot %s: changing location from %s to %s; data is not migrated", ch.slot, ch.oldLoc, ch.newLoc)
}

func storageChangeBlockedError(changes []storageLocationChange) error {
	var b strings.Builder
	for i, ch := range changes {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(fmt.Sprintf("changing the type or path of storage slot %s from %s to %s is blocked because data is not migrated", ch.slot, ch.oldLoc, ch.newLoc))
	}
	b.WriteString("; pass allow_storage_change: true to proceed")
	return fmt.Errorf("%s", b.String())
}

func asStorageEntry(raw interface{}, loc string) (map[string]interface{}, error) {
	if raw == nil {
		return nil, fmt.Errorf("storage at %s must be an object", loc)
	}
	entry, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("storage at %s must be an object", loc)
	}
	return entry, nil
}

// prepareUpdateAppStorage implements the update_app storage rules: omit
// storage to leave it unchanged; fill a partial storage object from the
// current config, including same-location slot top-level keys and
// host_path_config/nfs_config/cifs_config fields; leave identical existing
// entries (including ix_volume) alone; require host_path for new entries
// and for type or path changes that are not a same-type NFS/CIFS location
// change; and require allow_storage_change when an existing slot's type or
// location changes. read_only and ACL-only changes are warned, not blocked.
func prepareUpdateAppStorage(ctx context.Context, client *truenas.Client, schema, values, current map[string]interface{}, allowStorageChange bool) (map[string]interface{}, []string, []string, []string, error) {
	if values == nil {
		return nil, nil, nil, nil, fmt.Errorf("values parameter is required")
	}
	if current == nil {
		current = map[string]interface{}{}
	}
	_, storagePresent := values["storage"]
	var err error
	prepared := values
	if storagePresent {
		prepared, err = fillStorageFromCurrent(values, current)
		if err != nil {
			return nil, nil, nil, nil, err
		}
	} else {
		prepared = cloneJSONMap(values)
		if prepared == nil {
			prepared = map[string]interface{}{}
		}
	}

	if schema == nil {
		return nil, nil, nil, nil, fmt.Errorf("app schema is required to enforce host_path storage")
	}
	pools, err := listPoolNames(ctx, client)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	slots, err := collectStorageSlots(schema)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	var changes []storageLocationChange
	var collected []string
	var accessWarnings []string
	for _, slot := range slots {
		if !storagePresent && isStorageSectionPath(slot.path) {
			continue
		}
		ch, paths, access, err := inspectUpdateStorageSlot(ctx, client, prepared, current, slot, pools)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		changes = append(changes, ch...)
		collected = append(collected, paths...)
		accessWarnings = append(accessWarnings, access...)
	}

	if len(changes) > 0 && !allowStorageChange {
		return nil, nil, nil, nil, storageChangeBlockedError(changes)
	}

	var warnings []string
	for _, ch := range changes {
		warnings = append(warnings, formatStorageChangeLine(ch))
	}
	warnings = append(warnings, accessWarnings...)

	missing, err := verifyDatasetPathsExist(ctx, client, collected)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return prepared, collected, missing, warnings, nil
}

func inspectUpdateStorageSlot(ctx context.Context, client *truenas.Client, values, current map[string]interface{}, slot storageSlot, pools map[string]bool) ([]storageLocationChange, []string, []string, error) {
	if slot.kind == "hostpath" || slot.kind == "hostpathfile" || slot.kind == "hostpathdirectory" {
		ch, paths, err := inspectUpdateHostPathField(ctx, client, values, current, slot, pools)
		return ch, paths, nil, err
	}
	if slot.list {
		return inspectUpdateStorageList(values, current, slot, pools)
	}
	return inspectUpdateStorageEntry(values, current, slot, pools)
}

func inspectUpdateHostPathField(ctx context.Context, client *truenas.Client, values, current map[string]interface{}, slot storageSlot, pools map[string]bool) ([]storageLocationChange, []string, error) {
	dotted := slot.path
	if slot.list && !strings.Contains(dotted, "[]") {
		dotted += "[]"
	}
	located, err := iterValuesAt(values, dotted, "")
	if err != nil {
		return nil, nil, err
	}
	var changes []storageLocationChange
	var collected []string
	for _, lv := range located {
		if lv.val == nil {
			continue
		}
		p, ok := lv.val.(string)
		if !ok {
			return nil, nil, fmt.Errorf("host path at %s must be a string", lv.loc)
		}
		if p == "" {
			return nil, nil, fmt.Errorf("host path at %s must be a non-empty string", lv.loc)
		}
		old := ""
		if cur, ok := lookupValuesPath(current, strings.TrimSuffix(lv.loc, "")); ok {
			if s, ok := cur.(string); ok {
				old = s
			}
		}
		// Best-effort current lookup at the same path string as the slot item.
		if old == "" {
			if cur, ok := lookupValuesPath(current, slot.path); ok {
				if s, ok := cur.(string); ok {
					old = s
				}
			}
		}
		if old != "" && old == p {
			continue
		}
		if old != "" && old != p {
			changes = append(changes, storageLocationChange{slot: lv.loc, oldLoc: old, newLoc: p})
		}
		target, err := hostPathDatasetTarget(ctx, client, p, slot.kind, lv.loc, pools)
		if err != nil {
			return nil, nil, err
		}
		collected = append(collected, target)
	}
	return changes, collected, nil
}

func inspectUpdateStorageList(values, current map[string]interface{}, slot storageSlot, pools map[string]bool) ([]storageLocationChange, []string, []string, error) {
	raw, ok := lookupValuesPath(values, slot.path)
	if !ok || raw == nil {
		return nil, nil, nil, nil
	}
	arr, ok := raw.([]interface{})
	if !ok {
		return nil, nil, nil, fmt.Errorf("storage at %s must be a list of storage entries", slot.path)
	}
	var currentArr []interface{}
	if cur, ok := lookupValuesPath(current, slot.path); ok {
		currentArr, _ = cur.([]interface{})
	}
	var changes []storageLocationChange
	var collected []string
	var access []string
	for i, item := range arr {
		loc := fmt.Sprintf("%s[%d]", slot.path, i)
		entry, err := asStorageEntry(item, loc)
		if err != nil {
			return nil, nil, nil, err
		}
		var curEntry map[string]interface{}
		if i < len(currentArr) {
			curEntry, _ = currentArr[i].(map[string]interface{})
		}
		ch, path, acc, err := classifyStorageEntry(entry, curEntry, loc, pools)
		if err != nil {
			return nil, nil, nil, err
		}
		if ch != nil {
			changes = append(changes, *ch)
		}
		if path != "" {
			collected = append(collected, path)
		}
		access = append(access, acc...)
	}
	for i := len(arr); i < len(currentArr); i++ {
		loc := fmt.Sprintf("%s[%d]", slot.path, i)
		curEntry, _ := currentArr[i].(map[string]interface{})
		oldLoc := storageEntryLocation(curEntry)
		if oldLoc == "" {
			oldLoc = "existing entry"
		}
		changes = append(changes, storageLocationChange{slot: loc, oldLoc: oldLoc, newLoc: "(removed)"})
	}
	return changes, collected, access, nil
}

func inspectUpdateStorageEntry(values, current map[string]interface{}, slot storageSlot, pools map[string]bool) ([]storageLocationChange, []string, []string, error) {
	raw, ok := lookupValuesPath(values, slot.path)
	if !ok {
		return nil, nil, nil, fmt.Errorf("storage at %s is required and must use type='host_path' with host_path_config.path", slot.path)
	}
	entry, err := asStorageEntry(raw, slot.path)
	if err != nil {
		return nil, nil, nil, err
	}
	var curEntry map[string]interface{}
	if cur, ok := lookupValuesPath(current, slot.path); ok {
		curEntry, _ = cur.(map[string]interface{})
	}
	ch, path, acc, err := classifyStorageEntry(entry, curEntry, slot.path, pools)
	if err != nil {
		return nil, nil, nil, err
	}
	var changes []storageLocationChange
	if ch != nil {
		changes = append(changes, *ch)
	}
	var collected []string
	if path != "" {
		collected = append(collected, path)
	}
	return changes, collected, acc, nil
}

func classifyStorageEntry(entry, current map[string]interface{}, loc string, pools map[string]bool) (*storageLocationChange, string, []string, error) {
	if current != nil && storageLocationUnchanged(current, entry) {
		return nil, "", accessChangeWarnings(loc, current, entry), nil
	}
	var change *storageLocationChange
	if current != nil {
		change = &storageLocationChange{
			slot:   loc,
			oldLoc: storageEntryLocation(current),
			newLoc: storageEntryLocation(entry),
		}
		typ := storageEntryType(entry)
		if typ == storageEntryType(current) && isShareStorageType(typ) {
			if err := validateShareConfig(entry, typ, loc); err != nil {
				return nil, "", nil, err
			}
			return change, "", nil, nil
		}
	}
	path, err := validateHostPathEntry(entry, loc, pools)
	if err != nil {
		return nil, "", nil, err
	}
	return change, path, nil, nil
}

func collectSlotPaths(ctx context.Context, client *truenas.Client, values map[string]interface{}, slot storageSlot, pools map[string]bool, collected *[]string) error {
	if slot.kind == "hostpath" || slot.kind == "hostpathfile" || slot.kind == "hostpathdirectory" {
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
			target, err := hostPathDatasetTarget(ctx, client, p, slot.kind, lv.loc, pools)
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
	loc := joinValuePath(prefix, variable)
	if loc == "" {
		loc = "questions"
	}
	raw, ok := q["schema"]
	if !ok || raw == nil {
		return fmt.Errorf("app schema question at %s has no schema map; refusing to validate storage", loc)
	}
	sch, ok := raw.(map[string]interface{})
	if !ok {
		return fmt.Errorf("app schema question at %s has no schema map; refusing to validate storage", loc)
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
		isStorage, err := isStorageDictSchema(item, valuePath)
		if err != nil {
			return err
		}
		if isStorage {
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
	isStorage, err := isStorageDictSchema(schema, valuePath)
	if err != nil {
		return err
	}
	if isStorage {
		// Stop here so host_path_config.path is handled by validateHostPathEntry
		// (which also covers acl.path) rather than as a bare hostpath field.
		*slots = append(*slots, storageSlot{path: valuePath, list: false, kind: "storage"})
		return nil
	}
	attrs, err := schemaAttrs(schema, valuePath)
	if err != nil {
		return err
	}
	for _, attr := range attrs {
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
	case "hostpathdirectory":
		return "hostpathdirectory"
	case "hostpath":
		return "hostpath"
	default:
		return ""
	}
}

func appSchemaType(schema map[string]interface{}) string {
	t, _ := schema["type"].(string)
	return t
}

func schemaAttrs(schema map[string]interface{}, loc string) ([]map[string]interface{}, error) {
	raw, ok := schema["attrs"]
	if !ok || raw == nil {
		return nil, nil
	}
	list, ok := raw.([]interface{})
	if !ok {
		if loc == "" {
			loc = "schema"
		}
		return nil, fmt.Errorf("app schema attrs at %s is not a list; refusing to validate storage", loc)
	}
	var out []map[string]interface{}
	for i, a := range list {
		m, ok := a.(map[string]interface{})
		if !ok {
			if loc == "" {
				loc = "schema"
			}
			return nil, fmt.Errorf("app schema attrs[%d] at %s is not a map; refusing to validate storage", i, loc)
		}
		out = append(out, m)
	}
	return out, nil
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

func isStorageDictSchema(schema map[string]interface{}, loc string) (bool, error) {
	typ := appSchemaType(schema)
	if typ != "" && typ != "dict" && typ != "object" {
		return false, nil
	}
	attrs, err := schemaAttrs(schema, loc)
	if err != nil {
		return false, err
	}
	for _, attr := range attrs {
		if attr["variable"] == "type" {
			sub, _ := attr["schema"].(map[string]interface{})
			if sub != nil && isStorageTypeAttr(sub) {
				return true, nil
			}
		}
	}
	return false, nil
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

func hostPathDatasetTarget(ctx context.Context, client *truenas.Client, p, kind, loc string, pools map[string]bool) (string, error) {
	if err := validateHostPathLocation(p, loc, pools); err != nil {
		return "", err
	}
	switch kind {
	case "hostpathfile":
		return hostPathFileParent(p, loc, pools)
	case "hostpathdirectory":
		return p, nil
	case "hostpath":
		// Catalog type hostpath is used for both directories (webdav,
		// rsyncd) and files (nextcloud custom_confs). Classify by
		// whether the path exists as a dataset, not by a "." in the name.
		return resolveHostPathTarget(ctx, client, p, loc, pools)
	default:
		return p, nil
	}
}

func hostPathFileParent(p, loc string, pools map[string]bool) (string, error) {
	parent := path.Dir(p)
	if err := validateHostPathLocation(parent, loc, pools); err != nil {
		return "", fmt.Errorf("invalid path %q at %s: file must be under a dataset below the pool (not the pool root)", p, loc)
	}
	return parent, nil
}

// resolveHostPathTarget classifies an ambiguous hostpath value. If the path
// exists as a dataset, it is a directory. If it does not, and the parent is
// a valid location under /mnt/<pool>/ whose dataset exists, it is a file.
// If it is not a dataset and the parent is not a valid file location
// (for example the path sits directly under the pool), the path itself is
// returned so the missing-dataset check can report it. A typo for a missing
// subdirectory whose parent dataset exists is accepted as a file;
// distinguishing that from a real file would need a filesystem stat.
func resolveHostPathTarget(ctx context.Context, client *truenas.Client, p, loc string, pools map[string]bool) (string, error) {
	exists, err := hostPathDatasetExists(ctx, client, p)
	if err != nil {
		return "", err
	}
	if exists {
		return p, nil
	}
	parent, err := hostPathFileParent(p, loc, pools)
	if err != nil {
		// A file can never sit directly under the pool. Keep the path so
		// the later dataset check can report it as missing (the real call
		// gives the create_dataset hint; dry-run warns).
		return p, nil
	}
	parentExists, err := hostPathDatasetExists(ctx, client, parent)
	if err != nil {
		return "", err
	}
	if parentExists {
		return parent, nil
	}
	// Neither the path nor the parent exists. Keep the path so the
	// later dataset check can report it as missing (dry-run warns).
	return p, nil
}

func hostPathDatasetExists(ctx context.Context, client *truenas.Client, p string) (bool, error) {
	missing, err := verifyDatasetPathsExist(ctx, client, []string{p})
	if err != nil {
		return false, err
	}
	return len(missing) == 0, nil
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
func verifyDatasetPathsExist(ctx context.Context, client *truenas.Client, paths []string) ([]string, error) {
	missing := []string{}

	for _, path := range paths {
		_, dataset, err := parseStoragePath(path)
		if err != nil {
			return nil, err
		}

		result, err := client.Call(ctx, "pool.dataset.query",
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
