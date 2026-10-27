package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/truenas/truenas-mcp/redact"
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

	if err := validateVolblocksize(args, dsType); err != nil {
		return "", err
	}
	if err := validateUserProperties(args); err != nil {
		return "", err
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

		if volblocksize, ok := args["volblocksize"].(string); ok {
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

	// Boolean parameters - create_ancestors defaults to true
	if createAncestors, ok := args["create_ancestors"].(bool); ok {
		payload["create_ancestors"] = createAncestors
	} else {
		// Default to true to auto-create parent datasets
		payload["create_ancestors"] = true
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

	// Encryption. The middleware only honours encryption_options together
	// with encryption=true, and requires inherit_encryption=false then.
	encrypted, encOpts, err := parseDatasetEncryption(args)
	if err != nil {
		return "", err
	}
	if encrypted {
		payload["encryption"] = true
		payload["encryption_options"] = encOpts
		payload["inherit_encryption"] = false
	} else if inheritEnc, ok := args["inherit_encryption"].(bool); ok {
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
			"payload":        maskDatasetPayload(payload),
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

	// Call the API (error will include payload automatically)
	result, err := client.Call("pool.dataset.create", payload)
	if err != nil {
		// The middleware error can quote request values; scrub the literal
		// passphrase/key (the redactor only recognises labelled secrets).
		return "", fmt.Errorf("failed to create dataset: %s", scrubDatasetCreateError(err, encOpts))
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
	}
	if mode := encryptionMode(encOpts); mode != "" {
		response["encryption_warning"] = keyBackupGuidance(mode)
	} else if encrypted, ok := dataset["encrypted"].(bool); ok && encrypted {
		response["encryption_warning"] = keyBackupGuidance("")
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

// volblocksizes are the volume block sizes the middleware's
// pool.dataset.create accepts (TrueNAS 25.10 and 27.0).
var volblocksizes = []string{"512", "512B", "1K", "2K", "4K", "8K", "16K", "32K", "64K", "128K"}

// validateVolblocksize rejects a volblocksize that the middleware would
// reject (or, for FILESYSTEM, that the handler would otherwise ignore).
func validateVolblocksize(args map[string]interface{}, dsType string) error {
	v, present := args["volblocksize"]
	if !present {
		return nil
	}
	if dsType != "VOLUME" {
		return fmt.Errorf("volblocksize applies only to VOLUME datasets, not %s", dsType)
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("volblocksize must be a string, one of %s", strings.Join(volblocksizes, ", "))
	}
	for _, allowed := range volblocksizes {
		if s == allowed {
			return nil
		}
	}
	return fmt.Errorf("volblocksize must be one of %s", strings.Join(volblocksizes, ", "))
}

// validateUserProperties checks user_properties the way the middleware does:
// a list of {key, value} objects whose key is in namespace:property form
// (contains a colon) and whose value is a string. Values are never echoed.
func validateUserProperties(args map[string]interface{}) error {
	v, present := args["user_properties"]
	if !present {
		return nil
	}
	list, ok := v.([]interface{})
	if !ok {
		return fmt.Errorf("user_properties must be an array of {key, value} objects")
	}
	for i, item := range list {
		prop, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("user_properties[%d] must be an object with string fields key and value", i)
		}
		key, ok := prop["key"].(string)
		if !ok || !strings.Contains(key, ":") {
			return fmt.Errorf("user_properties[%d].key must be a string in namespace:property form (it must contain a colon), e.g. org:note", i)
		}
		if _, ok := prop["value"].(string); !ok {
			return fmt.Errorf("user_properties[%d].value must be a string", i)
		}
		if len(prop) != 2 {
			return fmt.Errorf("user_properties[%d] must contain only the fields key and value", i)
		}
	}
	return nil
}

// parseDatasetEncryption validates the encryption, encryption_options and
// inherit_encryption arguments the way the middleware's pool.dataset.create
// does (TrueNAS 25.10 and 27.0) and returns whether a new encryption root is
// requested together with the options to send. Error messages never include
// the passphrase or key.
func parseDatasetEncryption(args map[string]interface{}) (bool, map[string]interface{}, error) {
	encrypted := false
	if v, present := args["encryption"]; present {
		b, ok := v.(bool)
		if !ok {
			return false, nil, fmt.Errorf("encryption must be a boolean")
		}
		encrypted = b
	}

	var opts map[string]interface{}
	if v, present := args["encryption_options"]; present {
		m, ok := v.(map[string]interface{})
		if !ok {
			return false, nil, fmt.Errorf("encryption_options must be an object")
		}
		opts = m
	}

	if !encrypted {
		// TrueNAS would ignore the options and silently create an
		// unencrypted dataset, so refuse instead.
		if len(opts) > 0 {
			return false, nil, fmt.Errorf("encryption_options requires encryption=true; without it TrueNAS ignores the options and creates an unencrypted dataset")
		}
		return false, nil, nil
	}

	if inherit, ok := args["inherit_encryption"].(bool); ok && inherit {
		return false, nil, fmt.Errorf("inherit_encryption must be false (or omitted) when encryption is true")
	}
	if err := validateEncryptionOptions(opts); err != nil {
		return false, nil, err
	}

	// Send only the fields that were provided.
	out := map[string]interface{}{}
	for _, k := range []string{"generate_key", "passphrase", "key"} {
		if v, ok := opts[k]; ok && v != nil && v != false && v != "" {
			out[k] = v
		}
	}
	if v, ok := opts["pbkdf2iters"]; ok && v != nil {
		out["pbkdf2iters"] = int64(v.(float64))
	}
	return true, out, nil
}

// encryptionMode names the key source in validated encryption options, or ""
// when there are none.
func encryptionMode(opts map[string]interface{}) string {
	switch {
	case opts["passphrase"] != nil:
		return "passphrase"
	case opts["key"] != nil:
		return "key"
	case opts["generate_key"] != nil:
		return "generate_key"
	}
	return ""
}

// keyBackupGuidance tells the user what they must do to keep access to a
// dataset created with the given key source ("" for an encrypted dataset
// whose key source is not known, for example one inheriting from its parent).
func keyBackupGuidance(mode string) string {
	switch mode {
	case "generate_key":
		return "IMPORTANT: TrueNAS generated this dataset's key and stored it in its key database; it is not returned by this tool. This tool cannot export it. Back it up by exporting the dataset key in the TrueNAS web UI (Datasets > select the dataset > Encryption > Export Key), and keep the copy somewhere other than this system, because the key is lost if the system or its configuration database is lost."
	case "passphrase":
		return "IMPORTANT: TrueNAS does not store the passphrase and it cannot be recovered. If it is lost, the data on this dataset is lost."
	case "key":
		return "IMPORTANT: You supplied this dataset's key and must keep a copy of it. This tool does not return or recover keys."
	}
	return "IMPORTANT: This dataset is encrypted. Make sure its key or passphrase is backed up somewhere other than this system."
}

// scrubDatasetCreateError returns the text of a pool.dataset.create error with
// the literal passphrase and key removed from what the server said (its
// message, details and trace). The request block of the error is not
// scrubbed: it was masked by key name already, and replacing a short or
// common passphrase there would only garble it (field names such as
// encryption_options, the dataset name, or the JSON indentation).
func scrubDatasetCreateError(err error, encOpts map[string]interface{}) string {
	var apiErr *truenas.APICallError
	if errors.As(err, &apiErr) {
		return apiErr.MapServerText(func(s string) string {
			return redactLiteralSecrets(s, encOpts)
		}).Error()
	}
	// Not an API error (for example a connection failure): it has no request
	// block, so the whole text is server or transport text.
	return redactLiteralSecrets(err.Error(), encOpts)
}

// redactLiteralSecrets replaces the passphrase and key in encOpts, including
// their JSON-escaped forms, wherever they appear in msg. Both are matched
// case-insensitively: hex digits of a key may be echoed in either case, and a
// passphrase may be echoed upper- or lower-cased.
//
// Scope: this is a backstop for the literal value, as the middleware echoes it
// or as a JSON string escapes it. Other encodings (base64, hex or URL
// encoding, doubly escaped JSON, Python-style escapes) are not covered, because
// no middleware path emits them.
func redactLiteralSecrets(msg string, encOpts map[string]interface{}) string {
	for _, field := range []string{"passphrase", "key"} {
		secret, _ := encOpts[field].(string)
		if secret == "" {
			continue
		}
		variants := []string{secret}
		if b, err := json.Marshal(secret); err == nil && len(b) > 2 {
			if esc := string(b[1 : len(b)-1]); esc != secret {
				variants = append(variants, esc)
			}
		}
		for _, v := range variants {
			msg = regexp.MustCompile("(?i)"+regexp.QuoteMeta(v)).ReplaceAllString(msg, redact.Marker)
		}
	}
	return msg
}

// minPBKDF2Iters is the lowest pbkdf2iters any supported TrueNAS version
// accepts (25.10); 27.0 raised the minimum to 1300000 and the middleware
// enforces that itself.
const minPBKDF2Iters = 100000

// maxPassphraseBytes is OpenZFS's passphrase limit (MAX_PASSPHRASE_LEN); the
// middleware model sets no maximum, so longer values would only fail later at
// the ZFS layer.
const maxPassphraseBytes = 512

// validateEncryptionOptions validates the encryption_options object for a new
// encrypted dataset: exactly one of generate_key=true, passphrase (at least 8
// characters) or key (64 hex characters), plus an optional pbkdf2iters that is
// only meaningful with a passphrase. Unknown fields are rejected; algorithm is
// not configurable (TrueNAS always uses AES-256-GCM, and 27.0 rejects the
// field). Secret values are never echoed. Fields are checked in sorted order so
// the reported error is deterministic.
func validateEncryptionOptions(encOpts map[string]interface{}) error {
	names := make([]string, 0, len(encOpts))
	for k := range encOpts {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		switch k {
		case "generate_key", "passphrase", "key", "pbkdf2iters":
		case "algorithm":
			return fmt.Errorf("encryption_options.algorithm is not configurable through this tool; new encrypted datasets use AES-256-GCM")
		default:
			if plainFieldName.MatchString(k) {
				return fmt.Errorf("unknown encryption_options field %q (allowed: generate_key, passphrase, key, pbkdf2iters)", k)
			}
			return fmt.Errorf("unknown encryption_options field with invalid characters (allowed: generate_key, passphrase, key, pbkdf2iters)")
		}
	}

	genKey := false
	if v, ok := encOpts["generate_key"]; ok && v != nil {
		b, isBool := v.(bool)
		if !isBool {
			return fmt.Errorf("encryption_options.generate_key must be a boolean")
		}
		genKey = b
	}
	passphrase, hasPassphrase, err := optionalSecretString(encOpts, "passphrase")
	if err != nil {
		return err
	}
	key, hasKey, err := optionalSecretString(encOpts, "key")
	if err != nil {
		return err
	}

	provided := 0
	if genKey {
		provided++
	}
	if hasPassphrase {
		provided++
	}
	if hasKey {
		provided++
	}
	switch {
	case provided == 0:
		return fmt.Errorf("encryption_options requires exactly one of generate_key=true, passphrase or key")
	case provided > 1:
		return fmt.Errorf("encryption_options accepts only one of generate_key=true, passphrase or key, not several")
	}

	if hasPassphrase && utf8.RuneCountInString(passphrase) < 8 {
		return fmt.Errorf("passphrase must be at least 8 characters")
	}
	if hasPassphrase && len(passphrase) > maxPassphraseBytes {
		return fmt.Errorf("passphrase must be at most %d bytes (OpenZFS limit)", maxPassphraseBytes)
	}
	if hasKey && !hexKey64.MatchString(key) {
		return fmt.Errorf("key must be exactly 64 hexadecimal characters")
	}

	if v, ok := encOpts["pbkdf2iters"]; ok && v != nil {
		if !hasPassphrase {
			return fmt.Errorf("pbkdf2iters applies only to passphrase encryption")
		}
		n, isNum := v.(float64)
		if !isNum || n != math.Trunc(n) || n < minPBKDF2Iters || n > math.MaxInt32 {
			return fmt.Errorf("pbkdf2iters must be an integer of at least %d (TrueNAS 27.0 requires at least 1300000)", minPBKDF2Iters)
		}
	}
	return nil
}

var hexKey64 = regexp.MustCompile(`^[0-9A-Fa-f]{64}$`)

// plainFieldName matches field names that are safe to echo in an error.
var plainFieldName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// optionalSecretString reads a string field that is absent when missing, null
// or empty, without ever including its value in an error.
func optionalSecretString(m map[string]interface{}, field string) (string, bool, error) {
	v, ok := m[field]
	if !ok || v == nil {
		return "", false, nil
	}
	s, isStr := v.(string)
	if !isStr {
		return "", false, fmt.Errorf("encryption_options.%s must be a string", field)
	}
	return s, s != "", nil
}

// maskDatasetPayload returns a copy of a pool.dataset.create payload that is
// safe to show: the passphrase and key in encryption_options are replaced.
func maskDatasetPayload(payload map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(payload))
	for k, v := range payload {
		out[k] = v
	}
	if opts, ok := payload["encryption_options"].(map[string]interface{}); ok {
		masked := make(map[string]interface{}, len(opts))
		for k, v := range opts {
			if k == "passphrase" || k == "key" {
				v = redact.Marker
			}
			masked[k] = v
		}
		out["encryption_options"] = masked
	}
	return out
}
