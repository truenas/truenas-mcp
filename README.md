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

## Features

TrueNAS MCP provides comprehensive management capabilities through natural language:

### Core Categories
- 📊 **Monitoring** - System info, health, alerts, performance metrics
- 💾 **Storage** - Pools, datasets, snapshots, shares (SMB/NFS)
- 🖥️ **Virtualization** - VM management and status
- 📈 **Capacity Planning** - Utilization analysis and trend projections
- 🔄 **Maintenance** - System updates, boot environments, pool scrubs
- 📦 **Applications** - App status and upgrades
- ⚙️ **Tasks** - Long-running operation tracking

### Key Capabilities
- **Intelligent Filtering & Sorting** - Query datasets, snapshots, VMs with smart filters
- **Dry-Run Mode** - Preview changes before execution for all write operations
- **Task Tracking** - Automatic progress monitoring for updates, upgrades, and scrubs
- **Safety Checks** - Built-in validation prevents dangerous operations
- **Natural Language** - Ask questions in plain English, get actionable insights

📖 **[View complete feature list →](docs/full-features.md)**

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

Once connected via an MCP client, ask questions in natural language:

### Quick Start Examples

**Monitoring:**
- "What version of TrueNAS is running?"
- "Are there any system alerts?"
- "Show me CPU and memory usage over the past day"

**Storage:**
- "Show me all storage pools and their health status"
- "What are the top 10 datasets using the most space?"
- "List snapshots for the tank/shares/data dataset"

**Maintenance:**
- "Check if there are any TrueNAS system updates available"
- "What's the scrub status of my pools?"
- "Show me boot environments that are safe to delete"

**Management:**
- "Create a new dataset for file sharing"
- "Set up a weekly scrub schedule for tank on Sunday at 2am"
- "Upgrade the plex app to the latest version"

💬 **[View complete example queries →](docs/examples.md)**

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
