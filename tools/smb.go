package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/truenas/truenas-mcp/truenas"
)

// handleCreateSMBShare creates a new SMB share
func handleCreateSMBShare(client *truenas.Client, args map[string]interface{}) (string, error) {
	// Extract required parameters
	name, ok := args["name"].(string)
	if !ok || name == "" {
		return "", fmt.Errorf("name is required")
	}

	path, ok := args["path"].(string)
	if !ok || path == "" {
		return "", fmt.Errorf("path is required")
	}

	// Validate share name
	if err := validateShareName(name); err != nil {
		return "", err
	}

	// Validate path
	if err := validateSharePath(path); err != nil {
		return "", err
	}

	// Build the payload
	payload := map[string]interface{}{
		"name": name,
		"path": path,
	}

	// Optional parameters with defaults
	if purpose, ok := args["purpose"].(string); ok && purpose != "" {
		payload["purpose"] = purpose
	}

	if enabled, ok := args["enabled"].(bool); ok {
		payload["enabled"] = enabled
	} else {
		payload["enabled"] = true // Default to enabled
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

	// Audit configuration
	if audit, ok := args["audit"].(map[string]interface{}); ok {
		payload["audit"] = audit
	}

	// Purpose-specific options
	if options, ok := args["options"].(map[string]interface{}); ok {
		payload["options"] = options
	}

	// Host access control
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

	// Check if this is a dry run
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		// Return preview of what would be created
		preview := map[string]interface{}{
			"dry_run":      true,
			"operation":    "sharing.smb.create",
			"payload":      payload,
			"note":         "This is a preview. No SMB share has been created.",
			"next_step":    "Remove dry_run parameter or set to false to execute",
			"network_path": fmt.Sprintf("\\\\truenas\\%s", name),
		}

		// Add security warnings
		warnings := []string{}
		if browsable, ok := payload["browsable"].(bool); ok && browsable {
			if hostsallow, ok := payload["hostsallow"]; !ok || hostsallow == nil {
				warnings = append(warnings, "Share will be visible and accessible from any network host (browsable=true, no hostsallow restrictions)")
			}
		}
		if readonly, ok := payload["readonly"].(bool); ok && !readonly {
			warnings = append(warnings, "Share allows read-write access")
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
	result, err := client.Call("sharing.smb.create", payload)
	if err != nil {
		return "", fmt.Errorf("failed to create SMB share: %w", err)
	}

	var share map[string]interface{}
	if err := json.Unmarshal(result, &share); err != nil {
		return "", fmt.Errorf("failed to parse SMB share response: %w", err)
	}

	// Format response with key information
	response := map[string]interface{}{
		"success": true,
		"id":      share["id"],
		"name":    share["name"],
		"path":    share["path"],
		"enabled": share["enabled"],
	}

	if purpose, ok := share["purpose"].(string); ok {
		response["purpose"] = purpose
	}

	if comment, ok := share["comment"].(string); ok && comment != "" {
		response["comment"] = comment
	}

	// Add connection information
	response["network_path"] = fmt.Sprintf("\\\\truenas\\%s", name)
	response["note"] = "Share is now accessible over the network. You may need to configure permissions."

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

// validateSharePath validates the share path
func validateSharePath(path string) error {
	if path == "" {
		return fmt.Errorf("path cannot be empty")
	}

	// Special case for external DFS shares
	if path == "EXTERNAL" {
		return nil
	}

	// Path must start with /mnt/
	if !strings.HasPrefix(path, "/mnt/") {
		return fmt.Errorf("path must start with /mnt/ (got: %s)", path)
	}

	// Basic validation that it looks like a valid path
	if strings.Contains(path, "//") {
		return fmt.Errorf("path cannot contain consecutive slashes")
	}

	return nil
}
