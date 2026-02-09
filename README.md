# TrueNAS MCP Server

> **⚠️ Research Preview**
> This project is in active development and released as a research preview. APIs and features may change. Not recommended for production use.

A Model Context Protocol (MCP) server for TrueNAS that enables AI models to interact with the TrueNAS API using natural language queries.

## Table of Contents

- [Features](#features)
- [Architecture](#architecture)
- [Building](#building)
- [Installation](#installation)
  - [Step 1: Download or Build Binary](#step-1-download-or-build-binary)
  - [Step 2: Get TrueNAS API Key](#step-2-get-truenas-api-key)
  - [Step 3: Configure Your MCP Client](#step-3-configure-your-mcp-client)
    - [Claude Desktop](#claude-desktop)
    - [Claude Code](#claude-code)
  - [Step 4: Restart Your MCP Client](#step-4-restart-your-mcp-client)
  - [Step 5: Verify the Connection](#step-5-verify-the-connection)
- [Command-Line Options](#command-line-options)
- [Connection Details](#connection-details)
- [Security](#security)
- [Example Usage](#example-usage)
- [Advanced Features](#advanced-features)
  - [MCP Tasks for Long-Running Operations](#mcp-tasks-for-long-running-operations)
  - [Dry-Run Mode for Write Operations](#dry-run-mode-for-write-operations)
- [Limitations](#limitations)
- [Development](#development)
- [Next Steps](#next-steps)

## Features

Read-only tools for common TrueNAS operations:

- **system_info** - Get system information (version, hostname, platform)
- **system_health** - Check system health including alerts, active jobs, and capacity warnings
- **query_jobs** - Query system jobs (running, pending, or completed tasks like replication, snapshots, scrubs)
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
- **query_vms** - Query virtual machines with intelligent filtering and sorting
  - Returns simplified VM information with resource allocation, status, and device summary
  - Filter by VM name (partial match), state (RUNNING/STOPPED), or autostart setting
  - Sort by name (default, alphabetical), memory usage, or status (running first)
  - Shows CPU/memory config, bootloader, devices (disks, NICs, displays), and current state
  - Automatically excludes sensitive data like display passwords for security
  - Perfect for questions like "what VMs are running?" or "show VMs with autostart enabled"
- **query_shares** - Query SMB and NFS share configurations
- **list_alerts** - List system alerts with filtering
- **dismiss_alert** / **restore_alert** - Manage system alerts
- **get_system_metrics** - Get CPU, memory, and load performance metrics
- **get_network_metrics** - Get network interface traffic metrics
- **get_disk_metrics** - Get disk I/O performance metrics
- **query_apps** - List installed applications with status and available updates

Capacity planning and analysis tools:

- **analyze_capacity** - Comprehensive capacity analysis with historical trends and projections
  - CPU, memory, network, and disk I/O utilization analysis
  - Current, average, and peak utilization percentages
  - Trend detection (increasing/stable/decreasing)
  - Capacity status warnings (healthy/warning/critical at 70%/85% thresholds)
  - Growth projections when metrics are trending upward
  - Time ranges: HOUR, DAY, WEEK, MONTH, YEAR
  - Overall recommendations based on all metrics

- **get_pool_capacity_details** - Detailed pool and dataset capacity information
  - Current pool capacity (total, used, available bytes)
  - Utilization percentages for each pool
  - Per-dataset breakdown with capacity metrics
  - Capacity status warnings (healthy/warning/critical)
  - Note: Historical pool capacity trends not available in TrueNAS API (limitation documented)

Write operations (requires confirmation):

- **create_dataset** - Create ZFS datasets for storage (reusable for all protocols)
  - Create filesystems or volumes (for iSCSI/VMs)
  - Share type optimization (SMB, NFS, MULTIPROTOCOL, APPS)
  - Encryption with auto-generated keys or passphrases
  - Compression (LZ4, ZSTD, GZIP), quotas, and ACL configuration
  - Dry-run mode to preview before creating
  - Wizard-style guidance for SMB/NFS/iSCSI setup

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

- **upgrade_app** - Upgrade an application to a newer version with optional snapshot backup
  - Supports dry-run mode to preview changes before execution
  - Returns a task ID for tracking long-running operations

System update and maintenance operations:

**Recommended System Update Workflow**:

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

**Available Tools**:

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

Boot environment management:

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

Pool scrub management:

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

Task management tools (for long-running operations):

- **tasks_list** - List all active and recent tasks
- **tasks_get** - Get detailed status of a specific task by ID
  - Automatic background polling of TrueNAS job status
  - Tasks update automatically without manual polling

## Architecture

**Single native binary** that runs on your desktop and connects directly to TrueNAS:

```
┌──────────────────┐
│  Claude Desktop  │
└────────┬─────────┘
         │ stdio (JSON-RPC)
┌────────▼───────────────────┐
│  truenas-mcp               │ (Your Desktop)
│  - stdio interface         │
│  - Tool registry           │
│  - WebSocket client        │
└────────┬───────────────────┘
         │ Secure WebSocket (wss://)
         │ + TrueNAS API key auth
┌────────▼──────────────────┐
│  TrueNAS Middleware       │
│  - WebSocket HTTPS endpoint│
│  - Port 443 (wss)          │
└───────────────────────────┘
```

**Key Benefits:**
- ✅ No deployment to TrueNAS required
- ✅ Runs entirely on your desktop
- ✅ Secure WebSocket connection (wss://) to TrueNAS middleware
- ✅ Self-signed certificate support (works with TrueNAS defaults)
- ✅ Cross-platform support (macOS, Linux, Windows)
- ✅ Simple configuration with hostname or full WebSocket URL
- ✅ API key protection (requires encrypted connections)

## Building

```bash
# Download dependencies
go mod download

# Build for local platform
make build

# Build for all platforms (macOS, Linux, Windows)
make build-all
```

## Installation

### Step 1: Download or Build Binary

Choose the appropriate binary for your platform:

**macOS (Apple Silicon):**
```bash
sudo cp truenas-mcp-darwin-arm64 /usr/local/bin/truenas-mcp
sudo chmod +x /usr/local/bin/truenas-mcp
```

**macOS (Intel):**
```bash
sudo cp truenas-mcp-darwin-amd64 /usr/local/bin/truenas-mcp
sudo chmod +x /usr/local/bin/truenas-mcp
```

**Linux:**
```bash
sudo cp truenas-mcp-linux-amd64 /usr/local/bin/truenas-mcp
sudo chmod +x /usr/local/bin/truenas-mcp
```

**Windows:**
```powershell
copy truenas-mcp-windows-amd64.exe C:\Windows\System32\truenas-mcp.exe
```

### Step 2: Get TrueNAS API Key

1. Log into your TrueNAS web interface
2. Go to **System Settings → API Keys**
3. Click **Add** to create a new API key
4. Give it a name (e.g., "Claude Desktop MCP")
5. Make sure it has appropriate permissions (admin recommended)
6. **Copy the API key** - you'll need it for configuration

### Step 3: Configure Your MCP Client

#### Claude Desktop

Edit your Claude Desktop configuration file:

**macOS:**
```bash
vi ~/Library/Application\ Support/Claude/claude_desktop_config.json
```

**Linux:**
```bash
vi ~/.config/Claude/claude_desktop_config.json
```

**Windows:**
```
%APPDATA%\Claude\claude_desktop_config.json
```

Add the TrueNAS MCP server configuration:

```json
{
  "mcpServers": {
    "truenas": {
      "command": "truenas-mcp",
      "args": [
        "--truenas-url", "truenas.local",
        "--api-key", "your-api-key-here"
      ]
    }
  }
}
```

**Configuration options:**

**Option 1: Hostname (automatically uses wss://):**
```json
"args": [
  "--truenas-url", "192.168.0.31",
  "--api-key", "your-api-key-here"
]
```

**Option 2: Full WebSocket URL (explicit protocol):**
```json
"args": [
  "--truenas-url", "wss://truenas.local/websocket",
  "--api-key", "your-api-key-here"
]
```

**Option 3: Using environment variables:**
```json
{
  "mcpServers": {
    "truenas": {
      "command": "truenas-mcp",
      "env": {
        "TRUENAS_URL": "192.168.0.31",
        "TRUENAS_API_KEY": "your-api-key-here"
      }
    }
  }
}
```

#### Claude Code

Claude Code uses the `claude mcp` command to configure MCP servers:

**Add TrueNAS MCP server with hostname:**
```bash
claude mcp add truenas -- truenas-mcp --truenas-url 192.168.0.31 --api-key your-api-key-here
```

**Add with full WebSocket URL:**
```bash
claude mcp add truenas -- truenas-mcp --truenas-url wss://truenas.local/websocket --api-key your-api-key-here
```

**Verify the configuration:**
```bash
claude mcp list
claude mcp get truenas
```

**Manage the server:**
```bash
# Remove the server
claude mcp remove truenas

# Re-add with updated configuration
claude mcp add truenas -- truenas-mcp --truenas-url 192.168.0.31 --api-key new-api-key
```

### Step 4: Restart Your MCP Client

**Claude Desktop:** Quit Claude Desktop completely and restart it.

**Claude Code:** The MCP server will be loaded automatically when you use Claude Code.

### Step 5: Verify the Connection

You should now be able to ask TrueNAS questions:

- "What version of TrueNAS is running?"
- "Show me all storage pools and their health"
- "List all datasets"
- "What shares are configured?"
- "Show me system metrics for the past hour"

## Command-Line Options

### Flags

- `--truenas-url` - TrueNAS hostname or WebSocket URL (required, or use `TRUENAS_URL` env var)
  - Hostname: `truenas.local` or `192.168.0.31` (uses `wss://` on port 443)
  - Full URL: `wss://truenas.local/websocket` (custom port/path)
  - ⚠️ **Note**: `ws://` (unencrypted) is **not allowed** - TrueNAS will revoke API keys used over unencrypted connections
- `--api-key` - TrueNAS API key for authentication (required, or use `TRUENAS_API_KEY` env var)
- `--insecure` - Skip TLS verification (not needed - self-signed certs accepted by default)
- `--debug` - Enable debug logging
- `--version` - Print version and exit

### Examples

```bash
# Basic usage with hostname
./truenas-mcp --truenas-url 192.168.0.31 --api-key your-api-key

# With full WebSocket URL
./truenas-mcp --truenas-url wss://truenas.local/websocket --api-key your-api-key

# Using environment variables
export TRUENAS_URL=192.168.0.31
export TRUENAS_API_KEY=your-api-key
./truenas-mcp

# With debug logging
./truenas-mcp --truenas-url 192.168.0.31 --api-key your-api-key --debug
```

## Connection Details

### How It Works

The binary connects directly to TrueNAS middleware's WebSocket endpoint:

1. **Uses secure WebSocket (wss://)**: Connects to `wss://your-truenas:443/websocket`
2. **Self-signed certs accepted**: Works with TrueNAS default self-signed certificates
3. **Authenticates via API key**: Uses `auth.login_with_api_key` method

### ⚠️ Security Requirement

**IMPORTANT**: TrueNAS **requires** encrypted connections (`wss://`) for API key authentication. Using unencrypted `ws://` will cause your API key to be **revoked** as a security measure. This binary defaults to `wss://` to protect your credentials.

### Troubleshooting

**Connection Issues:**
- Verify TrueNAS is accessible from your machine
- Check firewall allows port 443 (wss)
- Verify API key is valid and has admin permissions

**Authentication Failures:**
- Generate a new API key in TrueNAS System Settings → API Keys
- Ensure the key has appropriate permissions
- Check that the key wasn't accidentally truncated when copying

## Security

- **Authentication**: TrueNAS API key required for all operations
- **TLS/SSL**: Only supports wss:// (encrypted) - ws:// is rejected for security
- **Self-signed certificates**: Accepted by default (common for TrueNAS)
- **Network**: Client-only (no listening ports, all connections outbound)
- **API Key Storage**: Recommend using environment variables instead of command-line args

### Security Best Practices

1. **Always use secure WebSocket (wss://)** - enforced by default, ws:// is rejected
2. **Generate dedicated API key** for MCP use only
3. **Use environment variables** for API keys in Claude Desktop config
4. **Restrict API key permissions** to minimum required
5. **Rotate API keys periodically**

## Example Usage

Once connected via an MCP client:

**System Information:**
- "What version of TrueNAS is running?"
- "Are there any system alerts?"
- "What's the system health status?"
- "Are there any active jobs or tasks running?"

**Jobs & Tasks:**
- "Show me all running jobs"
- "Are there any replications in progress?"
- "What tasks have completed recently?"

**Storage:**
- "Show me all storage pools and their health status"
- "What are the top 10 datasets using the most space?"
- "Which datasets are encrypted?"
- "Show me datasets in the tank pool"
- "List datasets sorted by available space"
- "What's taking up space in my replications dataset?"
- "What SMB shares are configured?"

**Snapshots:**
- "Show me all snapshots in the tank pool"
- "What are the 20 most recent snapshots?"
- "List snapshots for the tank/shares/data dataset"
- "Show me snapshots that have holds"
- "What snapshots exist for my important datasets?"
- "List snapshots created by automatic snapshot tasks"

**Virtual Machines:**
- "What VMs are currently running?"
- "Show me all virtual machines"
- "List VMs that are set to autostart"
- "What's the memory allocation for my VMs?"
- "Show me details for the homeassistant VM"
- "Which VMs are stopped?"

**Performance:**
- "Show me CPU and memory usage over the past day"
- "What's the network traffic on the main interface?"
- "Show me disk I/O metrics for the past week"

**Capacity Planning:**
- "How near to CPU capacity is my TrueNAS?"
- "Analyze system capacity over the past 90 days"
- "What's my current storage pool utilization?"
- "Show me detailed capacity information for the tank pool"
- "Are there any capacity warnings I should be aware of?"
- "Based on current trends, when should I plan to expand?"

**Applications:**
- "What apps are installed and running?"
- "Are there any app updates available?"
- "Upgrade the plex app to the latest version"

**System Updates:**
- "Check if there are any TrueNAS system updates available"
- "What version of TrueNAS am I running?"
- "Download the latest TrueNAS system update"
- "What's the status of my system update?"
- "Apply the downloaded system update"
- "Apply the update and reboot the system"
- "Reboot the TrueNAS system"

**Managing Boot Environments:**
- "Which boot environments do I have?"
- "What boot environment am I currently running?"
- "Show me boot environments that are safe to delete"
- "How much space are boot environments using?"
- "Delete boot environment '23.10-MASTER-20231015-120000' (dry run first)"
- "Delete boot environment '23.10-MASTER-20231015-120000'"
- "Show me the 10 oldest boot environments"
- "Which boot environments are protected?"

**Managing Pool Scrubs:**
- "What's the scrub status of my pools?"
- "When was tank last scrubbed?"
- "Show me all scrub schedules"
- "Is there a scrub running on tank?"
- "Create a weekly scrub schedule for tank on Sunday at 2am"
- "Create a monthly scrub for flash on the 1st at 3am"
- "Start a scrub on tank"
- "Check progress of my scrub"
- "Delete the scrub schedule for flash"
- "How often should I scrub my pools?"
- "What's the recommended scrub frequency for home use?"

**Dataset Creation:**
- "Create a new dataset for file sharing"
- "Create an encrypted dataset with a 500GB quota"
- "Set up a dataset optimized for SMB shares"
- "Create a dataset in the tank pool for my documents"
- "I need a dataset with LZ4 compression for app storage"

**SMB Share Creation:**
- "Create a new SMB share for my team"
- "Set up a Time Machine share for macOS backups"
- "Create a read-only share for archives"
- "I want to share my photos with Windows clients"
- "Set up an encrypted SMB share with access restrictions"
- "Create a share that's only accessible from my local network"

**NFS Share Creation:**
- "Create an NFS share for my Linux servers"
- "Set up a read-only NFS export for backups"
- "I need an NFS share restricted to my 192.168.1.0/24 network"
- "Create an NFS share with root squashing for security"
- "Share my data directory with specific hosts only"

**Task Management:**
- "Show me all active tasks"
- "What's the status of task abc123?"
- "Check the progress of my app upgrade"

**Dry-Run Mode:**
- "Show me what would happen if I upgrade the plex app"
- "Preview the changes before upgrading nextcloud"

## Storage Maintenance Best Practices

### Understanding ZFS Scrubs

ZFS scrubs are critical maintenance operations that verify data integrity by reading all blocks and checking checksums. They detect and repair silent corruption (bit rot) before it causes data loss. Scrubs are essential for long-term data integrity, especially for archival data.

**What Scrubs Do:**
- Read all data blocks on the pool
- Verify checksums match the data
- Automatically repair any corruption found
- Detect and report hardware issues early
- Maintain ZFS's self-healing capabilities

### Scheduling Recommendations

**Frequency Guidelines:**
- **Home/personal use**: Monthly scrubs are adequate
- **Production/business**: Weekly or bi-weekly scrubs
- **Archival/cold storage**: Monthly minimum
- **Active development**: Weekly recommended
- **Timing**: Schedule during off-peak hours (2-4am typical)

**Why Frequency Matters:**
- Regular scrubs catch corruption early
- Prevents cascading failures
- Verifies redundancy is working
- Essential for detecting failing drives

### Performance Impact

**During Scrubs:**
- Scrubs add background I/O load but don't block operations
- Performance impact is typically 10-30% depending on pool activity
- Can be safely paused and resumed
- Multiple pools can scrub simultaneously (each will be slower)
- No data loss risk from interrupting a scrub

**Scrub Duration:**
- Varies by pool size, data amount, and hardware
- Typical speeds: 100-500 MB/s depending on disk type
- 10 TiB pool: 6-28 hours on modern hardware
- Large pools (50+ TiB): Can take 2-5 days

### When to Run Manual Scrubs

**Before Critical Operations:**
- Before major system upgrades or migrations
- Before critical backups
- After recovering from degraded pool state

**After Events:**
- After hardware changes or repairs
- When alerts suggest possible corruption
- If scheduled scrub was missed (system powered off)
- After extended power outage

**Regular Maintenance:**
- If you delete automated schedule, run manual scrubs monthly
- Test your backup restoration process

### Best Practices

1. **Always have a schedule**: Use `create_scrub_schedule` for automated maintenance
2. **Monitor results**: Check `get_scrub_status` after scrubs complete
3. **Address errors immediately**: Scrub errors indicate hardware problems
4. **Keep systems powered**: Ensure TrueNAS is on when scrubs are scheduled
5. **Don't skip scrubs**: Regular scrubs are insurance against data loss

## Advanced Features

### MCP Tasks for Long-Running Operations

The server implements MCP Tasks specification for operations that take time to complete (like app upgrades):

**How it works:**
1. Write operations (like `upgrade_app`) return a `task_id` instead of blocking
2. Tasks are automatically tracked in the background
3. Use `tasks_get` with the task ID to check progress
4. Tasks update automatically - no manual polling needed

**Example:**
```
User: "Upgrade the plex app"
→ Returns: {"task_id": "abc-123", "status": "working", ...}

User: "Check task abc-123"
→ Returns: {"status": "completed", "result": {...}}
```

**Task States:**
- `working` - Operation in progress
- `completed` - Operation finished successfully
- `failed` - Operation encountered an error
- `cancelled` - Operation was cancelled

### Dry-Run Mode for Write Operations

Write operations support previewing changes before execution:

**How to use:**
Add `"dry_run": true` to any write operation to preview what would happen without making changes.

**Example:**
```
Tool: upgrade_app
Args: {"app_name": "plex", "dry_run": true}

Returns:
{
  "tool": "upgrade_app",
  "current_state": {
    "name": "plex",
    "version": "1.32.5.7349",
    "state": "RUNNING"
  },
  "planned_actions": [
    {
      "step": 1,
      "description": "Stop application containers",
      "operation": "stop",
      "target": "plex"
    },
    {
      "step": 2,
      "description": "Upgrade from 1.32.5.7349 to latest",
      "operation": "upgrade",
      "target": "plex"
    },
    {
      "step": 3,
      "description": "Start application with new version",
      "operation": "start",
      "target": "plex"
    }
  ],
  "warnings": [],
  "estimated_time": {
    "min_seconds": 30,
    "max_seconds": 300,
    "note": "Time varies based on image size and network speed"
  }
}
```

**Benefits:**
- See exactly what will change before committing
- Understand prerequisites and warnings
- Get time estimates for operations
- Build confidence before making changes

## Limitations

### Pool Capacity Historical Data

The TrueNAS API (as of v26.04) does not expose historical pool capacity metrics through the reporting endpoints, despite the Netdata backend collecting this data. This means:

- ✅ **Available**: Current pool capacity snapshots
- ✅ **Available**: CPU, memory, network, disk I/O historical trends
- ❌ **Not Available**: Historical pool capacity over time
- ❌ **Not Available**: Storage growth rate calculations
- ❌ **Not Available**: Pool capacity trend projections

**Workaround**: Query `get_pool_capacity_details` periodically and track results externally to build your own trend data.

**Future**: This may be resolved in future TrueNAS releases if the `usage` chart is added to the reporting API schema.

## Development

```bash
# Run linters
make lint

# Run tests
make test

# Clean build artifacts
make clean
```

## Next Steps

- Add more read-only tools (services, network, disks)
- Implement write operations (with safety checks)
- Add API endpoint discovery tool
- TLS support (or document reverse proxy setup)
- Rate limiting
- Audit logging
