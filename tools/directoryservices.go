package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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

// dsJobTTL is how long the task of a directoryservices.update or
// directoryservices.leave job is tracked. A first join or a leave talks to
// domain controllers and can take several minutes.
const dsJobTTL = 15 * time.Minute

// parseJobID returns the job id from the result of a job method: a bare
// integer, or a list whose first element is the id. It returns 0 if there is
// none.
func parseJobID(result json.RawMessage) int {
	var jobID int
	if err := json.Unmarshal(result, &jobID); err == nil {
		return jobID
	}
	var jobIDs []int
	if err := json.Unmarshal(result, &jobIDs); err == nil && len(jobIDs) > 0 {
		return jobIDs[0]
	}
	return 0
}

func (r *Registry) handleConfigureDirectoryService(client *truenas.Client, args map[string]interface{}) (string, error) {
	payload, err := buildDirectoryServiceUpdatePayload(args)
	if err != nil {
		return "", err
	}
	serviceType, _ := payload["service_type"].(string)
	enabled, _ := payload["enable"].(bool)
	label := serviceType
	if label == "" {
		label = "the configured directory service"
	}

	// directoryservices.update is a job: the call returns its job ID.
	result, err := client.Call("directoryservices.update", payload)
	if err != nil {
		return "", fmt.Errorf("failed to configure %s: %w", label, err)
	}

	jobID := parseJobID(result)

	response := map[string]interface{}{
		"success": true,
		"enabled": enabled,
	}
	if serviceType != "" {
		response["directory_type"] = serviceType
	}
	if enabled {
		response["message"] = fmt.Sprintf("%s configuration was submitted to TrueNAS. Enabling it for the first time joins the domain, which may take a few minutes.", serviceType)
	} else {
		response["message"] = fmt.Sprintf("Disabling %s was submitted to TrueNAS; its settings are kept. Domain authentication stops until it is enabled again.", label)
	}

	if jobID > 0 {
		// Never keep the credential values in the stored task arguments.
		task, err := r.taskManager.CreateJobTask(
			"configure_directory_service",
			maskDirectoryServicePayload(args),
			jobID,
			dsJobTTL,
		)
		if err != nil {
			return "", fmt.Errorf("directoryservices.update was submitted as job %d but task creation failed: %w", jobID, err)
		}
		response["task_id"] = task.TaskID
		response["job_id"] = jobID
	} else {
		response["warning"] = "TrueNAS did not return a job ID, so progress cannot be tracked; check get_directory_service_status."
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func (r *Registry) handleLeaveDirectoryService(client *truenas.Client, args map[string]interface{}) (string, error) {
	ctx := context.Background()

	payload, err := buildDirectoryServiceLeavePayload(args)
	if err != nil {
		return "", err
	}

	// Check current status
	status, err := getDirectoryServiceStatus(ctx, client)
	if err != nil {
		return "", err
	}
	if err := checkDirectoryServiceCanLeave(status); err != nil {
		return "", err
	}

	// directoryservices.leave is a job: the call returns its job ID.
	result, err := client.Call("directoryservices.leave", payload)
	if err != nil {
		return "", fmt.Errorf("failed to leave %s: %w", status.Type, err)
	}

	jobID := parseJobID(result)

	response := map[string]interface{}{
		"success":        true,
		"directory_type": status.Type,
		"message":        fmt.Sprintf("Leaving %s was submitted to TrueNAS; track the task for the result.", status.Type),
	}

	if jobID > 0 {
		// Never keep the credential values in the stored task arguments.
		task, err := r.taskManager.CreateJobTask(
			"leave_directory_service",
			maskDirectoryServicePayload(args),
			jobID,
			dsJobTTL,
		)
		if err != nil {
			return "", fmt.Errorf("directoryservices.leave was submitted as job %d but task creation failed: %w", jobID, err)
		}
		response["task_id"] = task.TaskID
		response["job_id"] = jobID
	} else {
		response["warning"] = "TrueNAS did not return a job ID, so progress cannot be tracked; check get_directory_service_status."
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// DryRunnable implementations

type configureDirectoryServiceDryRun struct{}

func (d *configureDirectoryServiceDryRun) ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	ctx := context.Background()

	payload, err := buildDirectoryServiceUpdatePayload(args)
	if err != nil {
		return nil, err
	}
	serviceType, _ := payload["service_type"].(string)
	enabled, _ := payload["enable"].(bool)
	credType := ""
	if cred, ok := payload["credential"].(map[string]interface{}); ok {
		credType, _ = cred["credential_type"].(string)
	}

	// Get current status
	status, err := getDirectoryServiceStatus(ctx, client)
	if err != nil {
		return nil, err
	}

	// A bare disable names no service_type: it applies to what is configured.
	target := serviceType
	nothingToDisable := serviceType == "" && status.Type == "none"
	if target == "" {
		target = status.Type
	}
	if nothingToDisable {
		target = "the configured directory service"
	}

	warnings := []string{}

	if nothingToDisable {
		warnings = append(warnings, "No directory service is currently configured; there is nothing to disable")
	}
	if serviceType != "" && status.Type != "none" && status.Type != serviceType {
		warnings = append(warnings,
			fmt.Sprintf("CONFLICT: %s is currently configured and will be replaced by %s", status.Type, serviceType))
	}

	if enabled {
		warnings = append(warnings,
			fmt.Sprintf("Directory service will be enabled; if TrueNAS has not joined this %s domain/server yet it will attempt to join", serviceType))
		warnings = append(warnings,
			"Network connectivity to domain controllers/LDAP servers is required")
		warnings = append(warnings,
			"DNS must be properly configured to resolve domain/LDAP servers")
		if serviceType != "LDAP" && credType != "KERBEROS_USER" {
			warnings = append(warnings,
				"The first join of an Active Directory or IPA domain requires a KERBEROS_USER credential with administrator privileges")
		}
	}
	if force, _ := payload["force"].(bool); force {
		warnings = append(warnings,
			"force=true bypasses the check for an existing server with this hostname registered in the domain; it can replace that server")
	}
	if !enabled && !nothingToDisable {
		warnings = append(warnings,
			fmt.Sprintf("Disabling %s stops domain/directory authentication: its users and groups stop resolving, and SMB/NFS access that depends on them fails until the service is enabled again", target),
			"The settings are kept and the computer account is not removed from the domain; use leave_directory_service to leave an Active Directory or IPA domain")
	}
	if credType != "" {
		warnings = append(warnings,
			fmt.Sprintf("Credential type %s will be sent; secret values are masked in this preview", credType))
	}

	operation, verb := "configure", "configure"
	if !enabled {
		operation, verb = "disable", "disable"
	}
	actions := []PlannedAction{
		{
			Step:        1,
			Description: fmt.Sprintf("Call directoryservices.update to %s %s", verb, target),
			Operation:   operation,
			Target:      target,
			Details: map[string]interface{}{
				"method":  "directoryservices.update",
				"enabled": enabled,
				// Exactly what will be sent to TrueNAS, with secret values masked.
				"payload": maskDirectoryServicePayload(payload),
			},
		},
	}

	if enabled {
		actions = append(actions, PlannedAction{
			Step:        2,
			Description: fmt.Sprintf("Join %s domain/server if not already joined", serviceType),
			Operation:   "join",
			Target:      serviceType,
		})
	}

	var conditions []string
	if enabled {
		conditions = []string{
			fmt.Sprintf("Connectivity to %s servers", serviceType),
			"Proper DNS configuration",
			"Firewall rules allowing directory service traffic",
			fmt.Sprintf("Valid %s credentials", credType),
		}
	} else {
		conditions = []string{"A directory service is configured on TrueNAS"}
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

	payload, err := buildDirectoryServiceLeavePayload(args)
	if err != nil {
		return nil, err
	}
	cred, _ := payload["credential"].(map[string]interface{})
	username, _ := cred["username"].(string)

	// Get current status
	status, err := getDirectoryServiceStatus(ctx, client)
	if err != nil {
		return nil, err
	}
	if err := checkDirectoryServiceCanLeave(status); err != nil {
		return nil, err
	}

	warnings := []string{
		fmt.Sprintf("System will leave the %s domain/server", status.Type),
		"Domain users will no longer be able to authenticate",
		"Shares with domain ACLs may become inaccessible",
		"This operation cannot be undone automatically - rejoin will be required",
		fmt.Sprintf("The %s credential for %q is sent to directoryservices.leave; the password is masked in this preview", dsLeaveCredentialType, username),
	}

	actions := []PlannedAction{
		{
			Step:        1,
			Description: fmt.Sprintf("Call directoryservices.leave to leave the %s domain", status.Type),
			Operation:   "leave",
			Target:      status.Type,
			Details: map[string]interface{}{
				"method": "directoryservices.leave",
				// Exactly what will be sent to TrueNAS, with the password masked.
				"payload": maskDirectoryServicePayload(payload),
			},
		},
		{
			Step:        2,
			Description: fmt.Sprintf("TrueNAS then resets its local %s configuration and disables the service", status.Type),
			Operation:   "disable",
			Target:      status.Type,
		},
	}

	requirements := &Requirements{
		Conditions: []string{
			fmt.Sprintf("Connectivity to %s servers", status.Type),
			"Proper DNS configuration",
			"A KERBEROS_USER credential with the administrative privileges needed to remove the computer account",
		},
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
