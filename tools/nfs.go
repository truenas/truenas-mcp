package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/truenas/truenas-mcp/truenas"
)

// handleCreateNFSShare creates a new NFS share
func handleCreateNFSShare(client *truenas.Client, args map[string]interface{}) (string, error) {
	// Extract required parameter
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return "", fmt.Errorf("path is required")
	}

	// Validate path
	if err := validateSharePath(path); err != nil {
		return "", err
	}

	// Build the payload
	payload := map[string]interface{}{
		"path": path,
	}

	// Optional parameters with defaults
	if enabled, ok := args["enabled"].(bool); ok {
		payload["enabled"] = enabled
	} else {
		payload["enabled"] = true // Default to enabled
	}

	if comment, ok := args["comment"].(string); ok && comment != "" {
		payload["comment"] = comment
	}

	if ro, ok := args["ro"].(bool); ok {
		payload["ro"] = ro
	}

	// Network access control
	if networks, ok := args["networks"].([]interface{}); ok && len(networks) > 0 {
		// Validate CIDR notation
		for _, net := range networks {
			if netStr, ok := net.(string); ok {
				if err := validateCIDR(netStr); err != nil {
					return "", fmt.Errorf("invalid network CIDR '%s': %w", netStr, err)
				}
			}
		}
		payload["networks"] = networks
	}

	if hosts, ok := args["hosts"].([]interface{}); ok && len(hosts) > 0 {
		// Validate hosts (no quotes or spaces)
		for _, host := range hosts {
			if hostStr, ok := host.(string); ok {
				if err := validateNFSHost(hostStr); err != nil {
					return "", fmt.Errorf("invalid host '%s': %w", hostStr, err)
				}
			}
		}
		payload["hosts"] = hosts
	}

	// User/group mapping
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

	// Security settings
	if security, ok := args["security"].([]interface{}); ok && len(security) > 0 {
		payload["security"] = security
	}

	// Check if this is a dry run
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		// Return preview of what would be created
		preview := map[string]interface{}{
			"dry_run":   true,
			"operation": "sharing.nfs.create",
			"payload":   payload,
			"note":      "This is a preview. No NFS share has been created.",
			"next_step": "Remove dry_run parameter or set to false to execute",
		}

		// Add security warnings
		warnings := []string{}
		if networks, ok := payload["networks"]; !ok || networks == nil {
			if hosts, ok := payload["hosts"]; !ok || hosts == nil {
				warnings = append(warnings, "Share will be accessible from any network/host (no network or host restrictions)")
			}
		}
		if ro, ok := payload["ro"].(bool); !ok || !ro {
			warnings = append(warnings, "Share allows read-write access")
		}
		if maprootUser, ok := payload["maproot_user"]; !ok || maprootUser == nil {
			warnings = append(warnings, "No maproot_user configured - root clients will have root access (security risk)")
		}

		// Add directory service warnings if applicable
		ctx := context.Background()
		if tnClient, err := GetTrueNASClient(); err == nil {
			dirWarnings := checkDirectoryServiceForShareWarnings(ctx, tnClient)
			warnings = append(warnings, dirWarnings...)
		}

		if len(warnings) > 0 {
			preview["security_warnings"] = warnings
		}

		formatted, err := json.MarshalIndent(preview, "", "  ")
		if err != nil {
			return "", err
		}
		return string(formatted), nil
	}

	// Call the API
	result, err := client.Call("sharing.nfs.create", payload)
	if err != nil {
		return "", fmt.Errorf("failed to create NFS share: %w", err)
	}

	var share map[string]interface{}
	if err := json.Unmarshal(result, &share); err != nil {
		return "", fmt.Errorf("failed to parse NFS share response: %w", err)
	}

	// Format response with key information
	response := map[string]interface{}{
		"success": true,
		"id":      share["id"],
		"path":    share["path"],
		"enabled": share["enabled"],
	}

	if comment, ok := share["comment"].(string); ok && comment != "" {
		response["comment"] = comment
	}

	if ro, ok := share["ro"].(bool); ok {
		response["read_only"] = ro
	}

	// Add mount instructions
	response["mount_example"] = fmt.Sprintf("mount -t nfs truenas:%s /mnt/point", path)
	response["note"] = "NFS share is now accessible. Ensure NFS service is running and firewall allows NFS traffic."

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
