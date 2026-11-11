package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/redact"
	"github.com/truenas/truenas-mcp/truenas"
)

func (r *Registry) registerDatasetTools() {
	// Dataset creation (write operation)
	r.tools["create_dataset"] = Tool{
		Definition: mcp.Tool{
			Name:        "create_dataset",
			Description: "Create a ZFS dataset (filesystem or volume) for storage. This tool is reusable for SMB shares, NFS exports, iSCSI LUNs, and application storage. Supports native ZFS encryption (encryption=true with encryption_options), compression, quotas, and advanced ZFS features.\n\n**WIZARD GUIDANCE FOR LLM:**\nWhen helping users create datasets, ask these questions in order:\n\n1. **Pool Selection**: Query available pools first, ask which pool to use\n2. **Dataset Name**: Suggest format 'pool/shares/name' or 'pool/apps/name'\n3. **Dataset Type**: FILESYSTEM (default, for files) or VOLUME (for block storage/VMs)\n4. **Share Type Optimization** (if for sharing):\n   - SMB: Windows/Mac file shares (recommend for SMB shares)\n   - NFS: Unix/Linux file shares\n   - MULTIPROTOCOL: Both SMB and NFS access\n   - APPS: Application storage\n   - GENERIC: General purpose (default)\n5. **Encryption** (recommend for sensitive data):\n   - Ask: \"Is this for sensitive data?\"\n   - If yes: set encryption=true and encryption_options with exactly ONE method; ask which the user wants and do not default to one: generate_key=true, passphrase (8-512 bytes) or key (64 hex characters). encryption_options without encryption=true is rejected\n   - generate_key: TrueNAS stores the key in its key database and does not return it; this tool cannot export it, so tell the user to back it up in the TrueNAS web UI (Datasets > select the dataset > Encryption > Export Key) and keep a copy somewhere other than this system\n   - passphrase: TrueNAS does not store it; if it is lost the data is lost\n   - key: the user must keep it; this tool does not return or recover it\n   - Under a passphrase-encrypted parent, the new dataset must use a passphrase (TrueNAS refuses generate_key and key there)\n   - The algorithm is not configurable through this tool; new encrypted datasets use AES-256-GCM\n6. **Compression**: LZ4 (recommended, balanced), ZSTD (modern), GZIP (higher compression), OFF\n7. **Space Quota** (optional): Ask if they want to limit size\n8. **ACL Type** (for SMB): NFSV4 (recommended for SMB/Windows), POSIX (Unix)\n9. **Advanced** (usually skip unless user asks):\n   - Deduplication: Warn about RAM overhead, recommend OFF\n   - Checksum, snapdir, atime, readonly\n\n**IMPORTANT RECOMMENDATIONS:**\n- For SMB shares: share_type=SMB, acltype=NFSV4, compression=LZ4\n- For NFS exports: share_type=NFS, acltype=POSIX, compression=LZ4\n- For multi-protocol: share_type=MULTIPROTOCOL, acltype=NFSV4\n- For apps: share_type=APPS, compression=LZ4 or ZSTD\n- Always recommend compression=LZ4 unless user has specific needs\n- Warn: Deduplication uses ~5GB RAM per TB, not recommended for most users\n- Warn: Encryption cannot be removed later, only option is to copy data elsewhere\n\n**BEFORE EXECUTING:**\n1. Use dry_run=true to preview the configuration\n2. Display summary showing: name, type, optimization, compression, encryption, quota, mountpoint\n3. Get explicit user confirmation with \"Shall I proceed?\"\n4. Warn: This is a WRITE operation creating permanent storage\n5. If encryption enabled, remind the user of the key-backup note for the chosen method after creation\n\n**DRY RUN:**\nSet dry_run=true to preview what will be created without executing. The preview's ancestors_to_create lists any missing parent datasets that would be created (create_ancestors defaults to true), from parent to child. Those ancestors are created with no properties of their own; they inherit compression, ACL type and encryption from their parent and have no quota of their own, and do not receive this dataset's share_type, compression or encryption. Under an unencrypted parent, ancestors of an encrypted dataset are unencrypted. The preview fails if the pool does not exist, the dataset already exists, or the name is invalid. Show user the preview, then ask for confirmation to proceed.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Dataset path including pool (e.g., 'tank/shares/documents' or 'pool/apps/immich')",
					},
					"type": map[string]interface{}{
						"type":        "string",
						"description": "FILESYSTEM (default, for files/directories) or VOLUME (for block storage/iSCSI/VMs)",
						"enum":        []string{"FILESYSTEM", "VOLUME"},
						"default":     "FILESYSTEM",
					},
					"volsize": map[string]interface{}{
						"type":        "integer",
						"description": "Required for VOLUME type: size in bytes (e.g., 1099511627776 for 1TB)",
						"minimum":     1,
					},
					"share_type": map[string]interface{}{
						"type":        "string",
						"description": "Optimization hint: GENERIC (default), SMB, NFS, MULTIPROTOCOL, APPS",
						"enum":        []string{"GENERIC", "SMB", "NFS", "MULTIPROTOCOL", "APPS"},
					},
					"compression": map[string]interface{}{
						"type":        "string",
						"description": "LZ4 (recommended, balanced), ZSTD (modern), GZIP (higher compression), OFF, or INHERIT (default)",
						"enum":        []string{"LZ4", "ZSTD", "GZIP", "GZIP-1", "GZIP-9", "OFF", "INHERIT"},
					},
					"acltype": map[string]interface{}{
						"type":        "string",
						"description": "NFSV4 (recommended for SMB/Windows ACLs) or POSIX (Unix permissions)",
						"enum":        []string{"NFSV4", "POSIX", "INHERIT"},
					},
					"encryption": map[string]interface{}{
						"type":        "boolean",
						"description": "Create the dataset as a new ZFS encryption root (cannot be removed later). Requires encryption_options. inherit_encryption is sent as false automatically; passing inherit_encryption=true with this is rejected",
					},
					"encryption_options": map[string]interface{}{
						"type":        "object",
						"description": "Key material for a new encrypted dataset; only valid together with encryption=true. Provide exactly one of generate_key=true, passphrase or key. The passphrase and key are secrets and are masked in dry-run output",
						"properties": map[string]interface{}{
							"generate_key": map[string]interface{}{
								"type":        "boolean",
								"description": "Let TrueNAS generate the key and store it in its key database; it is not returned. This tool cannot export it: back it up in the TrueNAS web UI (Datasets > select the dataset > Encryption > Export Key) and keep a copy elsewhere. Refused under a passphrase-encrypted parent",
							},
							"passphrase": map[string]interface{}{
								"type":        "string",
								"description": "Passphrase of 8 to 512 bytes. It is not stored by TrueNAS: if it is lost the data is lost",
							},
							"key": map[string]interface{}{
								"type":        "string",
								"description": "Raw encryption key as exactly 64 hexadecimal characters. You must keep a copy; this tool never returns it. Refused under a passphrase-encrypted parent",
							},
							"pbkdf2iters": map[string]interface{}{
								"type":        "integer",
								"description": "PBKDF2 iterations for a passphrase (optional; only with passphrase; minimum 100000, TrueNAS 27.0 requires at least 1300000). Omit to use the TrueNAS default",
							},
						},
					},
					"quota": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum space for dataset + children in bytes (e.g., 1099511627776 for 1TB)",
						"minimum":     0,
					},
					"refquota": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum space for dataset only (excluding children) in bytes",
						"minimum":     0,
					},
					"create_ancestors": map[string]interface{}{
						"type":        "boolean",
						"description": "Auto-create missing parent datasets (default: true). A dry-run preview lists any missing ancestors that would be created, from parent to child, in ancestors_to_create. Those ancestors inherit compression, ACL type and encryption from their parent and have no quota of their own",
						"default":     true,
					},
					"readonly": map[string]interface{}{
						"type":        "boolean",
						"description": "Make dataset read-only (default: false)",
						"default":     false,
					},
					"deduplication": map[string]interface{}{
						"type":        "string",
						"description": "OFF (recommended), ON, or VERIFY. Warning: Uses ~5GB RAM per TB of storage",
						"enum":        []string{"OFF", "ON", "VERIFY", "INHERIT"},
					},
					"checksum": map[string]interface{}{
						"type":        "string",
						"description": "Data integrity algorithm: SHA256 (default), BLAKE3, SHA512, etc.",
					},
					"snapdir": map[string]interface{}{
						"type":        "string",
						"description": "Snapshot directory visibility: VISIBLE or HIDDEN",
						"enum":        []string{"VISIBLE", "HIDDEN", "INHERIT"},
					},
					"atime": map[string]interface{}{
						"type":        "string",
						"description": "File access time tracking: ON or OFF (OFF improves performance)",
						"enum":        []string{"ON", "OFF", "INHERIT"},
					},
					"volblocksize": map[string]interface{}{
						"type":        "string",
						"description": "VOLUME type only (rejected for FILESYSTEM): volume block size. Defaults to 16K, or 128K on a dRAID pool",
						"enum":        []string{"512", "512B", "1K", "2K", "4K", "8K", "16K", "32K", "64K", "128K"},
					},
					"inherit_encryption": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional. Whether the dataset inherits the parent's encryption settings (TrueNAS default: true). false asks for an unencrypted dataset, which TrueNAS rejects under an encrypted parent. To create a new encrypted dataset use encryption=true with encryption_options instead (inherit_encryption is then sent as false automatically); inherit_encryption=true together with encryption=true is rejected",
					},
					"user_properties": map[string]interface{}{
						"type":        "array",
						"description": "Optional ZFS user properties to set on the dataset, each an object with a 'key' containing a colon in namespace:property form (e.g. 'org:note') and a string 'value'. Property names should use only lowercase letters, digits and ': . _ -' (newer TrueNAS versions reject anything else) and must not repeat within the list. Do not set properties TrueNAS manages itself (org.freenas:description, org.freenas:quota_warning, org.freenas:quota_critical, org.freenas:refquota_warning, org.freenas:refquota_critical, org.truenas:managedby); use the dedicated fields (comments, quota_warning, ...) instead",
						"items": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"key": map[string]interface{}{
									"type":        "string",
									"description": "User property name in namespace:property form; must contain a colon, e.g. 'org:note'",
								},
								"value": map[string]interface{}{
									"type":        "string",
									"description": "User property value",
								},
							},
							"required": []string{"key", "value"},
						},
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview what will be created without executing (default: false)",
						"default":     false,
					},
				},
				"required": []string{"name"},
			},
		},
		Handler: handleCreateDataset,
		Plan:    planCreateDataset,
	}
}

// Middleware method names used as writePlan.Method must be declared in
// this file (same-file contract for TestMiddlewareMethodContract).
const poolDatasetCreateMethod = "pool.dataset.create"

// buildCreateDatasetPayload constructs the pool.dataset.create body.
// The map is unchanged from main: create_ancestors defaults to true, and
// encryption_options are only sent together with encryption=true and
// inherit_encryption=false.
func buildCreateDatasetPayload(args map[string]interface{}) (map[string]interface{}, map[string]interface{}, error) {
	name, ok := args["name"].(string)
	if !ok || name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}

	dsType, ok := args["type"].(string)
	if !ok || dsType == "" {
		dsType = "FILESYSTEM"
	}

	if err := validateDatasetName(name); err != nil {
		return nil, nil, err
	}
	if dsType != "FILESYSTEM" && dsType != "VOLUME" {
		return nil, nil, fmt.Errorf("type must be FILESYSTEM or VOLUME, got: %s", dsType)
	}
	if err := validateVolblocksize(args, dsType); err != nil {
		return nil, nil, err
	}
	if err := validateUserProperties(args); err != nil {
		return nil, nil, err
	}

	payload := map[string]interface{}{
		"name": name,
		"type": dsType,
	}

	if dsType == "VOLUME" {
		volsize, ok := args["volsize"].(float64)
		if !ok || volsize <= 0 {
			return nil, nil, fmt.Errorf("volsize (in bytes) is required for VOLUME type")
		}
		payload["volsize"] = int64(volsize)
		if volblocksize, ok := args["volblocksize"].(string); ok {
			payload["volblocksize"] = volblocksize
		}
	}

	if shareType, ok := args["share_type"].(string); ok && shareType != "" {
		payload["share_type"] = shareType
	}
	if compression, ok := args["compression"].(string); ok && compression != "" {
		payload["compression"] = compression
	}
	if acltype, ok := args["acltype"].(string); ok && acltype != "" {
		payload["acltype"] = acltype
	}
	if quota, ok := args["quota"].(float64); ok && quota > 0 {
		payload["quota"] = int64(quota)
	}
	if refquota, ok := args["refquota"].(float64); ok && refquota > 0 {
		payload["refquota"] = int64(refquota)
	}
	if createAncestors, ok := args["create_ancestors"].(bool); ok {
		payload["create_ancestors"] = createAncestors
	} else {
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

	encrypted, encOpts, err := parseDatasetEncryption(args)
	if err != nil {
		return nil, nil, err
	}
	if encrypted {
		payload["encryption"] = true
		payload["encryption_options"] = encOpts
		payload["inherit_encryption"] = false
	} else if inheritEnc, ok := args["inherit_encryption"].(bool); ok {
		payload["inherit_encryption"] = inheritEnc
	}

	if userProps, ok := args["user_properties"].([]interface{}); ok && len(userProps) > 0 {
		payload["user_properties"] = userProps
	}
	return payload, encOpts, nil
}

// planCreateDataset builds the create_dataset plan used by both the dry-run
// preview and the real call. Preflight (pool exists, dataset name is free)
// runs on both paths so a dry run cannot succeed when the create would
// be refused.
func planCreateDataset(ctx context.Context, client *truenas.Client, args map[string]interface{}) (*writePlan, error) {
	payload, encOpts, err := buildCreateDatasetPayload(args)
	if err != nil {
		return nil, err
	}
	name, _ := payload["name"].(string)
	if err := checkCreateDatasetPreflight(ctx, client, name); err != nil {
		return nil, err
	}

	var missing []string
	var ancestorsNote string
	if createAncestors, _ := payload["create_ancestors"].(bool); createAncestors {
		var err error
		missing, err = missingAncestorDatasets(ctx, client, name)
		if err != nil {
			return nil, err
		}
		if len(missing) > 0 {
			ancestorsNote = ancestorsInheritNote
		}
	} else {
		missing = []string{}
	}

	current := map[string]interface{}{
		"payload":             maskDatasetPayload(payload),
		"ancestors_to_create": missing,
		"estimated_path":      fmt.Sprintf("/mnt/%s", name),
	}
	if ancestorsNote != "" {
		current["ancestors_note"] = ancestorsNote
	}

	return &writePlan{
		Tool:         "create_dataset",
		Method:       poolDatasetCreateMethod,
		CallParams:   []interface{}{payload},
		ParamNames:   []string{"data"},
		TargetName:   name,
		CurrentState: current,
		Actions: []PlannedAction{{
			Step:        1,
			Description: fmt.Sprintf("Create dataset %s", name),
			Operation:   "create",
			Target:      poolDatasetCreateMethod,
			Details: map[string]interface{}{
				"payload":             payload,
				"ancestors_to_create": missing,
				"estimated_path":      fmt.Sprintf("/mnt/%s", name),
			},
		}},
		Result: map[string]interface{}{
			"enc_opts": encOpts,
			"name":     name,
		},
	}, nil
}

// handleCreateDataset creates a new ZFS dataset (filesystem or volume)
func handleCreateDataset(ctx context.Context, client *truenas.Client, args map[string]interface{}) (string, error) {
	plan, err := planCreateDataset(ctx, client, args)
	if err != nil {
		return "", err
	}
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	if dryRun, ok := args["dry_run"].(bool); ok && dryRun {
		return plan.marshalDryRun()
	}
	return executeCreateDataset(ctx, client, args, plan)
}

func executeCreateDataset(ctx context.Context, client *truenas.Client, args map[string]interface{}, plan *writePlan) (string, error) {
	if err := plan.blockedReply(); err != nil {
		return "", err
	}
	encOpts, _ := plan.Result["enc_opts"].(map[string]interface{})
	result, err := callPlan(ctx, client, plan)
	if err != nil {
		return "", fmt.Errorf("failed to create dataset: %s", scrubDatasetCreateError(err, encOpts))
	}

	var dataset map[string]interface{}
	if err := json.Unmarshal(result, &dataset); err != nil {
		return "", fmt.Errorf("failed to parse dataset response: %w", err)
	}

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

// datasetAncestorNames returns intermediate parent datasets of name, from
// the pool's first child down to the parent of the leaf (not the pool or
// the dataset being created).
func datasetAncestorNames(name string) []string {
	parts := strings.Split(name, "/")
	if len(parts) < 3 {
		return nil
	}
	ancestors := make([]string, 0, len(parts)-2)
	for i := 1; i < len(parts)-1; i++ {
		ancestors = append(ancestors, strings.Join(parts[:i+1], "/"))
	}
	return ancestors
}

// ancestorsInheritNote explains that missing parents are created as plain
// filesystems that inherit from their parent, not with the leaf's properties.
const ancestorsInheritNote = "Missing ancestors are created with no properties of their own; they inherit compression, ACL type and encryption from their parent and have no quota of their own. They do not receive this dataset's share_type, compression or encryption. Under an unencrypted parent, ancestors of an encrypted dataset are unencrypted."

// maxDatasetNameLen is OpenZFS's ZFS_MAX_DATASET_NAME_LEN minus the NUL.
const maxDatasetNameLen = 255

func datasetNameExists(ctx context.Context, client *truenas.Client, name string) (bool, error) {
	result, err := client.Call(ctx, "pool.dataset.query",
		[]interface{}{[]interface{}{"name", "=", name}},
		map[string]interface{}{},
	)
	if err != nil {
		return false, fmt.Errorf("failed to check dataset %s: %w", name, err)
	}
	var rows []interface{}
	if err := json.Unmarshal(result, &rows); err != nil {
		return false, fmt.Errorf("failed to parse dataset query for %s: %w", name, err)
	}
	return len(rows) > 0, nil
}

// checkCreateDatasetPreflight rejects a dry-run preview that would read as
// if the create would succeed when the middleware would refuse it.
func checkCreateDatasetPreflight(ctx context.Context, client *truenas.Client, name string) error {
	pool := strings.SplitN(name, "/", 2)[0]
	pools, err := listPoolNames(ctx, client)
	if err != nil {
		return err
	}
	if !pools[pool] {
		return fmt.Errorf("pool %q does not exist", pool)
	}
	exists, err := datasetNameExists(ctx, client, name)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("dataset %q already exists", name)
	}
	return nil
}

// missingAncestorDatasets returns ancestors of name that do not yet exist,
// in parent-to-child order.
func missingAncestorDatasets(ctx context.Context, client *truenas.Client, name string) ([]string, error) {
	ancestors := datasetAncestorNames(name)
	if len(ancestors) == 0 {
		return []string{}, nil
	}
	missing := make([]string, 0, len(ancestors))
	for _, ancestor := range ancestors {
		exists, err := datasetNameExists(ctx, client, ancestor)
		if err != nil {
			return nil, err
		}
		if !exists {
			missing = append(missing, ancestor)
		}
	}
	return missing, nil
}

// datasetNameInvalidChars matches characters ZFS (and this tool) reject
// in a dataset name.
var datasetNameInvalidChars = regexp.MustCompile(`[^a-zA-Z0-9/_.-]`)

// validateDatasetName validates the dataset name format
func validateDatasetName(name string) error {
	if name == "" {
		return fmt.Errorf("dataset name cannot be empty")
	}

	// Must contain at least one slash (pool/dataset)
	if !strings.Contains(name, "/") {
		return fmt.Errorf("dataset name must include pool name (e.g., 'pool/dataset')")
	}

	if len(name) > maxDatasetNameLen {
		return fmt.Errorf("dataset name is too long (maximum %d characters)", maxDatasetNameLen)
	}

	// Check for invalid characters
	if datasetNameInvalidChars.MatchString(name) {
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

	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." {
			return fmt.Errorf("dataset name cannot contain '.' or '..' path components")
		}
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
