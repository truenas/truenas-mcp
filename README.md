# TrueNAS MCP Server

A Model Context Protocol (MCP) server for TrueNAS that enables AI models to interact with the TrueNAS API using natural language queries.

## Features

Read-only tools for common TrueNAS operations:

- **system_info** - Get system information (version, hostname, platform)
- **system_health** - Check system health including alerts, active jobs, and capacity warnings
- **query_jobs** - Query system jobs (running, pending, or completed tasks like replication, snapshots, scrubs)
- **query_pools** - Query storage pools with status and capacity
- **query_datasets** - Query datasets with optional pool filtering
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

- **upgrade_app** - Upgrade an application to a newer version with optional snapshot backup

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

### Step 3: Configure Claude Desktop

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
  "--api-key", "18-NoKVv1EyfStph6AGaOZPpD8nu3GLsTeEYXrRxCNXEv0oi3aHJgfFeCBgFUxx467P"
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

### Step 4: Restart Claude Desktop

Quit Claude Desktop completely and restart it. The MCP connection will be established automatically.

### Step 5: Verify the Connection

In Claude Desktop, you should now be able to ask TrueNAS questions:

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
- "List all datasets in the tank pool"
- "What SMB shares are configured?"

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
