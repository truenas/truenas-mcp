# Complete TrueNAS MCP Feature List

This document provides a comprehensive list of all available MCP tools for TrueNAS management.

Tools are classified as read, write or destructive (`tools/safety.go`). Write and destructive tools are hidden and refused in `--read-only` mode (except `upgrade_app`, which stays available for `dry_run: true` previews only), and `--allow-tools` / `--deny-tools` restrict the exposed set. The eighteen destructive tools (marked below) execute only after confirmation. When the MCP client supports form elicitation, the server asks the user to approve a preview in the client whose message names the tool, the target, a headline, and the key facts being approved (versions, names, changed configuration keys and storage paths, and similar), already redacted and cut at 2 KiB when needed, and executes only if they accept; the token-path dry-run preview includes the same facts as `confirm_summary`; a declined, cancelled or timed-out prompt executes nothing. Clients without elicitation use a `confirmation_token`: called without a valid one they change nothing and return a `confirmation_required` result with their dry-run preview and a single-use token, valid for 5 minutes and bound to the tool and the exact arguments; repeat the call with the same arguments plus `confirmation_token` to execute it. `--require-human-confirm` never issues or redeems a confirmation token: clients without elicitation are refused, and a client that declared it is prompted even if a token was supplied. A preview that says the operation cannot proceed returns an error and issues no token and no prompt. The token proves that a preview of these arguments happened, not that a human approved it; elicitation raises the bar but is not proof of a human either, because the capability is self-declared. `dry_run` (and the old `confirm`, which no longer executes anything) must be JSON booleans (`true` or `false`); a string such as `"true"` or a number is rejected and nothing is executed. Every other argument is checked against the tool's input schema before anything runs, too: unknown or missing required arguments, wrong JSON types (for example `"browsable": "false"`, `"create_ancestors": "false"` or `"quota": "10G"`), fractional integers, and values outside an enum or range (such as a negative `quota` or a `limit` of 0) are rejected with an error that names the argument without echoing its value; `null` on an optional argument means omitted (see [Argument validation](safety.md#safety-controls)). **Breaking change:** `"confirm": true` no longer executes a destructive tool (it gets the preview, a token and an explanation), and `update_app` is now destructive. See [Safety controls](safety.md#safety-controls).

Every tool result and error sent to the MCP client is at most 100 KiB. Larger output is redacted first, then cut, with a notice telling the model to narrow the query with `limit`, `select`, or filters. Middleware error traces are not sent to the client; `--debug` logs them to stderr (see [Security and output redaction](safety.md#security-and-output-redaction) and [Debug logging](configuration.md#debug-logging)).

The minimum supported TrueNAS release is 25.10, checked at startup. Tools that a connected release cannot run (see [TrueNAS version compatibility](configuration.md#truenas-version-compatibility)) are hidden from `tools/list` and refused if called.

## Read-Only Monitoring Tools

### System Information
- **system_info** - Get system information (version, hostname, platform)
  - Datetimes (`buildtime`, `boottime`, `datetime`) are ISO-8601 UTC strings (e.g. `"2026-01-01T00:00:00Z"`) instead of the middleware's `{"$date": <ms>}` objects. Values outside 2000-01-01..now+10 years are passed through raw
- **system_health** - Check system health including alerts, active jobs, and capacity warnings
  - `alerts` lists only active (not dismissed) alerts. `alert_count` counts them; `critical_alert_count` counts active CRITICAL/ALERT/EMERGENCY alerts and `dismissed_alert_count` dismissed ones, which are not listed (use `list_alerts` with `dismissed: true` or `include_dismissed: true` to see them)
  - The memory warning is based on memory used excluding the ZFS ARC (latest complete available sample minus the latest ARC size) when the ARC size is known; otherwise it says the figure may include the ARC. If the ARC size was used but its age could not be checked (no timestamp in the reporting data), `arc_freshness_note` says so, as in `analyze_capacity`
  - Alert datetimes are ISO-8601 UTC strings, as in `list_alerts`
  - The CPU check uses the legend series `cpu` (column 1 if absent) and skips samples without data (all-zero rows, see `get_system_metrics`)
- **query_jobs** - Query system jobs (running, pending, or completed tasks like replication, snapshots, scrubs). Returns every state by default; `state` (case-insensitive) and `method` (exact job method, e.g. `app.upgrade`) narrow the list. Reports `has_more` and `total` when the list was cut by `limit` (default 50); each entry is at most 4 KiB
  - `time_started` and `time_finished` are ISO-8601 UTC strings (same rules as `system_info`)

### Storage Management
- **query_pools** - Query storage pools with status and capacity
  - Returns the same pool records as main, including topology, with `size`, `allocated`, and `free` as integers
- **query_datasets** - Query datasets with intelligent filtering and sorting
  - Returns simplified, human-readable dataset information (~15 fields instead of 40+)
  - Filter by pool name (exact `pool` match, so `tank` does not include `tank2`), encryption status
  - Asks the middleware for only the properties this tool uses (including `dedup` and encryption properties so `encrypted_only` works), without nested `children` trees (`retrieve_children: false`) and without user properties. An unfiltered call still sends a `type in [FILESYSTEM, VOLUME]` filter so middleware walks every dataset
  - A compact name listing (name and encryption only) supplies `children_count` and `total_datasets`. The full-property query then sends `limit` and a server-side `order_by` (`-used.parsed` by default, `-available.parsed`, or `name`) so a large system does not return every dataset in one response
  - Sort by space usage (default), available space, or name
  - Limit results for manageable responses (default: 50, maximum: 1000). A requested limit above 1000 is capped, and the response note says so
  - `total_datasets` is the count after `encrypted_only`, before `limit`
  - Shows capacity (used/available), compression ratios, encryption status, usage breakdown
  - Perfect for questions like "what datasets use the most space?" or "show me encrypted datasets"

- **query_snapshots** - Query ZFS snapshots with intelligent filtering and sorting
  - Returns simplified snapshot information with creation time, sizes and dataset; holds with `include_holds` or `holds_only`
  - Requests the ZFS `creation`, `used` and `referenced` properties explicitly (`extra.properties`), since TrueNAS 27.0 does not return them by default
  - Holds (`holds`, `holds_count`) are requested (`extra.holds`) only when `include_holds` or `holds_only` is true, because the middleware looks them up once per snapshot. `holds_included` says whether they were requested; without it, snapshots show no holds even if they have some
  - `created` (ISO-8601 UTC) and `created_timestamp` (Unix seconds) come from the `creation` property; if it is missing, `created_date` is parsed from the snapshot name. `created_source` is `zfs_property` or `snapshot_name`
  - `used_bytes`/`used` (space unique to the snapshot) and `referenced_bytes`/`referenced` (data it refers to), raw and human-readable
  - Filter by dataset name, pool name, or holds presence (on 27.0 the middleware reports only its own `truenas` hold)
  - Sort by snapshot name (default, newest first), dataset, or creation time (`created`: newest first by `created_timestamp`, then `created_date`, then name)
  - Limit results for manageable responses (default: 50, maximum: 1000). A requested limit above 1000 is capped, and the response note says so
  - Every query pushes `limit` and a server-side `order_by` (`-snapshot_name`, `dataset`, or `-properties.creation.parsed` for `created`). `createtxg` is an unpadded string, so it is not used as the creation order key. If the page is full, a `count` query supplies `total_snapshots`. If that count query fails, `total_snapshots` is omitted and a note says the total is unknown
  - `holds_only` pages through results with `limit`/`offset` up to a 1000-row scan budget and says so when that budget is reached; more snapshots with holds may exist, so narrow the query with `pool` or `dataset`
  - `include_retention` (opt-in, `extra.retention`) adds `retention` to each snapshot: `removal_date` (ISO-8601 UTC), `removal_timestamp`, `source` (`periodic_snapshot_task` or `property`) and `periodic_snapshot_task_id`, or `null` when nothing schedules removal. `retention_included` says whether it was requested
  - Perfect for questions like "what recent snapshots exist?" or "show snapshots with holds"

- **query_shares** - Query SMB and NFS share configurations
  - Returns the same SMB and NFS share records as main (no field select), including SMB `options` (`hostsallow`/`hostsdeny`), `access_based_share_enumeration`, `audit`, and `locked`, and NFS `security`, `expose_snapshots`, `locked`, and `aliases`

- **get_share_acl** - Show the share-level ACL of an SMB share, by `share_name` or `share_id`
  - Each entry has who it applies to (name, SID, Unix id), `ALLOWED` or `DENIED`, and the permission level (`FULL`, `CHANGE`, `READ`)
  - Also returns the share settings that block access: path, enabled, readonly, `hostsallow`/`hostsdeny`
  - `HOMES` means the home-directory share. The share ACL only applies over SMB; the filesystem ACL must allow the access too
- **get_filesystem_acl** - Show the filesystem ACL of a path under `/mnt/<pool>`
  - ACL type (`NFS4`, `POSIX1E` or `DISABLED`), owner and group with names, mode bits, and each entry: tag, who, `ALLOW`/`DENY`, permissions and inheritance flags (NFSv4, in evaluation order), or access/default scope and effective permissions after the mask (POSIX)
  - Only absolute paths under a pool mountpoint (`/mnt/<pool>`) are accepted. Relative paths, `..`, other system paths and symlinks anywhere in the path that lead outside `/mnt/<pool>` are refused before the ACL is read. Each path component is checked, and a symlink inside a pool is read at its target
  - `simplified: true` shows NFSv4 permissions and flags as basic names (`FULL_CONTROL`, `MODIFY`, `READ`, `TRAVERSE`; `INHERIT`, `NOINHERIT`) where they match exactly

### Virtualization
- **query_vms** - Query virtual machines with intelligent filtering and sorting
  - Requests only the VM fields this tool uses
  - Returns simplified VM information with resource allocation, status, and device summary
  - Filter by VM name (partial match), state (RUNNING/STOPPED), or autostart setting
  - Sort by name (default, alphabetical), memory usage, or status (running first)
  - Shows CPU/memory config, bootloader, devices (disks, NICs, displays), and current state
  - CPU: `sockets` (the API's `vcpus`), `cores_per_socket` (`cores`), `threads_per_core` (`threads`) and `total_vcpus` (their product, what the guest sees; a missing value counts as 1, the schema default)
  - Memory: `memory_mib` (the API value, in MiB), `memory_gib` (e.g. `"4.0 GiB"`; two decimals below 1 GiB, e.g. `"0.50 GiB"`) and `min_memory_mib` when set. These replace `vcpus`/`cores`/`threads` and `memory_mb`/`memory_gb`
  - Automatically excludes sensitive data like display passwords for security
  - Perfect for questions like "what VMs are running?" or "show VMs with autostart enabled"

### Accounts
- **query_users** - Query user accounts
  - Returns username, uid, full name, `builtin`, `immutable`, `locked`, `password_disabled`, `ssh_password_enabled`, `primary_group`/`primary_gid`, secondary `groups` by name, home, shell, `smb`, `two_factor_enabled`, `sudo`/`sudo_nopasswd` (whether any sudo commands are set, not the commands) and `roles`
  - Never returns password hashes, password history, 2FA secrets, SSH keys or API keys: they are not requested and the output is built from a fixed field list
  - Filter by exact `username`, `builtin` and `locked`. Local accounts only unless `include_directory_users` is true. `limit` defaults to 50 (maximum 1000)
- **query_groups** - Query groups
  - Returns name, gid, `builtin`, `immutable`, `smb`, `sudo`/`sudo_nopasswd`, `roles`, and `members` by username (up to 100 per group; `member_count` is the full count and `members_truncated` is set when names were cut)
  - Filter by exact `name` and `builtin`. Local groups only unless `include_directory_groups` is true. `limit` defaults to 50 (maximum 1000)
  - Group names (for `query_users`) and usernames (for `query_groups`, only for the members listed) come from one extra lookup per call, local accounts only unless directory accounts were asked for; ids that could not be resolved are listed under `unresolved_group_ids` / `unresolved_member_ids`

### Disks and Services
- **query_disks** - List physical disks: name, serial, model, size, type (`HDD`, `SSD` or `NVMe`), bus and pool
  - `usage` is `USED` (in an imported pool, including the boot pool, or in an exported pool that could be imported; `exported_pool` names it), `UNUSED`, or `UNKNOWN` when TrueNAS did not report the disk. If that lookup fails, `usage` is `UNKNOWN` and `in_use` falls back to pool membership
  - Filter by `name`, `pool` or `unused_only`; `limit` bounds the list (default 50, maximum 1000). SED passwords and free-text descriptions are not returned
- **get_disk_health** - One call to find a failing disk
  - For each pool device (data pools and the boot pool): ZFS state, read/write/checksum error counts and the parent vdev. Disks in no pool are included with temperature only
  - Temperature and critical threshold (cached by TrueNAS for up to 5 minutes) and current disk temperature alerts. An alert dismissed in the UI still counts while TrueNAS keeps reporting it, and is marked `(dismissed)`
  - Each disk has `healthy` and `problems`; unhealthy disks come first and are named in `unhealthy`. A device that is no longer on the system has `name` null, its serial and model from the TrueNAS disk record, and `last_known_name` set to the name it had (Linux may have given that name to another disk since); it is labelled by its ZFS guid
  - TrueNAS 25.10 has no public SMART test API, so SMART self-test results and attributes are not included
  - Filter by `name` (also matches a removed device's `last_known_name`) or `pool`. If temperatures or alerts cannot be read, the rest is still returned with a `note`
- **query_services** - List system services (`cifs` is SMB) with `state` (`RUNNING`, `STOPPED`, or `UNKNOWN` if TrueNAS could not read it in time), `running` and `enabled_at_boot`. `name` returns one service
- **control_service** - Start, stop, restart or reload a service and wait for the result (**destructive**)
  - **Requires a `confirmation_token`** to execute, for every action; without one returns a preview and a token and changes nothing
  - Input: `service` (as `query_services` lists it) and `action` (`START`, `STOP`, `RESTART` or `RELOAD`). Sends the `service.control` job with `{"silent": false}`, so a failed action fails the job instead of returning `false`, then waits up to 150 seconds for it (TrueNAS gives up after 120). A job that has not finished by then is reported as `running` with a `task_id` for `tasks_get`. A job that is no longer listed, runs a different method, or can no longer be identified after a reconnect is reported as `unknown`, with no task; check `query_services` before retrying
  - On an HA system, TrueNAS also runs the action on the standby controller when this controller is active, failover is healthy and the service may run on the standby (the 25.10 default `ha_propagate: true`, which this tool does not change)
  - The preview shows the current `state` and `enabled_at_boot`. STOP and RESTART of `cifs`, `nfs`, `iscsitarget`, `nvmet` and `ups` warn that connected clients are disrupted. RELOAD of a service TrueNAS cannot reload (for example `ups` or `snmp`) is a restart, and the preview says so; it is checked and warned like a RESTART
  - STOP and RESTART of `ssh` (and a RELOAD of a protected service that TrueNAS performs as a restart) are refused unless `allow_protected_service` is true; the preview and confirmation show the flag. RELOAD of `ssh` is a real reload and is allowed
  - Always refused: services `query_services` does not list (`smb` suggests `cifs`), and `http` (the web server this server connects through). Also refused when nothing would change: START of a RUNNING service, STOP or RELOAD of a STOPPED one
  - The token is bound to the previewed state, boot setting and `allow_protected_service`: a service started, stopped or re-enabled after the preview rejects it
  - Does not change whether the service starts at boot
- **set_service_enabled** - Set whether a service starts at boot
  - Input: `service` and `enable` (boolean). Sends `service.update` with `{"enable": ...}` and reads the setting back
  - A write tool: it runs without a confirmation token unless `dry_run` is set. The preview shows the current `state` and `enabled_at_boot`; nothing is started or stopped now (use `control_service`)
  - Disabling `ssh` at boot is always refused, with no override, because this tool has no confirmation step; change it in the TrueNAS web UI. Unknown services and `http` are refused. If the service already has the requested setting, nothing is sent and the result says `changed: false`

### Alerts
- **list_alerts** - List system alerts with filtering
  - **Default changed:** without arguments, only active alerts are listed and dismissed alerts are excluded (previously all alerts were listed). `include_dismissed: true` lists all alerts; `dismissed: true` lists only dismissed alerts and `dismissed: false` only active ones
  - Alerts that have no `dismissed` field are kept by the default view, but an explicit `dismissed: false` (like `dismissed: true`) keeps only alerts whose `dismissed` field matches, so it drops them
  - `dismissed` and `include_dismissed` must be JSON booleans (`"true"` or `1` is rejected), and a contradictory combination such as `dismissed: true, include_dismissed: false` is rejected; in both cases the call fails before anything is queried
  - `datetime`, `last_occurrence` and other datetimes are ISO-8601 UTC strings
- **dismiss_alert** / **restore_alert** - Manage system alerts (write operations; support dry-run mode)

### Performance Metrics
- **get_system_metrics** - Get CPU, memory, and load performance metrics
  - Each graph has a `unit` (`%CPU`, `Bytes` of available memory, `Load`, `Celsius`, `Seconds`)
  - TrueNAS fills periods without data with zeros (common for MONTH/YEAR). Rows whose values are all 0 or null are treated as gaps: they are left out of `data_sample`, and when there are any, `aggregations` is recomputed without them (the middleware's values are kept in `aggregations_reported`) and `gap_samples_excluded`, `data_points_valid` and `gap_note` are added. `data_points_total` counts all rows. A genuine all-zero sample (e.g. zero load) is also treated as a gap
  - For `cputemp` and `disktemp`, whose middleware aggregations already skip every zero value (`skip_zero_values_in_aggregation`), `aggregations` is kept as reported; gap rows are still left out of `data_sample`
  - `last_sample_after_end_seconds` appears when the newest sample is stamped after the graph's `end`. That sample is a partial bucket: netdata averages samples into aligned buckets stamped by their end, so for long ranges the last bucket can lie past `end` (about 2 minutes has been seen for MONTH) and cover only part of its interval. `get_system_metrics` returns it raw, in `data_sample` and `aggregations`; `analyze_capacity` skips it for current figures
- **get_network_metrics** - Get network interface traffic metrics
  - `unit` is `Kilobits/s` (series `received` and `sent`)
- **get_disk_metrics** - Get disk I/O performance metrics
  - `unit` is the graph's `vertical_label` from `reporting.graphs` (`Kibibytes/s` for `disk`, series `reads` and `writes`)
- **get_arc_metrics** - Get ZFS ARC reporting metrics (e.g. `arcsize`)
  - The ARC graphs available depend on the TrueNAS version; the default is every ARC graph the server supports, discovered from `core.get_methods` and `reporting.graphs` (falls back to `arcsize`)
  - A graph that fails is reported under `errors.<graph>` while the others are returned; if every graph fails the call is an error

### Applications
- **query_apps** - List installed applications with status, `human_version`, and `upgrade_available` (whether an upgrade exists) and `latest_version` (newest catalog version, always set, in catalog form rather than `human_version` form)
- **search_app_catalog** - Search TrueNAS app catalog by name, category, or keyword
  - The query is a case-insensitive literal substring match (not a regular expression), so `cloud` finds nextcloud
  - `category` is an exact match against the app's categories list
  - `limit` must be at least 1 (default 20)
  - Search across all catalog trains (stable, enterprise, community)
  - Filter by category (media, productivity, database, etc.)
  - Returns app information with versions and installation status
- **get_app_catalog_details** - Get detailed information about a specific app
  - Returns README documentation, version info, categories, and maintainers
  - Returns the app schema (groups and questions, including each question's defaults)
  - When the schema defines run-as user/group or resource limits, those defaults are included in a `defaults` object; they are omitted when the schema has none
  - Provides storage volume hints (detected from README)
  - Recommends dataset layout: /mnt/<pool>/apps/<appname>/<volume>
  - Tool descriptions (this tool and `install_app`) carry the install workflow, worded so any MCP client can follow it

## Capacity Planning and Analysis

### System-Wide Analysis
- **analyze_capacity** - Comprehensive capacity analysis with historical trends and projections
  - CPU, memory, network, and disk I/O utilization analysis
  - Current, average, and peak utilization percentages
  - Trend detection (increasing/stable/decreasing)
  - Capacity status warnings (healthy/warning/critical at 70%/85% thresholds)
  - Growth projections when metrics are trending upward
  - The default time range is MONTH (about 30 days), unlike `get_system_metrics`, `get_network_metrics` and `get_disk_metrics`, which default to HOUR. The range used is given as `time_range` in the CPU and memory sections and in each interface and disk entry
  - "Current" figures use complete samples only. A sample stamped after the graph's `end` is a partial bucket (the same sample `get_system_metrics` returns raw and flags with `last_sample_after_end_seconds`): it is skipped for current figures and `partial_sample_note` is added; it still counts toward average, peak and trend. If every sample is past `end`, they are used as they are and `partial_sample_note` says the current figures come from a partial bucket. CPU, network and disk `current` values are the mean of the last 5 complete samples; memory uses the latest complete sample
  - CPU: the legend series `cpu` (column 1 if absent); samples without data (all-zero rows) are skipped
  - Network: per interface, `received` and `sent` are analysed separately (`current_mbps`, `average_mbps`, `peak_mbps`, utilization against `link_speed_mbps`), converted from the graph's Kilobits/s. TrueNAS 27.0 interfaces report no `link_speed`, so on 27.0 there is no `link_speed_mbps` and the `*_utilization_pct` and `capacity_status` fields are absent
  - Disk: per disk, `reads` and `writes` in `Kibibytes/s` (`unit`, the same spelling as `get_disk_metrics`)
  - Network and disk samples are not gap-filtered: zero is a real value for traffic, so periods TrueNAS zero-fills (common for MONTH and longer) count as zero traffic and can lower `average`, `current` and `trend`
  - Memory: used = total - available. `current_used_bytes`, `current_available_bytes`, `current_utilization_pct`, `current_utilization_including_arc_pct` and `used_excluding_arc_bytes` all come from the latest complete sample, so `current_used_bytes - arc_bytes = used_excluding_arc_bytes`. When the latest ARC size is known and recent, `current_utilization_pct` and `capacity_status` use memory used excluding the ZFS ARC (`current_utilization_basis: "excluding_arc"`, also `arc_bytes`, `used_excluding_arc_bytes`, `utilization_excluding_arc_pct`) and `current_utilization_including_arc_pct` gives the ARC-inclusive figure. Otherwise `current_utilization_basis` is `"including_arc"`. average/peak/trend still include the ARC, and no memory projection is given in that case (`projection_note`), since only the latest ARC size is known. "Recent" means at most 15 minutes older than the newest timestamp in the NAS's own reporting data (the memory graph's latest sample or the graph `end`), so clock skew between the NAS and the MCP host does not matter; if no such timestamp is available the ARC size is used and `arc_freshness_note` says its age could not be checked
  - Time ranges: HOUR, DAY, WEEK, MONTH, YEAR
  - Overall recommendations based on all metrics

### Storage Capacity
- **get_pool_capacity_details** - Detailed pool and dataset capacity information
  - Current pool capacity (total, used, available bytes)
  - Utilization percentages for each pool
  - Optional `pool_name` is an exact pool match (`tank` does not include `tank2`)
  - Per-dataset breakdown uses the same `pool` filter, `retrieve_children: false`, and property list as `query_datasets`, and reads top-level `used` / `available` (the same parser as `query_datasets`)
  - Returns the top 50 datasets by used space (name breaks ties) plus `total_datasets`; a `datasets_note` says so when the list is limited. The full-row query asks TrueNAS for only those 50 rows (`order_by` used descending, with `limit`; ties are ordered by name client-side); `total_datasets` and `children_count` come from a separate compact name listing of the pool, so they cover every dataset
  - Dataset query or parse errors are reported on the pool as `datasets_error`; the `datasets` key is still present
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
  - `create_ancestors` defaults to true; the dry-run preview lists any missing parent datasets that would be created, in order from parent to child (`ancestors_to_create`). Those ancestors are created with no properties of their own; they inherit compression, ACL type and encryption from their parent and have no quota of their own (they do not receive the leaf's share_type, compression or encryption). Under an unencrypted parent, ancestors of an encrypted dataset are unencrypted. The preview fails if the pool does not exist, the dataset already exists, or the name is invalid (including `.` / `..` components and names longer than 255 characters)
  - Wizard-style guidance for SMB/NFS/iSCSI setup

### Share Management
- **create_smb_share** - Create SMB shares for Windows/macOS file sharing
  - Interactive wizard walks through share configuration
  - Purpose-based setup (standard, Time Machine, multi-protocol, home dirs)
  - Access control (IP restrictions, read-only, browsability)
  - Audit logging for compliance
  - Security warnings for public shares
  - Dry-run mode to preview with security analysis
  - The "any network host" warning looks at `options.hostsallow` and treats an omitted `browsable` as true (the TrueNAS default)
  - `network_path` uses the short hostname from `system.info` (preview and result), which is not the HA virtual hostname. If that lookup fails, the path uses the shell-safe placeholder `NAS_HOSTNAME` and a `hostname_note` marks it as a placeholder

- **create_nfs_share** - Create NFS shares for Unix/Linux file sharing
  - Interactive wizard walks through NFS configuration
  - Network/host access restrictions (CIDR notation, IP/hostname lists)
  - User mapping for security (maproot, mapall)
  - Read-only or read-write access
  - Security level selection (SYS, Kerberos)
  - Security warnings for unrestricted access
  - Dry-run mode to preview with mount examples
  - Root-mapping warning: `no_root_squash` only when both `maproot_user` and `maproot_group` are `root`/`wheel`. A single root mapping keeps `root_squash` and sets `anonuid`/`anongid`. If the mapped user is uid 0, the warning says that means root file access. Other maproot names (for example `alice`) are reported as mapped to that account, not to nobody. `mapall_*` is described as `all_squash` to the named account, and mapall to uid 0 is called out as root file access for every client
  - `mount_example` uses the short hostname from `system.info` (preview and result), which is not the HA virtual hostname. If that lookup fails, the example uses the shell-safe placeholder `NAS_HOSTNAME` and a `hostname_note` marks it as a placeholder

### Application Management
- **install_app** - Install applications from the catalog with guided storage setup
  - When `train` is omitted, the catalog train that has the app is looked up (stable is preferred if several do), as with get_app_catalog_details. If the lookup itself fails, the stable train is assumed and the response includes a `train_note` saying so
  - Tool descriptions guide the install workflow (review schema groups, query pools, create datasets, use host_path, use schema defaults when present)
  - ALWAYS uses host-path volumes (NEVER ix-volumes)
  - Fetches the app schema and requires every storage entry to be present as `type: host_path` with a cleaned path under `/mnt/<pool>/` (at least one component below an existing pool). `host_path_config.acl_enable` must be a JSON boolean; when it is true, the mounted `acl.path` is the path that is validated and checked for an existing dataset. Fields whose schema type is `hostpath`, `hostpathdirectory`, or `hostpathfile` are also checked. `hostpathfile` is checked as a file against its parent dataset; `hostpathdirectory` is checked as a directory. Catalog type `hostpath` is used for both directories (webdav and rsyncd shares) and files (for example Nextcloud `custom_confs`): the path is treated as a directory if it exists as a dataset, and as a file if it does not and its parent is a dataset below the pool. A hostpath that is not an existing dataset and sits directly under the pool is reported as a missing dataset (a create_dataset hint on the real call, or a CRITICAL warning on dry-run), not as a classification error. An app schema with no questions list, an empty list, items that cannot be parsed, a question with no schema map, or an attrs entry that is not a map is refused, including on dry runs. Off-schema storage-like types must be the exact string `host_path`; an unrecognized type is also refused when the object carries a volume config key. Paths under `/mnt/<pool>/.system` or `/mnt/<pool>/ix-applications` are refused.
  - Enforces structured dataset layout: /mnt/<pool>/apps/<appname>/<volume>
  - Validates app instance names (lowercase, alphanumeric, hyphens)
  - Verifies datasets exist before installation
  - Refuses, naming the path, any value that is or contains `[REDACTED]` (for example copied from another app's `get_app_config`): a new app has no stored value to keep, so the marker would be installed as the real value
  - Supports dry-run mode to preview installation
  - Returns task ID for tracking installation progress
  - The `install_app` and `get_app_catalog_details` tool descriptions describe the install workflow (search, review the schema, plan storage, create datasets, dry-run, then install). Run-as and resource defaults come from the app schema, not from fixed values in the tool result.

- **delete_app** - Remove installed applications (**destructive**)
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
  - IMPORTANT: Host-path datasets are NOT deleted (preserved for data safety)
  - The preview lists host-path locations from `app.config` (storage slots, including `additional_storage` and bare host-path entries, and Compose service `volumes` bind mounts); `ix_volume` slots and non-volume arrays such as `command` are omitted
  - Data remains in original locations for manual cleanup
  - Optional removal of container images
  - Supports dry-run mode to preview deletion
  - Returns task ID for tracking deletion progress

- **upgrade_app** - Upgrade an application to a newer version with optional snapshot backup (**destructive**: the app goes offline and migrations may be one-way). The task completes only once the app is RUNNING (`health_timeout_seconds`: integer 0-600, default 60; an app still starting at the deadline fails the task with a message that the upgrade itself succeeded). TrueNAS always stops the app and snapshots its ix-volumes; `snapshot_hostpaths` only requests snapshots of host paths and is not a confirmation that one was taken
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
  - Supports dry-run mode to preview changes before execution
  - Returns a task ID for tracking long-running operations

- **update_app** - Change an installed application's configuration (**destructive**: replaces the configuration in place and restarts the app)
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
  - Storage may be omitted to leave current storage unchanged, including existing `ix_volume` entries. If a partial storage object is sent, omitted slots are filled from the current config before the update is submitted, so they do not revert to schema defaults. When a provided slot keeps the same type and location, omitted top-level keys (such as `read_only`) and omitted fields of `host_path_config`, `nfs_config`, and `cifs_config` are also filled from the current slot
  - New storage entries, and type or location changes that are not a same-type NFS or CIFS server/path change, must be `type: host_path` with a cleaned path under `/mnt/<pool>/` on an existing dataset (the same path and dataset-exists checks as install_app). Unchanged existing entries, including `ix_volume`, are left as they are
  - Changing the type or location of an existing slot, including an NFS or CIFS server or path, requires `allow_storage_change=true`. The preview and confirmation prompt name each changed slot, its old and new location, and state that data is not migrated
  - A change to `read_only` or ACL settings (`acl_enable` or `acl`) on a slot whose type and location are unchanged does not need the flag, but is listed as a plan warning with the old and new values
  - A value that is exactly `[REDACTED]` (as `get_app_config` shows secrets) keeps the value stored at that path, so a nested object can be resent without overwriting its secret. It is refused, naming the path, if nothing redacted is stored there, if the change would leave the kept value unredacted (for example a renamed or reordered environment entry), if an enclosing entry changes its `name`, `key`, `variable`, `username`, `principal`, `binddn` or `credential_type` (for example `DB_PASSWORD` renamed to `API_TOKEN`; a legitimate rename that keeps its secret is refused too, so send the real value), or if the enclosing storage entry changes type or location (judged from both the stored and the sent entry). The preview and confirmation prompt name each kept path, never the value (see [docs/safety.md](safety.md#dry-runs-and-previews))
  - The confirmation prompt lists each changed leaf field (`network.hostname: "a" -> "b"`) rather than whole nested objects; a changed secret is shown as `changed (secret, not shown)`, and kept paths are listed separately
  - Supports dry-run mode

- **start_app** - Start a stopped application
  - Dry-run and the real call share one plan: the preview lists only the `app.start` call that will be made. Preview details use named fields (masked for secrets); the real call sends the positional parameters
  - A STOPPED app is started with no "must be STOPPED" warning
  - An already RUNNING app is refused on both dry-run and the real call (an error; nothing is started)
  - A DEPLOYING or STOPPING app is also refused, with a "retry when it settles" message. A CRASHED app can still be started
  - Supports dry-run mode; returns a task ID for tracking

- **stop_app** - Stop a running application (**destructive**: the service goes offline)
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
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
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
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
  - A secret sent back as `[REDACTED]` (as `query_directory_services` shows it, for example `credential.bindpw` or an idmap `ldap_user_dn_password`) keeps its stored value when the account, backend and directory it belongs to (including LDAP `auxiliary_parameters`) are unchanged and no `validate_certificates` or `starttls` flag is turned off (leaving out `starttls`, whose default is false, counts as turning it off); otherwise the call is refused, naming the path. The preview and confirmation prompt name each kept path, never the value (see [docs/safety.md](safety.md#dry-runs-and-previews))
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
  - Returns `status: "submitted"` with task_id for tracking the long-running domain join operation (the job has been accepted, not completed)
  - Automatic domain join when enable=true
  - Use enable=false to disable without leaving domain

- **leave_directory_service** - Disconnect from directory service (**destructive**)
  - Refuses a credential value that is or contains `[REDACTED]`, naming the path, before contacting TrueNAS
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
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
  - Returns `status: "submitted"` with task_id for tracking the leave operation (the job has been accepted, not completed)
  - Consider using configure_directory_service with enable=false instead for temporary disable

### Integration with Other Tools
- **system_health** - Now includes directory service status
  - Automatically checks directory service health
  - Shows service type and status in response
  - Generates critical warnings if service is FAULTED
  - Reports ongoing operations (JOINING/LEAVING)

- **create_smb_share** and **create_nfs_share** - Directory service awareness
  - Dry-run mode shows warnings when a directory service is enabled (the share handlers pass their TrueNAS client; middleware types such as `ACTIVEDIRECTORY`, `LDAP`, and `IPA` are recognised, including an enabled notice for IPA)
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
{"credential_type": "KERBEROS_USER", "username": "admin@CORP.EXAMPLE.COM", "password": "********"}

// Principal from a keytab already on TrueNAS (more secure, recommended for production)
{"credential_type": "KERBEROS_PRINCIPAL", "principal": "host/truenas.corp.example.com"}
```

**LDAP Credentials:**
```json
// Plain authentication with bind DN
{"credential_type": "LDAP_PLAIN", "binddn": "cn=admin,dc=example,dc=com", "bindpw": "********"}

// Anonymous (no credentials)
{"credential_type": "LDAP_ANONYMOUS"}

// Mutual TLS with a client certificate (its name on TrueNAS)
{"credential_type": "LDAP_MTLS", "client_certificate": "ldap-client"}

// Kerberos authentication (same as Active Directory)
{"credential_type": "KERBEROS_USER", "username": "admin", "password": "********"}
{"credential_type": "KERBEROS_PRINCIPAL", "principal": "host/truenas"}
```

**IPA/FreeIPA Credentials:**
```json
// Same as Active Directory Kerberos options
{"credential_type": "KERBEROS_USER", "username": "admin", "password": "********"}
{"credential_type": "KERBEROS_PRINCIPAL", "principal": "host/truenas"}
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
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
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
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
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
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
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
  - An empty `pool` is treated as unset and returns every pool
  - Shows current scrub progress if running
  - Displays last scrub date and results
  - Lists next scheduled scrub times
  - Combines schedule and runtime info in one view
  - Perfect for "when was tank last scrubbed?"
  - A pool that is configured but not imported (`OFFLINE`, size unavailable) is still listed, with `size_bytes: null` and `size_human: "unknown"`, and does not fail the report for other pools

- **create_scrub_schedule** - Schedule automatic scrubs
  - Weekly, monthly, or custom cron schedules
  - Dry-run mode to preview configuration
  - Validates pool exists and schedule is valid
  - Recommends optimal timing (off-peak hours)
  - Best practices: Weekly for production, monthly for home use

- **run_scrub** - Manually start a scrub
  - Returns task ID for progress tracking; `poll_interval` is the task manager's poll interval
  - Starts with `pool.scrub.scrub` on every version (present on TrueNAS 25.10–27; deprecated on 26+ but still working) and tracks the job id that call returns. `zpool.scrub.run` is not used to start, because it can finish successfully without starting a scrub (HA standby, a race with a scheduled run, or the concurrent-scrub limit)
  - Job lookups still match both `pool.scrub.scrub` (string argument) and `zpool.scrub.run` (`pool_name` argument), including scheduled scrubs
  - If a start returns no job id, reports an error instead of `scrub_started: true` with `job_id` 0
  - If the last scrub is newer than the threshold (default 7 days), returns `scrub_started: false` and does not start a scrub. The threshold matches middleware: a start is due once `(threshold-1)*24h` have passed since the last finished scrub
  - Safety checks prevent double-scrubbing
  - Allows only `ONLINE` or `DEGRADED` pools; `FAULTED`, `UNAVAIL`, `REMOVED`, and `OFFLINE` are refused before any start call
  - Duration-estimate fields are left out when pool size is unknown
  - Dry-run shows estimated duration and impact, or the same status refusal
  - Integrates with task manager for monitoring
  - Use before backups or after hardware changes

- **delete_scrub_schedule** - Remove scrub schedule (**destructive**)
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
  - Dry-run shows what will be removed
  - Warns about loss of automatic scrubbing
  - Recommends manual scrub frequency
  - **WARNING**: Pool will no longer auto-scrub

## Replication and Snapshot Tasks

- **query_replications** - List replication tasks and their last run
  - Direction (`PUSH`/`PULL`), transport, source datasets, target dataset, recursion, excludes, retention policy and schedule (with a readable form)
  - `trigger` says what starts the task: its own schedule, the periodic snapshot tasks it is bound to (`bound_snapshot_tasks`), or manual only. A task bound to a periodic snapshot task ships the snapshots that task created; it does not create any
  - `last_run` has the state, time, error, warnings and `last_snapshot` (the last snapshot sent); `current_job` is the middleware job of a run in progress or the last run. `last_run.datetime` is in the NAS time zone (like the scrub tools); if that cannot be read, times are UTC and `timezone_note` says so
  - SSH connection details are never returned: no host, user, port, key pair or host key, only the keychain connection id (`ssh_connection_id`). Encryption keys and key locations, netcat addresses and property overrides are left out as well; only the `encryption` flag is shown
  - `task_id` returns one task; `limit` bounds the list (default 50, maximum 1000). When the list is cut, `total_replications` and a `note` say so
- **query_snapshot_tasks** - List periodic snapshot tasks and what their last run covered
  - Dataset, recursion, excludes, naming schema, schedule, retention, `allow_empty` and `last_snapshot` (the snapshot the task last created, which shows it ran). `last_run.datetime` is in the NAS time zone
  - For a recursive task, `coverage` lists the datasets the task covers today and which have no snapshot from the last run, for example children created after that run. A replication of the task has nothing to send for those datasets until the task runs again. Dataset lists are capped at 100 entries; the counts are complete. If no covered dataset has the snapshot, the note also says to query again, because a dataset removed while the lookup runs can empty the result
  - `bound_replications` lists the replication tasks that run after this task
  - `task_id` returns one task, `dataset` filters by exact dataset, and `limit` bounds the list (default 50, maximum 1000)
- **run_snapshot_task** - Run a periodic snapshot task now (**destructive**)
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
  - Input is `task_id` only; the task's own configuration decides what is snapshotted
  - The preview names the task, the datasets it covers today, the naming schema, retention, and the replication tasks that start right after it (bound, automatic, enabled and without their own schedule). Those replications may delete target snapshots per their retention, which is why this tool is destructive. It also says a real run does not create a job: the tool polls the snapshot task's state for up to about 2 minutes, and if the run has not finished by then it returns `QUEUED` (check `query_snapshot_tasks`)
  - Refused (no token) for a disabled task, a task whose dataset is gone, or a task that is already running
  - TrueNAS queues the run and returns no job (`pool.snapshottask.run` is not a job method on 25.10 or 27.0), so there is no `job_id` or `tasks_get` task. The tool follows the task's own state (`pool.snapshottask.query`) for up to two minutes, until a run newer than the one before the call is `FINISHED` or `ERROR`
  - When it finishes (`state: FINISHED`), reads back the task's `last_snapshot` and the snapshots on each covered dataset and returns `snapshot_name`, `new_snapshot`, `snapshots_created` (`dataset@name`) and `datasets_without_snapshot`. With `allow_empty` off, unchanged datasets get no snapshot and are listed there
  - If `last_snapshot` did not change after a `FINISHED` run, the snapshot name already existed and zettarepl treated that as success: it does so only during a DST step-back. The tool returns `new_snapshot: false` and says no new snapshot was created. At any other time an existing name is an `ERROR`, returned as a failure with the task's error text
  - An `ERROR` state is returned as an error with the task's (redacted, bounded) error text
  - If the run is not seen to finish in time, or the call ends first, returns `state: QUEUED`: the run may still be going. Check `query_snapshot_tasks task_id` (its `last_run` and `last_snapshot`) before running it again
- **run_replication** - Run a replication task now (**destructive**)
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
  - Input is `task_id` only. Sends `replication.run` with the task id (accepted by 25.10 and 27.0)
  - The preview shows direction, source, target, transport, retention and `allow_from_scratch`, and an `estimate`: for a `PUSH` task, the number of snapshots on the source matching the task's naming schemas (or `name_regex`) that are newer than the last snapshot sent. The estimate looks at one source dataset; for `PULL` tasks it is unknown (`would_send: null`)
  - If the task is bound to a periodic snapshot task, the preview, the confirmation and the result say so: the run ships snapshots that already exist and creates none
  - Refused (no token) while the task is `RUNNING` or on `HOLD`. A disabled task is refused on TrueNAS 25.10, which does not run it; on 26 and later it still runs, and the preview warns. If the version is not known, the preview explains both
  - Does not wait for the transfer: returns `job_id` and a `task_id` for `tasks_get`. When the job finishes, the task's `statusMessage` and `result` say one of:
    - "no new snapshots on <dataset>; nothing transferred" (`transferred: false`), only when the job has no progress text: TrueNAS sets it when a snapshot send starts
    - "sent N snapshot(s) across all source datasets and children, the last <dataset@snapshot>" (`transferred: true`, `snapshots_sent`, `snapshots_planned`, `last_snapshot_sent`). The count is TrueNAS's own task-wide send counter, which covers every source dataset and child of a recursive or multi-source run. With a name pattern (`name_regex`) one incremental send can carry intermediate snapshots too. `last_source_size` reports used space for the last source dataset: `target_used` is the target's used size, and `max_used` is the larger of source used and target used (middleware uses that max as the "of" value so it is never smaller than the target). This is not the amount of data sent
    - "could not determine what was transferred" (`transferred: null`) with the redacted, bounded `progress_description`, for any other progress text
- **create_snapshot** - Create a snapshot on demand
  - Input: `dataset`, `name` (the part after `@`; letters, digits, `_`, `-`, `.`, `:`), optional `recursive`
  - Dry-run mode previews the datasets the snapshot would cover
  - A write tool like `create_dataset` and `run_scrub`: it runs without a confirmation token unless `dry_run` is set
  - The preview warns when the name matches the naming schema of a periodic snapshot task on the dataset or a parent (that task's retention may remove it later, and its replications may ship it), when it matches the naming rules of a push replication whose source includes the dataset (its next run may ship it), and when the dataset is (inside) the target of a replication on this system (a manual snapshot there can make the next incremental receive fail until it is removed)
  - Refused if the dataset does not exist or a snapshot with that name already exists on any covered dataset
  - Returns the resulting snapshot name (`dataset@name`); a recursive snapshot also lists the snapshots read back on each child
  - Unless a periodic snapshot task's retention applies (see the warnings above), the snapshot stays until it is deleted
- **clone_snapshot** - Clone a snapshot to a new dataset, for example to recover files without rolling back
  - Input: `snapshot` (`dataset@name`) and `target_dataset` (a new dataset name, not a `/mnt` path). Sends `pool.snapshot.clone` with `snapshot` and `dataset_dst` only
  - A write tool: it runs without a confirmation token unless `dry_run` is set. The preview shows the source snapshot, the target and the target's mountpoint (or `/dev/zvol` device for a zvol snapshot)
  - Refused if the snapshot does not exist, the target already exists, is in another pool, is the pool root, or is a path; if the snapshot's dataset or the target is inside a dataset TrueNAS manages itself (the same rule as `rollback_snapshot`: the boot pool `boot-pool`/`freenas-boot`, `.system`, `ix-applications`, `ix-apps`, `.ix-virt` or any other hidden, dot-prefixed dataset); if the target's parent does not exist (the clone does not create parents), is a zvol, is locked, or is not mounted at or below `/mnt/<pool>`; or if the source dataset is locked
  - While the clone exists the snapshot cannot be deleted
- **hold_snapshot** - Place a hold on a snapshot so it cannot be deleted
  - Input: `snapshot` (`dataset@name`). Sends `pool.snapshot.hold` with `{"recursive": false}`; TrueNAS adds the tag `truenas`
  - The preview shows the snapshot and its current holds. If the `truenas` hold is already there, nothing is sent and the result says `already_held: true`
  - While held, a manual delete and periodic snapshot task or replication retention fail until the hold is released
- **release_snapshot** - Release the `truenas` hold on a snapshot (**destructive**)
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
  - Input: `snapshot` (`dataset@name`). Sends `pool.snapshot.release` with `{"recursive": false}`, which removes every hold on the snapshot, so it only runs when `truenas` is the only one
  - The preview and confirmation say the snapshot becomes deletable again, name the hold, and show any retention that schedules its removal
  - Refused (no token) if the snapshot has no holds, has a hold with another tag, has more holds (`userrefs`) than tags listed, or its hold count cannot be read; other holds must be managed outside this tool
  - The token is bound to the previewed holds, hold count and `createtxg`: a hold added or the snapshot recreated after the preview rejects it
- **rollback_snapshot** - Roll a dataset back to one of its snapshots (**destructive**)
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
  - Input: `snapshot` (`dataset@name`), optional `allow_destroy_newer` and `destroy_clones`. Sends `pool.snapshot.rollback` with explicit options: `recursive` only when newer snapshots are destroyed, `recursive_clones` only when their clones are, and `force` and `recursive_rollback` always false
  - Everything written to the dataset since the snapshot is lost. Only the named dataset is rolled back, not its children
  - By default the snapshot must be the dataset's newest. Rolling back to an older one needs `allow_destroy_newer: true`, and the preview lists every newer snapshot that is destroyed (newer bookmarks are destroyed too)
  - Refused (no token) if a newer snapshot is held or its hold count cannot be read; if a newer snapshot has clones and `destroy_clones` is not set; if a clone to destroy has snapshots or child datasets of its own, or is a hidden dataset; if a flag is set that the rollback does not need; for a pool's root dataset (rolling it back also reverts the ownership and permissions of `/mnt/<pool>`); for datasets TrueNAS manages itself, using the 25.10 middleware's internal-dataset rule (the boot pool `boot-pool`/`freenas-boot`, `.system`, `ix-applications`, `ix-apps`, `.ix-virt`) plus any other hidden (dot-prefixed) dataset; and for any dataset `query_datasets` does not list. Clones are found from both the clones' `origin` and the snapshots' `clones` property, and `destroy_clones` lists them in the preview
  - The token is bound to the target and every newer snapshot (names, `createtxg`, holds and clones): a newer snapshot taken, deleted or recreated, the target recreated, a hold added or a clone created after the preview rejects it
  - A newer bookmark makes ZFS refuse a rollback to the newest snapshot; the error says so, and the tool does not destroy the bookmark
  - A snapshot or clone created between the final re-plan and the call is destroyed by `allow_destroy_newer` or `destroy_clones` without being previewed; the middleware has no compare-and-swap to prevent it
  - Snapshot lists use the names-only `pool.snapshot.query` form filtered by dataset (with `extra.min_txg`), and each snapshot's details a single `id =` query, so no call lists every snapshot on the system
  - The result lists the snapshots and clones destroyed and confirms the target is now the newest snapshot
- **delete_snapshot** - Delete a snapshot permanently (**destructive**)
  - **Requires a `confirmation_token`** to execute; without one returns a preview and a token and changes nothing
  - Input: `snapshot` (`dataset@name`), optional `recursive` and `defer_if_cloned`. Sends `pool.snapshot.delete` with the snapshot id; `recursive` is sent only when a child dataset has a snapshot of the same name, and `defer` only with `defer_if_cloned`
  - `recursive: true` also deletes the snapshot of the same name on every child dataset; the preview lists every affected snapshot, and each is checked for holds and clones
  - Refused (no token) if any affected snapshot is held or its hold count cannot be read, if any has clones and `defer_if_cloned` is not set, if `defer_if_cloned` is set but there are no clones, or if the snapshot (or, for a recursive delete, a child snapshot) is of a dataset TrueNAS manages itself (the boot pool, `.system`, `ix-applications`, `ix-apps`, `.ix-virt` or another hidden dataset) or of a dataset `query_datasets` does not list
  - With `defer_if_cloned: true` a snapshot with clones is only marked for deferred deletion: ZFS deletes it when its last clone is destroyed, and the clones are not changed. The preview lists the clones; the result reports the snapshots that still have clones as `pending_deletion`, and any other snapshot still present under `remaining`
  - The preview warns when a periodic snapshot task's retention manages a snapshot, and that deleting the last snapshot shared with a replication target can force a full resend
  - The token is bound to each affected snapshot's `createtxg`, holds and clones: a snapshot recreated, held or cloned after the preview rejects it
  - With `recursive`, a child snapshot of the same name created between the final re-plan and the call is deleted without being previewed; the middleware has no compare-and-swap to prevent it

## Network Configuration

- **get_network_config** - Global network settings, configured and applied
  - Hostname, domain, IPv4 and IPv6 gateways and nameservers as saved, next to what is in effect (`applied`, from `state`)
  - `gateway_status` per family: `applied`, `not_applied`, `dynamic` (a default route from DHCP or router advertisements, no static gateway) or `none`
  - `default_routes` from the routing table, and `connection_local_ip`, the TrueNAS address this server is connected to
- **query_network_interfaces** - Interfaces with configured aliases and the addresses actually on them
  - Each applied address is marked `static`, `dynamic` (DHCP or SLAAC) or `link_local`; `this_server_connection` marks the interface this server uses
  - `name` returns one interface. `pending_changes` shows the check-in state when the key has the role to read it
- **get_pending_network_changes** - Whether interface changes are staged or waiting for check-in, `remaining_seconds`, and the outcome of the last change this server made
- **update_network_config** - Change gateways, nameservers, hostname or domain (**destructive**)
  - **Requires a `confirmation_token`**. Sends only the values that change
  - Applies immediately with no automatic rollback; the preview lists the previous values
  - An empty string clears a gateway or nameserver. A link-local IPv6 gateway is given without a zone, e.g. `fe80::1`; the `%<interface>` form is refused because TrueNAS rejects it. After the update the reply says per changed gateway whether it is applied or only saved (`status: saved_not_applied`)
  - Warns when the gateway is outside every prefix on the system, when SLAAC is on (the static gateway replaces the router-advertised route), and when the change affects this server's own connection
  - Refused while interface changes are pending
- **update_network_interface** - Add or remove static IPv4/IPv6 aliases and turn `ipv6_auto` on or off (**destructive**)
  - **Requires a `confirmation_token`**. Aliases are CIDR (`2001:db8:1::10/64`); link-local and multicast addresses are refused, and SLAAC or DHCP addresses cannot be removed
  - Stages and commits the change with a check-in window (`checkin_timeout_seconds`, default 60). Without `checkin_network_changes` in time, TrueNAS rolls it back
  - Returns `awaiting_checkin`, `remaining_seconds`, `rollback_at` and the addresses now on the interface
  - Refused when changes are already pending or when it would remove this server's connection address. See [Network check-in window](safety.md#network-check-in-window)
- **checkin_network_changes** - Keep a committed interface change before the window ends; refused when nothing is waiting or the waiting change was not made by this server
- **rollback_network_changes** - Revert pending interface changes now, committed or only staged

Example: add a static IPv6 address next to SLAAC and set a default gateway.

1. `update_network_interface` with `interface: "eth0"` and `add_aliases: ["2001:db8:1::10/64"]`, then confirm
2. `query_network_interfaces name=eth0`: the new address is listed as `static` next to the `dynamic` SLAAC address
3. `checkin_network_changes` within the window
4. `update_network_config` with `ipv6gateway: "2001:db8:1::1"`, then confirm
5. `get_network_config`: `gateway_status.ipv6.status` is `applied` and `default_routes` lists the gateway

## Task Management

For long-running operations like app upgrades, system updates, scrubs, and replications:

- **tasks_list** - List running tasks, and finished ones for 24 hours after they finish
- **tasks_get** - Get the status of a specific task by ID
  - Automatic background polling of TrueNAS job status
  - Tasks update automatically without manual polling
  - Each task shows `tool`, `job_id` and, once the job succeeded, a bounded `result` (redacted, then cut to at most 4 KiB; `result_truncated` and `result_size_bytes` when cut). Arguments are never returned
  - The job's method and start time are checked on every poll, because job ids restart after a middleware restart or reboot. A job that is gone, or whose id now belongs to another job, fails the task with a message naming the tool to check the state with (`apply_update`: `system_info` and `update_status`)
  - Repeated poll failures are reported in `statusMessage` and logged; the task keeps working
  - A task still working when its `ttl` runs out fails with "tracking timed out"
  - Finished tasks are kept for 24 hours, at most 500 (the ones that finished first are removed early)
  - These are ordinary tools, not the MCP tasks extension
