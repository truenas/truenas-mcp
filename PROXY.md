# TrueNAS MCP Proxy

The `truenas-mcp-proxy` is a desktop client that bridges Claude Desktop's stdio interface to the TrueNAS MCP server's SSE transport. This eliminates the need for SSH while maintaining secure API key authentication.

## Architecture

```
┌─────────────────┐
│ Claude Desktop  │
└────────┬────────┘
         │ stdio (JSON-RPC)
         │
┌────────▼─────────────────────────┐
│   truenas-mcp-proxy (desktop)    │
│   - Reads stdin                  │
│   - POSTs to /messages           │
│   - Listens to SSE stream        │
│   - Writes to stdout             │
└──────────────┬───────────────────┘
               │ HTTP/SSE + Bearer token
               │
┌──────────────▼───────────────────┐
│  truenas-mcp (TrueNAS)           │
│  - GET /sse (SSE stream)         │
│  - POST /messages (requests)     │
└──────────────────────────────────┘
```

## Installation

### macOS

```bash
# For Apple Silicon (M1/M2/M3)
cp truenas-mcp-proxy-darwin-arm64 /usr/local/bin/truenas-mcp-proxy

# For Intel Macs
cp truenas-mcp-proxy-darwin-amd64 /usr/local/bin/truenas-mcp-proxy

# Make executable
chmod +x /usr/local/bin/truenas-mcp-proxy
```

### Linux

```bash
cp truenas-mcp-proxy-linux-amd64 /usr/local/bin/truenas-mcp-proxy
chmod +x /usr/local/bin/truenas-mcp-proxy
```

### Windows

Copy `truenas-mcp-proxy-windows-amd64.exe` to a directory in your PATH, or reference it directly in Claude Desktop config.

## Configuration

### Claude Desktop Setup

Edit your Claude Desktop configuration file:
- **macOS**: `~/Library/Application Support/Claude/claude_desktop_config.json`
- **Windows**: `%APPDATA%\Claude\claude_desktop_config.json`
- **Linux**: `~/.config/Claude/claude_desktop_config.json`

Add the TrueNAS MCP server:

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

### Environment Variables (More Secure)

Instead of putting the API key in the config file, use environment variables:

```json
{
  "mcpServers": {
    "truenas": {
      "command": "truenas-mcp-proxy",
      "args": [
        "--server-url", "http://192.168.0.31:8089"
      ],
      "env": {
        "TRUENAS_MCP_API_KEY": "your-secure-key-here"
      }
    }
  }
}
```

## Command-Line Options

### Required Flags

- `--server-url` - URL of TrueNAS MCP server (e.g., `http://192.168.0.31:8089`)
  - Alternative: `TRUENAS_MCP_SERVER_URL` environment variable
- `--api-key` - API key for authentication
  - Alternative: `TRUENAS_MCP_API_KEY` environment variable

### Optional Flags

- `--timeout` - Request timeout duration (default: `30s`)
  - Examples: `10s`, `1m`, `500ms`
- `--debug` - Enable verbose debug logging to stderr
  - Useful for troubleshooting connection issues
- `--insecure` - Skip TLS certificate verification
  - Only use for development with self-signed certificates
  - NOT recommended for production
- `--version` - Print version and exit

## Usage Examples

### Basic Usage

```bash
truenas-mcp-proxy \
  --server-url http://192.168.0.31:8089 \
  --api-key my-secret-key
```

### With Environment Variables

```bash
export TRUENAS_MCP_SERVER_URL=http://192.168.0.31:8089
export TRUENAS_MCP_API_KEY=my-secret-key
truenas-mcp-proxy
```

### With Debug Logging

```bash
truenas-mcp-proxy \
  --server-url http://192.168.0.31:8089 \
  --api-key my-secret-key \
  --debug
```

### Testing Manually

You can test the proxy manually with JSON-RPC requests:

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"test","version":"1.0"}}}' | \
  truenas-mcp-proxy \
    --server-url http://192.168.0.31:8089 \
    --api-key testkey
```

## How It Works

### Request Flow

1. Claude Desktop sends JSON-RPC request to proxy via stdin
2. Proxy POSTs request to TrueNAS server's `/messages` endpoint
3. Server processes request and broadcasts response via SSE
4. Proxy receives response on SSE stream
5. Proxy correlates response by ID and writes to stdout
6. Claude Desktop receives response

### Connection Management

- Proxy connects to server's `/sse` endpoint on startup
- Uses `r3labs/sse` library with automatic reconnection
- Maintains request/response correlation across reconnects
- Requests timeout after 30s (configurable) if no response

### Error Handling

- **Connection failures**: Automatic reconnection with backoff
- **Timeout**: Returns JSON-RPC error after timeout period
- **Invalid JSON**: Returns parse error to Claude Desktop
- **Server errors**: Forwards error response from server
- **Stdin EOF**: Graceful shutdown

## Security Considerations

### API Key Protection

- Store API keys in environment variables, not config files
- Never commit API keys to version control
- Use strong, randomly generated keys (e.g., `openssl rand -hex 32`)
- Rotate keys periodically

### TLS/HTTPS

For production deployments:

1. Use HTTPS URLs for `--server-url`
2. Run TrueNAS server behind reverse proxy with valid TLS certificate
3. Only use `--insecure` flag for local development

Example nginx config for TLS termination:

```nginx
server {
    listen 443 ssl http2;
    server_name truenas.example.com;

    ssl_certificate /etc/ssl/certs/truenas.crt;
    ssl_certificate_key /etc/ssl/private/truenas.key;

    location / {
        proxy_pass http://127.0.0.1:8089;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_buffering off;
        proxy_cache off;
    }
}
```

### Network Security

- Use firewall rules to restrict access to TrueNAS server port
- Consider VPN or SSH tunnel for remote access
- Monitor server logs for suspicious activity

## Troubleshooting

### Proxy Won't Connect

```bash
# Test with debug logging
truenas-mcp-proxy \
  --server-url http://192.168.0.31:8089 \
  --api-key testkey \
  --debug
```

Check for:
- Network connectivity to server
- Correct server URL and port
- Server is running and listening
- Firewall rules allowing connection

### Authentication Errors

Verify:
- API key matches server configuration
- Authorization header is being sent
- Server logs show authentication attempts

### Timeout Errors

Increase timeout:
```bash
truenas-mcp-proxy \
  --server-url http://192.168.0.31:8089 \
  --api-key testkey \
  --timeout 60s
```

Check:
- Server is responding to requests
- Network latency is not excessive
- Server logs for processing errors

### Claude Desktop Integration Issues

1. Verify proxy binary is executable and in PATH
2. Check Claude Desktop logs for errors
3. Test proxy manually (see Usage Examples)
4. Restart Claude Desktop after config changes

### Server Connection Test

Test server connectivity without proxy:

```bash
# Test SSE endpoint
curl -N -H "Authorization: Bearer testkey" \
  http://192.168.0.31:8089/sse

# Test messages endpoint
curl -X POST \
  -H "Authorization: Bearer testkey" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  http://192.168.0.31:8089/messages
```

## Development

### Building from Source

```bash
# Build for current platform
make build-proxy

# Build for all platforms
make build-proxy-all
```

### Running Tests

```bash
go test ./proxy/...
```

### Project Structure

```
cmd/truenas-mcp-proxy/
  main.go              # Entry point
proxy/
  config.go            # Configuration management
  stdio.go             # Stdin/stdout handling
  proxy.go             # Main proxy logic
mcp/
  sse_client.go        # SSE client implementation
  types.go             # Shared types
```

## Comparison with SSH Method

### Proxy Method (Recommended)

**Pros:**
- No SSH required
- Simpler Claude Desktop configuration
- Direct HTTP/SSE connection
- Better error handling
- Automatic reconnection

**Cons:**
- Requires separate binary installation
- Network firewall configuration needed

### Direct SSE Method (Local Only)

**Pros:**
- No proxy binary needed
- Simplest setup for local TrueNAS

**Cons:**
- Only works with local TrueNAS
- Claude Desktop must support SSE transport
- No request correlation for multi-client scenarios

## Future Enhancements

- Connection pooling for improved performance
- Request retry with exponential backoff
- Structured logging with levels
- Metrics/observability endpoints
- Configuration file support (`~/.truenas-mcp-proxy.yaml`)
- Homebrew formula for easy installation on macOS
- Windows installer package
