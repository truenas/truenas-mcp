package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerNFSShareTools() {
	// NFS share creation (write operation)
	r.tools["create_nfs_share"] = Tool{
		Definition: mcp.Tool{
			Name:        "create_nfs_share",
			Description: "Create an NFS (Network File System) share for Unix/Linux file sharing. This makes a ZFS dataset accessible over the network via the NFS protocol.\n\n**WIZARD GUIDANCE FOR LLM:**\nWhen helping users create NFS shares, follow this conversation flow:\n\n**1. Dataset Selection:**\n- Ask: \"Do you want to create a new dataset or use an existing ZFS dataset?\"\n- If NEW: Use create_dataset tool first (with share_type=NFS, acltype=POSIX)\n- If EXISTING: \n  * Query available datasets first with query_datasets\n  * Present options to user (NEVER suggest pool root like 'tank' or 'flash')\n  * Use the dataset's mountpoint as the path\n  * Warn: \"Never share a pool root - always use a child dataset\"\n- After dataset creation, use its mountpoint as the path\n\n**2. Access Control:**\n- Ask: \"Read-only or read-write?\" (default: read-write)\n- Ask: \"Restrict to specific networks?\" (CIDR notation: 192.168.1.0/24)\n- Ask: \"Restrict to specific hosts?\" (IP addresses or hostnames)\n- Recommend: At least one restriction (network or host) for security\n\n**3. User Mapping (Important for Security):**\n- Ask: \"How should root access be handled?\"\n  * **maproot_user**: Map root clients to specific user (recommended: 'nobody')\n  * **maproot_group**: Map root clients to specific group (recommended: 'nogroup')\n  * Warn if not set: \"Root clients will have full root access (security risk)\"\n- Ask: \"Map all users to a specific user?\" (optional, for anonymous access)\n  * **mapall_user**: Maps all clients to one user\n  * **mapall_group**: Maps all client groups to one group\n\n**4. Security Level (Optional):**\n- Default: SYS (system authentication)\n- Advanced: KRB5, KRB5I, KRB5P (Kerberos, requires setup)\n- Usually skip unless user specifically needs Kerberos\n\n**IMPORTANT RECOMMENDATIONS:**\n- For NFS shares: share_type=NFS, acltype=POSIX (in dataset creation)\n- Compression: LZ4 recommended for balanced performance\n- Always set maproot_user='nobody' to prevent root access\n- Use network/host restrictions to limit access\n- Read-only for shared data that shouldn't be modified\n\n**SECURITY WARNINGS TO DISPLAY:**\n- If no network/host restrictions: \"Share accessible from any host\"\n- If no maproot_user: \"Root clients will have full root access\"\n- If read-write + no restrictions: \"Any host can modify/delete files\"\n- Remind: \"Ensure NFS service is running and firewall allows NFS traffic (port 2049)\"\n\n**BEFORE EXECUTING:**\n1. Use dry_run=true to preview the configuration\n2. Display complete summary including:\n   - Local path\n   - Access type (read-only/read-write)\n   - Network/host restrictions\n   - User mapping settings\n   - Security warnings if applicable\n3. Get explicit user confirmation: \"Shall I create this NFS share?\"\n4. Warn: \"This is a WRITE operation that exposes data over your network\"\n5. After creation: Provide mount command example\n\n**DRY RUN:**\nSet dry_run=true to preview what will be created without executing. Show user the preview including security warnings, then ask for confirmation.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "ZFS dataset mountpoint starting with /mnt/ (e.g., /mnt/tank/shares/data, NOT /mnt/tank)",
					},
					"enabled": map[string]interface{}{
						"type":        "boolean",
						"description": "Enable share for network access (default: true)",
						"default":     true,
					},
					"comment": map[string]interface{}{
						"type":        "string",
						"description": "Description for the share (optional)",
					},
					"ro": map[string]interface{}{
						"type":        "boolean",
						"description": "Read-only export (default: false for read-write)",
						"default":     false,
					},
					"networks": map[string]interface{}{
						"type":        "array",
						"description": "Authorized networks in CIDR notation (e.g., ['192.168.1.0/24']). Empty = allow all networks.",
						"items": map[string]interface{}{
							"type": "string",
						},
					},
					"hosts": map[string]interface{}{
						"type":        "array",
						"description": "Authorized IP addresses or hostnames (e.g., ['192.168.1.10', 'client.local']). No quotes or spaces. Empty = allow all hosts.",
						"items": map[string]interface{}{
							"type": "string",
						},
					},
					"maproot_user": map[string]interface{}{
						"type":        "string",
						"description": "Map root clients to this user (recommended: 'nobody' for security)",
					},
					"maproot_group": map[string]interface{}{
						"type":        "string",
						"description": "Map root clients to this group (recommended: 'nogroup' for security)",
					},
					"mapall_user": map[string]interface{}{
						"type":        "string",
						"description": "Map all clients to this user (optional, for anonymous access)",
					},
					"mapall_group": map[string]interface{}{
						"type":        "string",
						"description": "Map all client groups to this group (optional, for anonymous access)",
					},
					"security": map[string]interface{}{
						"type":        "array",
						"description": "Security mechanisms: ['SYS'] (default), ['KRB5'], ['KRB5I'], ['KRB5P']",
						"items": map[string]interface{}{
							"type": "string",
							"enum": []string{"SYS", "KRB5", "KRB5I", "KRB5P"},
						},
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview what will be created without executing (default: false)",
						"default":     false,
					},
				},
				"required": []string{"path"},
			},
		},
		Handler: handleCreateNFSShare,
		Plan:    planCreateNFSShare,
	}
}

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const sharingNFSCreateMethod = "sharing.nfs.create"

// buildCreateNFSSharePayload constructs the sharing.nfs.create body.
// The map is unchanged from main: enabled defaults to true.
func buildCreateNFSSharePayload(args map[string]interface{}) (map[string]interface{}, error) {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return nil, fmt.Errorf("path is required")
	}
	if err := validateSharePath(path); err != nil {
		return nil, err
	}

	payload := map[string]interface{}{
		"path": path,
	}
	if enabled, ok := args["enabled"].(bool); ok {
		payload["enabled"] = enabled
	} else {
		payload["enabled"] = true
	}
	if comment, ok := args["comment"].(string); ok && comment != "" {
		payload["comment"] = comment
	}
	if ro, ok := args["ro"].(bool); ok {
		payload["ro"] = ro
	}
	if networks, ok := args["networks"].([]interface{}); ok && len(networks) > 0 {
		for _, net := range networks {
			if netStr, ok := net.(string); ok {
				if err := validateCIDR(netStr); err != nil {
					return nil, fmt.Errorf("invalid network CIDR '%s': %w", netStr, err)
				}
			}
		}
		payload["networks"] = networks
	}
	if hosts, ok := args["hosts"].([]interface{}); ok && len(hosts) > 0 {
		for _, host := range hosts {
			if hostStr, ok := host.(string); ok {
				if err := validateNFSHost(hostStr); err != nil {
					return nil, fmt.Errorf("invalid host '%s': %w", hostStr, err)
				}
			}
		}
		payload["hosts"] = hosts
	}
	if maprootUser, ok := args["maproot_user"].(string); ok && maprootUser != "" {
		payload["maproot_user"] = maprootUser
	}
	if maprootGroup, ok := args["maproot_group"].(string); ok && maprootGroup != "" {
		payload["maproot_group"] = maprootGroup
	}
	if mapallUser, ok := args["mapall_user"].(string); ok && mapallUser != "" {
		payload["mapall_user"] = mapallUser
	}
	if mapallGroup, ok := args["mapall_group"].(string); ok && mapallGroup != "" {
		payload["mapall_group"] = mapallGroup
	}
	if security, ok := args["security"].([]interface{}); ok && len(security) > 0 {
		payload["security"] = security
	}
	return payload, nil
}

func nfsShareSecurityWarnings(ctx context.Context, client *truenas.Client, payload map[string]interface{}) []string {
	warnings := []string{}
	if networks, ok := payload["networks"]; !ok || networks == nil {
		if hosts, ok := payload["hosts"]; !ok || hosts == nil {
			warnings = append(warnings, "Share will be accessible from any network/host (no network or host restrictions)")
		}
	}
	if ro, ok := payload["ro"].(bool); !ok || !ro {
		warnings = append(warnings, "Share allows read-write access")
	}
	warnings = append(warnings, nfsRootMappingWarning(payload))
	warnings = append(warnings, checkDirectoryServiceForShareWarnings(ctx, client)...)
	return warnings
}

// planCreateNFSShare builds the create_nfs_share plan used by both the
// dry-run preview and the real call.
func planCreateNFSShare(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	payload, err := buildCreateNFSSharePayload(args)
	if err != nil {
		return nil, err
	}
	path, _ := payload["path"].(string)
	host, hostNote := lookupNASHostname(ctx, client)
	mountExample := fmt.Sprintf("mount -t nfs %s:%s /mnt/point", host, path)
	warnings := nfsShareSecurityWarnings(ctx, client, payload)

	current := map[string]interface{}{
		"payload":       payload,
		"mount_example": mountExample,
	}
	if hostNote != "" {
		current["hostname_note"] = hostNote
	}

	return &writePlan{
		Tool:         "create_nfs_share",
		Method:       sharingNFSCreateMethod,
		CallParams:   []interface{}{payload},
		ParamNames:   []string{"data"},
		TargetName:   path,
		CurrentState: current,
		Warnings:     warnings,
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Create NFS share at %s", path),
			Operation:   "create",
			Target:      sharingNFSCreateMethod,
			Details: map[string]interface{}{
				"payload":       payload,
				"mount_example": mountExample,
			},
		}},
		Result: map[string]interface{}{
			"path":          path,
			"mount_example": mountExample,
			"hostname_note": hostNote,
		},
	}, nil
}

// handleCreateNFSShare creates a new NFS share
func handleCreateNFSShare(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planCreateNFSShare(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeCreateNFSShare(ctx, client, args, plan)
}

func executeCreateNFSShare(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to create NFS share: %w", err)
	}
	var share map[string]interface{}
	if err := json.Unmarshal(result, &share); err != nil {
		return "", fmt.Errorf("failed to parse NFS share response: %w", err)
	}
	mountExample, _ := plan.Result["mount_example"].(string)
	hostNote, _ := plan.Result["hostname_note"].(string)
	response := map[string]interface{}{
		"success":       true,
		"id":            share["id"],
		"path":          share["path"],
		"enabled":       share["enabled"],
		"mount_example": mountExample,
		"note":          "NFS share is now accessible. Ensure NFS service is running and firewall allows NFS traffic.",
	}
	if comment, ok := share["comment"].(string); ok && comment != "" {
		response["comment"] = comment
	}
	if ro, ok := share["ro"].(bool); ok {
		response["read_only"] = ro
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

// validateCIDR validates CIDR notation (network/mask)
func validateCIDR(cidr string) error {
	if cidr == "" {
		return fmt.Errorf("CIDR cannot be empty")
	}

	// Basic CIDR validation: must contain a slash
	if !strings.Contains(cidr, "/") {
		return fmt.Errorf("CIDR must be in format 'network/mask' (e.g., 192.168.1.0/24)")
	}

	// Split into network and mask
	parts := strings.Split(cidr, "/")
	if len(parts) != 2 {
		return fmt.Errorf("CIDR must be in format 'network/mask'")
	}

	// Validate mask is a number
	mask := parts[1]
	if matched, _ := regexp.MatchString(`^\d+$`, mask); !matched {
		return fmt.Errorf("CIDR mask must be a number (e.g., /24)")
	}

	return nil
}

// validateNFSHost validates host specification (no quotes or spaces)
func validateNFSHost(host string) error {
	if host == "" {
		return fmt.Errorf("host cannot be empty")
	}

	// Check for invalid characters
	if strings.ContainsAny(host, `"' `) {
		return fmt.Errorf("host cannot contain quotes or spaces")
	}

	return nil
}

// nfsRootIDName reports whether name is treated as uid/gid 0. Middleware
// only emits no_root_squash when both maproot_user and maproot_group
// resolve to id 0 (exports.mako on 25.10 and 27.0). "root" and the
// group "wheel" are the local names for those ids.
func nfsRootIDName(name string, group bool) bool {
	if strings.EqualFold(name, "root") {
		return true
	}
	return group && strings.EqualFold(name, "wheel")
}

// nfsMappedAccount describes a user and/or group for export-mapping text.
// root/wheel are shown as uid 0 / gid 0 to match the export options.
func nfsMappedAccount(user, group string) string {
	var parts []string
	if user != "" {
		if nfsRootIDName(user, false) {
			parts = append(parts, "uid 0")
		} else {
			parts = append(parts, user)
		}
	}
	if group != "" {
		if nfsRootIDName(group, true) {
			parts = append(parts, "gid 0")
		} else {
			parts = append(parts, group)
		}
	}
	return strings.Join(parts, " / ")
}

// nfsRootMappingWarning describes how the export will map root clients.
// no_root_squash is used only when maproot_user and maproot_group both
// resolve to id 0. Otherwise the export keeps root_squash and sets
// anonuid/anongid for any maproot names. mapall_* adds all_squash.
func nfsRootMappingWarning(payload map[string]interface{}) string {
	mapallUser, _ := payload["mapall_user"].(string)
	mapallGroup, _ := payload["mapall_group"].(string)
	if mapallUser != "" || mapallGroup != "" {
		msg := fmt.Sprintf("All client users are mapped to %s (all_squash)", nfsMappedAccount(mapallUser, mapallGroup))
		if nfsRootIDName(mapallUser, false) {
			msg += ", so every client has root file access"
		}
		return msg
	}
	maprootUser, _ := payload["maproot_user"].(string)
	maprootGroup, _ := payload["maproot_group"].(string)
	if nfsRootIDName(maprootUser, false) && nfsRootIDName(maprootGroup, true) {
		return "maproot maps root clients to local root (no_root_squash); root clients will have root access"
	}
	if maprootUser == "" && maprootGroup == "" {
		return "Root clients are squashed to nobody (root_squash)"
	}
	msg := fmt.Sprintf("Root clients are mapped to %s (root_squash)", nfsMappedAccount(maprootUser, maprootGroup))
	if nfsRootIDName(maprootUser, false) {
		msg += ", so they keep root file access"
	}
	return msg
}
