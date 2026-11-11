package tools

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

func (r *Registry) registerDirectoryServiceTools() {
	// Directory Services
	r.tools["get_directory_service_status"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_directory_service_status",
			Description: "Get current directory service status and health. Returns service type (ACTIVEDIRECTORY, IPA, LDAP), status (DISABLED, HEALTHY, FAULTED, JOINING, LEAVING), and error messages if any. Use for quick health checks.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleGetDirectoryServiceStatus,
	}

	r.tools["query_directory_services"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_directory_services",
			Description: "Query full directory service configuration. Returns service type, enabled status, credentials (masked for security), and service-specific settings. All passwords and keytabs are masked in output.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleQueryDirectoryServices,
	}

	r.tools["list_directory_certificates"] = Tool{
		Definition: mcp.Tool{
			Name:        "list_directory_certificates",
			Description: "List available certificates for LDAP MTLS authentication. Returns certificate IDs and names that can be used with LDAP_MTLS credential type.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleListDirectoryCertificates,
	}

	r.tools["refresh_directory_cache"] = Tool{
		Definition: mcp.Tool{
			Name:        "refresh_directory_cache",
			Description: "Refresh cached user and group data from the directory service. Use after making changes in Active Directory, LDAP, or IPA that need to be reflected immediately in TrueNAS. Supports dry-run mode.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview without refreshing (default: false)",
						"default":     false,
					},
				},
			},
		},
		Handler: r.handleRefreshDirectoryCacheWithDryRun,
		Plan:    planRefreshDirectoryCache,
	}

	r.tools["configure_directory_service"] = Tool{
		Definition: mcp.Tool{
			Name: "configure_directory_service",
			Description: `Configure and join a directory service (Active Directory, LDAP, or IPA) with TrueNAS directoryservices.update (TrueNAS 25.10 or later). Setting enable=true joins the domain automatically the first time. The arguments are the fields of that API call. Omitted optional fields keep their current value.

**To enable or change a service, service_type, credential, configuration and enable are all required, and configuration must match service_type. The old flat arguments (type, server, bindname, netbiosname, ssl, domain, ...) no longer exist: put them in credential and configuration as below.**

**To disable the configured service, send only enable=false** (no service_type, credential or configuration): the settings are kept and domain authentication stops until it is enabled again. When service_type is sent with enable=false, a KERBEROS_USER credential is rejected for ACTIVEDIRECTORY and IPA (TrueNAS does not store it); use KERBEROS_PRINCIPAL.

**Credential (credential_type and its fields):**

Active Directory and IPA (Kerberos only; the first join needs KERBEROS_USER with domain admin rights):
- KERBEROS_USER: {credential_type: "KERBEROS_USER", username: "admin", password: "pass"}
- KERBEROS_PRINCIPAL: {credential_type: "KERBEROS_PRINCIPAL", principal: "host/truenas@EXAMPLE.COM"} (the principal must already be in a keytab on TrueNAS)

LDAP (also accepts the Kerberos types above, which need kerberos_realm to be set):
- LDAP_PLAIN: {credential_type: "LDAP_PLAIN", binddn: "cn=admin,dc=example,dc=com", bindpw: "pass"}
- LDAP_ANONYMOUS: {credential_type: "LDAP_ANONYMOUS"}
- LDAP_MTLS: {credential_type: "LDAP_MTLS", client_certificate: "name of a certificate on TrueNAS"}

**Configuration (fields per service_type; * = required):**
- ACTIVEDIRECTORY: hostname*, domain*, site, computer_account_ou, use_default_domain, enable_trusted_domains, trusted_domains, idmap
  e.g. {hostname: "truenas", domain: "corp.example.com"}
- IPA: target_server*, hostname*, domain*, basedn*, smb_domain, validate_certificates
  e.g. {target_server: "ipa.example.com", hostname: "truenas", domain: "example.com", basedn: "dc=example,dc=com"}
- LDAP: server_urls* (ldap:// or ldaps:// URIs), basedn*, starttls, validate_certificates, schema (RFC2307 or RFC2307BIS), search_bases, attribute_maps, auxiliary_parameters
  e.g. {server_urls: ["ldaps://ldap.example.com"], basedn: "dc=example,dc=com"}
idmap, trusted_domains, smb_domain, search_bases and attribute_maps are passed to TrueNAS unchanged; see the TrueNAS API docs for directoryservices.update for their fields.

**Security:**
- Credentials are stored in TrueNAS configuration
- Use Kerberos principals with keytabs instead of passwords for production
- Dry-run shows the exact payload that would be sent with every secret value masked (password, bindpw, ldap_user_dn_password in idmap and trusted_domains, ...); credential_type and username stay visible

**Returns:** task_id for tracking the long-running domain join job (2-10 minutes typical)`,
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"service_type": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"ACTIVEDIRECTORY", "LDAP", "IPA"},
						"description": "Directory service type. Selects which configuration fields apply.",
					},
					"enable": map[string]interface{}{
						"type":        "boolean",
						"description": "Enable the service (true joins the domain the first time; false disables it but keeps the settings)",
					},
					"credential": map[string]interface{}{
						"type":        "object",
						"description": "Credential used to bind to the directory service. credential_type selects the other fields: KERBEROS_USER (username, password), KERBEROS_PRINCIPAL (principal), LDAP_PLAIN (binddn, bindpw), LDAP_ANONYMOUS (none), LDAP_MTLS (client_certificate). LDAP_* types are only valid for service_type LDAP.",
						"properties": map[string]interface{}{
							"credential_type": map[string]interface{}{
								"type": "string",
								"enum": []string{"KERBEROS_USER", "KERBEROS_PRINCIPAL", "LDAP_PLAIN", "LDAP_ANONYMOUS", "LDAP_MTLS"},
							},
							"username":           map[string]interface{}{"type": "string", "description": "KERBEROS_USER: account used to obtain a Kerberos ticket"},
							"password":           map[string]interface{}{"type": "string", "description": "KERBEROS_USER: password of that account"},
							"principal":          map[string]interface{}{"type": "string", "description": "KERBEROS_PRINCIPAL: principal present in a keytab on TrueNAS"},
							"binddn":             map[string]interface{}{"type": "string", "description": "LDAP_PLAIN: distinguished name to bind as"},
							"bindpw":             map[string]interface{}{"type": "string", "description": "LDAP_PLAIN: password of the bind DN"},
							"client_certificate": map[string]interface{}{"type": "string", "description": "LDAP_MTLS: name of the client certificate on TrueNAS"},
						},
						"required":             []string{"credential_type"},
						"additionalProperties": false,
					},
					"configuration": map[string]interface{}{
						"type":        "object",
						"description": "service_type specific configuration; see the tool description for the fields of each service type and which are required",
						"properties": map[string]interface{}{
							"hostname":               map[string]interface{}{"type": "string", "description": "ACTIVEDIRECTORY, IPA: hostname TrueNAS registers in the domain (required)"},
							"domain":                 map[string]interface{}{"type": "string", "description": "ACTIVEDIRECTORY, IPA: DNS domain name (required)"},
							"target_server":          map[string]interface{}{"type": "string", "description": "IPA: name of the IPA server used when joining or leaving (required)"},
							"basedn":                 map[string]interface{}{"type": "string", "description": "IPA, LDAP: base DN for LDAP operations (required)"},
							"server_urls":            map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "LDAP: server URIs beginning with ldap:// or ldaps:// (required)"},
							"site":                   map[string]interface{}{"type": "string", "description": "ACTIVEDIRECTORY: Active Directory site (detected automatically when omitted)"},
							"computer_account_ou":    map[string]interface{}{"type": "string", "description": "ACTIVEDIRECTORY: OU in which the computer account is created on first join"},
							"use_default_domain":     map[string]interface{}{"type": "boolean", "description": "ACTIVEDIRECTORY: drop the domain prefix from user and group names"},
							"enable_trusted_domains": map[string]interface{}{"type": "boolean", "description": "ACTIVEDIRECTORY: support trusted domains (set trusted_domains too)"},
							"trusted_domains":        map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}, "description": "ACTIVEDIRECTORY: idmap configuration for each trusted domain"},
							"idmap":                  map[string]interface{}{"type": "object", "description": "ACTIVEDIRECTORY: idmap settings (builtin and idmap_domain), passed to TrueNAS unchanged"},
							"smb_domain":             map[string]interface{}{"type": "object", "description": "IPA: SMB domain settings, passed to TrueNAS unchanged"},
							"validate_certificates":  map[string]interface{}{"type": "boolean", "description": "IPA, LDAP: validate the directory server's certificates (TrueNAS default: true)"},
							"starttls":               map[string]interface{}{"type": "boolean", "description": "LDAP: use StartTLS"},
							"schema":                 map[string]interface{}{"type": "string", "enum": []string{"RFC2307", "RFC2307BIS"}, "description": "LDAP: attribute schema of the server"},
							"search_bases":           map[string]interface{}{"type": "object", "description": "LDAP: base_user, base_group and base_netgroup search bases, passed to TrueNAS unchanged"},
							"attribute_maps":         map[string]interface{}{"type": "object", "description": "LDAP: attribute mappings for non-standard servers, passed to TrueNAS unchanged"},
							"auxiliary_parameters":   map[string]interface{}{"type": "string", "description": "LDAP: extra SSSD parameters (not validated by TrueNAS)"},
						},
						"additionalProperties": false,
					},
					"enable_account_cache": map[string]interface{}{
						"type":        "boolean",
						"description": "Cache user and group lists (TrueNAS default: true; omit to keep the current value)",
					},
					"enable_dns_updates": map[string]interface{}{
						"type":        "boolean",
						"description": "Automatic DNS updates via nsupdate (TrueNAS default: true; omit to keep the current value)",
					},
					"timeout": map[string]interface{}{
						"type":        "integer",
						"minimum":     5,
						"maximum":     60,
						"description": "DNS query and LDAP request timeout in seconds (5-60; TrueNAS default: 10)",
					},
					"kerberos_realm": map[string]interface{}{
						"type":        "string",
						"description": "Kerberos realm used to authenticate to the directory service. Required for Kerberos credentials with service_type LDAP; detected automatically when first joining AD or IPA.",
					},
					"force": map[string]interface{}{
						"type":        "boolean",
						"description": "Skip the check for an existing server registered with the same hostname in the domain. Can replace that server; do not use casually.",
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview configuration without executing (default: false)",
						"default":     false,
					},
				},
				"required": []string{"enable"},
			},
		},
		Handler: r.handleConfigureDirectoryServiceWithDryRun,
		Plan:    planConfigureDirectoryService,
		Execute: r.executeConfigureDirectoryService,
	}

	r.tools["leave_directory_service"] = Tool{
		Definition: mcp.Tool{
			Name: "leave_directory_service",
			Description: `Leave the Active Directory or IPA domain TrueNAS has joined, with TrueNAS directoryservices.leave (TrueNAS 25.10 or later).

TrueNAS removes its computer account from the domain using the credential you provide, then resets its local directory service configuration and disables the service. LDAP cannot be left; use configure_directory_service with enable=false to disable it.

**credential is required:** a KERBEROS_USER credential, {credential_type: "KERBEROS_USER", username: "admin", password: "pass"}, for an account with the administrative privileges needed to remove the computer account. It is used for this operation only and is never shown: the password is masked in the dry run and in stored task data.

**WARNING:** This is a destructive operation:
- Removes TrueNAS from the domain
- Clears all cached user/group data
- All domain user authentication will stop working
- SMB/NFS shares configured with domain users will become inaccessible

**Alternative:** Use configure_directory_service with enable=false for temporary disable without leaving the domain.

**Returns:** task_id for tracking the leave job (30 seconds to 5 minutes typical)`,
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"credential": map[string]interface{}{
						"type":        "object",
						"description": "KERBEROS_USER credential used to remove the computer account from the domain",
						"properties": map[string]interface{}{
							"credential_type": map[string]interface{}{
								"type":        "string",
								"enum":        []string{"KERBEROS_USER"},
								"description": "Must be KERBEROS_USER, the only credential type directoryservices.leave accepts",
							},
							"username": map[string]interface{}{"type": "string", "description": "Account used to obtain a Kerberos ticket; it must exist on the domain controller"},
							"password": map[string]interface{}{"type": "string", "description": "Password of that account"},
						},
						"required":             []string{"credential_type", "username", "password"},
						"additionalProperties": false,
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview operation without executing (default: false, STRONGLY RECOMMENDED to use dry_run first)",
						"default":     false,
					},
				},
				"required": []string{"credential"},
			},
		},
		Handler: r.handleLeaveDirectoryServiceWithDryRun,
		Plan:    planLeaveDirectoryService,
		Execute: r.executeLeaveDirectoryService,
	}
}

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
	result, err := client.Call(ctx, "directoryservices.status")
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

	// Middleware reports type as ACTIVEDIRECTORY / LDAP / IPA.
	if status.Enabled && strings.EqualFold(status.Type, "activedirectory") {
		warnings = append(warnings,
			"NOTICE: Active Directory is enabled - ensure ACLs are configured for domain users/groups")
	}

	if status.Enabled && strings.EqualFold(status.Type, "ldap") {
		warnings = append(warnings,
			"NOTICE: LDAP is enabled - ensure POSIX attributes are configured for LDAP users")
	}

	if status.Enabled && strings.EqualFold(status.Type, "ipa") {
		warnings = append(warnings,
			"NOTICE: IPA is enabled - ensure permissions are configured for IPA users/groups")
	}

	return warnings
}

// Read-only handlers

func handleGetDirectoryServiceStatus(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
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

func handleQueryDirectoryServices(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {

	// Use the unified directoryservices.config API
	result, err := client.Call(ctx, "directoryservices.config")
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

func handleListDirectoryCertificates(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	// Query all certificates
	result, err := client.Call(ctx, "certificate.query")
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

func planRefreshDirectoryCache(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	status, err := getDirectoryServiceStatus(ctx, client)
	if err != nil {
		return nil, err
	}
	if status.Type == "none" {
		return nil, fmt.Errorf("no directory service is configured")
	}
	if !status.Enabled {
		return nil, fmt.Errorf("%s is not enabled", status.Type)
	}
	return &writePlan{
		Tool:         "refresh_directory_cache",
		Method:       directoryServicesCacheRefreshMethod,
		JobMethod:    directoryServicesCacheRefreshMethod,
		CallParams:   []interface{}{},
		TargetName:   status.Type,
		CurrentState: status,
		Actions: []PlannedAction{{
			Step:        1,
			Description: "Refresh cached directory users and groups",
			Operation:   "refresh",
			Target:      directoryServicesCacheRefreshMethod,
		}},
	}, nil
}

func (r *Registry) executeRefreshDirectoryCache(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to refresh cache: %w", err)
	}
	dirType := plan.TargetName
	jobID, jobErr := parseJobID(result)
	response := map[string]interface{}{
		"status":         "submitted",
		"directory_type": dirType,
		"message":        fmt.Sprintf("%s cache refresh was submitted to TrueNAS; track the task for the result.", dirType),
	}
	if jobErr == nil {
		opts := []tasks.TaskOption{}
		if plan.JobMethod != "" {
			opts = append(opts, tasks.WithExpectedMethod(plan.JobMethod))
		}
		task, err := r.taskManager.CreateJobTask(ctx,
			"refresh_directory_cache",
			args,
			jobID,
			dsJobTTL,
			opts...,
		)
		if err != nil {
			return "", fmt.Errorf("directoryservices.cache_refresh was submitted as job %d but task creation failed: %w", jobID, err)
		}
		response["task_id"] = task.TaskID
		response["job_id"] = jobID
		response["task_status"] = task.Status
		response["poll_interval"] = task.PollInterval
	} else {
		response["warning"] = "TrueNAS did not return a job ID, so progress cannot be tracked; check get_directory_service_status."
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

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const (
	directoryServicesUpdateMethod       = "directoryservices.update"
	directoryServicesLeaveMethod        = "directoryservices.leave"
	directoryServicesCacheRefreshMethod = "directoryservices.cache_refresh"
)

func planConfigureDirectoryService(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
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
	label := serviceType
	if label == "" {
		label = "the configured directory service"
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
				"method":  directoryServicesUpdateMethod,
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

	force, _ := payload["force"].(bool)
	return &writePlan{
		Tool:       "configure_directory_service",
		Method:     directoryServicesUpdateMethod,
		CallParams: []interface{}{payload},
		JobMethod:  directoryServicesUpdateMethod,
		ParamNames: []string{"payload"},
		CurrentState: map[string]interface{}{
			"current_service": status.Type,
			"current_enabled": status.Enabled,
			"current_healthy": status.Healthy,
		},
		Actions:  actions,
		Warnings: warnings,
		Requirements: &Requirements{
			Conditions: conditions,
		},
		EstimatedTime: &EstimatedTime{
			MinSeconds: 10,
			MaxSeconds: 120,
			Note:       "Time varies based on network and domain controller response",
		},
		Result: map[string]interface{}{
			"service_type": serviceType,
			"enabled":      enabled,
			"label":        label,
		},
		TargetName: target,
		ConfirmSummary: newConfirmSummary("Configure a directory service",
			append([]string{
				confirmFact("service_type", target),
				confirmBoolFact("enable", enabled),
				confirmBoolFact("force", force),
			}, directoryServiceTargetFacts(payload)...)...,
		),
	}, nil
}

func (r *Registry) executeConfigureDirectoryService(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}

	serviceType, _ := plan.Result["service_type"].(string)
	enabled, _ := plan.Result["enabled"].(bool)
	label, _ := plan.Result["label"].(string)
	if label == "" {
		label = "the configured directory service"
	}

	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to configure %s: %w", label, err)
	}

	jobID, jobErr := parseJobID(result)

	response := map[string]interface{}{
		"status":  "submitted",
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

	if jobErr == nil {
		// Never keep the credential values in the stored task arguments.
		opts := []tasks.TaskOption{}
		if plan.JobMethod != "" {
			opts = append(opts, tasks.WithExpectedMethod(plan.JobMethod))
		}
		task, err := r.taskManager.CreateJobTask(ctx,
			"configure_directory_service",
			maskDirectoryServicePayload(args),
			jobID,
			dsJobTTL,
			opts...,
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

func planLeaveDirectoryService(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	payload, err := buildDirectoryServiceLeavePayload(args)
	if err != nil {
		return nil, err
	}
	cred, _ := payload["credential"].(map[string]interface{})
	username, _ := cred["username"].(string)

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
				"method": directoryServicesLeaveMethod,
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

	return &writePlan{
		Tool:       "leave_directory_service",
		Method:     directoryServicesLeaveMethod,
		CallParams: []interface{}{payload},
		JobMethod:  directoryServicesLeaveMethod,
		ParamNames: []string{"payload"},
		CurrentState: map[string]interface{}{
			"directory_type": status.Type,
			"enabled":        status.Enabled,
			"healthy":        status.Healthy,
		},
		Actions:  actions,
		Warnings: warnings,
		Requirements: &Requirements{
			Conditions: []string{
				fmt.Sprintf("Connectivity to %s servers", status.Type),
				"Proper DNS configuration",
				"A KERBEROS_USER credential with the administrative privileges needed to remove the computer account",
			},
		},
		EstimatedTime: &EstimatedTime{
			MinSeconds: 5,
			MaxSeconds: 60,
			Note:       "Time varies based on network and domain controller response",
		},
		Result:     map[string]interface{}{"directory_type": status.Type},
		TargetName: status.Type,
		ConfirmSummary: newConfirmSummary("Leave a directory service",
			confirmFact("service_type", status.Type),
			confirmBoolFact("enabled", status.Enabled),
			confirmFact("status", directoryStatusLabel(status)),
		),
	}, nil
}

func (r *Registry) executeLeaveDirectoryService(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}

	dsType, _ := plan.Result["directory_type"].(string)

	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to leave %s: %w", dsType, err)
	}

	jobID, jobErr := parseJobID(result)

	response := map[string]interface{}{
		"status":         "submitted",
		"directory_type": dsType,
		"message":        fmt.Sprintf("Leaving %s was submitted to TrueNAS; track the task for the result.", dsType),
	}

	if jobErr == nil {
		// Never keep the credential values in the stored task arguments.
		opts := []tasks.TaskOption{}
		if plan.JobMethod != "" {
			opts = append(opts, tasks.WithExpectedMethod(plan.JobMethod))
		}
		task, err := r.taskManager.CreateJobTask(ctx,
			"leave_directory_service",
			maskDirectoryServicePayload(args),
			jobID,
			dsJobTTL,
			opts...,
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

func (r *Registry) handleConfigureDirectoryServiceWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planConfigureDirectoryService(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeConfigureDirectoryService(ctx, client, args, plan)
}

func (r *Registry) handleLeaveDirectoryServiceWithDryRun(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planLeaveDirectoryService(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return r.executeLeaveDirectoryService(ctx, client, args, plan)
}
