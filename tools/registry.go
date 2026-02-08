package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/truenas/truenas-mcp/mcp"
	"github.com/truenas/truenas-mcp/tasks"
	"github.com/truenas/truenas-mcp/truenas"
)

type Registry struct {
	client      *truenas.Client
	taskManager *tasks.Manager
	tools       map[string]Tool
}

type Tool struct {
	Definition mcp.Tool
	Handler    func(*truenas.Client, map[string]interface{}) (string, error)
}

func NewRegistry(client *truenas.Client, taskManager *tasks.Manager) *Registry {
	r := &Registry{
		client:      client,
		taskManager: taskManager,
		tools:       make(map[string]Tool),
	}
	r.registerTools()
	return r
}

func (r *Registry) registerTools() {
	// System info tool
	r.tools["system_info"] = Tool{
		Definition: mcp.Tool{
			Name:        "system_info",
			Description: "Get TrueNAS system information including version, hostname, and platform details",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleSystemInfo,
	}

	// System health tool
	r.tools["system_health"] = Tool{
		Definition: mcp.Tool{
			Name:        "system_health",
			Description: "Get system health status including alerts and diagnostics",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleSystemHealth,
	}

	// System update tools
	r.tools["check_updates"] = Tool{
		Definition: mcp.Tool{
			Name:        "check_updates",
			Description: "Check for available TrueNAS system updates",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleCheckUpdates,
	}

	r.tools["download_update"] = Tool{
		Definition: mcp.Tool{
			Name:        "download_update",
			Description: "Download TrueNAS system update. Supports dry-run mode to preview changes. Returns a task ID for tracking download progress. This is a write operation.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview changes without executing (default: false)",
						"default":     false,
					},
				},
			},
		},
		Handler: r.handleDownloadUpdateWithDryRun,
	}

	r.tools["apply_update"] = Tool{
		Definition: mcp.Tool{
			Name:        "apply_update",
			Description: "Apply downloaded TrueNAS system update. System will reboot if reboot parameter is true. Supports dry-run mode to preview changes. Returns a task ID for tracking progress. This is a write operation.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"reboot": map[string]interface{}{
						"type":        "boolean",
						"description": "Reboot after update completes (default: false for safety)",
						"default":     false,
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview changes without executing (default: false)",
						"default":     false,
					},
				},
			},
		},
		Handler: r.handleApplyUpdateWithDryRun,
	}

	r.tools["update_status"] = Tool{
		Definition: mcp.Tool{
			Name:        "update_status",
			Description: "Get current TrueNAS system update status and progress",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleUpdateStatus,
	}

	// System reboot tool
	r.tools["system_reboot"] = Tool{
		Definition: mcp.Tool{
			Name:        "system_reboot",
			Description: "Reboot the TrueNAS system. This will disconnect all active sessions and services. Use after applying system updates.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleSystemReboot,
	}

	// Storage pools query
	r.tools["query_pools"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_pools",
			Description: "Query storage pools with their status, capacity, and health information",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		Handler: handleQueryPools,
	}

	// Dataset query
	r.tools["query_datasets"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_datasets",
			Description: "Query datasets with optional filtering and sorting. Returns simplified dataset information with capacity, encryption status, and usage details. Use 'limit' to control result size, 'order_by' to sort by size, and 'encrypted_only' to filter.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pool": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Filter datasets by pool name",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: Maximum number of datasets to return (default: 50 for manageable response size)",
					},
					"order_by": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Sort by 'used' (space usage), 'available', or 'name' (default: used descending)",
						"enum":        []string{"used", "available", "name"},
					},
					"encrypted_only": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: Return only encrypted datasets (default: false)",
					},
				},
			},
		},
		Handler: handleQueryDatasets,
	}

	// Snapshots query
	r.tools["query_snapshots"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_snapshots",
			Description: "Query ZFS snapshots with optional filtering and sorting. Returns simplified snapshot information with creation info, dataset, and holds status. Use 'limit' to control result size, 'order_by' to sort.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"dataset": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Filter snapshots by parent dataset name",
					},
					"pool": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Filter snapshots by pool name",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: Maximum number of snapshots to return (default: 50 for manageable response size)",
					},
					"order_by": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Sort by 'name' (snapshot name, default descending), 'dataset' (parent dataset), or 'created' (parsed from name if available)",
						"enum":        []string{"name", "dataset", "created"},
					},
					"holds_only": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: Return only snapshots with holds that prevent deletion (default: false)",
					},
				},
			},
		},
		Handler: handleQuerySnapshots,
	}

	// Shares query
	r.tools["query_shares"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_shares",
			Description: "Query SMB and NFS shares configuration",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"share_type": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"smb", "nfs", "all"},
						"description": "Type of shares to query (default: all)",
						"default":     "all",
					},
				},
			},
		},
		Handler: handleQueryShares,
	}

	// VM query
	r.tools["query_vms"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_vms",
			Description: "Query virtual machines with optional filtering and sorting. Returns simplified VM information with resource allocation, status, and device summary. Excludes sensitive data like display passwords.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Filter VMs by name (partial match)",
					},
					"state": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Filter by VM state (default: all)",
						"enum":        []string{"RUNNING", "STOPPED", "all"},
					},
					"autostart": map[string]interface{}{
						"type":        "boolean",
						"description": "Optional: Filter by autostart setting",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Optional: Maximum number of VMs to return (default: 50)",
					},
					"order_by": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Sort by 'name' (default, alphabetical), 'memory' (descending), or 'status' (running first)",
						"enum":        []string{"name", "memory", "status"},
					},
				},
			},
		},
		Handler: handleQueryVMs,
	}

	// Dataset creation (write operation)
	r.tools["create_dataset"] = Tool{
		Definition: mcp.Tool{
			Name:        "create_dataset",
			Description: "Create a ZFS dataset (filesystem or volume) for storage. This tool is reusable for SMB shares, NFS exports, iSCSI LUNs, and application storage. Supports encryption, compression, quotas, and advanced ZFS features.\n\n**WIZARD GUIDANCE FOR LLM:**\nWhen helping users create datasets, ask these questions in order:\n\n1. **Pool Selection**: Query available pools first, ask which pool to use\n2. **Dataset Name**: Suggest format 'pool/shares/name' or 'pool/apps/name'\n3. **Dataset Type**: FILESYSTEM (default, for files) or VOLUME (for block storage/VMs)\n4. **Share Type Optimization** (if for sharing):\n   - SMB: Windows/Mac file shares (recommend for SMB shares)\n   - NFS: Unix/Linux file shares\n   - MULTIPROTOCOL: Both SMB and NFS access\n   - APPS: Application storage\n   - GENERIC: General purpose (default)\n5. **Encryption** (recommend for sensitive data):\n   - Ask: \"Is this for sensitive data?\"\n   - If yes: Recommend generate_key=true for simplicity\n   - If user wants passphrase: min 8 characters\n   - Algorithm: AES-256-GCM recommended\n6. **Compression**: LZ4 (recommended, balanced), ZSTD (modern), GZIP (higher compression), OFF\n7. **Space Quota** (optional): Ask if they want to limit size\n8. **ACL Type** (for SMB): NFSV4 (recommended for SMB/Windows), POSIX (Unix)\n9. **Advanced** (usually skip unless user asks):\n   - Deduplication: Warn about RAM overhead, recommend OFF\n   - Checksum, snapdir, atime, readonly\n\n**IMPORTANT RECOMMENDATIONS:**\n- For SMB shares: share_type=SMB, acltype=NFSV4, compression=LZ4\n- For NFS exports: share_type=NFS, acltype=POSIX, compression=LZ4\n- For multi-protocol: share_type=MULTIPROTOCOL, acltype=NFSV4\n- For apps: share_type=APPS, compression=LZ4 or ZSTD\n- Always recommend compression=LZ4 unless user has specific needs\n- Warn: Deduplication uses ~5GB RAM per TB, not recommended for most users\n- Warn: Encryption cannot be removed later, only option is to copy data elsewhere\n\n**BEFORE EXECUTING:**\n1. Use dry_run=true to preview the configuration\n2. Display summary showing: name, type, optimization, compression, encryption, quota, mountpoint\n3. Get explicit user confirmation with \"Shall I proceed?\"\n4. Warn: This is a WRITE operation creating permanent storage\n5. If encryption enabled, remind user to back up the key after creation\n\n**DRY RUN:**\nSet dry_run=true to preview what will be created without executing. Show user the preview, then ask for confirmation to proceed.",
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
					"encryption_options": map[string]interface{}{
						"type":        "object",
						"description": "Encryption configuration (cannot be removed later)",
						"properties": map[string]interface{}{
							"generate_key": map[string]interface{}{
								"type":        "boolean",
								"description": "Auto-generate encryption key (recommended for simplicity)",
							},
							"passphrase": map[string]interface{}{
								"type":        "string",
								"description": "User passphrase (min 8 chars) - alternative to generate_key",
							},
							"algorithm": map[string]interface{}{
								"type":        "string",
								"description": "Encryption algorithm (default: AES-256-GCM recommended)",
								"enum":        []string{"AES-128-CCM", "AES-192-CCM", "AES-256-CCM", "AES-128-GCM", "AES-192-GCM", "AES-256-GCM"},
							},
						},
					},
					"quota": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum space for dataset + children in bytes (e.g., 1099511627776 for 1TB)",
					},
					"refquota": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum space for dataset only (excluding children) in bytes",
					},
					"create_ancestors": map[string]interface{}{
						"type":        "boolean",
						"description": "Auto-create missing parent datasets (default: false)",
						"default":     false,
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
	}

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
	}

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
	}

	// Alert list with filtering
	r.tools["list_alerts"] = Tool{
		Definition: mcp.Tool{
			Name:        "list_alerts",
			Description: "List system alerts with optional filtering by dismissed status",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"dismissed": map[string]interface{}{
						"type":        "boolean",
						"description": "Filter by dismissed status (true=dismissed only, false=active only, omit=all)",
					},
				},
			},
		},
		Handler: handleListAlerts,
	}

	// Dismiss alert
	r.tools["dismiss_alert"] = Tool{
		Definition: mcp.Tool{
			Name:        "dismiss_alert",
			Description: "Dismiss a system alert by UUID",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"uuid": map[string]interface{}{
						"type":        "string",
						"description": "UUID of the alert to dismiss",
					},
				},
				"required": []string{"uuid"},
			},
		},
		Handler: handleDismissAlert,
	}

	// Restore alert
	r.tools["restore_alert"] = Tool{
		Definition: mcp.Tool{
			Name:        "restore_alert",
			Description: "Restore (un-dismiss) a previously dismissed alert by UUID",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"uuid": map[string]interface{}{
						"type":        "string",
						"description": "UUID of the alert to restore",
					},
				},
				"required": []string{"uuid"},
			},
		},
		Handler: handleRestoreAlert,
	}

	// System reporting metrics
	r.tools["get_system_metrics"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_system_metrics",
			Description: "Get system performance metrics (CPU, memory, load average)",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"graphs": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "string",
							"enum": []string{"cpu", "memory", "load"},
						},
						"description": "Metrics to retrieve (default: all)",
					},
					"unit": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"HOUR", "DAY", "WEEK", "MONTH", "YEAR"},
						"description": "Time range for metrics (default: HOUR)",
						"default":     "HOUR",
					},
				},
			},
		},
		Handler: handleGetSystemMetrics,
	}

	// Network reporting metrics
	r.tools["get_network_metrics"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_network_metrics",
			Description: "Get network interface traffic metrics",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"interface": map[string]interface{}{
						"type":        "string",
						"description": "Network interface name (e.g., 'eth0'). If omitted, returns all interfaces.",
					},
					"unit": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"HOUR", "DAY", "WEEK", "MONTH", "YEAR"},
						"description": "Time range for metrics (default: HOUR)",
						"default":     "HOUR",
					},
				},
			},
		},
		Handler: handleGetNetworkMetrics,
	}

	// Disk I/O reporting metrics
	r.tools["get_disk_metrics"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_disk_metrics",
			Description: "Get disk I/O performance metrics",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"disk": map[string]interface{}{
						"type":        "string",
						"description": "Disk name (e.g., 'sda'). If omitted, returns all disks.",
					},
					"unit": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"HOUR", "DAY", "WEEK", "MONTH", "YEAR"},
						"description": "Time range for metrics (default: HOUR)",
						"default":     "HOUR",
					},
				},
			},
		},
		Handler: handleGetDiskMetrics,
	}

	// Query installed apps
	r.tools["query_apps"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_apps",
			Description: "Query installed applications with their status, versions, and available updates",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"app_name": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Filter by specific app name",
					},
					"include_config": map[string]interface{}{
						"type":        "boolean",
						"description": "Include app configuration details (default: false)",
						"default":     false,
					},
				},
			},
		},
		Handler: handleQueryApps,
	}

	// Upgrade app
	r.tools["upgrade_app"] = Tool{
		Definition: mcp.Tool{
			Name:        "upgrade_app",
			Description: "Upgrade an application to a newer version. Supports dry-run mode to preview changes. Returns a task ID for tracking progress. This is a write operation that modifies the system.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"app_name": map[string]interface{}{
						"type":        "string",
						"description": "Name of the application to upgrade",
					},
					"version": map[string]interface{}{
						"type":        "string",
						"description": "Target version to upgrade to (default: 'latest')",
						"default":     "latest",
					},
					"snapshot_hostpaths": map[string]interface{}{
						"type":        "boolean",
						"description": "Create snapshots of host volumes before upgrade (default: true for safety)",
						"default":     true,
					},
					"dry_run": map[string]interface{}{
						"type":        "boolean",
						"description": "Preview changes without executing (default: false)",
						"default":     false,
					},
				},
				"required": []string{"app_name"},
			},
		},
		Handler: r.handleUpgradeAppWithDryRun,
	}

	// Query jobs
	r.tools["query_jobs"] = Tool{
		Definition: mcp.Tool{
			Name:        "query_jobs",
			Description: "Query system jobs (running, pending, or completed tasks like replication, snapshots, scrubs, etc.)",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"state": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"RUNNING", "WAITING", "SUCCESS", "FAILED", "ABORTED", "all"},
						"description": "Filter by job state (default: RUNNING)",
						"default":     "RUNNING",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum number of jobs to return (default: 50)",
						"default":     50,
					},
				},
			},
		},
		Handler: handleQueryJobs,
	}

	// Capacity analysis tool
	r.tools["analyze_capacity"] = Tool{
		Definition: mcp.Tool{
			Name:        "analyze_capacity",
			Description: "Analyze system capacity utilization and trends for capacity planning. Provides utilization percentages, growth rates, and projections based on historical metrics. Includes CPU, memory, network, and disk I/O analysis.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"time_range": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"DAY", "WEEK", "MONTH", "YEAR"},
						"description": "Historical time range for trend analysis (default: MONTH for ~90 days)",
						"default":     "MONTH",
					},
					"metrics": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "string",
							"enum": []string{"cpu", "memory", "network", "disk", "all"},
						},
						"description": "Metrics to analyze (default: all)",
					},
				},
			},
		},
		Handler: handleAnalyzeCapacity,
	}

	// Pool capacity details tool
	r.tools["get_pool_capacity_details"] = Tool{
		Definition: mcp.Tool{
			Name:        "get_pool_capacity_details",
			Description: "Get detailed pool and dataset capacity information with utilization analysis. Returns current capacity snapshot with breakdown by dataset. Note: Historical capacity trends are not available from TrueNAS API; use Netdata graphs if available.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pool_name": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Specific pool to analyze",
					},
				},
			},
		},
		Handler: handleGetPoolCapacityDetails,
	}

	// Task management tools
	r.tools["tasks_list"] = Tool{
		Definition: mcp.Tool{
			Name:        "tasks_list",
			Description: "List all active and recent tasks. Tasks represent long-running operations like app upgrades.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"cursor": map[string]interface{}{
						"type":        "string",
						"description": "Pagination cursor from previous response",
					},
					"limit": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum number of tasks to return (default: 50)",
						"default":     50,
					},
				},
			},
		},
		Handler: r.handleTasksList,
	}

	r.tools["tasks_get"] = Tool{
		Definition: mcp.Tool{
			Name:        "tasks_get",
			Description: "Get detailed status of a specific task by ID. Use this to track progress of long-running operations.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"task_id": map[string]interface{}{
						"type":        "string",
						"description": "Task ID to retrieve",
					},
				},
				"required": []string{"task_id"},
			},
		},
		Handler: r.handleTasksGet,
	}
}

func (r *Registry) ListTools() []mcp.Tool {
	tools := make([]mcp.Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		tools = append(tools, tool.Definition)
	}
	return tools
}

func (r *Registry) CallTool(name string, args map[string]interface{}) (string, error) {
	tool, exists := r.tools[name]
	if !exists {
		return "", fmt.Errorf("unknown tool: %s", name)
	}

	return tool.Handler(r.client, args)
}

// Tool handlers

func handleSystemInfo(client *truenas.Client, args map[string]interface{}) (string, error) {
	result, err := client.Call("system.info")
	if err != nil {
		return "", err
	}

	var info map[string]interface{}
	if err := json.Unmarshal(result, &info); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	formatted, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleSystemHealth(client *truenas.Client, args map[string]interface{}) (string, error) {
	// Get alerts
	result, err := client.Call("alert.list")
	if err != nil {
		return "", err
	}

	var alerts []map[string]interface{}
	if err := json.Unmarshal(result, &alerts); err != nil {
		return "", fmt.Errorf("failed to parse alerts: %w", err)
	}

	// Get active jobs
	jobsResult, err := client.Call("core.get_jobs", []interface{}{
		[]interface{}{"state", "=", "RUNNING"},
	})
	if err != nil {
		return "", fmt.Errorf("failed to get jobs: %w", err)
	}

	var jobs []map[string]interface{}
	if err := json.Unmarshal(jobsResult, &jobs); err != nil {
		return "", fmt.Errorf("failed to parse jobs: %w", err)
	}

	// Create summary of active jobs
	activeTasks := make([]map[string]interface{}, 0)
	for _, job := range jobs {
		taskSummary := map[string]interface{}{
			"id":          job["id"],
			"method":      job["method"],
			"state":       job["state"],
			"description": job["description"],
		}
		if progress, ok := job["progress"]; ok {
			taskSummary["progress"] = progress
		}
		activeTasks = append(activeTasks, taskSummary)
	}

	// Add capacity warnings
	capacityWarnings := make([]string, 0)

	// Quick capacity check using reporting data (last hour)
	cpuResult, err := client.Call("reporting.get_data", []interface{}{
		map[string]interface{}{
			"name":       "cpu",
			"identifier": nil,
		},
	}, map[string]interface{}{"unit": "HOUR"})
	if err == nil {
		var cpuData []map[string]interface{}
		if err := json.Unmarshal(cpuResult, &cpuData); err == nil && len(cpuData) > 0 {
			if dataPoints, err := extractDataPoints(cpuData[0]); err == nil {
				avgCPU := calculateAverage(dataPoints)
				if avgCPU > 85 {
					capacityWarnings = append(capacityWarnings,
						fmt.Sprintf("CPU utilization critical: %.1f%%", avgCPU))
				} else if avgCPU > 70 {
					capacityWarnings = append(capacityWarnings,
						fmt.Sprintf("CPU utilization elevated: %.1f%%", avgCPU))
				}
			}
		}
	}

	// Check memory
	sysInfoResult, err := client.Call("system.info")
	var totalMemory float64
	if err == nil {
		var sysInfo map[string]interface{}
		if err := json.Unmarshal(sysInfoResult, &sysInfo); err == nil {
			if physMem, ok := sysInfo["physmem"].(float64); ok {
				totalMemory = physMem
			}
		}
	}

	if totalMemory > 0 {
		memResult, err := client.Call("reporting.get_data", []interface{}{
			map[string]interface{}{
				"name":       "memory",
				"identifier": nil,
			},
		}, map[string]interface{}{"unit": "HOUR"})
		if err == nil {
			var memData []map[string]interface{}
			if err := json.Unmarshal(memResult, &memData); err == nil && len(memData) > 0 {
				if dataPoints, err := extractDataPoints(memData[0]); err == nil {
					// Convert to percentage
					avgMemBytes := calculateAverage(dataPoints)
					avgMemPct := (avgMemBytes / totalMemory) * 100
					if avgMemPct > 85 {
						capacityWarnings = append(capacityWarnings,
							fmt.Sprintf("Memory utilization critical: %.1f%%", avgMemPct))
					} else if avgMemPct > 70 {
						capacityWarnings = append(capacityWarnings,
							fmt.Sprintf("Memory utilization elevated: %.1f%%", avgMemPct))
					}
				}
			}
		}
	}

	// Check pool capacity
	poolResult, err := client.Call("pool.query")
	if err == nil {
		var pools []map[string]interface{}
		if err := json.Unmarshal(poolResult, &pools); err == nil {
			for _, pool := range pools {
				poolName, _ := pool["name"].(string)
				capacity := calculatePoolCapacity(pool)

				if utilPct, ok := capacity["utilization_pct"].(float64); ok {
					if utilPct > 85 {
						capacityWarnings = append(capacityWarnings,
							fmt.Sprintf("Pool '%s' capacity critical: %.1f%%", poolName, utilPct))
					} else if utilPct > 70 {
						capacityWarnings = append(capacityWarnings,
							fmt.Sprintf("Pool '%s' capacity elevated: %.1f%%", poolName, utilPct))
					}
				}
			}
		}
	}

	response := map[string]interface{}{
		"alerts":            alerts,
		"alert_count":       len(alerts),
		"active_jobs":       activeTasks,
		"job_count":         len(activeTasks),
		"capacity_warnings": capacityWarnings,
		"health_check":      "OK",
	}

	if len(alerts) > 0 {
		response["health_check"] = "ALERTS_PRESENT"
	}

	if len(activeTasks) > 0 {
		if response["health_check"] == "OK" {
			response["health_check"] = "ACTIVE_TASKS"
		} else {
			response["health_check"] = "ALERTS_AND_ACTIVE_TASKS"
		}
	}

	if len(capacityWarnings) > 0 {
		if response["health_check"] == "OK" {
			response["health_check"] = "CAPACITY_WARNINGS"
		} else {
			response["health_check"] = response["health_check"].(string) + "_AND_CAPACITY"
		}
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleQueryPools(client *truenas.Client, args map[string]interface{}) (string, error) {
	result, err := client.Call("pool.query")
	if err != nil {
		return "", err
	}

	var pools []map[string]interface{}
	if err := json.Unmarshal(result, &pools); err != nil {
		return "", fmt.Errorf("failed to parse pools (raw response: %s): %w", string(result), err)
	}

	formatted, err := json.MarshalIndent(pools, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleQueryDatasets(client *truenas.Client, args map[string]interface{}) (string, error) {
	// Build query filters - initialize as empty array, not nil (API expects [] not null)
	filters := []interface{}{}
	if pool, ok := args["pool"].(string); ok && pool != "" {
		filters = []interface{}{
			[]interface{}{"name", "^", pool},
		}
	}

	// Options parameter (required by API even if empty)
	options := map[string]interface{}{}

	result, err := client.Call("pool.dataset.query", filters, options)
	if err != nil {
		return "", err
	}

	var datasets []map[string]interface{}
	if err := json.Unmarshal(result, &datasets); err != nil {
		return "", fmt.Errorf("failed to parse datasets: %w", err)
	}

	// Simplify response
	simplified := make([]map[string]interface{}, 0, len(datasets))
	for _, ds := range datasets {
		summary := simplifyDataset(ds)
		simplified = append(simplified, summary)
	}

	// Filter by encryption status if requested
	if encryptedOnly, ok := args["encrypted_only"].(bool); ok && encryptedOnly {
		filtered := make([]map[string]interface{}, 0)
		for _, ds := range simplified {
			if encrypted, ok := ds["encrypted"].(bool); ok && encrypted {
				filtered = append(filtered, ds)
			}
		}
		simplified = filtered
	}

	// Sort datasets
	orderBy := "used" // default to sorting by space usage
	if order, ok := args["order_by"].(string); ok && order != "" {
		orderBy = order
	}
	sortDatasets(simplified, orderBy)

	// Apply limit (default to 50 for manageable response size)
	limit := 50
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	if len(simplified) > limit {
		simplified = simplified[:limit]
	}

	// Add metadata wrapper
	response := map[string]interface{}{
		"datasets":       simplified,
		"dataset_count":  len(simplified),
		"total_datasets": len(datasets),
	}
	if pool, ok := args["pool"].(string); ok && pool != "" {
		response["pool_filter"] = pool
	}
	if len(simplified) < len(datasets) {
		response["note"] = fmt.Sprintf("Showing %d of %d datasets (limited)", len(simplified), len(datasets))
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// simplifyDataset extracts the most relevant fields from a raw dataset object
func simplifyDataset(ds map[string]interface{}) map[string]interface{} {
	summary := map[string]interface{}{
		"name": ds["name"],
		"type": ds["type"],
		"pool": ds["pool"],
	}

	// Helper to extract parsed value from property object
	getParsed := func(prop interface{}) interface{} {
		if propMap, ok := prop.(map[string]interface{}); ok {
			return propMap["parsed"]
		}
		return nil
	}

	// Helper to extract human-readable value from property object
	getValue := func(prop interface{}) interface{} {
		if propMap, ok := prop.(map[string]interface{}); ok {
			if val := propMap["value"]; val != nil {
				return val
			}
			return propMap["parsed"]
		}
		return nil
	}

	// Mountpoint (direct field, not nested)
	if mp, ok := ds["mountpoint"].(string); ok && mp != "" {
		summary["mountpoint"] = mp
	}

	// Capacity fields (CRITICAL for user queries)
	if used := getParsed(ds["used"]); used != nil {
		summary["used_bytes"] = used
		summary["used"] = getValue(ds["used"]) // Human readable like "1008.3 GiB"
	}
	if avail := getParsed(ds["available"]); avail != nil {
		summary["available_bytes"] = avail
		summary["available"] = getValue(ds["available"]) // Human readable like "5.87 TiB"
	}

	// Usage breakdown (useful for understanding where space goes)
	if snapUsed := getParsed(ds["usedbysnapshots"]); snapUsed != nil {
		if bytes, ok := snapUsed.(float64); ok && bytes > 0 {
			summary["used_by_snapshots"] = getValue(ds["usedbysnapshots"])
		}
	}
	if dsUsed := getParsed(ds["usedbydataset"]); dsUsed != nil {
		summary["used_by_dataset"] = getValue(ds["usedbydataset"])
	}
	if childUsed := getParsed(ds["usedbychildren"]); childUsed != nil {
		if bytes, ok := childUsed.(float64); ok && bytes > 0 {
			summary["used_by_children"] = getValue(ds["usedbychildren"])
		}
	}

	// Compression
	if comp := getParsed(ds["compression"]); comp != nil {
		summary["compression"] = comp
		if ratio := getParsed(ds["compressratio"]); ratio != nil {
			summary["compression_ratio"] = ratio
		}
	}

	// Deduplication (only if enabled)
	if dedup := getParsed(ds["deduplication"]); dedup != nil {
		if dedupStr, ok := dedup.(string); ok && dedupStr != "off" {
			summary["deduplication"] = dedup
		}
	}

	// Quotas (only if set)
	if quota := getParsed(ds["quota"]); quota != nil {
		summary["quota"] = getValue(ds["quota"])
	}
	if refquota := getParsed(ds["refquota"]); refquota != nil {
		summary["refquota"] = getValue(ds["refquota"])
	}

	// Encryption
	if encrypted, ok := ds["encrypted"].(bool); ok {
		summary["encrypted"] = encrypted
		if encrypted {
			if locked, ok := ds["locked"].(bool); ok {
				summary["locked"] = locked
			}
			if keyLoaded, ok := ds["key_loaded"].(bool); ok && keyLoaded {
				summary["key_loaded"] = keyLoaded
			}
		}
	}

	// Children count (useful for understanding hierarchy)
	if children, ok := ds["children"].([]interface{}); ok {
		summary["children_count"] = len(children)
	}

	return summary
}

// sortDatasets sorts a slice of simplified datasets by the specified field
func sortDatasets(datasets []map[string]interface{}, orderBy string) {
	sort.Slice(datasets, func(i, j int) bool {
		switch orderBy {
		case "used":
			// Sort by used_bytes descending (largest first)
			iUsed, iOk := datasets[i]["used_bytes"].(float64)
			jUsed, jOk := datasets[j]["used_bytes"].(float64)
			if iOk && jOk {
				return iUsed > jUsed
			}
			return false
		case "available":
			// Sort by available_bytes descending (most available first)
			iAvail, iOk := datasets[i]["available_bytes"].(float64)
			jAvail, jOk := datasets[j]["available_bytes"].(float64)
			if iOk && jOk {
				return iAvail > jAvail
			}
			return false
		case "name":
			// Sort by name alphabetically
			iName, iOk := datasets[i]["name"].(string)
			jName, jOk := datasets[j]["name"].(string)
			if iOk && jOk {
				return iName < jName
			}
			return false
		default:
			// Default to name
			iName, iOk := datasets[i]["name"].(string)
			jName, jOk := datasets[j]["name"].(string)
			if iOk && jOk {
				return iName < jName
			}
			return false
		}
	})
}

func handleQueryShares(client *truenas.Client, args map[string]interface{}) (string, error) {
	shareType := "all"
	if st, ok := args["share_type"].(string); ok && st != "" {
		shareType = st
	}

	response := make(map[string]interface{})

	// Query SMB shares
	if shareType == "smb" || shareType == "all" {
		result, err := client.Call("sharing.smb.query")
		if err != nil {
			return "", fmt.Errorf("failed to query SMB shares: %w", err)
		}

		var smbShares []map[string]interface{}
		if err := json.Unmarshal(result, &smbShares); err != nil {
			return "", fmt.Errorf("failed to parse SMB shares: %w", err)
		}
		response["smb_shares"] = smbShares
	}

	// Query NFS shares
	if shareType == "nfs" || shareType == "all" {
		result, err := client.Call("sharing.nfs.query")
		if err != nil {
			return "", fmt.Errorf("failed to query NFS shares: %w", err)
		}

		var nfsShares []map[string]interface{}
		if err := json.Unmarshal(result, &nfsShares); err != nil {
			return "", fmt.Errorf("failed to parse NFS shares: %w", err)
		}
		response["nfs_shares"] = nfsShares
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleQuerySnapshots(client *truenas.Client, args map[string]interface{}) (string, error) {
	// Build query filters - initialize as empty array, not nil (API expects [] not null)
	filters := []interface{}{}
	if dataset, ok := args["dataset"].(string); ok && dataset != "" {
		filters = append(filters, []interface{}{"dataset", "=", dataset})
	}
	if pool, ok := args["pool"].(string); ok && pool != "" {
		filters = append(filters, []interface{}{"pool", "=", pool})
	}

	// Options parameter (required by API even if empty)
	options := map[string]interface{}{}

	result, err := client.Call("pool.snapshot.query", filters, options)
	if err != nil {
		return "", err
	}

	var snapshots []map[string]interface{}
	if err := json.Unmarshal(result, &snapshots); err != nil {
		return "", fmt.Errorf("failed to parse snapshots: %w", err)
	}

	// Simplify response
	simplified := make([]map[string]interface{}, 0, len(snapshots))
	for _, snap := range snapshots {
		summary := simplifySnapshot(snap)
		simplified = append(simplified, summary)
	}

	// Filter by holds_only if requested
	if holdsOnly, ok := args["holds_only"].(bool); ok && holdsOnly {
		filtered := make([]map[string]interface{}, 0)
		for _, snap := range simplified {
			if holdsCount, ok := snap["holds_count"].(int); ok && holdsCount > 0 {
				filtered = append(filtered, snap)
			}
		}
		simplified = filtered
	}

	// Sort snapshots
	orderBy := "name" // default to sorting by snapshot name descending
	if order, ok := args["order_by"].(string); ok && order != "" {
		orderBy = order
	}
	sortSnapshots(simplified, orderBy)

	// Apply limit (default to 50 for manageable response size)
	limit := 50
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	totalSnapshots := len(simplified)
	if len(simplified) > limit {
		simplified = simplified[:limit]
	}

	// Add metadata wrapper
	response := map[string]interface{}{
		"snapshots":       simplified,
		"snapshot_count":  len(simplified),
		"total_snapshots": totalSnapshots,
	}
	if dataset, ok := args["dataset"].(string); ok && dataset != "" {
		response["dataset_filter"] = dataset
	}
	if pool, ok := args["pool"].(string); ok && pool != "" {
		response["pool_filter"] = pool
	}
	if holdsOnly, ok := args["holds_only"].(bool); ok && holdsOnly {
		response["holds_filter"] = "only snapshots with holds"
	}
	if len(simplified) < totalSnapshots {
		response["note"] = fmt.Sprintf("Showing %d of %d snapshots (limited)", len(simplified), totalSnapshots)
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// simplifySnapshot extracts the most relevant fields from a raw snapshot object
func simplifySnapshot(snap map[string]interface{}) map[string]interface{} {
	summary := map[string]interface{}{
		"snapshot_name": snap["snapshot_name"],
		"dataset":       snap["dataset"],
		"pool":          snap["pool"],
	}

	// Parse creation date from snapshot name if it matches pattern
	if snapName, ok := snap["snapshot_name"].(string); ok {
		if parsedDate := parseSnapshotDate(snapName); parsedDate != "" {
			summary["created_date"] = parsedDate
		}
	}

	// Add createtxg for reference
	if txg, ok := snap["createtxg"].(string); ok {
		summary["createtxg"] = txg
	}

	// Count holds and extract names
	if holds, ok := snap["holds"].(map[string]interface{}); ok {
		if len(holds) > 0 {
			summary["holds_count"] = len(holds)
			holdNames := make([]string, 0, len(holds))
			for name := range holds {
				holdNames = append(holdNames, name)
			}
			summary["holds"] = holdNames
		}
	}

	// Include full snapshot ID for reference
	if id, ok := snap["id"].(string); ok {
		summary["full_name"] = id
	}

	return summary
}

// parseSnapshotDate attempts to extract date information from snapshot names
func parseSnapshotDate(name string) string {
	// Common patterns used by automatic snapshot tasks
	patterns := []struct {
		layout string
		prefix string
	}{
		{"2006-01-02_15-04", "auto-"},    // auto-YYYY-MM-DD_HH-MM
		{"2006-01-02", "auto-"},          // auto-YYYY-MM-DD
		{"2006-01-02_15-04", ""},         // YYYY-MM-DD_HH-MM
		{"2006-01-02", ""},               // YYYY-MM-DD
		{"20060102-1504", "auto-"},       // auto-YYYYMMDD-HHMM
		{"20060102", "auto-"},            // auto-YYYYMMDD
		{"2006-01-02_15-04-05", "auto-"}, // auto-YYYY-MM-DD_HH-MM-SS
		{"2006-01-02_1504", ""},          // YYYY-MM-DD_HHMM
	}

	for _, p := range patterns {
		// Try to extract date substring
		dateStr := name
		if p.prefix != "" && strings.HasPrefix(name, p.prefix) {
			dateStr = strings.TrimPrefix(name, p.prefix)
		}

		// Try parsing with this layout
		if t, err := time.Parse(p.layout, dateStr); err == nil {
			return t.Format("2006-01-02 15:04")
		}

		// Also try just the first part before any underscore
		if idx := strings.Index(dateStr, "_"); idx > 0 {
			if t, err := time.Parse("2006-01-02", dateStr[:idx]); err == nil {
				return t.Format("2006-01-02")
			}
		}
	}

	return "" // No date found
}

// sortSnapshots sorts a slice of simplified snapshots by the specified field
func sortSnapshots(snapshots []map[string]interface{}, orderBy string) {
	sort.Slice(snapshots, func(i, j int) bool {
		switch orderBy {
		case "name":
			// Sort by snapshot_name descending (newest automatic snapshots first)
			iName, iOk := snapshots[i]["snapshot_name"].(string)
			jName, jOk := snapshots[j]["snapshot_name"].(string)
			if iOk && jOk {
				return iName > jName // Descending
			}
			return false
		case "dataset":
			// Sort by dataset path alphabetically ascending
			iDataset, iOk := snapshots[i]["dataset"].(string)
			jDataset, jOk := snapshots[j]["dataset"].(string)
			if iOk && jOk {
				return iDataset < jDataset
			}
			return false
		case "created":
			// Sort by parsed created_date descending, fallback to name
			iCreated, iOk := snapshots[i]["created_date"].(string)
			jCreated, jOk := snapshots[j]["created_date"].(string)
			if iOk && jOk {
				return iCreated > jCreated
			}
			// Fallback to name comparison
			iName, iOk := snapshots[i]["snapshot_name"].(string)
			jName, jOk := snapshots[j]["snapshot_name"].(string)
			if iOk && jOk {
				return iName > jName
			}
			return false
		default:
			// Default to name descending
			iName, iOk := snapshots[i]["snapshot_name"].(string)
			jName, jOk := snapshots[j]["snapshot_name"].(string)
			if iOk && jOk {
				return iName > jName
			}
			return false
		}
	})
}

func handleQueryVMs(client *truenas.Client, args map[string]interface{}) (string, error) {
	// Call vm.query with no filters (we'll filter in post-processing)
	result, err := client.Call("vm.query")
	if err != nil {
		return "", err
	}

	var vms []map[string]interface{}
	if err := json.Unmarshal(result, &vms); err != nil {
		return "", fmt.Errorf("failed to parse VMs: %w", err)
	}

	// Simplify response
	simplified := make([]map[string]interface{}, 0, len(vms))
	for _, vm := range vms {
		summary := simplifyVM(vm)
		simplified = append(simplified, summary)
	}

	// Filter by name (partial match)
	if name, ok := args["name"].(string); ok && name != "" {
		filtered := make([]map[string]interface{}, 0)
		nameLower := strings.ToLower(name)
		for _, vm := range simplified {
			if vmName, ok := vm["name"].(string); ok {
				if strings.Contains(strings.ToLower(vmName), nameLower) {
					filtered = append(filtered, vm)
				}
			}
		}
		simplified = filtered
	}

	// Filter by state
	if state, ok := args["state"].(string); ok && state != "" && state != "all" {
		filtered := make([]map[string]interface{}, 0)
		for _, vm := range simplified {
			if vmState, ok := vm["state"].(string); ok && vmState == state {
				filtered = append(filtered, vm)
			}
		}
		simplified = filtered
	}

	// Filter by autostart
	if autostart, ok := args["autostart"].(bool); ok {
		filtered := make([]map[string]interface{}, 0)
		for _, vm := range simplified {
			if vmAutostart, ok := vm["autostart"].(bool); ok && vmAutostart == autostart {
				filtered = append(filtered, vm)
			}
		}
		simplified = filtered
	}

	// Sort VMs
	orderBy := "name" // default to sorting by name
	if order, ok := args["order_by"].(string); ok && order != "" {
		orderBy = order
	}
	sortVMs(simplified, orderBy)

	// Apply limit (default to 50)
	limit := 50
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	totalVMs := len(simplified)
	if len(simplified) > limit {
		simplified = simplified[:limit]
	}

	// Add metadata wrapper
	response := map[string]interface{}{
		"vms":       simplified,
		"vm_count":  len(simplified),
		"total_vms": totalVMs,
	}
	if name, ok := args["name"].(string); ok && name != "" {
		response["name_filter"] = name
	}
	if state, ok := args["state"].(string); ok && state != "" && state != "all" {
		response["state_filter"] = state
	}
	if autostart, ok := args["autostart"].(bool); ok {
		response["autostart_filter"] = autostart
	}
	if len(simplified) < totalVMs {
		response["note"] = fmt.Sprintf("Showing %d of %d VMs (limited)", len(simplified), totalVMs)
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// simplifyVM extracts the most relevant fields from a raw VM object
func simplifyVM(vm map[string]interface{}) map[string]interface{} {
	summary := map[string]interface{}{
		"id":   vm["id"],
		"name": vm["name"],
		"uuid": vm["uuid"],
	}

	// Description (only if not empty)
	if desc, ok := vm["description"].(string); ok && desc != "" {
		summary["description"] = desc
	}

	// CPU configuration
	if vcpus, ok := vm["vcpus"].(float64); ok {
		summary["vcpus"] = int(vcpus)
	}
	if cores, ok := vm["cores"].(float64); ok {
		summary["cores"] = int(cores)
	}
	if threads, ok := vm["threads"].(float64); ok {
		summary["threads"] = int(threads)
	}
	if cpuMode, ok := vm["cpu_mode"].(string); ok {
		summary["cpu_mode"] = cpuMode
	}

	// Memory (convert to GB for readability)
	if memory, ok := vm["memory"].(float64); ok {
		summary["memory_mb"] = int(memory)
		summary["memory_gb"] = fmt.Sprintf("%.1f GB", memory/1024.0)
	}

	// Boot configuration
	if bootloader, ok := vm["bootloader"].(string); ok {
		summary["bootloader"] = bootloader
	}
	if autostart, ok := vm["autostart"].(bool); ok {
		summary["autostart"] = autostart
	}

	// Status information
	if status, ok := vm["status"].(map[string]interface{}); ok {
		if state, ok := status["state"].(string); ok {
			summary["state"] = state
		}
		if pid, ok := status["pid"].(float64); ok && pid > 0 {
			summary["pid"] = int(pid)
		}
	}

	// Device summary (sanitized - no passwords or sensitive data)
	if devices, ok := vm["devices"].([]interface{}); ok {
		deviceSummary := simplifyVMDevices(devices)
		for k, v := range deviceSummary {
			summary[k] = v
		}
	}

	return summary
}

// simplifyVMDevices extracts device information without sensitive data
func simplifyVMDevices(devices []interface{}) map[string]interface{} {
	summary := map[string]interface{}{
		"device_count": len(devices),
	}

	var disks []map[string]interface{}
	var nics []map[string]interface{}
	var displays []map[string]interface{}

	for _, dev := range devices {
		device, ok := dev.(map[string]interface{})
		if !ok {
			continue
		}

		attrs, ok := device["attributes"].(map[string]interface{})
		if !ok {
			continue
		}

		dtype, _ := attrs["dtype"].(string)

		switch dtype {
		case "DISK":
			disk := map[string]interface{}{}
			if path, ok := attrs["path"].(string); ok {
				disk["path"] = path
			}
			if diskType, ok := attrs["type"].(string); ok {
				disk["type"] = diskType
			}
			if serial, ok := attrs["serial"].(string); ok {
				disk["serial"] = serial
			}
			disks = append(disks, disk)

		case "NIC":
			nic := map[string]interface{}{}
			if nicType, ok := attrs["type"].(string); ok {
				nic["type"] = nicType
			}
			if attach, ok := attrs["nic_attach"].(string); ok {
				nic["attached_to"] = attach
			}
			if mac, ok := attrs["mac"].(string); ok {
				nic["mac"] = mac
			}
			nics = append(nics, nic)

		case "DISPLAY":
			display := map[string]interface{}{}
			if displayType, ok := attrs["type"].(string); ok {
				display["type"] = displayType
			}
			if port, ok := attrs["port"].(float64); ok {
				display["port"] = int(port)
			}
			if webPort, ok := attrs["web_port"].(float64); ok {
				display["web_port"] = int(webPort)
			}
			if bind, ok := attrs["bind"].(string); ok {
				display["bind"] = bind
			}
			// Explicitly exclude password field for security
			displays = append(displays, display)
		}
	}

	if len(disks) > 0 {
		summary["disks"] = disks
		summary["disk_count"] = len(disks)
	}
	if len(nics) > 0 {
		summary["nics"] = nics
		summary["nic_count"] = len(nics)
	}
	if len(displays) > 0 {
		summary["displays"] = displays
		summary["display_count"] = len(displays)
	}

	return summary
}

// sortVMs sorts a slice of simplified VMs by the specified field
func sortVMs(vms []map[string]interface{}, orderBy string) {
	sort.Slice(vms, func(i, j int) bool {
		switch orderBy {
		case "name":
			// Sort by name alphabetically ascending
			iName, iOk := vms[i]["name"].(string)
			jName, jOk := vms[j]["name"].(string)
			if iOk && jOk {
				return iName < jName
			}
			return false
		case "memory":
			// Sort by memory descending (largest first)
			iMem, iOk := vms[i]["memory_mb"].(int)
			jMem, jOk := vms[j]["memory_mb"].(int)
			if iOk && jOk {
				return iMem > jMem
			}
			return false
		case "status":
			// Sort by state (RUNNING first, then others)
			iState, iOk := vms[i]["state"].(string)
			jState, jOk := vms[j]["state"].(string)
			if iOk && jOk {
				if iState == "RUNNING" && jState != "RUNNING" {
					return true
				}
				if jState == "RUNNING" && iState != "RUNNING" {
					return false
				}
				// If both same state, sort by name
				iName, _ := vms[i]["name"].(string)
				jName, _ := vms[j]["name"].(string)
				return iName < jName
			}
			return false
		default:
			// Default to name
			iName, iOk := vms[i]["name"].(string)
			jName, jOk := vms[j]["name"].(string)
			if iOk && jOk {
				return iName < jName
			}
			return false
		}
	})
}

// Alert management handlers

func handleListAlerts(client *truenas.Client, args map[string]interface{}) (string, error) {
	// alert.list doesn't take filter parameters in the same way as other queries
	// It just returns all alerts, so we'll filter in post-processing if needed
	result, err := client.Call("alert.list")
	if err != nil {
		return "", err
	}

	var alerts []map[string]interface{}
	if err := json.Unmarshal(result, &alerts); err != nil {
		return "", fmt.Errorf("failed to parse alerts: %w", err)
	}

	// Post-filter by dismissed status if requested
	if dismissed, ok := args["dismissed"].(bool); ok {
		filtered := make([]map[string]interface{}, 0)
		for _, alert := range alerts {
			if isDismissed, ok := alert["dismissed"].(bool); ok && isDismissed == dismissed {
				filtered = append(filtered, alert)
			}
		}
		alerts = filtered
	}

	formatted, err := json.MarshalIndent(alerts, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleDismissAlert(client *truenas.Client, args map[string]interface{}) (string, error) {
	uuid, ok := args["uuid"].(string)
	if !ok || uuid == "" {
		return "", fmt.Errorf("uuid parameter is required")
	}

	result, err := client.Call("alert.dismiss", uuid)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Alert %s dismissed successfully: %s", uuid, string(result)), nil
}

func handleRestoreAlert(client *truenas.Client, args map[string]interface{}) (string, error) {
	uuid, ok := args["uuid"].(string)
	if !ok || uuid == "" {
		return "", fmt.Errorf("uuid parameter is required")
	}

	result, err := client.Call("alert.restore", uuid)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Alert %s restored successfully: %s", uuid, string(result)), nil
}

// Reporting handlers

func handleGetSystemMetrics(client *truenas.Client, args map[string]interface{}) (string, error) {
	unit := "HOUR"
	if u, ok := args["unit"].(string); ok && u != "" {
		unit = u
	}

	// Default graphs if not specified
	graphs := []string{"cpu", "memory", "load"}
	if g, ok := args["graphs"].([]interface{}); ok && len(g) > 0 {
		graphs = make([]string, len(g))
		for i, v := range g {
			if s, ok := v.(string); ok {
				graphs[i] = s
			}
		}
	}

	response := make(map[string]interface{})

	for _, graph := range graphs {
		var apiGraph string
		switch graph {
		case "cpu":
			apiGraph = "cpu"
		case "memory":
			apiGraph = "memory"
		case "load":
			apiGraph = "load"
		default:
			continue
		}

		result, err := client.Call("reporting.get_data", []interface{}{
			map[string]interface{}{
				"name":       apiGraph,
				"identifier": nil,
			},
		}, map[string]interface{}{"unit": unit})
		if err != nil {
			response[graph] = map[string]string{"error": err.Error()}
			continue
		}

		var fullData []map[string]interface{}
		if err := json.Unmarshal(result, &fullData); err != nil {
			response[graph] = map[string]string{"error": fmt.Sprintf("parse error: %v", err)}
			continue
		}

		// Keep aggregations and metadata, but sample data points to reduce size
		summary := make(map[string]interface{})
		if len(fullData) > 0 {
			for key, value := range fullData[0] {
				if key == "data" {
					// Include sample of data points: first 10 and last 10
					if dataArray, ok := value.([]interface{}); ok {
						summary["data_points_total"] = len(dataArray)
						sample := make([]interface{}, 0)

						// First 10 points
						for i := 0; i < 10 && i < len(dataArray); i++ {
							sample = append(sample, dataArray[i])
						}

						// Last 10 points (if we have more than 20 total)
						if len(dataArray) > 20 {
							for i := len(dataArray) - 10; i < len(dataArray); i++ {
								sample = append(sample, dataArray[i])
							}
						}

						summary["data_sample"] = sample
					}
				} else {
					// Keep all other fields: aggregations, start, end, legend, name, identifier
					summary[key] = value
				}
			}
		}
		response[graph] = summary
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleGetNetworkMetrics(client *truenas.Client, args map[string]interface{}) (string, error) {
	unit := "HOUR"
	if u, ok := args["unit"].(string); ok && u != "" {
		unit = u
	}

	iface, _ := args["interface"].(string)

	// If no interface specified, get all interfaces
	var interfaces []string
	if iface != "" {
		interfaces = []string{iface}
	} else {
		// Query for available network interfaces
		result, err := client.Call("interface.query")
		if err != nil {
			return "", fmt.Errorf("failed to query interfaces: %w", err)
		}

		var ifaceList []map[string]interface{}
		if err := json.Unmarshal(result, &ifaceList); err != nil {
			return "", fmt.Errorf("failed to parse interface list: %w", err)
		}

		// Extract interface names
		for _, iface := range ifaceList {
			if name, ok := iface["name"].(string); ok && name != "" {
				interfaces = append(interfaces, name)
			}
		}

		if len(interfaces) == 0 {
			return `{"error": "no network interfaces found"}`, nil
		}
	}

	// Get metrics for each interface
	allMetrics := make(map[string]interface{})

	for _, ifaceName := range interfaces {
		result, err := client.Call("reporting.get_data", []interface{}{
			map[string]interface{}{
				"name":       "interface",
				"identifier": ifaceName,
			},
		}, map[string]interface{}{"unit": unit})

		if err != nil {
			allMetrics[ifaceName] = map[string]string{"error": err.Error()}
			continue
		}

		var fullData []map[string]interface{}
		if err := json.Unmarshal(result, &fullData); err != nil {
			allMetrics[ifaceName] = map[string]string{"error": fmt.Sprintf("parse error: %v", err)}
			continue
		}

		// Keep aggregations and metadata, sample data points to reduce size
		summaries := make([]map[string]interface{}, 0, len(fullData))
		for _, item := range fullData {
			summary := make(map[string]interface{})
			for key, value := range item {
				if key == "data" {
					// Include sample: first 10 and last 10 data points
					if dataArray, ok := value.([]interface{}); ok {
						summary["data_points_total"] = len(dataArray)
						if len(dataArray) > 0 {
							sample := make([]interface{}, 0)

							for i := 0; i < 10 && i < len(dataArray); i++ {
								sample = append(sample, dataArray[i])
							}

							if len(dataArray) > 20 {
								for i := len(dataArray) - 10; i < len(dataArray); i++ {
									sample = append(sample, dataArray[i])
								}
							}

							summary["data_sample"] = sample
						}
					}
				} else {
					summary[key] = value
				}
			}
			summaries = append(summaries, summary)
		}

		if len(summaries) == 1 {
			allMetrics[ifaceName] = summaries[0]
		} else {
			allMetrics[ifaceName] = summaries
		}
	}

	formatted, err := json.MarshalIndent(allMetrics, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleGetDiskMetrics(client *truenas.Client, args map[string]interface{}) (string, error) {
	unit := "HOUR"
	if u, ok := args["unit"].(string); ok && u != "" {
		unit = u
	}

	requestedDisk, _ := args["disk"].(string)

	// First, get available reporting graphs
	graphsResult, err := client.Call("reporting.graphs")
	if err != nil {
		return "", fmt.Errorf("failed to query reporting graphs: %w", err)
	}

	var graphs []map[string]interface{}
	if err := json.Unmarshal(graphsResult, &graphs); err != nil {
		return "", fmt.Errorf("failed to parse reporting graphs: %w", err)
	}

	// Find the disk graph and extract identifiers
	var diskIdentifiers []string
	for _, graph := range graphs {
		graphName, nameOk := graph["name"].(string)
		if nameOk && graphName == "disk" {
			// Get the identifiers array
			if identifiersRaw, ok := graph["identifiers"]; ok && identifiersRaw != nil {
				if identifiersArray, ok := identifiersRaw.([]interface{}); ok {
					for _, idRaw := range identifiersArray {
						if idStr, ok := idRaw.(string); ok {
							// Extract disk name from identifier string (e.g., "sda | Type: SSD...")
							diskName := idStr
							if idx := strings.Index(idStr, " |"); idx != -1 {
								diskName = idStr[:idx]
							}

							// If specific disk requested, filter by name
							if requestedDisk == "" || diskName == requestedDisk {
								diskIdentifiers = append(diskIdentifiers, idStr)
							}
						}
					}
				}
			}
			break
		}
	}

	if len(diskIdentifiers) == 0 {
		return `{"error": "no disk identifiers found in reporting graphs"}`, nil
	}

	// Get metrics for each disk identifier
	allMetrics := make(map[string]interface{})

	for _, identifier := range diskIdentifiers {
		// Extract disk name for the key (e.g., "sda" from "sda | Type: SSD...")
		diskName := identifier
		if idx := strings.Index(identifier, " |"); idx != -1 {
			diskName = identifier[:idx]
		}

		result, err := client.Call("reporting.get_data", []interface{}{
			map[string]interface{}{
				"name":       "disk",
				"identifier": identifier,
			},
		}, map[string]interface{}{"unit": unit})

		if err != nil {
			allMetrics[diskName] = map[string]string{"error": err.Error()}
			continue
		}

		var fullData []map[string]interface{}
		if err := json.Unmarshal(result, &fullData); err != nil {
			allMetrics[diskName] = map[string]string{"error": fmt.Sprintf("parse error: %v", err)}
			continue
		}

		// Keep aggregations and metadata, sample data points to reduce size
		summaries := make([]map[string]interface{}, 0, len(fullData))
		for _, item := range fullData {
			summary := make(map[string]interface{})
			for key, value := range item {
				if key == "data" {
					// Include sample: first 10 and last 10 data points
					if dataArray, ok := value.([]interface{}); ok {
						summary["data_points_total"] = len(dataArray)
						if len(dataArray) > 0 {
							sample := make([]interface{}, 0)

							for i := 0; i < 10 && i < len(dataArray); i++ {
								sample = append(sample, dataArray[i])
							}

							if len(dataArray) > 20 {
								for i := len(dataArray) - 10; i < len(dataArray); i++ {
									sample = append(sample, dataArray[i])
								}
							}

							summary["data_sample"] = sample
						}
					}
				} else {
					summary[key] = value
				}
			}
			summaries = append(summaries, summary)
		}

		if len(summaries) == 1 {
			allMetrics[diskName] = summaries[0]
		} else {
			allMetrics[diskName] = summaries
		}
	}

	formatted, err := json.MarshalIndent(allMetrics, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func handleQueryApps(client *truenas.Client, args map[string]interface{}) (string, error) {
	appName, _ := args["app_name"].(string)
	includeConfig, _ := args["include_config"].(bool)

	// Build query filters and options
	// Initialize as empty array, not nil (API expects [] not null)
	filters := []interface{}{}
	if appName != "" {
		filters = []interface{}{
			[]interface{}{"name", "=", appName},
		}
	}

	options := map[string]interface{}{
		"extra": map[string]interface{}{
			"retrieve_config": includeConfig,
		},
	}

	result, err := client.Call("app.query", filters, options)
	if err != nil {
		return "", fmt.Errorf("failed to query apps: %w", err)
	}

	var apps []map[string]interface{}
	if err := json.Unmarshal(result, &apps); err != nil {
		return "", fmt.Errorf("failed to parse app list: %w", err)
	}

	// Simplify the response to show most relevant information
	simplified := make([]map[string]interface{}, 0, len(apps))
	for _, app := range apps {
		summary := map[string]interface{}{
			"name":              app["name"],
			"id":                app["id"],
			"state":             app["state"],
			"version":           app["human_version"],
			"upgrade_available": app["upgrade_available"],
		}

		// Include update info if available
		if upgradeAvail, ok := app["upgrade_available"].(bool); ok && upgradeAvail {
			summary["latest_version"] = app["latest_app_version"]
		}

		// Include portals (web URLs) if available
		if portals, ok := app["portals"].([]interface{}); ok && len(portals) > 0 {
			summary["portals"] = portals
		}

		// Include active workload summary
		if workloads, ok := app["active_workloads"].(map[string]interface{}); ok {
			if containers, ok := workloads["containers"].(float64); ok {
				summary["active_containers"] = int(containers)
			}
		}

		// Include config if requested
		if includeConfig {
			if config, ok := app["config"]; ok {
				summary["config"] = config
			}
		}

		// Include metadata
		if metadata, ok := app["metadata"].(map[string]interface{}); ok {
			summary["app_metadata"] = map[string]interface{}{
				"train":       metadata["train"],
				"description": metadata["description"],
			}
		}

		simplified = append(simplified, summary)
	}

	formatted, err := json.MarshalIndent(simplified, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func (r *Registry) handleUpgradeApp(client *truenas.Client, args map[string]interface{}) (string, error) {
	appName, ok := args["app_name"].(string)
	if !ok || appName == "" {
		return "", fmt.Errorf("app_name is required")
	}

	version := "latest"
	if v, ok := args["version"].(string); ok && v != "" {
		version = v
	}

	snapshotHostpaths := true
	if s, ok := args["snapshot_hostpaths"].(bool); ok {
		snapshotHostpaths = s
	}

	// First, get upgrade summary to show what will be upgraded
	summaryResult, err := client.Call("app.upgrade_summary", appName, map[string]interface{}{
		"app_version": version,
	})
	if err != nil {
		return "", fmt.Errorf("failed to get upgrade summary: %w", err)
	}

	// Parse summary - can be either object or array depending on TrueNAS version/app
	var summary interface{}
	if err := json.Unmarshal(summaryResult, &summary); err != nil {
		return "", fmt.Errorf("failed to parse upgrade summary: %w", err)
	}

	// Perform the upgrade - this returns a job ID since it's a long-running operation
	upgradeOptions := map[string]interface{}{
		"app_version":        version,
		"snapshot_hostpaths": snapshotHostpaths,
	}

	result, err := client.Call("app.upgrade", appName, upgradeOptions)
	if err != nil {
		return "", fmt.Errorf("failed to upgrade app: %w", err)
	}

	// Parse the job ID (app.upgrade returns an integer job ID)
	var jobID int
	if err := json.Unmarshal(result, &jobID); err != nil {
		return "", fmt.Errorf("failed to parse job ID: %w", err)
	}

	// Create task to track upgrade progress
	task, err := r.taskManager.CreateJobTask(
		"upgrade_app",
		args,
		jobID,
		1*time.Hour, // 1 hour TTL
	)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	response := map[string]interface{}{
		"app_name":         appName,
		"upgrade_summary":  summary,
		"task_id":          task.TaskID,
		"task_status":      task.Status,
		"poll_interval":    task.PollInterval,
		"job_id":           jobID,
		"snapshot_created": snapshotHostpaths,
		"message":          fmt.Sprintf("Upgrade started. Track progress with tasks_get using task_id: %s", task.TaskID),
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// handleUpgradeAppWithDryRun wraps the upgrade handler with dry-run support
func (r *Registry) handleUpgradeAppWithDryRun(client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(client, args, &upgradeAppDryRun{}, r.handleUpgradeApp)
}

// upgradeAppDryRun implements dry-run preview for app upgrades
type upgradeAppDryRun struct{}

func (u *upgradeAppDryRun) ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	appName, ok := args["app_name"].(string)
	if !ok || appName == "" {
		return nil, fmt.Errorf("app_name is required")
	}

	version := "latest"
	if v, ok := args["version"].(string); ok && v != "" {
		version = v
	}

	snapshotHostpaths := true
	if s, ok := args["snapshot_hostpaths"].(bool); ok {
		snapshotHostpaths = s
	}

	// Get current app state
	currentResult, err := client.Call("app.query", []interface{}{
		[]interface{}{"name", "=", appName},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query app: %w", err)
	}

	var apps []map[string]interface{}
	if err := json.Unmarshal(currentResult, &apps); err != nil {
		return nil, fmt.Errorf("failed to parse app query: %w", err)
	}

	if len(apps) == 0 {
		return nil, fmt.Errorf("app %s not found", appName)
	}
	currentApp := apps[0]

	// Get upgrade summary
	summaryResult, err := client.Call("app.upgrade_summary", appName, map[string]interface{}{
		"app_version": version,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get upgrade summary: %w", err)
	}

	// Parse summary - can be either object or array depending on TrueNAS version/app
	var summary interface{}
	if err := json.Unmarshal(summaryResult, &summary); err != nil {
		return nil, fmt.Errorf("failed to parse upgrade summary: %w", err)
	}

	// Build current state
	currentState := map[string]interface{}{
		"name":    currentApp["name"],
		"version": currentApp["human_version"],
		"state":   currentApp["state"],
	}

	// Build planned actions
	actions := []PlannedAction{
		{
			Step:        1,
			Description: "Stop application containers",
			Operation:   "stop",
			Target:      appName,
		},
		{
			Step:        2,
			Description: fmt.Sprintf("Upgrade from %v to %v", currentApp["human_version"], version),
			Operation:   "upgrade",
			Target:      appName,
			Details:     summary,
		},
		{
			Step:        3,
			Description: "Start application with new version",
			Operation:   "start",
			Target:      appName,
		},
	}

	result := &DryRunResult{
		Tool:           "upgrade_app",
		CurrentState:   currentState,
		PlannedActions: actions,
		EstimatedTime: &EstimatedTime{
			MinSeconds: 30,
			MaxSeconds: 300,
			Note:       "Time varies based on image size and network speed",
		},
	}

	// Add warnings if no snapshot
	if !snapshotHostpaths {
		result.Warnings = []string{
			"WARNING: snapshot_hostpaths is disabled. No backup will be created before upgrade.",
		}
	}

	return result, nil
}

func handleQueryJobs(client *truenas.Client, args map[string]interface{}) (string, error) {
	state := "RUNNING"
	if s, ok := args["state"].(string); ok && s != "" {
		state = s
	}

	limit := 50
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
	}

	// Build query filters based on state
	var filters []interface{}
	if state != "all" {
		filters = []interface{}{
			[]interface{}{"state", "=", state},
		}
	} else {
		filters = []interface{}{}
	}

	// Build options
	options := map[string]interface{}{
		"limit":    limit,
		"order_by": []string{"-time_started"}, // Most recent first
	}

	result, err := client.Call("core.get_jobs", filters, options)
	if err != nil {
		return "", fmt.Errorf("failed to query jobs: %w", err)
	}

	var jobs []map[string]interface{}
	if err := json.Unmarshal(result, &jobs); err != nil {
		return "", fmt.Errorf("failed to parse jobs: %w", err)
	}

	// Create simplified response with relevant fields
	simplified := make([]map[string]interface{}, 0, len(jobs))
	for _, job := range jobs {
		jobInfo := map[string]interface{}{
			"id":          job["id"],
			"method":      job["method"],
			"state":       job["state"],
			"description": job["description"],
		}

		// Add optional fields if present
		if progress, ok := job["progress"]; ok && progress != nil {
			jobInfo["progress"] = progress
		}
		if timeStarted, ok := job["time_started"]; ok && timeStarted != nil {
			jobInfo["time_started"] = timeStarted
		}
		if timeFinished, ok := job["time_finished"]; ok && timeFinished != nil {
			jobInfo["time_finished"] = timeFinished
		}
		if result, ok := job["result"]; ok && result != nil {
			jobInfo["result"] = result
		}
		if errorMsg, ok := job["error"]; ok && errorMsg != nil {
			jobInfo["error"] = errorMsg
		}
		if exception, ok := job["exception"]; ok && exception != nil {
			jobInfo["exception"] = exception
		}
		if abortable, ok := job["abortable"]; ok {
			jobInfo["abortable"] = abortable
		}

		simplified = append(simplified, jobInfo)
	}

	response := map[string]interface{}{
		"jobs":         simplified,
		"job_count":    len(simplified),
		"state_filter": state,
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// Capacity analysis handlers

func handleAnalyzeCapacity(client *truenas.Client, args map[string]interface{}) (string, error) {
	timeRange := "MONTH"
	if tr, ok := args["time_range"].(string); ok && tr != "" {
		timeRange = tr
	}

	// Default to all metrics
	metrics := []string{"cpu", "memory", "network", "disk"}
	if m, ok := args["metrics"].([]interface{}); ok && len(m) > 0 {
		metrics = make([]string, 0, len(m))
		for _, v := range m {
			if s, ok := v.(string); ok {
				if s == "all" {
					metrics = []string{"cpu", "memory", "network", "disk"}
					break
				}
				metrics = append(metrics, s)
			}
		}
	}

	analysis := make(map[string]interface{})

	// Analyze each metric
	for _, metric := range metrics {
		switch metric {
		case "cpu":
			cpuAnalysis, err := analyzeCPUCapacity(client, timeRange)
			if err != nil {
				analysis["cpu"] = map[string]string{"error": err.Error()}
			} else {
				analysis["cpu"] = cpuAnalysis
			}
		case "memory":
			memAnalysis, err := analyzeMemoryCapacity(client, timeRange)
			if err != nil {
				analysis["memory"] = map[string]string{"error": err.Error()}
			} else {
				analysis["memory"] = memAnalysis
			}
		case "network":
			netAnalysis, err := analyzeNetworkCapacity(client, timeRange)
			if err != nil {
				analysis["network"] = map[string]string{"error": err.Error()}
			} else {
				analysis["network"] = netAnalysis
			}
		case "disk":
			diskAnalysis, err := analyzeDiskCapacity(client, timeRange)
			if err != nil {
				analysis["disk"] = map[string]string{"error": err.Error()}
			} else {
				analysis["disk"] = diskAnalysis
			}
		}
	}

	// Add summary and recommendations
	analysis["summary"] = generateCapacityRecommendations(analysis)

	formatted, err := json.MarshalIndent(analysis, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

func analyzeCPUCapacity(client *truenas.Client, timeRange string) (map[string]interface{}, error) {
	// Get CPU metrics for time range
	result, err := client.Call("reporting.get_data", []interface{}{
		map[string]interface{}{
			"name":       "cpu",
			"identifier": nil,
		},
	}, map[string]interface{}{"unit": timeRange})

	if err != nil {
		return nil, err
	}

	var metricsData []map[string]interface{}
	if err := json.Unmarshal(result, &metricsData); err != nil {
		return nil, err
	}

	if len(metricsData) == 0 {
		return nil, fmt.Errorf("no CPU metrics data available")
	}

	// Extract data points from the first metric (CPU usage)
	dataPoints, err := extractDataPoints(metricsData[0])
	if err != nil {
		return nil, err
	}

	// Calculate statistics
	current := calculateRecentAverage(dataPoints, 5) // Last 5 points
	average := calculateAverage(dataPoints)
	peak := calculateMax(dataPoints)
	trend := calculateTrendDirection(dataPoints)
	status := determineCapacityStatus(current, 70.0, 85.0)

	analysis := map[string]interface{}{
		"metric":                  "CPU",
		"time_range":              timeRange,
		"current_utilization_pct": fmt.Sprintf("%.2f", current),
		"average_utilization_pct": fmt.Sprintf("%.2f", average),
		"peak_utilization_pct":    fmt.Sprintf("%.2f", peak),
		"trend":                   trend,
		"capacity_status":         status,
	}

	// Add projections if trending up
	if trend == "increasing" {
		projections := calculateProjections(dataPoints, current, 70.0, 85.0)
		if len(projections) > 0 {
			analysis["projections"] = projections
		}
	}

	return analysis, nil
}

func analyzeMemoryCapacity(client *truenas.Client, timeRange string) (map[string]interface{}, error) {
	// Get system info to find total memory
	sysInfoResult, err := client.Call("system.info")
	if err != nil {
		return nil, fmt.Errorf("failed to get system info: %w", err)
	}

	var sysInfo map[string]interface{}
	if err := json.Unmarshal(sysInfoResult, &sysInfo); err != nil {
		return nil, fmt.Errorf("failed to parse system info: %w", err)
	}

	// Get total physical memory in bytes
	totalMemory := 0.0
	if physMem, ok := sysInfo["physmem"].(float64); ok {
		totalMemory = physMem
	} else {
		return nil, fmt.Errorf("could not determine total system memory")
	}

	// Get memory metrics
	result, err := client.Call("reporting.get_data", []interface{}{
		map[string]interface{}{
			"name":       "memory",
			"identifier": nil,
		},
	}, map[string]interface{}{"unit": timeRange})

	if err != nil {
		return nil, err
	}

	var metricsData []map[string]interface{}
	if err := json.Unmarshal(result, &metricsData); err != nil {
		return nil, err
	}

	if len(metricsData) == 0 {
		return nil, fmt.Errorf("no memory metrics data available")
	}

	// Extract data points (in bytes)
	dataPoints, err := extractDataPoints(metricsData[0])
	if err != nil {
		return nil, err
	}

	// Convert to percentages
	dataPointsPct := make([]float64, len(dataPoints))
	for i, dp := range dataPoints {
		dataPointsPct[i] = (dp / totalMemory) * 100
	}

	// Calculate statistics
	current := calculateRecentAverage(dataPointsPct, 5)
	average := calculateAverage(dataPointsPct)
	peak := calculateMax(dataPointsPct)
	trend := calculateTrendDirection(dataPointsPct)
	status := determineCapacityStatus(current, 70.0, 85.0)

	analysis := map[string]interface{}{
		"metric":                  "Memory",
		"time_range":              timeRange,
		"current_utilization_pct": fmt.Sprintf("%.2f", current),
		"average_utilization_pct": fmt.Sprintf("%.2f", average),
		"peak_utilization_pct":    fmt.Sprintf("%.2f", peak),
		"trend":                   trend,
		"capacity_status":         status,
		"total_memory_bytes":      int64(totalMemory),
	}

	// Add projections if trending up
	if trend == "increasing" {
		projections := calculateProjections(dataPointsPct, current, 70.0, 85.0)
		if len(projections) > 0 {
			analysis["projections"] = projections
		}
	}

	return analysis, nil
}

func analyzeNetworkCapacity(client *truenas.Client, timeRange string) (map[string]interface{}, error) {
	// Get all network interfaces
	ifaceResult, err := client.Call("interface.query")
	if err != nil {
		return nil, fmt.Errorf("failed to query interfaces: %w", err)
	}

	var ifaceList []map[string]interface{}
	if err := json.Unmarshal(ifaceResult, &ifaceList); err != nil {
		return nil, fmt.Errorf("failed to parse interface list: %w", err)
	}

	interfaceAnalysis := make(map[string]interface{})

	for _, iface := range ifaceList {
		ifaceName, ok := iface["name"].(string)
		if !ok || ifaceName == "" {
			continue
		}

		// Get link speed if available
		var linkSpeed float64
		if state, ok := iface["state"].(map[string]interface{}); ok {
			if speed, ok := state["link_speed"].(float64); ok {
				linkSpeed = speed // In Mbps
			}
		}

		// Get network metrics for this interface
		result, err := client.Call("reporting.get_data", []interface{}{
			map[string]interface{}{
				"name":       "interface",
				"identifier": ifaceName,
			},
		}, map[string]interface{}{"unit": timeRange})

		if err != nil {
			interfaceAnalysis[ifaceName] = map[string]string{"error": err.Error()}
			continue
		}

		var metricsData []map[string]interface{}
		if err := json.Unmarshal(result, &metricsData); err != nil {
			interfaceAnalysis[ifaceName] = map[string]string{"error": fmt.Sprintf("parse error: %v", err)}
			continue
		}

		if len(metricsData) == 0 {
			continue
		}

		// Analyze both TX and RX
		ifaceInfo := make(map[string]interface{})
		if linkSpeed > 0 {
			ifaceInfo["link_speed_mbps"] = linkSpeed
		}

		for _, metric := range metricsData {
			legend, _ := metric["legend"].(string)
			dataPoints, err := extractDataPoints(metric)
			if err != nil {
				continue
			}

			// Convert bits/s to Mbps for comparison with link speed
			dataPointsMbps := make([]float64, len(dataPoints))
			for i, dp := range dataPoints {
				dataPointsMbps[i] = dp / 1000000.0
			}

			current := calculateRecentAverage(dataPointsMbps, 5)
			average := calculateAverage(dataPointsMbps)
			peak := calculateMax(dataPointsMbps)

			metricInfo := map[string]interface{}{
				"current_mbps": fmt.Sprintf("%.2f", current),
				"average_mbps": fmt.Sprintf("%.2f", average),
				"peak_mbps":    fmt.Sprintf("%.2f", peak),
			}

			// Calculate utilization percentage if we have link speed
			if linkSpeed > 0 {
				currentPct := (current / linkSpeed) * 100
				avgPct := (average / linkSpeed) * 100
				peakPct := (peak / linkSpeed) * 100

				metricInfo["current_utilization_pct"] = fmt.Sprintf("%.2f", currentPct)
				metricInfo["average_utilization_pct"] = fmt.Sprintf("%.2f", avgPct)
				metricInfo["peak_utilization_pct"] = fmt.Sprintf("%.2f", peakPct)
				metricInfo["capacity_status"] = determineCapacityStatus(currentPct, 70.0, 85.0)
			}

			ifaceInfo[legend] = metricInfo
		}

		interfaceAnalysis[ifaceName] = ifaceInfo
	}

	return interfaceAnalysis, nil
}

func analyzeDiskCapacity(client *truenas.Client, timeRange string) (map[string]interface{}, error) {
	// Get available disk graphs
	graphsResult, err := client.Call("reporting.graphs")
	if err != nil {
		return nil, fmt.Errorf("failed to query reporting graphs: %w", err)
	}

	var graphs []map[string]interface{}
	if err := json.Unmarshal(graphsResult, &graphs); err != nil {
		return nil, fmt.Errorf("failed to parse reporting graphs: %w", err)
	}

	// Find disk identifiers
	var diskIdentifiers []string
	for _, graph := range graphs {
		if graphName, ok := graph["name"].(string); ok && graphName == "disk" {
			if identifiersRaw, ok := graph["identifiers"]; ok && identifiersRaw != nil {
				if identifiersArray, ok := identifiersRaw.([]interface{}); ok {
					for _, idRaw := range identifiersArray {
						if idStr, ok := idRaw.(string); ok {
							diskIdentifiers = append(diskIdentifiers, idStr)
						}
					}
				}
			}
			break
		}
	}

	if len(diskIdentifiers) == 0 {
		return nil, fmt.Errorf("no disk identifiers found")
	}

	diskAnalysis := make(map[string]interface{})

	for _, identifier := range diskIdentifiers {
		diskName := identifier
		if idx := strings.Index(identifier, " |"); idx != -1 {
			diskName = identifier[:idx]
		}

		result, err := client.Call("reporting.get_data", []interface{}{
			map[string]interface{}{
				"name":       "disk",
				"identifier": identifier,
			},
		}, map[string]interface{}{"unit": timeRange})

		if err != nil {
			diskAnalysis[diskName] = map[string]string{"error": err.Error()}
			continue
		}

		var metricsData []map[string]interface{}
		if err := json.Unmarshal(result, &metricsData); err != nil {
			diskAnalysis[diskName] = map[string]string{"error": fmt.Sprintf("parse error: %v", err)}
			continue
		}

		if len(metricsData) == 0 {
			continue
		}

		// Analyze I/O metrics (read/write operations and throughput)
		diskInfo := make(map[string]interface{})
		for _, metric := range metricsData {
			legend, _ := metric["legend"].(string)
			dataPoints, err := extractDataPoints(metric)
			if err != nil {
				continue
			}

			current := calculateRecentAverage(dataPoints, 5)
			average := calculateAverage(dataPoints)
			peak := calculateMax(dataPoints)
			trend := calculateTrendDirection(dataPoints)

			metricInfo := map[string]interface{}{
				"current": fmt.Sprintf("%.2f", current),
				"average": fmt.Sprintf("%.2f", average),
				"peak":    fmt.Sprintf("%.2f", peak),
				"trend":   trend,
			}

			diskInfo[legend] = metricInfo
		}

		diskAnalysis[diskName] = diskInfo
	}

	return diskAnalysis, nil
}

func handleGetPoolCapacityDetails(client *truenas.Client, args map[string]interface{}) (string, error) {
	poolName, _ := args["pool_name"].(string)

	// Get pool information
	poolResult, err := client.Call("pool.query")
	if err != nil {
		return "", err
	}

	var pools []map[string]interface{}
	if err := json.Unmarshal(poolResult, &pools); err != nil {
		return "", err
	}

	// Filter by pool name if specified
	var targetPools []map[string]interface{}
	for _, pool := range pools {
		if poolName == "" || pool["name"] == poolName {
			targetPools = append(targetPools, pool)
		}
	}

	analysis := make([]map[string]interface{}, 0, len(targetPools))

	for _, pool := range targetPools {
		poolAnalysis := make(map[string]interface{})

		poolAnalysis["name"] = pool["name"]
		poolAnalysis["status"] = pool["status"]
		poolAnalysis["healthy"] = pool["healthy"]

		// Get datasets for this pool
		var datasets []map[string]interface{}
		datasetResult, err := client.Call("pool.dataset.query",
			[]interface{}{[]interface{}{"name", "^", pool["name"]}})
		if err == nil {
			if err := json.Unmarshal(datasetResult, &datasets); err == nil {
				poolAnalysis["datasets"] = analyzeDatasetCapacity(datasets)
			}
		}

		// Calculate capacity metrics from topology
		capacity := calculatePoolCapacity(pool)
		poolAnalysis["capacity"] = capacity

		// Determine warning level
		if utilPct, ok := capacity["utilization_pct"].(float64); ok {
			poolAnalysis["capacity_warning"] = determineCapacityStatus(utilPct, 70.0, 85.0)
		}

		analysis = append(analysis, poolAnalysis)
	}

	result := map[string]interface{}{
		"pools": analysis,
		"note":  "Historical capacity trends are not available from TrueNAS API. This shows current snapshot only. For growth trend analysis, query this tool periodically and track results externally.",
	}

	formatted, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// Helper functions for capacity analysis

func extractDataPoints(metric map[string]interface{}) ([]float64, error) {
	dataRaw, ok := metric["data"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("no data field in metric")
	}

	dataPoints := make([]float64, 0, len(dataRaw))
	for _, pointRaw := range dataRaw {
		if point, ok := pointRaw.([]interface{}); ok && len(point) >= 2 {
			if val, ok := point[1].(float64); ok {
				dataPoints = append(dataPoints, val)
			}
		}
	}

	if len(dataPoints) == 0 {
		return nil, fmt.Errorf("no valid data points")
	}

	return dataPoints, nil
}

func calculateAverage(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func calculateRecentAverage(values []float64, count int) float64 {
	if len(values) == 0 {
		return 0
	}

	start := len(values) - count
	if start < 0 {
		start = 0
	}

	return calculateAverage(values[start:])
}

func calculateMax(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	max := values[0]
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	return max
}

func calculateTrendDirection(values []float64) string {
	if len(values) < 2 {
		return "stable"
	}

	// Simple linear regression to determine trend
	n := float64(len(values))
	sumX := 0.0
	sumY := 0.0
	sumXY := 0.0
	sumX2 := 0.0

	for i, y := range values {
		x := float64(i)
		sumX += x
		sumY += y
		sumXY += x * y
		sumX2 += x * x
	}

	// Calculate slope
	slope := (n*sumXY - sumX*sumY) / (n*sumX2 - sumX*sumX)

	// Determine trend based on slope
	avgValue := sumY / n
	if avgValue == 0 {
		return "stable"
	}

	// Calculate relative slope (% change per time unit)
	relativeSlope := (slope / avgValue) * 100

	if relativeSlope > 1.0 {
		return "increasing"
	} else if relativeSlope < -1.0 {
		return "decreasing"
	}
	return "stable"
}

func determineCapacityStatus(current, warningThreshold, criticalThreshold float64) string {
	if current >= criticalThreshold {
		return "critical"
	} else if current >= warningThreshold {
		return "warning"
	}
	return "healthy"
}

func calculateProjections(values []float64, current, warningThreshold, criticalThreshold float64) []string {
	projections := make([]string, 0)

	if len(values) < 2 {
		return projections
	}

	// Calculate growth rate (% per time unit)
	n := float64(len(values))
	sumX := 0.0
	sumY := 0.0
	sumXY := 0.0
	sumX2 := 0.0

	for i, y := range values {
		x := float64(i)
		sumX += x
		sumY += y
		sumXY += x * y
		sumX2 += x * x
	}

	slope := (n*sumXY - sumX*sumY) / (n*sumX2 - sumX*sumX)

	if slope <= 0 {
		return projections
	}

	// Project when we'll hit thresholds
	if current < warningThreshold {
		timeToWarning := (warningThreshold - current) / slope
		if timeToWarning > 0 && timeToWarning < 1000 {
			projections = append(projections, fmt.Sprintf("Warning threshold (%.0f%%) projected in ~%.0f time units", warningThreshold, timeToWarning))
		}
	}

	if current < criticalThreshold {
		timeToCritical := (criticalThreshold - current) / slope
		if timeToCritical > 0 && timeToCritical < 1000 {
			projections = append(projections, fmt.Sprintf("Critical threshold (%.0f%%) projected in ~%.0f time units", criticalThreshold, timeToCritical))
		}
	}

	return projections
}

func generateCapacityRecommendations(analysis map[string]interface{}) map[string]interface{} {
	recommendations := make([]string, 0)
	overallStatuses := make([]string, 0)

	// Check CPU
	if cpuAnalysis, ok := analysis["cpu"].(map[string]interface{}); ok {
		if status, ok := cpuAnalysis["capacity_status"].(string); ok {
			overallStatuses = append(overallStatuses, status)
			if status == "warning" {
				recommendations = append(recommendations,
					"CPU utilization is elevated (>70%). Consider reviewing workloads or planning CPU upgrade.")
			} else if status == "critical" {
				recommendations = append(recommendations,
					"CPU utilization is critical (>85%). Immediate action recommended: optimize workloads or upgrade hardware.")
			}
		}
	}

	// Check memory
	if memAnalysis, ok := analysis["memory"].(map[string]interface{}); ok {
		if status, ok := memAnalysis["capacity_status"].(string); ok {
			overallStatuses = append(overallStatuses, status)
			if status == "warning" {
				recommendations = append(recommendations,
					"Memory utilization is elevated (>70%). Consider adding more RAM or optimizing memory usage.")
			} else if status == "critical" {
				recommendations = append(recommendations,
					"Memory utilization is critical (>85%). Immediate action recommended: add more RAM or reduce workload.")
			}
		}
	}

	// Check network interfaces
	if netAnalysis, ok := analysis["network"].(map[string]interface{}); ok {
		for ifaceName, ifaceData := range netAnalysis {
			if ifaceName == "error" {
				continue
			}
			if ifaceInfo, ok := ifaceData.(map[string]interface{}); ok {
				for metric, metricData := range ifaceInfo {
					if metric == "link_speed_mbps" {
						continue
					}
					if metricInfo, ok := metricData.(map[string]interface{}); ok {
						if status, ok := metricInfo["capacity_status"].(string); ok {
							overallStatuses = append(overallStatuses, status)
							if status == "warning" || status == "critical" {
								recommendations = append(recommendations,
									fmt.Sprintf("Network interface %s (%s) is nearing capacity. Consider upgrading link speed or load balancing.", ifaceName, metric))
							}
						}
					}
				}
			}
		}
	}

	// Determine overall status
	overallStatus := "healthy"
	for _, status := range overallStatuses {
		if status == "critical" {
			overallStatus = "critical"
			break
		} else if status == "warning" {
			overallStatus = "warning"
		}
	}

	if len(recommendations) == 0 {
		recommendations = append(recommendations, "All monitored capacity metrics are within healthy ranges.")
	}

	return map[string]interface{}{
		"recommendations": recommendations,
		"overall_status":  overallStatus,
	}
}

func calculatePoolCapacity(pool map[string]interface{}) map[string]interface{} {
	capacity := make(map[string]interface{})

	// Try to get capacity from topology
	if topology, ok := pool["topology"].(map[string]interface{}); ok {
		// Look for data vdevs
		if data, ok := topology["data"].([]interface{}); ok && len(data) > 0 {
			totalBytes := int64(0)
			for _, vdevRaw := range data {
				if vdev, ok := vdevRaw.(map[string]interface{}); ok {
					if stats, ok := vdev["stats"].(map[string]interface{}); ok {
						if size, ok := stats["size"].(float64); ok {
							totalBytes += int64(size)
						}
					}
				}
			}
			if totalBytes > 0 {
				capacity["total_bytes"] = totalBytes
			}
		}
	}

	// Get used/available from root dataset if available
	if name, ok := pool["name"].(string); ok {
		capacity["pool_name"] = name
	}

	// Try to get usage from pool-level stats
	if usedBytes, ok := pool["allocated"].(float64); ok {
		capacity["used_bytes"] = int64(usedBytes)
	}

	if freeBytes, ok := pool["free"].(float64); ok {
		capacity["available_bytes"] = int64(freeBytes)
	}

	// Calculate utilization percentage
	if used, ok := capacity["used_bytes"].(int64); ok {
		if available, ok := capacity["available_bytes"].(int64); ok {
			total := used + available
			if total > 0 {
				utilPct := (float64(used) / float64(total)) * 100
				capacity["utilization_pct"] = utilPct
				capacity["total_bytes"] = total
			}
		}
	}

	return capacity
}

func analyzeDatasetCapacity(datasets []map[string]interface{}) []map[string]interface{} {
	analysis := make([]map[string]interface{}, 0, len(datasets))

	for _, ds := range datasets {
		dsAnalysis := map[string]interface{}{
			"name": ds["name"],
			"type": ds["type"],
		}

		// Get properties
		if props, ok := ds["properties"].(map[string]interface{}); ok {
			// Extract used space
			if used, ok := props["used"].(map[string]interface{}); ok {
				if usedVal, ok := used["rawvalue"].(string); ok {
					dsAnalysis["used_bytes"] = usedVal
				}
				if usedParsed, ok := used["parsed"].(float64); ok {
					dsAnalysis["used_bytes_numeric"] = int64(usedParsed)
				}
			}

			// Extract available space
			if available, ok := props["available"].(map[string]interface{}); ok {
				if availVal, ok := available["rawvalue"].(string); ok {
					dsAnalysis["available_bytes"] = availVal
				}
				if availParsed, ok := available["parsed"].(float64); ok {
					dsAnalysis["available_bytes_numeric"] = int64(availParsed)
				}
			}

			// Extract referenced space
			if referenced, ok := props["referenced"].(map[string]interface{}); ok {
				if refVal, ok := referenced["rawvalue"].(string); ok {
					dsAnalysis["referenced_bytes"] = refVal
				}
			}

			// Calculate utilization if we have both used and available
			if usedNum, usedOk := dsAnalysis["used_bytes_numeric"].(int64); usedOk {
				if availNum, availOk := dsAnalysis["available_bytes_numeric"].(int64); availOk {
					total := usedNum + availNum
					if total > 0 {
						utilPct := (float64(usedNum) / float64(total)) * 100
						dsAnalysis["utilization_pct"] = fmt.Sprintf("%.2f", utilPct)
					}
				}
			}
		}

		analysis = append(analysis, dsAnalysis)
	}

	return analysis
}

// handleTasksList lists all active and recent tasks
func (r *Registry) handleTasksList(client *truenas.Client, args map[string]interface{}) (string, error) {
	cursor := ""
	if c, ok := args["cursor"].(string); ok {
		cursor = c
	}

	limit := 50
	if l, ok := args["limit"].(float64); ok {
		limit = int(l)
	}

	taskList, nextCursor, err := r.taskManager.List(cursor, limit)
	if err != nil {
		return "", fmt.Errorf("failed to list tasks: %w", err)
	}

	response := map[string]interface{}{
		"tasks": taskList,
	}
	if nextCursor != "" {
		response["next_cursor"] = nextCursor
	}

	formatted, _ := json.MarshalIndent(response, "", "  ")
	return string(formatted), nil
}

// handleTasksGet retrieves a specific task by ID
func (r *Registry) handleTasksGet(client *truenas.Client, args map[string]interface{}) (string, error) {
	taskID, ok := args["task_id"].(string)
	if !ok || taskID == "" {
		return "", fmt.Errorf("task_id is required")
	}

	task, err := r.taskManager.Get(taskID)
	if err != nil {
		return "", fmt.Errorf("failed to get task: %w", err)
	}

	formatted, _ := json.MarshalIndent(task, "", "  ")
	return string(formatted), nil
}

// System Update Handlers

// handleCheckUpdates checks for available TrueNAS system updates
func handleCheckUpdates(client *truenas.Client, args map[string]interface{}) (string, error) {
	result, err := client.Call("update.available_versions")
	if err != nil {
		return "", fmt.Errorf("failed to check for updates: %w", err)
	}

	var updates interface{}
	if err := json.Unmarshal(result, &updates); err != nil {
		return "", fmt.Errorf("failed to parse update information: %w", err)
	}

	formatted, err := json.MarshalIndent(updates, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// handleUpdateStatus gets current system update status
func handleUpdateStatus(client *truenas.Client, args map[string]interface{}) (string, error) {
	result, err := client.Call("update.status")
	if err != nil {
		return "", fmt.Errorf("failed to get update status: %w", err)
	}

	var status interface{}
	if err := json.Unmarshal(result, &status); err != nil {
		return "", fmt.Errorf("failed to parse update status: %w", err)
	}

	formatted, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// handleDownloadUpdate downloads a TrueNAS system update
func (r *Registry) handleDownloadUpdate(client *truenas.Client, args map[string]interface{}) (string, error) {
	train, _ := args["train"].(string)
	version, _ := args["version"].(string)

	// Check if update is already downloaded
	statusResult, err := client.Call("update.status")
	if err == nil {
		var status map[string]interface{}
		if err := json.Unmarshal(statusResult, &status); err == nil {
			// Check if download is complete
			if progress, ok := status["update_download_progress"].(map[string]interface{}); ok {
				if percent, ok := progress["percent"].(float64); ok && percent == 100 {
					if dlVersion, ok := progress["version"].(string); ok {
						// If no specific version requested, or versions match
						if version == "" || dlVersion == version {
							response := map[string]interface{}{
								"train":              train,
								"version":            dlVersion,
								"already_downloaded": true,
								"download_percent":   100,
								"message":            fmt.Sprintf("Update %s is already downloaded (100%%). Ready to apply.", dlVersion),
							}
							formatted, _ := json.MarshalIndent(response, "", "  ")
							return string(formatted), nil
						}
					}
				}
			}
		}
	}

	// Start the download (update.download typically takes no parameters)
	// TrueNAS downloads based on the configured train automatically
	result, err := client.Call("update.download")
	if err != nil {
		return "", fmt.Errorf("failed to start update download: %w", err)
	}

	// Parse job ID
	var jobID int
	if err := json.Unmarshal(result, &jobID); err != nil {
		return "", fmt.Errorf("failed to parse job ID: %w", err)
	}

	// Create task to track download progress
	task, err := r.taskManager.CreateJobTask(
		"download_update",
		args,
		jobID,
		2*time.Hour, // 2 hour TTL
	)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	response := map[string]interface{}{
		"train":         train,
		"version":       version,
		"task_id":       task.TaskID,
		"task_status":   task.Status,
		"poll_interval": task.PollInterval,
		"job_id":        jobID,
		"message":       fmt.Sprintf("Update download started. Track progress with tasks_get using task_id: %s", task.TaskID),
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// handleDownloadUpdateWithDryRun wraps the download handler with dry-run support
func (r *Registry) handleDownloadUpdateWithDryRun(client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(client, args, &downloadUpdateDryRun{}, r.handleDownloadUpdate)
}

// downloadUpdateDryRun implements dry-run preview for update downloads
type downloadUpdateDryRun struct{}

func (d *downloadUpdateDryRun) ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	train, _ := args["train"].(string)
	version, _ := args["version"].(string)

	// Get current system info
	sysInfoResult, err := client.Call("system.info")
	if err != nil {
		return nil, fmt.Errorf("failed to get system info: %w", err)
	}

	var sysInfo map[string]interface{}
	if err := json.Unmarshal(sysInfoResult, &sysInfo); err != nil {
		return nil, fmt.Errorf("failed to parse system info: %w", err)
	}

	currentVersion := sysInfo["version"].(string)

	actions := []PlannedAction{
		{
			Step:        1,
			Description: "Connect to TrueNAS update server",
			Operation:   "connect",
			Target:      "update.truenas.com",
		},
		{
			Step:        2,
			Description: fmt.Sprintf("Download update files for version %s", version),
			Operation:   "download",
			Target:      version,
			Details: map[string]interface{}{
				"train":   train,
				"version": version,
			},
		},
		{
			Step:        3,
			Description: "Verify update package integrity",
			Operation:   "verify",
			Target:      version,
		},
	}

	result := &DryRunResult{
		Tool: "download_update",
		CurrentState: map[string]interface{}{
			"current_version": currentVersion,
		},
		PlannedActions: actions,
		EstimatedTime: &EstimatedTime{
			MinSeconds: 120,
			MaxSeconds: 1800,
			Note:       "Time varies based on update size and network speed",
		},
	}

	return result, nil
}

// handleApplyUpdate applies a downloaded TrueNAS system update
func (r *Registry) handleApplyUpdate(client *truenas.Client, args map[string]interface{}) (string, error) {
	reboot := false
	if r, ok := args["reboot"].(bool); ok {
		reboot = r
	}

	// Build update options
	updateOptions := map[string]interface{}{
		"reboot": reboot,
	}

	// Start the update
	result, err := client.Call("update.run", updateOptions)
	if err != nil {
		return "", fmt.Errorf("failed to start update: %w", err)
	}

	// update.run returns a job ID
	var jobID int
	if err := json.Unmarshal(result, &jobID); err != nil {
		return "", fmt.Errorf("failed to parse job ID: %w", err)
	}

	// Create job-based task to track update progress
	task, err := r.taskManager.CreateJobTask(
		"apply_update",
		args,
		jobID,
		2*time.Hour, // 2 hour TTL
	)
	if err != nil {
		return "", fmt.Errorf("failed to create task: %w", err)
	}

	response := map[string]interface{}{
		"reboot":        reboot,
		"task_id":       task.TaskID,
		"task_status":   task.Status,
		"poll_interval": task.PollInterval,
		"job_id":        jobID,
		"message":       fmt.Sprintf("Update started. Track progress with tasks_get using task_id: %s", task.TaskID),
	}

	if reboot {
		response["warning"] = "System will reboot after update completes. Connection will be lost."
	}

	formatted, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}

// handleApplyUpdateWithDryRun wraps the apply handler with dry-run support
func (r *Registry) handleApplyUpdateWithDryRun(client *truenas.Client, args map[string]interface{}) (string, error) {
	return ExecuteWithDryRun(client, args, &applyUpdateDryRun{}, r.handleApplyUpdate)
}

// applyUpdateDryRun implements dry-run preview for update application
type applyUpdateDryRun struct{}

func (a *applyUpdateDryRun) ExecuteDryRun(client *truenas.Client, args map[string]interface{}) (*DryRunResult, error) {
	reboot := false
	if r, ok := args["reboot"].(bool); ok {
		reboot = r
	}

	// Get current system info
	sysInfoResult, err := client.Call("system.info")
	if err != nil {
		return nil, fmt.Errorf("failed to get system info: %w", err)
	}

	var sysInfo map[string]interface{}
	if err := json.Unmarshal(sysInfoResult, &sysInfo); err != nil {
		return nil, fmt.Errorf("failed to parse system info: %w", err)
	}

	currentVersion := sysInfo["version"].(string)

	// Check update status to get target version
	statusResult, err := client.Call("update.status")
	if err != nil {
		return nil, fmt.Errorf("failed to get update status: %w", err)
	}

	var status map[string]interface{}
	if err := json.Unmarshal(statusResult, &status); err != nil {
		return nil, fmt.Errorf("failed to parse update status: %w", err)
	}

	actions := []PlannedAction{
		{
			Step:        1,
			Description: "Stop critical system services",
			Operation:   "stop",
			Target:      "system services",
		},
		{
			Step:        2,
			Description: "Apply system update",
			Operation:   "update",
			Target:      "system",
			Details:     status,
		},
		{
			Step:        3,
			Description: "Verify update installation",
			Operation:   "verify",
			Target:      "system",
		},
	}

	if reboot {
		actions = append(actions, PlannedAction{
			Step:        4,
			Description: "Reboot system to complete update",
			Operation:   "reboot",
			Target:      "system",
		})
	}

	result := &DryRunResult{
		Tool: "apply_update",
		CurrentState: map[string]interface{}{
			"current_version": currentVersion,
			"update_status":   status,
		},
		PlannedActions: actions,
		EstimatedTime: &EstimatedTime{
			MinSeconds: 180,
			MaxSeconds: 900,
			Note:       "Time varies based on system configuration. Add 60-120s for reboot if enabled.",
		},
		Warnings: []string{
			"CRITICAL: This operation will update the TrueNAS system software.",
			"Services may be interrupted during the update process.",
		},
	}

	if reboot {
		result.Warnings = append(result.Warnings,
			"REBOOT ENABLED: System will automatically reboot after update completes.",
			"All connections will be lost during reboot.",
		)
	} else {
		result.Warnings = append(result.Warnings,
			"Manual reboot required after update to complete the process.",
		)
	}

	return result, nil
}

// handleSystemReboot reboots the TrueNAS system
func handleSystemReboot(client *truenas.Client, args map[string]interface{}) (string, error) {
	// Call system.reboot with reason parameter
	reason := "System reboot requested via MCP"
	result, err := client.Call("system.reboot", reason)
	if err != nil {
		return "", fmt.Errorf("failed to initiate system reboot: %w", err)
	}

	// system.reboot typically returns nothing or a simple acknowledgment
	var response map[string]interface{}
	if len(result) > 0 {
		_ = json.Unmarshal(result, &response)
	}

	returnMsg := map[string]interface{}{
		"status":  "reboot_initiated",
		"message": "System reboot initiated. All connections will be lost.",
		"warning": "TrueNAS system is rebooting. Wait approximately 2-3 minutes before reconnecting.",
	}

	formatted, err := json.MarshalIndent(returnMsg, "", "  ")
	if err != nil {
		return "", err
	}

	return string(formatted), nil
}
