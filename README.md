# TrueNAS MCP Server

A Model Context Protocol (MCP) server for TrueNAS that enables AI models to interact with the TrueNAS API using natural language queries over HTTP/SSE.

## Features

Read-only tools for common TrueNAS operations:

- **system_info** - Get system information (version, hostname, platform)
- **system_health** - Check system health and alerts
- **query_pools** - Query storage pools with status and capacity
- **query_datasets** - Query datasets with optional pool filtering
- **query_shares** - Query SMB and NFS share configurations
- **get_system_metrics** - Get CPU, memory, and load performance metrics
- **get_network_metrics** - Get network interface traffic metrics
- **get_disk_metrics** - Get disk I/O performance metrics
- **query_apps** - List installed applications with status and available updates

Write operations (requires confirmation):

- **upgrade_app** - Upgrade an application to a newer version with optional snapshot backup

## Architecture

This project provides two binaries:

1. **truenas-mcp** - Server running on TrueNAS
   - **Transport**: Server-Sent Events (SSE) over HTTP/HTTPS
   - **Protocol**: JSON-RPC 2.0 following MCP specification
   - **TrueNAS Client**: WebSocket over Unix socket to middleware
   - **Security**: API key authentication, CORS support

2. **truenas-mcp-proxy** - Desktop proxy for Claude Desktop
   - Bridges stdio (JSON-RPC) to SSE transport
   - Runs on user's desktop, connects to remote TrueNAS server
   - No SSH required - uses HTTP/SSE with API key authentication

## Building

### Server (runs on TrueNAS)

```bash
# Download dependencies
go mod download

# Build for local platform
make build

# Cross-compile for Linux x86_64 (TrueNAS)
make build-linux
```

### Proxy (runs on desktop)

```bash
# Build for current platform
make build-proxy

# Build for all platforms (macOS, Linux, Windows)
make build-proxy-all
```

## Installation & Deployment

### Step 1: Deploy to TrueNAS

#### Build the binary locally

```bash
# Build the TrueNAS server binary for Linux
make build-linux
```

This creates `truenas-mcp` binary compiled for Linux x86_64.

#### Deploy to TrueNAS

```bash
# Stop the service if already running
ssh root@your-truenas 'systemctl stop truenas-mcp'

# Copy the binary to TrueNAS
scp truenas-mcp root@your-truenas:/usr/local/bin/truenas-mcp

# Set executable permissions
ssh root@your-truenas 'chmod +x /usr/local/bin/truenas-mcp'

# Copy the systemd service file
scp truenas-mcp.service root@your-truenas:/etc/systemd/system/

# Reload systemd and enable the service
ssh root@your-truenas 'systemctl daemon-reload'
ssh root@your-truenas 'systemctl enable truenas-mcp'
ssh root@your-truenas 'systemctl start truenas-mcp'

# Verify the service is running
ssh root@your-truenas 'systemctl status truenas-mcp'
```

#### Configure the service

Edit the service file on TrueNAS to set your API key and listen address:

```bash
ssh root@your-truenas
vi /etc/systemd/system/truenas-mcp.service
```

Update the `ExecStart` line to include your desired configuration:

```ini
ExecStart=/usr/local/bin/truenas-mcp -listen 0.0.0.0:8089 -api-key your-secure-key-here
```

Then reload and restart:

```bash
systemctl daemon-reload
systemctl restart truenas-mcp
```

### Step 2: Setup Claude Desktop Proxy

#### Install the proxy binary

Choose the appropriate binary for your platform:

**macOS (Apple Silicon):**
```bash
sudo cp truenas-mcp-proxy-darwin-arm64 /usr/local/bin/truenas-mcp-proxy
sudo chmod +x /usr/local/bin/truenas-mcp-proxy
```

**macOS (Intel):**
```bash
sudo cp truenas-mcp-proxy-darwin-amd64 /usr/local/bin/truenas-mcp-proxy
sudo chmod +x /usr/local/bin/truenas-mcp-proxy
```

**Linux:**
```bash
sudo cp truenas-mcp-proxy-linux-amd64 /usr/local/bin/truenas-mcp-proxy
sudo chmod +x /usr/local/bin/truenas-mcp-proxy
```

**Windows:**
```powershell
copy truenas-mcp-proxy-windows-amd64.exe C:\Windows\System32\truenas-mcp-proxy.exe
```

#### Configure Claude Desktop

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
      "command": "truenas-mcp-proxy",
      "args": [
        "--server-url", "http://YOUR-TRUENAS-IP:8089",
        "--api-key", "your-secure-key-here"
      ]
    }
  }
}
```

Replace `YOUR-TRUENAS-IP` with your TrueNAS IP address and `your-secure-key-here` with the API key you configured in the systemd service.

#### Restart Claude Desktop

Quit Claude Desktop completely and restart it. The MCP server connection will be established automatically.

### Step 3: Verify the Connection

In Claude Desktop, you should now be able to ask TrueNAS questions:

- "What version of TrueNAS is running?"
- "Show me all storage pools and their health"
- "List all datasets"
- "What shares are configured?"

You can verify the server is running on TrueNAS:

```bash
# Check service status
ssh root@your-truenas 'systemctl status truenas-mcp'

# View logs
ssh root@your-truenas 'journalctl -u truenas-mcp -f'

# Test the health endpoint
curl http://YOUR-TRUENAS-IP:8089/health
```

## Running

### Command-line options

```bash
# Start with defaults (localhost:8080, no auth)
./truenas-mcp

# Specify listen address and API key
./truenas-mcp -listen 0.0.0.0:8443 -api-key your-secret-key

# Or use environment variables
TRUENAS_MCP_API_KEY=your-secret-key ./truenas-mcp -listen :8080

# Custom TrueNAS socket path
TRUENAS_SOCKET=/custom/path/middleware.sock ./truenas-mcp
```

### Flags

- `-listen` - Listen address (default: `localhost:8080`)
- `-api-key` - API key for authentication (can also use `TRUENAS_MCP_API_KEY` env var)
- `-version` - Print version and exit

## MCP Client Configuration

### Option 1: Direct SSE Connection (local TrueNAS only)

If TrueNAS is running locally, you can connect directly via SSE:

```json
{
  "mcpServers": {
    "truenas": {
      "url": "http://localhost:8080/sse",
      "headers": {
        "Authorization": "Bearer your-api-key-here"
      }
    }
  }
}
```

### Option 2: Proxy Client (recommended for remote TrueNAS)

For remote TrueNAS servers, use the proxy binary:

1. Install the proxy binary:
   ```bash
   # macOS (arm64)
   cp truenas-mcp-proxy-darwin-arm64 /usr/local/bin/truenas-mcp-proxy
   chmod +x /usr/local/bin/truenas-mcp-proxy

   # macOS (amd64)
   cp truenas-mcp-proxy-darwin-amd64 /usr/local/bin/truenas-mcp-proxy
   chmod +x /usr/local/bin/truenas-mcp-proxy

   # Linux
   cp truenas-mcp-proxy-linux-amd64 /usr/local/bin/truenas-mcp-proxy
   chmod +x /usr/local/bin/truenas-mcp-proxy
   ```

2. Configure Claude Desktop (`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS):
   ```json
   {
     "mcpServers": {
       "truenas": {
         "command": "truenas-mcp-proxy",
         "args": [
           "--server-url", "http://192.168.0.31:8089",
           "--api-key", "your-secure-key-here"
         ]
       }
     }
   }
   ```

3. Restart Claude Desktop

### Proxy Configuration Options

The proxy supports these flags:
- `--server-url` - Remote server URL (required, or use `TRUENAS_MCP_SERVER_URL` env var)
- `--api-key` - Authentication key (required, or use `TRUENAS_MCP_API_KEY` env var)
- `--timeout` - Request timeout (default: 30s)
- `--debug` - Enable verbose logging
- `--insecure` - Skip TLS verification (for self-signed certs, not recommended)
- `--version` - Print version and exit

For more details, see [PROXY.md](PROXY.md).

## Security

- **Authentication**: Required via API key in `Authorization: Bearer <key>` header
- **Bind Address**: Default is `localhost:8080` (local-only). Use `0.0.0.0:8080` for network access
- **TLS**: Not built-in. Use reverse proxy (nginx, caddy) for HTTPS
- **Read-only**: All current tools are read-only queries

### Recommended Production Setup

1. Run behind reverse proxy with TLS
2. Use strong API key
3. Enable firewall rules
4. Run as non-root user (if middleware socket permits)
5. Monitor logs via systemd journal

## API Endpoints

- `GET /sse` - SSE stream for server-to-client messages
- `POST /messages` - Client-to-server messages (JSON-RPC)
- `GET /health` - Health check endpoint (no auth required)

## Example Usage

Once connected via an MCP client:

**System Information:**
- "What version of TrueNAS is running?"
- "Are there any system alerts?"

**Storage:**
- "Show me all storage pools and their health status"
- "List all datasets in the tank pool"
- "What SMB shares are configured?"

**Performance:**
- "Show me CPU and memory usage over the past day"
- "What's the network traffic on the main interface?"
- "Show me disk I/O metrics for the past week"

**Applications:**
- "What apps are installed and running?"
- "Are there any app updates available?"
- "Upgrade the plex app to the latest version"

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
