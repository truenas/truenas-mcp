# Complete TrueNAS MCP Feature List

This document provides a comprehensive list of all available MCP tools for TrueNAS management.

Tools are classified as read, write or destructive (`tools/safety.go`). Write and destructive tools are hidden and refused in `--read-only` mode, and `--allow-tools` / `--deny-tools` restrict the exposed set. Destructive tools (marked below) require `confirm: true` to execute; without it they return a `confirmation_required` result with their dry-run preview. **Breaking change:** callers that used these nine tools without `confirm` now get the preview instead. See [Safety Controls](../README.md#safety-controls).

## Read-Only Monitoring Tools

### System Information
- **system_info** - Get system information (version, hostname, platform)
  - Datetimes (`buildtime`, `boottime`, `datetime`) are ISO-8601 UTC strings (e.g. `"2026-01-01T00:00:00Z"`) instead of the middleware's `{"$date": <ms>}` objects. Values outside 2000-01-01..now+10 years are passed through raw
- **system_health** - Check system health including alerts, active jobs, and capacity warnings
  - `alert_count` counts active (not dismissed) alerts; `critical_alert_count` counts active CRITICAL/ALERT/EMERGENCY alerts and `dismissed_alert_count` dismissed ones
  - The memory warning is based on memory used excluding the ZFS ARC (latest available sample minus the latest ARC size) when the ARC size is known; otherwise it says the figure may include the ARC. If the ARC size was used but its age could not be checked (no timestamp in the reporting data), `arc_freshness_note` says so, as in `analyze_capacity`
  - Alert datetimes are ISO-8601 UTC strings, as in `list_alerts`
  - The CPU check skips samples without data (all-zero rows, see `get_system_metrics`)
- **query_jobs** - Query system jobs (running, pending, or completed tasks like replication, snapshots, scrubs)
  - `time_started` and `time_finished` are ISO-8601 UTC strings (same rules as `system_info`)

### Storage Management
- **query_pools** - Query storage pools with status and capacity
- **query_datasets** - Query datasets with intelligent filtering and sorting
  - Returns simplified, human-readable dataset information (~15 fields instead of 40+)
  - Filter by pool name, encryption status
  - Sort by space usage (default), available space, or name
  - Limit results for manageable responses (default: 50, configurable)
  - Shows capacity (used/available), compression ratios, encryption status, usage breakdown
  - Perfect for questions like "what datasets use the most space?" or "show me encrypted datasets"

- **query_snapshots** - Query ZFS snapshots with intelligent filtering and sorting
  - Returns simplified snapshot information with creation time, sizes, dataset, and holds status
  - Requests the ZFS `creation`, `used` and `referenced` properties and holds explicitly (`extra.properties`, `extra.holds`), since TrueNAS 27.0 returns neither by default
  - `created` (ISO-8601 UTC) and `created_timestamp` (Unix seconds) come from the `creation` property; if it is missing, `created_date` is parsed from the snapshot name. `created_source` is `zfs_property` or `snapshot_name`
  - `used_bytes`/`used` (space unique to the snapshot) and `referenced_bytes`/`referenced` (data it refers to), raw and human-readable
  - Filter by dataset name, pool name, or holds presence (on 27.0 the middleware reports only its own `truenas` hold)
  - Sort by snapshot name (default, newest first), dataset, or creation time (`created`: newest first by `created_timestamp`, then `created_date`, then name)
  - Limit results for manageable responses (default: 50, configurable)
  - Perfect for questions like "what recent snapshots exist?" or "show snapshots with holds"

- **query_shares** - Query SMB and NFS share configurations

### Virtualization
- **query_vms** - Query virtual machines with intelligent filtering and sorting
  - Returns simplified VM information with resource allocation, status, and device summary
  - Filter by VM name (partial match), state (RUNNING/STOPPED), or autostart setting
  - Sort by name (default, alphabetical), memory usage, or status (running first)
  - Shows CPU/memory config, bootloader, devices (disks, NICs, displays), and current state
  - CPU: `sockets` (the API's `vcpus`), `cores_per_socket` (`cores`), `threads_per_core` (`threads`) and `total_vcpus` (their product, what the guest sees)
  - Memory: `memory_mib` (the API value, in MiB), `memory_gib` (e.g. `"4.0 GiB"`) and `min_memory_mib` when set. These replace `vcpus`/`cores`/`threads` and `memory_mb`/`memory_gb`
  - Automatically excludes sensitive data like display passwords for security
  - Perfect for questions like "what VMs are running?" or "show VMs with autostart enabled"

### Alerts
- **list_alerts** - List system alerts with filtering
  - `datetime`, `last_occurrence` and other datetimes are ISO-8601 UTC strings
- **dismiss_alert** / **restore_alert** - Manage system alerts (write operations; support dry-run mode)

### Performance Metrics
- **get_system_metrics** - Get CPU, memory, and load performance metrics
  - Each graph has a `unit` (`%CPU`, `Bytes` of available memory, `Load`, `Celsius`, `Seconds`)
  - TrueNAS fills periods without data with zeros (common for MONTH/YEAR). Rows whose values are all 0 or null are treated as gaps: they are left out of `data_sample`, and when there are any, `aggregations` is recomputed without them (the middleware's values are kept in `aggregations_reported`) and `gap_samples_excluded`, `data_points_valid` and `gap_note` are added. `data_points_total` counts all rows. A genuine all-zero sample (e.g. zero load) is also treated as a gap
  - `last_sample_after_end_seconds` appears when the newest sample is stamped after the graph's `end`. This is expected for long ranges: samples are averaged into aligned buckets (about 2 minutes past `end` has been seen for MONTH) and is harmless
- **get_network_metrics** - Get network interface traffic metrics
  - `unit` is `Kilobits/s` (series `received` and `sent`)
- **get_disk_metrics** - Get disk I/O performance metrics
  - `unit` is the graph's `vertical_label` from `reporting.graphs` (`Kibibytes/s` for `disk`, series `reads` and `writes`)
- **get_arc_metrics** - Get ZFS ARC reporting metrics (e.g. `arcsize`)
  - The ARC graphs available depend on the TrueNAS version; the default is every ARC graph the server supports, discovered from `core.get_methods` and `reporting.graphs` (falls back to `arcsize`)
  - A graph that fails is reported under `errors.<graph>` while the others are returned; if every graph fails the call is an error

### Applications
- **query_apps** - List installed applications with status and available updates
- **search_app_catalog** - Search TrueNAS app catalog by name, category, or keyword
  - Search across all catalog trains (stable, enterprise, community)
  - Filter by category (media, productivity, database, etc.)
  - Returns app information with versions and installation status
- **get_app_catalog_details** - Get detailed information about a specific app
  - Returns README documentation and setup instructions
  - Shows version info, categories, and maintainers
  - Provides storage volume hints (detected from README)
  - Recommends dataset layout: /mnt/<pool>/apps/<appname>/<volume>

## Capacity Planning and Analysis

### System-Wide Analysis
- **analyze_capacity** - Comprehensive capacity analysis with historical trends and projections
  - CPU, memory, network, and disk I/O utilization analysis
  - Current, average, and peak utilization percentages
  - Trend detection (increasing/stable/decreasing)
  - Capacity status warnings (healthy/warning/critical at 70%/85% thresholds)
  - Growth projections when metrics are trending upward
  - CPU: samples without data (all-zero rows) are skipped
  - Network: per interface, `received` and `sent` are analysed separately (`current_mbps`, `average_mbps`, `peak_mbps`, utilization against `link_speed_mbps`), converted from the graph's Kilobits/s
  - Disk: per disk, `reads` and `writes` in KiB/s (`unit`)
  - Memory: used = total - available; when the latest ARC size is known and recent, `current_utilization_pct` and `capacity_status` use memory used excluding the ZFS ARC (`current_utilization_basis: "excluding_arc"`, also `arc_bytes`, `used_excluding_arc_bytes`, `utilization_excluding_arc_pct`) and `current_utilization_including_arc_pct` gives the ARC-inclusive figure. Otherwise `current_utilization_basis` is `"including_arc"`. average/peak/trend still include the ARC, and no memory projection is given in that case (`projection_note`), since only the latest ARC size is known. "Recent" means at most 15 minutes older than the newest timestamp in the NAS's own reporting data (the memory graph's latest sample or the graph `end`), so clock skew between the NAS and the MCP host does not matter; if no such timestamp is available the ARC size is used and `arc_freshness_note` says its age could not be checked
  - Time ranges: HOUR, DAY, WEEK, MONTH, YEAR
  - Overall recommendations based on all metrics

### Storage Capacity
- **get_pool_capacity_details** - Detailed pool and dataset capacity information
  - Current pool capacity (total, used, available bytes)
  - Utilization percentages for each pool
  - Per-dataset breakdown with capacity metrics
  - Capacity status warnings (healthy/warning/critical)
  - Note: Historical pool capacity trends not available in TrueNAS API (limitation documented)

## Write Operations

### Dataset Management
- **create_dataset** - Create ZFS datasets for storage (reusable for all protocols)
  - Create filesystems or volumes (for iSCSI/VMs)
  - Share type optimization (SMB, NFS, MULTIPROTOCOL, APPS)
  - Encryption with auto-generated keys or passphrases
  - Compression (LZ4, ZSTD, GZIP), quotas, and ACL configuration
  - Dry-run mode to preview before creating
  - Wizard-style guidance for SMB/NFS/iSCSI setup

### Share Management
- **create_smb_share** - Create SMB shares for Windows/macOS file sharing
  - Interactive wizard walks through share configuration
  - Purpose-based setup (standard, Time Machine, multi-protocol, home dirs)
  - Access control (IP restrictions, read-only, browsability)
  - Audit logging for compliance
  - Security warnings for public shares
  - Dry-run mode to preview with security analysis

- **create_nfs_share** - Create NFS shares for Unix/Linux file sharing
  - Interactive wizard walks through NFS configuration
  - Network/host access restrictions (CIDR notation, IP/hostname lists)
  - User mapping for security (maproot, mapall)
  - Read-only or read-write access
  - Security level selection (SYS, Kerberos)
  - Security warnings for unrestricted access
  - Dry-run mode to preview with mount examples

### Application Management
- **install_app** - Install applications from the catalog with guided storage setup
  - Multi-step wizard guides through app installation process
  - ALWAYS uses host-path volumes (NEVER ix-volumes)
  - Enforces structured dataset layout: /mnt/<pool>/apps/<appname>/<volume>
  - Validates app instance names (lowercase, alphanumeric, hyphens)
  - Verifies datasets exist before installation
  - Supports dry-run mode to preview installation
  - Returns task ID for tracking installation progress
  - Includes comprehensive wizard guidance:
    - Step 1: Search and select app from catalog
    - Step 2: Understand app storage requirements
    - Step 3: Plan storage layout with pool selection
    - Step 4: Create missing datasets
    - Step 5: Validate app instance name
    - Step 6: Build storage configuration
    - Step 7: Preview with dry-run
    - Step 8: Execute installation

- **delete_app** - Remove installed applications (**destructive**)
  - **Requires `confirm: true`** to execute; without it returns a preview and changes nothing
  - IMPORTANT: Host-path datasets are NOT deleted (preserved for data safety)
  - Data remains in original locations for manual cleanup
  - Optional removal of container images
  - Supports dry-run mode to preview deletion
  - Returns task ID for tracking deletion progress

- **upgrade_app** - Upgrade an application to a newer version with optional snapshot backup (**destructive**: the app goes offline and migrations may be one-way)
  - **Requires `confirm: true`** to execute; without it returns a preview and changes nothing
  - Supports dry-run mode to preview changes before execution
  - Returns a task ID for tracking long-running operations

- **update_app** - Change an installed application's configuration
  - Write operation (no confirm needed), but advertises `destructiveHint: true` because it replaces the configuration in place
  - Supports dry-run mode

- **start_app** - Start a stopped application
  - Supports dry-run mode; returns a task ID for tracking

- **stop_app** - Stop a running application (**destructive**: the service goes offline)
  - **Requires `confirm: true`** to execute; without it returns a preview and changes nothing
  - Supports dry-run mode; returns a task ID for tracking

## Directory Services

### Read-Only Tools
- **get_directory_service_status** - Quick health check for directory service
  - Returns service type (ACTIVEDIRECTORY, IPA, LDAP)
  - Returns status (DISABLED, HEALTHY, FAULTED, JOINING, LEAVING)
  - Shows error messages if service is faulted
  - Use for fast health verification

- **query_directory_services** - Get full directory service configuration
  - Returns service type and enabled status
  - Shows credential types (all passwords/keytabs masked for security)
  - Displays service-specific configuration (domain, hostname, etc.)
  - Shows account caching, DNS updates, timeout settings
  - Perfect for understanding current directory service setup

- **list_directory_certificates** - List certificates for LDAP MTLS authentication
  - Returns available certificate IDs and names
  - Use when configuring LDAP with mutual TLS authentication

- **refresh_directory_cache** - Refresh cached user and group data
  - Write operation: hidden and refused in read-only mode
  - Refreshes cached information from directory service
  - Use after making changes in Active Directory, LDAP, or IPA
  - Supports dry-run mode to show the current directory service status first
  - Quick operation, no task tracking needed

### Write Operations
- **configure_directory_service** - Configure and join directory service (**destructive**: disabling or re-pointing a joined service cuts off domain users)
  - **Requires `confirm: true`** to execute; without it returns a preview and changes nothing
  - Supports Active Directory, LDAP, and FreeIPA/IPA
  - Multiple credential types:
    - **Active Directory**: KERBEROS_USER (username/password) or KERBEROS_PRINCIPAL (principal/keytab)
    - **LDAP**: LDAP_PLAIN (binddn/password), LDAP_ANONYMOUS, LDAP_MTLS (certificate), KERBEROS_USER, KERBEROS_PRINCIPAL
    - **IPA**: KERBEROS_USER or KERBEROS_PRINCIPAL
  - Configurable options:
    - Account caching (default: enabled)
    - DNS updates (default: enabled)
    - Query timeout (5-60 seconds, default: 10)
    - Kerberos realm (optional)
    - Service-specific configuration (domain, hostname, etc.)
  - Security features:
    - All passwords/keytabs masked in output
    - Validation of credential types and required fields
    - Warnings about credential storage
    - Recommendations to use keytabs instead of passwords
  - Dry-run mode shows:
    - Planned actions (5 steps: validate, create account, register DNS, join domain, cache data)
    - Network and DNS requirements
    - Security warnings and recommendations
    - Estimated time (1-10 minutes typical)
  - Returns task_id for tracking long-running domain join operation
  - Automatic domain join when enable=true
  - Use enable=false to disable without leaving domain

- **leave_directory_service** - Disconnect from directory service (**destructive**)
  - **Requires `confirm: true`** to execute; without it returns a preview and changes nothing
  - Removes TrueNAS from the domain
  - Deletes computer account from directory (if possible)
  - Removes DNS records
  - Clears all cached user/group data
  - **WARNING**: All domain authentication will stop working
  - **WARNING**: SMB/NFS shares using domain accounts become inaccessible
  - Dry-run mode (STRONGLY RECOMMENDED first) shows:
    - Current service type and status
    - Planned disconnection steps
    - Critical impact warnings
    - Alternative suggestion (use enable=false for temporary disable)
    - Estimated time (30 seconds to 5 minutes typical)
  - Returns task_id for tracking leave operation
  - Consider using configure_directory_service with enable=false instead for temporary disable

### Integration with Other Tools
- **system_health** - Now includes directory service status
  - Automatically checks directory service health
  - Shows service type and status in response
  - Generates critical warnings if service is FAULTED
  - Reports ongoing operations (JOINING/LEAVING)

- **create_smb_share** and **create_nfs_share** - Directory service awareness
  - Dry-run mode shows warnings when directory service is enabled
  - Reminds users that domain accounts will be used for permissions
  - Warns if directory service is FAULTED (authentication may not work)
  - Helps prevent configuration mistakes

### Security Notes
- **Credential Storage**: Credentials are stored in TrueNAS configuration
- **Masking**: All passwords and keytabs are masked in tool outputs
- **Recommendations**:
  - Use Kerberos principals with keytabs instead of passwords for production
  - Ensure DNS is properly configured before joining
  - Verify network connectivity to directory service
  - Test with dry-run mode before executing operations
- **Requirements**:
  - DNS resolution must work for the domain
  - Network access to directory service (ports 389/636 for LDAP, 88/464 for Kerberos)
  - Time synchronization between TrueNAS and directory service

### Credential Type Reference

**Active Directory Credentials:**
```json
// Username and password (simpler but less secure)
{"type": "KERBEROS_USER", "username": "admin@CORP.EXAMPLE.COM", "password": "********"}

// Principal with keytab (more secure, recommended for production)
{"type": "KERBEROS_PRINCIPAL", "principal": "host/truenas.corp.example.com", "keytab": "[masked]"}
```

**LDAP Credentials:**
```json
// Plain authentication with bind DN
{"type": "LDAP_PLAIN", "binddn": "cn=admin,dc=example,dc=com", "bindpw": "********"}

// Anonymous (no credentials)
{"type": "LDAP_ANONYMOUS"}

// Mutual TLS with client certificate
{"type": "LDAP_MTLS", "certificate_id": 123}

// Kerberos authentication (same as Active Directory)
{"type": "KERBEROS_USER", "username": "admin", "password": "********"}
{"type": "KERBEROS_PRINCIPAL", "principal": "host/truenas", "keytab": "[masked]"}
```

**IPA/FreeIPA Credentials:**
```json
// Same as Active Directory Kerberos options
{"type": "KERBEROS_USER", "username": "admin", "password": "********"}
{"type": "KERBEROS_PRINCIPAL", "principal": "host/truenas", "keytab": "[masked]"}
```

## System Update and Maintenance

### Recommended System Update Workflow

1. **Before Update**: Check current state
   - Check for TrueNAS system updates (`check_updates`)
   - Check current boot environment (`get_current_boot_environment`)
   - List boot environments to know baseline (`query_boot_environments`)

2. **Download Update**: Get update files
   - Download update (dry run first for preview: `download_update` with `dry_run: true`)
   - Monitor progress with `update_status`

3. **Apply Update**: Install and reboot
   - Apply update (dry run first: `apply_update` with `dry_run: true`)
   - Apply update with reboot (`apply_update` with `reboot: true`)

4. **After Update**: Verify new system
   - Check system health (`system_health`)
   - List boot environments (verify new one exists: `query_boot_environments`)
   - Test system functionality

5. **Cleanup** (optional, after verifying system works):
   - List deletable boot environments (`query_boot_environments` with `show_deletable_only: true`)
   - Delete old boot environments (dry run first: `delete_boot_environment` with `dry_run: true`)
   - Keep at least 2-3 boot environments for recovery

### Available Tools

- **check_updates** - Check for available TrueNAS system updates
  - Queries TrueNAS update servers for new versions
  - Shows available update details and release notes
  - No system changes, safe to run anytime

- **download_update** - Download TrueNAS system update files
  - Downloads update files to the system
  - Supports dry-run mode to preview what will be downloaded
  - Returns a task ID for tracking download progress
  - Does not apply the update (use apply_update after download completes)

- **apply_update** - Apply downloaded TrueNAS system update (**destructive**)
  - **Requires `confirm: true`** to execute; without it returns a preview and changes nothing
  - Applies previously downloaded system update
  - Optional automatic reboot after update (default: false for safety)
  - Supports dry-run mode to preview update actions
  - Returns a task ID for tracking update progress
  - Creates a new boot environment automatically
  - **Best Practice**: After successful update and reboot, use query_boot_environments to check for old boot environments that can be safely pruned with delete_boot_environment. Recommend keeping 2-3 recent boot environments for rollback safety.
  - **WARNING**: This will update your TrueNAS system - ensure backups are current

- **update_status** - Get current system update status and progress
  - Shows download/apply progress for in-progress updates
  - Displays current system version and available updates
  - Useful for monitoring long-running update operations

- **system_reboot** - Reboot the TrueNAS system (**destructive**)
  - **Requires `confirm: true`** to execute; without it returns a preview and changes nothing
  - Supports dry-run mode showing hostname, version and uptime
  - Optional `delay` in seconds before rebooting, 0-86400 (default: immediately); a pending reboot cannot be cancelled through this server
  - Performs a clean system reboot
  - Disconnects all active sessions and services
  - Use after applying system updates that require a reboot
  - **WARNING**: This will interrupt all services and disconnect clients

## Boot Environment Management

- **query_boot_environments** - Query TrueNAS boot environments
  - Filter by name, show only protected or deletable environments
  - Sort by name, creation date (default), or size
  - Shows which are active/activated/protected/deletable
  - Displays deletion blockers and storage summary
  - `created` is ISO-8601 UTC (with `created_timestamp` in Unix seconds) when the server's value parses to a time between 2000-01-01 and 10 years from now; any other value is shown raw with `created_timestamp` 0
  - Perfect for "what old boot environments can I clean up?"

- **delete_boot_environment** - Delete a boot environment (**destructive**)
  - **Requires `confirm: true`** to execute; without it returns a preview and changes nothing
  - Dry-run mode shows what will be deleted and space freed
  - Safety checks prevent deleting active/activated/protected
  - Recommends keeping 2-3 boot environments for recovery
  - **WARNING**: Permanent and irreversible

- **get_current_boot_environment** - Quick reference
  - Shows currently running boot environment
  - Shows which will boot on next restart

## Pool Scrub Management

- **query_scrub_schedules** - List all scrub schedules
  - Filter by pool name or enabled status
  - Shows schedule details and next run time
  - View all scheduled maintenance at a glance
  - Human-readable cron schedule descriptions

- **get_scrub_status** - Comprehensive scrub status
  - Shows current scrub progress if running
  - Displays last scrub date and results
  - Lists next scheduled scrub times
  - Combines schedule and runtime info in one view
  - Perfect for "when was tank last scrubbed?"

- **create_scrub_schedule** - Schedule automatic scrubs
  - Weekly, monthly, or custom cron schedules
  - Dry-run mode to preview configuration
  - Validates pool exists and schedule is valid
  - Recommends optimal timing (off-peak hours)
  - Best practices: Weekly for production, monthly for home use

- **run_scrub** - Manually start a scrub
  - Returns task ID for progress tracking
  - Safety checks prevent double-scrubbing
  - Dry-run shows estimated duration and impact
  - Integrates with task manager for monitoring
  - Use before backups or after hardware changes

- **delete_scrub_schedule** - Remove scrub schedule (**destructive**)
  - **Requires `confirm: true`** to execute; without it returns a preview and changes nothing
  - Dry-run shows what will be removed
  - Warns about loss of automatic scrubbing
  - Recommends manual scrub frequency
  - **WARNING**: Pool will no longer auto-scrub

## Task Management

For long-running operations like app upgrades, system updates, and scrubs:

- **tasks_list** - List all active and recent tasks
- **tasks_get** - Get detailed status of a specific task by ID
  - Automatic background polling of TrueNAS job status
  - Tasks update automatically without manual polling
