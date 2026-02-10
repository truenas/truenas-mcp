package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/truenas/truenas-mcp/truenas"
)

// Type definitions for directory services

type DirectoryServiceStatus struct {
	Type          string                 `json:"type"`
	Enabled       bool                   `json:"enabled"`
	Healthy       bool                   `json:"healthy"`
	Configuration map[string]interface{} `json:"configuration,omitempty"`
	Status        map[string]interface{} `json:"status,omitempty"`
}

type DirectoryServiceConfig struct {
	Type          string                 `json:"type"`
	Domain        string                 `json:"domain,omitempty"`
	Hostname      string                 `json:"hostname,omitempty"`
	Basedn        string                 `json:"basedn,omitempty"`
	BindDN        string                 `json:"binddn,omitempty"`
	Kerberos      map[string]interface{} `json:"kerberos,omitempty"`
	SSL           string                 `json:"ssl,omitempty"`
	CertID        int                    `json:"cert_id,omitempty"`
	Enabled       bool                   `json:"enabled"`
	Configuration map[string]interface{} `json:"configuration,omitempty"`
}

type SimplifiedDirectoryConfig struct {
	Type     string `json:"type"`
	Enabled  bool   `json:"enabled"`
	Domain   string `json:"domain,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Basedn   string `json:"basedn,omitempty"`
	SSL      string `json:"ssl,omitempty"`
	HasCert  bool   `json:"has_cert"`
	Kerberos bool   `json:"kerberos_enabled"`
}

// Helper functions

func getDirectoryServiceStatus(ctx context.Context, client *truenas.Client) (*DirectoryServiceStatus, error) {
	// Use the unified directoryservices.status API
	result, err := client.Call("directoryservices.status")
	if err != nil {
		return nil, fmt.Errorf("failed to query directory service status: %w", err)
	}

	var statusData map[string]interface{}
	if err := json.Unmarshal(result, &statusData); err != nil {
		return nil, fmt.Errorf("failed to parse status: %w", err)
	}

	// Extract type, status, and status_msg from the response
	// API returns: {type: "ACTIVEDIRECTORY"|"IPA"|"LDAP"|null, status: "DISABLED"|"FAULTED"|"LEAVING"|"JOINING"|"HEALTHY"|null, status_msg: string|null}

	typeVal := "none"
	if t, ok := statusData["type"].(string); ok && t != "" {
		typeVal = t
	}

	statusVal := "DISABLED"
	if s, ok := statusData["status"].(string); ok && s != "" {
		statusVal = s
	}

	statusMsg := ""
	if msg, ok := statusData["status_msg"].(string); ok {
		statusMsg = msg
	}

	// Determine if service is enabled and healthy
	enabled := statusVal != "DISABLED" && typeVal != "none"
	healthy := statusVal == "HEALTHY"

	response := &DirectoryServiceStatus{
		Type:    typeVal,
		Enabled: enabled,
		Healthy: healthy,
		Status: map[string]interface{}{
			"status":     statusVal,
			"status_msg": statusMsg,
		},
	}

	return response, nil
}

func maskCredentials(config map[string]interface{}) map[string]interface{} {
	masked := make(map[string]interface{})
	for k, v := range config {
		// Mask sensitive fields
		if k == "bindpw" || k == "password" || k == "secret" {
			if v != nil && v != "" {
				masked[k] = "***MASKED***"
			}
		} else {
			masked[k] = v
		}
	}
	return masked
}

func simplifyDirectoryConfig(config map[string]interface{}, dsType string) SimplifiedDirectoryConfig {
	simple := SimplifiedDirectoryConfig{
		Type: dsType,
	}

	if enabled, ok := config["enable"].(bool); ok {
		simple.Enabled = enabled
	}

	if dsType == "activedirectory" {
		if domain, ok := config["domainname"].(string); ok {
			simple.Domain = domain
		}
		if kerberos, ok := config["kerberos_realm"].(float64); ok && kerberos > 0 {
			simple.Kerberos = true
		}
		if ssl, ok := config["ssl"].(string); ok {
			simple.SSL = ssl
		}
		if certID, ok := config["certificate"].(float64); ok && certID > 0 {
			simple.HasCert = true
		}
	} else if dsType == "ldap" {
		if basedn, ok := config["basedn"].(string); ok {
			simple.Basedn = basedn
		}
		if hostname, ok := config["hostname"].([]interface{}); ok && len(hostname) > 0 {
			if host, ok := hostname[0].(string); ok {
				simple.Hostname = host
			}
		}
		if ssl, ok := config["ssl"].(string); ok {
			simple.SSL = ssl
		}
		if certID, ok := config["certificate"].(float64); ok && certID > 0 {
			simple.HasCert = true
		}
		if kerberosRealm, ok := config["kerberos_realm"].(float64); ok && kerberosRealm > 0 {
			simple.Kerberos = true
		}
	}

	return simple
}

func validateDirectoryCredentials(args map[string]interface{}, dsType string) error {
	if dsType == "activedirectory" {
		domain, hasDomain := args["domain"].(string)
		if !hasDomain || domain == "" {
			return fmt.Errorf("domain is required for Active Directory")
		}

		bindname, hasBindname := args["bindname"].(string)
		bindpw, hasBindpw := args["bindpw"].(string)

		if (hasBindname && bindname != "") != (hasBindpw && bindpw != "") {
			return fmt.Errorf("both bindname and bindpw must be provided together")
		}
	} else if dsType == "ldap" {
		basedn, hasBasedn := args["basedn"].(string)
		if !hasBasedn || basedn == "" {
			return fmt.Errorf("basedn is required for LDAP")
		}

		hostname, hasHostname := args["hostname"].([]interface{})
		if !hasHostname || len(hostname) == 0 {
			return fmt.Errorf("hostname is required for LDAP")
		}

		binddn, hasBinddn := args["binddn"].(string)
		bindpw, hasBindpw := args["bindpw"].(string)

		if (hasBinddn && binddn != "") != (hasBindpw && bindpw != "") {
			return fmt.Errorf("both binddn and bindpw must be provided together")
		}
	}

	return nil
}

func checkDirectoryServiceForShareWarnings(ctx context.Context, client *truenas.Client) []string {
	warnings := []string{}

	status, err := getDirectoryServiceStatus(ctx, client)
	if err != nil {
		// Don't fail share creation if we can't check directory service
		return warnings
	}

	if status.Enabled && !status.Healthy {
		warnings = append(warnings,
			fmt.Sprintf("NOTICE: %s is enabled but not healthy - users may not authenticate properly", status.Type))
	}

	if status.Enabled && status.Type == "activedirectory" {
		warnings = append(warnings,
			"NOTICE: Active Directory is enabled - ensure ACLs are configured for domain users/groups")
	}

	if status.Enabled && status.Type == "ldap" {
		warnings = append(warnings,
			"NOTICE: LDAP is enabled - ensure POSIX attributes are configured for LDAP users")
	}

	return warnings
}

// GetTrueNASClient returns a TrueNAS client (stub for share handlers)
func GetTrueNASClient() (*truenas.Client, error) {
	// This is called from share handlers but they already have a client
	// Return error as this shouldn't be used directly
	return nil, fmt.Errorf("client should be passed directly")
}

// Read-only handlers

func handleGetDirectoryServiceStatus(client *truenas.Client, args map[string]interface{}) (string, error) {
	ctx := context.Background()
	status, err := getDirectoryServiceStatus(ctx, client)
	if err != nil {
		return "", err
	}

	response := map[string]interface{}{
		"directory_service": status,
	}

	if status.Type != "none" {
		response["message"] = fmt.Sprintf("%s is %s", status.Type, map[bool]string{true: "enabled and healthy", false: "enabled but not healthy"}[status.Healthy])
	} else {
		response["message"] = "No directory service configured"
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleQueryDirectoryServices(client *truenas.Client, args map[string]interface{}) (string, error) {
	ctx := context.Background()

	// Use the unified directoryservices.config API
	result, err := client.Call("directoryservices.config")
	if err != nil {
		return "", fmt.Errorf("failed to query directory service config: %w", err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(result, &config); err != nil {
		return "", fmt.Errorf("failed to parse config: %w", err)
	}

	// Get current status
	status, err := getDirectoryServiceStatus(ctx, client)
	if err != nil {
		return "", err
	}

	// Mask sensitive credentials
	maskedConfig := maskCredentials(config)

	response := map[string]interface{}{
		"configuration":   maskedConfig,
		"current_service": status.Type,
		"enabled":         status.Enabled,
		"healthy":         status.Healthy,
		"status":          status.Status,
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleListDirectoryCertificates(client *truenas.Client, args map[string]interface{}) (string, error) {
	// Query all certificates
	result, err := client.Call("certificate.query")
	if err != nil {
		return "", fmt.Errorf("failed to query certificates: %w", err)
	}

	var certs []map[string]interface{}
	if err := json.Unmarshal(result, &certs); err != nil {
		return "", fmt.Errorf("failed to parse certificates: %w", err)
	}

	simplified := []map[string]interface{}{}
	for _, cert := range certs {
		id, _ := cert["id"].(float64)
		name, _ := cert["name"].(string)
		from, _ := cert["from"].(string)
		until, _ := cert["until"].(string)
		issuer, _ := cert["issuer"].(string)

		simplified = append(simplified, map[string]interface{}{
			"id":     int(id),
			"name":   name,
			"from":   from,
			"until":  until,
			"issuer": issuer,
		})
	}

	response := map[string]interface{}{
		"certificates": simplified,
		"count":        len(simplified),
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleRefreshDirectoryCache(client *truenas.Client, args map[string]interface{}) (string, error) {
	ctx := context.Background()

	// Check which directory service is enabled
	status, err := getDirectoryServiceStatus(ctx, client)
	if err != nil {
		return "", err
	}

	if status.Type == "none" {
		return "", fmt.Errorf("no directory service is configured")
	}

	if !status.Enabled {
		return "", fmt.Errorf("%s is not enabled", status.Type)
	}

	// Call unified cache refresh method
	_, err = client.Call("directoryservices.cache_refresh")
	if err != nil {
		return "", fmt.Errorf("failed to refresh cache: %w", err)
	}

	response := map[string]interface{}{
		"success":         true,
		"directory_type":  status.Type,
		"cache_refreshed": true,
		"message":         fmt.Sprintf("%s cache has been refreshed", status.Type),
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// Registry write handlers

func (r *Registry) handleConfigureDirectoryService(client *truenas.Client, args map[string]interface{}) (string, error) {
	dsType, ok := args["type"].(string)
	if !ok || (dsType != "activedirectory" && dsType != "ldap") {
		return "", fmt.Errorf("type must be 'activedirectory' or 'ldap'")
	}

	// Validate credentials
	if err := validateDirectoryCredentials(args, dsType); err != nil {
		return "", err
	}

	// Build payload based on type
	payload := make(map[string]interface{})

	if dsType == "activedirectory" {
		domain, _ := args["domain"].(string)
		payload["domainname"] = domain

		if bindname, ok := args["bindname"].(string); ok && bindname != "" {
			payload["bindname"] = bindname
		}
		if bindpw, ok := args["bindpw"].(string); ok && bindpw != "" {
			payload["bindpw"] = bindpw
		}
		if netbiosname, ok := args["netbiosname"].(string); ok && netbiosname != "" {
			payload["netbiosname"] = netbiosname
		}
		if kerberosRealm, ok := args["kerberos_realm"].(float64); ok && kerberosRealm > 0 {
			payload["kerberos_realm"] = int(kerberosRealm)
		}
		if ssl, ok := args["ssl"].(string); ok && ssl != "" {
			payload["ssl"] = ssl
		}
		if certID, ok := args["certificate"].(float64); ok && certID > 0 {
			payload["certificate"] = int(certID)
		}
		if enabled, ok := args["enable"].(bool); ok {
			payload["enable"] = enabled
		} else {
			payload["enable"] = true
		}
	} else if dsType == "ldap" {
		basedn, _ := args["basedn"].(string)
		payload["basedn"] = basedn

		if hostname, ok := args["hostname"].([]interface{}); ok && len(hostname) > 0 {
			payload["hostname"] = hostname
		}
		if binddn, ok := args["binddn"].(string); ok && binddn != "" {
			payload["binddn"] = binddn
		}
		if bindpw, ok := args["bindpw"].(string); ok && bindpw != "" {
			payload["bindpw"] = bindpw
		}
		if ssl, ok := args["ssl"].(string); ok && ssl != "" {
			payload["ssl"] = ssl
		}
		if certID, ok := args["certificate"].(float64); ok && certID > 0 {
			payload["certificate"] = int(certID)
		}
		if kerberosRealm, ok := args["kerberos_realm"].(float64); ok && kerberosRealm > 0 {
			payload["kerberos_realm"] = int(kerberosRealm)
		}
		if enabled, ok := args["enable"].(bool); ok {
			payload["enable"] = enabled
		} else {
			payload["enable"] = true
		}
	}

	// Add service_type to payload for the unified API
	payload["service_type"] = strings.ToUpper(dsType)

	// Call unified update method
	result, err := client.Call("directoryservices.update", payload)
	if err != nil {
		return "", fmt.Errorf("failed to configure %s: %w", dsType, err)
	}

	var config map[string]interface{}
	if err := json.Unmarshal(result, &config); err != nil {
		return "", fmt.Errorf("failed to parse result: %w", err)
	}

	// Find the recent job for this operation
	jobID, err := findRecentJob(client, "directoryservices.update")
	if err != nil {
		// Job tracking is optional
		jobID = 0
	}

	var taskID string
	if jobID > 0 {
		// Create task for tracking
		task, err := r.taskManager.CreateJobTask(
			"configure_directory_service",
			args,
			jobID,
			10*60*1000, // 10 minutes
		)
		if err != nil {
			return "", fmt.Errorf("failed to create task: %w", err)
		}
		taskID = task.TaskID
	}

	response := map[string]interface{}{
		"success":        true,
		"directory_type": dsType,
		"enabled":        payload["enable"],
		"message":        fmt.Sprintf("%s has been configured. May take a few moments to join domain.", dsType),
	}

	if taskID != "" {
		response["task_id"] = taskID
		response["job_id"] = jobID
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func (r *Registry) handleLeaveDirectoryService(client *truenas.Client, args map[string]interface{}) (string, error) {
	ctx := context.Background()

	// Check current status
	status, err := getDirectoryServiceStatus(ctx, client)
	if err != nil {
		return "", err
	}

	if status.Type == "none" {
		return "", fmt.Errorf("no directory service is currently configured")
	}

	if !status.Enabled {
		return "", fmt.Errorf("%s is configured but not enabled", status.Type)
	}

	// Call unified leave method (no parameters needed)
	result, err := client.Call("directoryservices.leave")
	if err != nil {
		return "", fmt.Errorf("failed to leave %s: %w", status.Type, err)
	}

	// The leave operation may return a job ID or just success
	var jobID int
	if err := json.Unmarshal(result, &jobID); err != nil {
		// Not a job, just success
		jobID = 0
	}

	var taskID string
	if jobID > 0 {
		// Create task for tracking
		task, err := r.taskManager.CreateJobTask(
			"leave_directory_service",
			args,
			jobID,
			5*60*1000, // 5 minutes
		)
		if err != nil {
			return "", fmt.Errorf("failed to create task: %w", err)
		}
		taskID = task.TaskID
	}

	response := map[string]interface{}{
		"success":        true,
		"directory_type": status.Type,
		"message":        fmt.Sprintf("Successfully left %s", status.Type),
	}

	if taskID != "" {
		response["task_id"] = taskID
		response["job_id"] = jobID
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// Helper function to find recent job

func findRecentJob(client *truenas.Client, method string) (int, error) {
	result, err := client.Call("core.get_jobs", []interface{}{
		[]interface{}{"method", "=", method},
	})
	if err != nil {
		return 0, fmt.Errorf("failed to query jobs: %w", err)
	}

	var jobs []map[string]interface{}
	if err := json.Unmarshal(result, &jobs); err != nil {
		return 0, fmt.Errorf("failed to parse jobs: %w", err)
	}

	if len(jobs) == 0 {
		return 0, fmt.Errorf("no job found for method %s", method)
	}

	// Return most recent job
	latestJob := jobs[0]
	for _, job := range jobs {
		if timeStarted, ok := job["time_started"].(map[string]interface{}); ok {
			if latestStarted, ok := latestJob["time_started"].(map[string]interface{}); ok {
				if timeStarted["$date"].(float64) > latestStarted["$date"].(float64) {
					latestJob = job
				}
			}
		}
	}

	jobID, _ := latestJob["id"].(float64)
	return int(jobID), nil
}

// DryRunnable implementations

type configureDirectoryServiceDryRun struct{}

func (d *configureDirectoryServiceDryRun) ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	ctx := context.Background()

	dsType, ok := args["type"].(string)
	if !ok || (dsType != "activedirectory" && dsType != "ldap") {
		return nil, fmt.Errorf("type must be 'activedirectory' or 'ldap'")
	}

	// Validate credentials
	if err := validateDirectoryCredentials(args, dsType); err != nil {
		return nil, err
	}

	// Get current status
	status, err := getDirectoryServiceStatus(ctx, client)
	if err != nil {
		return nil, err
	}

	warnings := []string{}
	enabled := getOptionalBool(args, "enable", true)

	if status.Type != "none" && status.Type != dsType {
		warnings = append(warnings,
			fmt.Sprintf("CONFLICT: %s is currently configured and will be replaced by %s", status.Type, dsType))
	}

	if enabled {
		warnings = append(warnings,
			fmt.Sprintf("Directory service will be enabled and system will attempt to join %s", dsType))
		warnings = append(warnings,
			"Network connectivity to domain controllers/LDAP servers is required")
		warnings = append(warnings,
			"DNS must be properly configured to resolve domain/LDAP servers")
	}

	credFields := getCredentialFields(args, dsType)
	if len(credFields) > 0 {
		warnings = append(warnings,
			fmt.Sprintf("Credentials provided for: %v", credFields))
	} else {
		warnings = append(warnings,
			"No credentials provided - anonymous bind will be attempted (may fail)")
	}

	actions := []PlannedAction{
		{
			Step:        1,
			Description: fmt.Sprintf("Configure %s settings", dsType),
			Operation:   "configure",
			Target:      dsType,
			Details: map[string]interface{}{
				"enabled": enabled,
			},
		},
	}

	if enabled {
		actions = append(actions, PlannedAction{
			Step:        2,
			Description: fmt.Sprintf("Join %s domain/server", dsType),
			Operation:   "join",
			Target:      dsType,
		})
	}

	conditions := []string{
		fmt.Sprintf("Connectivity to %s servers", dsType),
		"Proper DNS configuration",
		"Firewall rules allowing directory service traffic",
	}
	if len(credFields) > 0 {
		conditions = append(conditions, fmt.Sprintf("Valid credentials: %v", credFields))
	}

	requirements := &Requirements{
		Conditions: conditions,
	}

	return &DryRunResult{
		Tool: "configure_directory_service",
		CurrentState: map[string]interface{}{
			"current_service": status.Type,
			"current_enabled": status.Enabled,
			"current_healthy": status.Healthy,
		},
		PlannedActions: actions,
		Warnings:       warnings,
		Requirements:   requirements,
		EstimatedTime: &EstimatedTime{
			MinSeconds: 10,
			MaxSeconds: 120,
			Note:       "Time varies based on network and domain controller response",
		},
	}, nil
}

type leaveDirectoryServiceDryRun struct{}

func (d *leaveDirectoryServiceDryRun) ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	ctx := context.Background()

	// Get current status
	status, err := getDirectoryServiceStatus(ctx, client)
	if err != nil {
		return nil, err
	}

	if status.Type == "none" {
		return nil, fmt.Errorf("no directory service is currently configured")
	}

	if !status.Enabled {
		return nil, fmt.Errorf("%s is configured but not enabled", status.Type)
	}

	warnings := []string{
		fmt.Sprintf("System will leave the %s domain/server", status.Type),
		"Domain users will no longer be able to authenticate",
		"Shares with domain ACLs may become inaccessible",
		"This operation cannot be undone automatically - rejoin will be required",
	}

	username, hasUsername := args["username"].(string)
	password, hasPassword := args["password"].(string)

	credFields := []string{}
	if hasUsername && username != "" {
		credFields = append(credFields, "username")
	}
	if hasPassword && password != "" {
		credFields = append(credFields, "password")
	}

	if len(credFields) > 0 {
		warnings = append(warnings,
			"Credentials provided - will perform authenticated leave")
	} else {
		warnings = append(warnings,
			"No credentials provided - will attempt force leave (may leave stale objects in directory)")
	}

	actions := []PlannedAction{
		{
			Step:        1,
			Description: fmt.Sprintf("Leave %s domain/server", status.Type),
			Operation:   "leave",
			Target:      status.Type,
		},
		{
			Step:        2,
			Description: fmt.Sprintf("Disable %s integration", status.Type),
			Operation:   "disable",
			Target:      status.Type,
		},
	}

	conditions := []string{
		fmt.Sprintf("Connectivity to %s servers (if authenticated leave)", status.Type),
		"Proper DNS configuration (if authenticated leave)",
	}
	if len(credFields) > 0 {
		conditions = append(conditions, fmt.Sprintf("Valid credentials: %v", credFields))
	}

	requirements := &Requirements{
		Conditions: conditions,
	}

	return &DryRunResult{
		Tool: "leave_directory_service",
		CurrentState: map[string]interface{}{
			"directory_type": status.Type,
			"enabled":        status.Enabled,
			"healthy":        status.Healthy,
		},
		PlannedActions: actions,
		Warnings:       warnings,
		Requirements:   requirements,
		EstimatedTime: &EstimatedTime{
			MinSeconds: 5,
			MaxSeconds: 60,
			Note:       "Time varies based on network and domain controller response",
		},
	}, nil
}

// WithDryRun wrappers

func (r *Registry) handleConfigureDirectoryServiceWithDryRun(client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(client, args, &configureDirectoryServiceDryRun{}, r.handleConfigureDirectoryService)
}

func (r *Registry) handleLeaveDirectoryServiceWithDryRun(client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(client, args, &leaveDirectoryServiceDryRun{}, r.handleLeaveDirectoryService)
}

// Helper functions for dry-run

func getCredentialFields(args map[string]interface{}, dsType string) []string {
	fields := []string{}

	if dsType == "activedirectory" {
		if bindname, ok := args["bindname"].(string); ok && bindname != "" {
			fields = append(fields, "bindname")
		}
		if bindpw, ok := args["bindpw"].(string); ok && bindpw != "" {
			fields = append(fields, "bindpw")
		}
	} else if dsType == "ldap" {
		if binddn, ok := args["binddn"].(string); ok && binddn != "" {
			fields = append(fields, "binddn")
		}
		if bindpw, ok := args["bindpw"].(string); ok && bindpw != "" {
			fields = append(fields, "bindpw")
		}
	}

	return fields
}

func getOptionalBool(args map[string]interface{}, key string, defaultVal bool) bool {
	if val, ok := args[key].(bool); ok {
		return val
	}
	return defaultVal
}

func getOptionalInt(args map[string]interface{}, key string, defaultVal int) int {
	if val, ok := args[key].(float64); ok {
		return int(val)
	}
	return defaultVal
}
