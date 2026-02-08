package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/truenas/truenas-mcp/truenas"
)

// handleCreateDataset creates a new ZFS dataset (filesystem or volume)
func handleCreateDataset(client *truenas.Client, args map[string]interface{}) (string, error) {
	// Extract required parameters
	name, ok := args["name"].(string)
	if !ok || name == "" {
		return "", fmt.Errorf("name is required")
	}

	dsType, ok := args["type"].(string)
	if !ok || dsType == "" {
		dsType = "FILESYSTEM" // Default to filesystem
	}

	// Validate dataset name
	if err := validateDatasetName(name); err != nil {
		return "", err
	}

	// Validate type
	if dsType != "FILESYSTEM" && dsType != "VOLUME" {
		return "", fmt.Errorf("type must be FILESYSTEM or VOLUME, got: %s", dsType)
	}

	// Build the payload
	payload := map[string]interface{}{
		"name": name,
		"type": dsType,
	}

	// Handle VOLUME-specific required parameter
	if dsType == "VOLUME" {
		volsize, ok := args["volsize"].(float64)
		if !ok || volsize <= 0 {
			return "", fmt.Errorf("volsize (in bytes) is required for VOLUME type")
		}
		payload["volsize"] = int64(volsize)

		// Optional volblocksize
		if volblocksize, ok := args["volblocksize"].(string); ok && volblocksize != "" {
			payload["volblocksize"] = volblocksize
		}
	}

	// Optional parameters with defaults
	if shareType, ok := args["share_type"].(string); ok && shareType != "" {
		payload["share_type"] = shareType
	}

	if compression, ok := args["compression"].(string); ok && compression != "" {
		payload["compression"] = compression
	}

	if acltype, ok := args["acltype"].(string); ok && acltype != "" {
		payload["acltype"] = acltype
	}

	// Quota parameters
	if quota, ok := args["quota"].(float64); ok && quota > 0 {
		payload["quota"] = int64(quota)
	}

	if refquota, ok := args["refquota"].(float64); ok && refquota > 0 {
		payload["refquota"] = int64(refquota)
	}

	// Boolean parameters
	if createAncestors, ok := args["create_ancestors"].(bool); ok {
		payload["create_ancestors"] = createAncestors
	}

	if readonly, ok := args["readonly"].(bool); ok {
		payload["readonly"] = readonly
	}

	if dedup, ok := args["deduplication"].(string); ok && dedup != "" {
		payload["deduplication"] = dedup
	}

	if checksum, ok := args["checksum"].(string); ok && checksum != "" {
		payload["checksum"] = checksum
	}

	if snapdir, ok := args["snapdir"].(string); ok && snapdir != "" {
		payload["snapdir"] = snapdir
	}

	if atime, ok := args["atime"].(string); ok && atime != "" {
		payload["atime"] = atime
	}

	// Encryption options
	if encOpts, ok := args["encryption_options"].(map[string]interface{}); ok && len(encOpts) > 0 {
		if err := validateEncryptionOptions(encOpts); err != nil {
			return "", err
		}
		payload["encryption_options"] = encOpts
	}

	// Inherit encryption
	if inheritEnc, ok := args["inherit_encryption"].(bool); ok {
		payload["inherit_encryption"] = inheritEnc
	}

	// User properties
	if userProps, ok := args["user_properties"].([]interface{}); ok && len(userProps) > 0 {
		payload["user_properties"] = userProps
	}

	// Check if this is a dry run
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		// Return preview of what would be created
		preview := map[string]interface{}{
			"dry_run":        true,
			"operation":      "pool.dataset.create",
			"payload":        payload,
			"note":           "This is a preview. No dataset has been created.",
			"next_step":      "Remove dry_run parameter or set to false to execute",
			"estimated_path": fmt.Sprintf("/mnt/%s", name),
		}

		formatted, err := json.MarshalIndent(preview, "", "  ")
		if err != nil {
			return "", err
		}
		return string(formatted), nil
	}

	// Call the API
	result, err := client.Call("pool.dataset.create", payload)
	if err != nil {
		return "", fmt.Errorf("failed to create dataset: %w", err)
	}

	var dataset map[string]interface{}
	if err := json.Unmarshal(result, &dataset); err != nil {
		return "", fmt.Errorf("failed to parse dataset response: %w", err)
	}

	// Format response with key information
	response := map[string]interface{}{
		"success":    true,
		"dataset_id": dataset["id"],
		"name":       dataset["name"],
		"type":       dataset["type"],
		"pool":       dataset["pool"],
	}

	if mountpoint, ok := dataset["mountpoint"].(string); ok {
		response["mountpoint"] = mountpoint
	}

	if encrypted, ok := dataset["encrypted"].(bool); ok {
		response["encrypted"] = encrypted
	}

	if keyLoaded, ok := dataset["key_loaded"].(bool); ok && keyLoaded {
		response["key_loaded"] = keyLoaded
		response["encryption_warning"] = "IMPORTANT: Back up your encryption key from Storage > Pools"
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// validateDatasetName validates the dataset name format
func validateDatasetName(name string) error {
	if name == "" {
		return fmt.Errorf("dataset name cannot be empty")
	}

	// Must contain at least one slash (pool/dataset)
	if !strings.Contains(name, "/") {
		return fmt.Errorf("dataset name must include pool name (e.g., 'pool/dataset')")
	}

	// Check for invalid characters
	invalidChars := regexp.MustCompile(`[^a-zA-Z0-9/_.-]`)
	if invalidChars.MatchString(name) {
		return fmt.Errorf("dataset name contains invalid characters (only alphanumeric, /, _, ., - allowed)")
	}

	// Cannot start or end with slash
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("dataset name cannot start or end with /")
	}

	// Cannot have consecutive slashes
	if strings.Contains(name, "//") {
		return fmt.Errorf("dataset name cannot contain consecutive slashes")
	}

	return nil
}

// validateEncryptionOptions validates encryption configuration
func validateEncryptionOptions(encOpts map[string]interface{}) error {
	genKey, hasGenKey := encOpts["generate_key"].(bool)
	passphrase, hasPassphrase := encOpts["passphrase"].(string)

	// Must specify either generate_key or passphrase, not both
	if hasGenKey && genKey && hasPassphrase && passphrase != "" {
		return fmt.Errorf("cannot specify both generate_key and passphrase - choose one encryption method")
	}

	if !hasGenKey && !hasPassphrase {
		return fmt.Errorf("encryption_options requires either generate_key=true or passphrase")
	}

	// Validate passphrase length
	if hasPassphrase && passphrase != "" {
		if len(passphrase) < 8 {
			return fmt.Errorf("passphrase must be at least 8 characters")
		}
	}

	// Validate algorithm if specified
	if algo, ok := encOpts["algorithm"].(string); ok {
		validAlgos := map[string]bool{
			"AES-128-CCM": true, "AES-192-CCM": true, "AES-256-CCM": true,
			"AES-128-GCM": true, "AES-192-GCM": true, "AES-256-GCM": true,
		}
		if !validAlgos[algo] {
			return fmt.Errorf("invalid encryption algorithm: %s", algo)
		}
	}

	return nil
}
