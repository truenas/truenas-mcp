package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerSMBShareTools() {
	// SMB share creation (write operation)
	r.tools["create_smb_share"] = Tool{
		Definition: mcp.Tool{
			Name:        "create_smb_share",
			Description: "Create an SMB (Windows/macOS file sharing) share. This makes a ZFS dataset accessible over the network via the SMB/CIFS protocol.\n\n**WIZARD GUIDANCE FOR LLM:**\nWhen helping users create SMB shares, follow this conversation flow:\n\n**1. Dataset Selection:**\n- Ask: \"Do you want to create a new dataset or use an existing ZFS dataset?\"\n- If NEW: Use create_dataset tool first (with share_type=SMB, acltype=NFSV4)\n- If EXISTING: \n  * Query available datasets first with query_datasets\n  * Present options to user (NEVER suggest pool root like 'tank' or 'flash')\n  * Use the dataset's mountpoint as the path\n  * Warn: \"Never share a pool root - always use a child dataset\"\n- After dataset creation, use its mountpoint as the path\n\n**2. Share Name:**\n- Ask: \"What name should appear when browsing the network?\"\n- Rules: Max 80 chars, no \\ / [ ] : | < > + = ; , * ? \"\n- Cannot use: global, printers, homes\n- Suggest: Use a friendly, descriptive name like \"TeamDocs\" or \"PhotoArchive\"\n\n**3. Description:**\n- Ask: \"Add a description?\" (optional, shown when browsing shares)\n\n**4. Purpose Selection:**\n- Ask: \"What's this share for?\"\n- Options:\n  * DEFAULT_SHARE: Standard file sharing (most common)\n  * TIMEMACHINE_SHARE: macOS Time Machine backups\n  * MULTIPROTOCOL_SHARE: Both SMB and NFS access (complex permissions)\n  * PRIVATE_DATASETS_SHARE: User home directories\n  * VEEAM_REPOSITORY_SHARE: Veeam backup storage\n- Recommend DEFAULT_SHARE unless specific use case\n\n**5. Access Control:**\n- Ask: \"Read-only or read-write?\" (default: read-write)\n- Ask: \"Should it be visible when browsing?\" (default: yes)\n- Ask: \"Restrict to specific IP addresses?\" (optional, for hostsallow)\n- Ask: \"Hide from unauthorized users?\" (access_based_share_enumeration)\n\n**6. Purpose-Specific Questions:**\n\nFor TIMEMACHINE_SHARE:\n- Ask: \"What's the backup size limit?\" (recommend 2-3x Mac's disk size)\n- Set time_machine_quota in options\n\nFor MULTIPROTOCOL_SHARE:\n- Warn: \"Multi-protocol shares have complex permission interactions\"\n- Recommend: \"Use either SMB OR NFS, not both, unless you understand the implications\"\n\nFor PRIVATE_DATASETS_SHARE:\n- Suggest: \"Create separate datasets per user for isolation\"\n- Recommend: \"Use access_based_share_enumeration=true\"\n\n**7. Auditing (Optional):**\n- Ask: \"Enable access auditing?\" (tracks who accesses files)\n- If yes: Ask which groups to audit (empty = audit all)\n\n**IMPORTANT RECOMMENDATIONS:**\n- Default: enabled=true, browsable=true, readonly=false\n- For sensitive data: Set access_based_share_enumeration=true\n- For public shares: Use hostsdeny to block unwanted networks\n- For Time Machine: Set appropriate quota to prevent filling pool\n- For multi-protocol: Strongly recommend against unless necessary\n\n**SECURITY WARNINGS TO DISPLAY:**\n- If browsable=true + no hostsallow: \"Share visible and accessible from any network\"\n- If readonly=false: \"Users can modify, delete, and create files\"\n- If no access restrictions: \"Anyone on your network can access this share\"\n- Remind: \"Configure share permissions in TrueNAS UI after creation\"\n\n**BEFORE EXECUTING:**\n1. Use dry_run=true to preview the configuration\n2. Display complete summary including:\n   - Share name and network path (\\\\truenas\\sharename)\n   - Local path\n   - Purpose and access settings\n   - Security warnings if applicable\n3. Get explicit user confirmation: \"Shall I create this share?\"\n4. Warn: \"This is a WRITE operation that exposes data over your network\"\n5. After creation: Remind user to configure permissions via TrueNAS UI\n\n**DRY RUN:**\nSet dry_run=true to preview what will be created without executing. Show user the preview including security warnings, then ask for confirmation.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Share name visible to clients (max 80 chars, case-insensitive, must be unique)",
					},
					"path": map[string]interface{}{
						"type":        "string",
						"description": "ZFS dataset mountpoint starting with /mnt/ (e.g., /mnt/tank/shares/docs, NOT /mnt/tank). Use 'EXTERNAL' only for DFS proxy shares.",
					},
					"purpose": map[string]interface{}{
						"type":        "string",
						"description": "Share purpose: DEFAULT_SHARE (standard), TIMEMACHINE_SHARE (macOS backups), MULTIPROTOCOL_SHARE (SMB+NFS), PRIVATE_DATASETS_SHARE (home dirs)",
						"enum":        []string{"DEFAULT_SHARE", "LEGACY_SHARE", "TIMEMACHINE_SHARE", "MULTIPROTOCOL_SHARE", "TIME_LOCKED_SHARE", "PRIVATE_DATASETS_SHARE", "EXTERNAL_SHARE", "VEEAM_REPOSITORY_SHARE", "FCP_SHARE"},
						"default":     "DEFAULT_SHARE",
					},
					"enabled": map[string]interface{}{
						"type":        "boolean",
						"description": "Enable share for network access (default: true)",
						"default":     true,
					},
					"comment": map[string]interface{}{
						"type":        "string",
						"description": "Description shown when clients list shares (optional)",
					},
					"readonly": map[string]interface{}{
						"type":        "boolean",
						"description": "Prevent clients from creating/modifying files (default: false)",
						"default":     false,
					},
					"browsable": map[string]interface{}{
						"type":        "boolean",
						"description": "Show share in network browse lists (default: true)",
						"default":     true,
					},
					"access_based_share_enumeration": map[string]interface{}{
						"type":        "boolean",
						"description": "Hide share from users without filesystem ACL access (default: false)",
						"default":     false,
					},
					"hostsallow": map[string]interface{}{
						"type":        "array",
						"description": "IP addresses/networks allowed to access (empty = allow all)",
						"items": map[string]interface{}{
							"type": "string",
						},
					},
					"hostsdeny": map[string]interface{}{
						"type":        "array",
						"description": "IP addresses/networks denied access (empty = deny none)",
						"items": map[string]interface{}{
							"type": "string",
						},
					},
					"audit": map[string]interface{}{
						"type":        "object",
						"description": "Audit configuration for tracking file access",
						"properties": map[string]interface{}{
							"enable": map[string]interface{}{
								"type":        "boolean",
								"description": "Enable audit logging",
							},
							"watch_list": map[string]interface{}{
								"type":        "array",
								"description": "Groups to audit (empty = audit all)",
								"items": map[string]interface{}{
									"type": "string",
								},
							},
							"ignore_list": map[string]interface{}{
								"type":        "array",
								"description": "Groups to exclude from auditing",
								"items": map[string]interface{}{
									"type": "string",
								},
							},
						},
					},
					"options": map[string]interface{}{
						"type":        "object",
						"description": "Purpose-specific options (varies by purpose)",
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview what will be created without executing (default: false)",
						"default":     false,
					},
				},
				"required": []string{"name", "path"},
			},
		},
		Handler: handleCreateSMBShare,
		Plan:    planCreateSMBShare,
	}
}

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const sharingSMBCreateMethod = "sharing.smb.create"

// buildCreateSMBSharePayload constructs the sharing.smb.create body.
// The map is unchanged from main: enabled defaults to true.
func buildCreateSMBSharePayload(args map[string]interface{}) (map[string]interface{}, error) {
	name, ok := args["name"].(string)
	if !ok || name == "" {
		return nil, fmt.Errorf("name is required")
	}
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return nil, fmt.Errorf("path is required")
	}
	if err := validateShareName(name); err != nil {
		return nil, err
	}
	if err := validateSharePath(path); err != nil {
		return nil, err
	}

	payload := map[string]interface{}{
		"name": name,
		"path": path,
	}
	if purpose, ok := args["purpose"].(string); ok && purpose != "" {
		payload["purpose"] = purpose
	}
	if enabled, ok := args["enabled"].(bool); ok {
		payload["enabled"] = enabled
	} else {
		payload["enabled"] = true
	}
	if comment, ok := args["comment"].(string); ok && comment != "" {
		payload["comment"] = comment
	}
	if readonly, ok := args["readonly"].(bool); ok {
		payload["readonly"] = readonly
	}
	if browsable, ok := args["browsable"].(bool); ok {
		payload["browsable"] = browsable
	}
	if abse, ok := args["access_based_share_enumeration"].(bool); ok {
		payload["access_based_share_enumeration"] = abse
	}
	if audit, ok := args["audit"].(map[string]interface{}); ok {
		payload["audit"] = audit
	}
	if options, ok := args["options"].(map[string]interface{}); ok {
		payload["options"] = options
	}
	if hostsallow, ok := args["hostsallow"].([]interface{}); ok && len(hostsallow) > 0 {
		if payload["options"] == nil {
			payload["options"] = make(map[string]interface{})
		}
		optionsMap := payload["options"].(map[string]interface{})
		optionsMap["hostsallow"] = hostsallow
	}
	if hostsdeny, ok := args["hostsdeny"].([]interface{}); ok && len(hostsdeny) > 0 {
		if payload["options"] == nil {
			payload["options"] = make(map[string]interface{})
		}
		optionsMap := payload["options"].(map[string]interface{})
		optionsMap["hostsdeny"] = hostsdeny
	}
	return payload, nil
}

func smbShareSecurityWarnings(ctx context.Context, client *truenas.Client, payload map[string]interface{}) []string {
	warnings := []string{}
	if smbShareBrowsable(payload) && !smbHasHostsallow(payload) {
		warnings = append(warnings, "Share will be visible and accessible from any network host (browsable=true, no hostsallow restrictions)")
	}
	if readonly, ok := payload["readonly"].(bool); ok && !readonly {
		warnings = append(warnings, "Share allows read-write access")
	}
	warnings = append(warnings, checkDirectoryServiceForShareWarnings(ctx, client)...)
	return warnings
}

// planCreateSMBShare builds the create_smb_share plan used by both the
// dry-run preview and the real call.
func planCreateSMBShare(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	payload, err := buildCreateSMBSharePayload(args)
	if err != nil {
		return nil, err
	}
	name, _ := payload["name"].(string)
	host, hostNote := lookupNASHostname(ctx, client)
	networkPath := fmt.Sprintf("\\\\%s\\%s", host, name)
	warnings := smbShareSecurityWarnings(ctx, client, payload)

	current := map[string]interface{}{
		"payload":      payload,
		"network_path": networkPath,
	}
	if hostNote != "" {
		current["hostname_note"] = hostNote
	}

	return &writePlan{
		Tool:         "create_smb_share",
		Method:       sharingSMBCreateMethod,
		CallParams:   []interface{}{payload},
		ParamNames:   []string{"data"},
		TargetName:   name,
		CurrentState: current,
		Warnings:     warnings,
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Create SMB share %s", name),
			Operation:   "create",
			Target:      sharingSMBCreateMethod,
			Details: map[string]interface{}{
				"payload":      payload,
				"network_path": networkPath,
			},
		}},
		Result: map[string]interface{}{
			"name":          name,
			"network_path":  networkPath,
			"hostname_note": hostNote,
		},
	}, nil
}

// handleCreateSMBShare creates a new SMB share
func handleCreateSMBShare(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planCreateSMBShare(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeCreateSMBShare(ctx, client, args, plan)
}

func executeCreateSMBShare(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to create SMB share: %w", err)
	}
	var share map[string]interface{}
	if err := json.Unmarshal(result, &share); err != nil {
		return "", fmt.Errorf("failed to parse SMB share response: %w", err)
	}
	networkPath, _ := plan.Result["network_path"].(string)
	hostNote, _ := plan.Result["hostname_note"].(string)
	response := map[string]interface{}{
		"success":      true,
		"id":           share["id"],
		"name":         share["name"],
		"path":         share["path"],
		"enabled":      share["enabled"],
		"network_path": networkPath,
		"note":         "Share is now accessible over the network. You may need to configure permissions.",
	}
	if purpose, ok := share["purpose"].(string); ok {
		response["purpose"] = purpose
	}
	if comment, ok := share["comment"].(string); ok && comment != "" {
		response["comment"] = comment
	}
	if hostNote != "" {
		response["hostname_note"] = hostNote
	}
	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}
	return string(formatted), nil
}

// validateShareName validates SMB share name according to TrueNAS rules
func validateShareName(name string) error {
	if name == "" {
		return fmt.Errorf("share name cannot be empty")
	}

	if len(name) > 80 {
		return fmt.Errorf("share name cannot exceed 80 characters")
	}

	// Check for invalid characters
	invalidChars := []string{`\`, `/`, `[`, `]`, `:`, `|`, `<`, `>`, `+`, `=`, `;`, `,`, `*`, `?`, `"`}
	for _, char := range invalidChars {
		if strings.Contains(name, char) {
			return fmt.Errorf("share name cannot contain: %s", strings.Join(invalidChars, " "))
		}
	}

	// Check for restricted names (case-insensitive)
	restrictedNames := map[string]bool{
		"global":   true,
		"printers": true,
		"homes":    true,
	}

	if restrictedNames[strings.ToLower(name)] {
		return fmt.Errorf("share name '%s' is reserved and cannot be used", name)
	}

	return nil
}

// validateSharePath validates the share path. After the existing empty,
// EXTERNAL, /mnt/ prefix, and consecutive-slash checks, the path is
// cleaned; any remaining ".." segment is rejected. The pool is the first
// component after /mnt/ on the original path, so /mnt/tank/../other
// cannot be re-read as pool "other". The cleaned path must be
// /mnt/<pool> or /mnt/<pool>/....
func validateSharePath(p string) error {
	if p == "" {
		return fmt.Errorf("path cannot be empty")
	}

	// Special case for external DFS shares
	if p == "EXTERNAL" {
		return nil
	}

	// Path must start with /mnt/
	if !strings.HasPrefix(p, "/mnt/") {
		return fmt.Errorf("path must start with /mnt/ (got: %s)", p)
	}

	// Basic validation that it looks like a valid path
	if strings.Contains(p, "//") {
		return fmt.Errorf("path cannot contain consecutive slashes")
	}

	cleaned := path.Clean(p)
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == ".." {
			return fmt.Errorf("path cannot contain '..' segments")
		}
	}

	rest := strings.TrimPrefix(p, "/mnt/")
	pool, _, _ := strings.Cut(rest, "/")
	if pool == "" || pool == "." || pool == ".." {
		return fmt.Errorf("path must be under /mnt/<pool>")
	}
	prefix := "/mnt/" + pool
	if cleaned != prefix && !strings.HasPrefix(cleaned, prefix+"/") {
		return fmt.Errorf("path must stay under /mnt/%s", pool)
	}
	return nil
}

// smbShareBrowsable reports whether the share will be browsable. TrueNAS
// defaults browsable to true, so an omitted value is treated as true.
func smbShareBrowsable(payload map[string]interface{}) bool {
	if v, ok := payload["browsable"].(bool); ok {
		return v
	}
	return true
}

// smbHasHostsallow reports whether payload.options.hostsallow has any entries.
func smbHasHostsallow(payload map[string]interface{}) bool {
	opts, _ := payload["options"].(map[string]interface{})
	list, _ := opts["hostsallow"].([]interface{})
	return len(list) > 0
}

const nasHostnamePlaceholder = "NAS_HOSTNAME"
const nasHostnameUnavailableNote = "NAS hostname unavailable; NAS_HOSTNAME is a placeholder"

// lookupNASHostname returns system.info hostname for share paths and mount
// examples. That value is the short hostname (socket.gethostname()), not
// the HA virtual hostname from network.configuration. If it is missing or
// unusable, the shell-safe placeholder NAS_HOSTNAME is returned with a
// note so the result cannot be mistaken for the factory-default hostname
// "truenas", and so a mount example can be pasted without being treated
// as a shell redirect.
func lookupNASHostname(ctx context.Context, client *truenas.Client) (host, note string) {
	if client == nil {
		return nasHostnamePlaceholder, nasHostnameUnavailableNote
	}
	result, err := client.Call(ctx, "system.info")
	if err != nil {
		return nasHostnamePlaceholder, nasHostnameUnavailableNote
	}
	var info map[string]interface{}
	if err := json.Unmarshal(result, &info); err != nil {
		return nasHostnamePlaceholder, nasHostnameUnavailableNote
	}
	host, _ = info["hostname"].(string)
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, `\/ "`) {
		return nasHostnamePlaceholder, nasHostnameUnavailableNote
	}
	return host, ""
}
