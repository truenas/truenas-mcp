# Complete TrueNAS MCP Feature List

This document provides a comprehensive list of all available MCP tools for TrueNAS management.

## Read-Only Monitoring Tools

### System Information
- **system_info** - Get system information (version, hostname, platform)
- **system_health** - Check system health including alerts, active jobs, and capacity warnings
- **query_jobs** - Query system jobs (running, pending, or completed tasks like replication, snapshots, scrubs)

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
  - Returns simplified snapshot information with creation date, dataset, and holds status
  - Filter by dataset name, pool name, or holds presence
  - Sort by snapshot name (default, newest first), dataset, or parsed creation date
  - Limit results for manageable responses (default: 50, configurable)
  - Shows snapshot names, parent datasets, creation dates (parsed from names), and holds
  - Perfect for questions like "what recent snapshots exist?" or "show snapshots with holds"

- **query_shares** - Query SMB and NFS share configurations

### Virtualization
- **query_vms** - Query virtual machines with intelligent filtering and sorting
  - Returns simplified VM information with resource allocation, status, and device summary
  - Filter by VM name (partial match), state (RUNNING/STOPPED), or autostart setting
  - Sort by name (default, alphabetical), memory usage, or status (running first)
  - Shows CPU/memory config, bootloader, devices (disks, NICs, displays), and current state
  - Automatically excludes sensitive data like display passwords for security
  - Perfect for questions like "what VMs are running?" or "show VMs with autostart enabled"

### Alerts
- **list_alerts** - List system alerts with filtering
- **dismiss_alert** / **restore_alert** - Manage system alerts

### Performance Metrics
- **get_system_metrics** - Get CPU, memory, and load performance metrics
- **get_network_metrics** - Get network interface traffic metrics
- **get_disk_metrics** - Get disk I/O performance metrics

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

- **delete_app** - Remove installed applications
  - IMPORTANT: Host-path datasets are NOT deleted (preserved for data safety)
  - Data remains in original locations for manual cleanup
  - Optional removal of container images
  - Supports dry-run mode to preview deletion
  - Returns task ID for tracking deletion progress

- **upgrade_app** - Upgrade an application to a newer version with optional snapshot backup
  - Supports dry-run mode to preview changes before execution
  - Returns a task ID for tracking long-running operations

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
  - Refreshes cached information from directory service
  - Use after making changes in Active Directory, LDAP, or IPA
  - Quick operation, no task tracking needed

### Write Operations
- **configure_directory_service** - Configure and join directory service
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

- **leave_directory_service** - Disconnect from directory service
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

- **apply_update** - Apply downloaded TrueNAS system update
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

- **system_reboot** - Reboot the TrueNAS system
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
  - Perfect for "what old boot environments can I clean up?"

- **delete_boot_environment** - Delete a boot environment
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

- **delete_scrub_schedule** - Remove scrub schedule
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
