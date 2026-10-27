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
  - [Proxies](#proxies)
- [Security](#security)
  - [Safety Controls](#safety-controls)
- [Example Usage](#example-usage)
- [Advanced Features](#advanced-features)
  - [MCP Tasks for Long-Running Operations](#mcp-tasks-for-long-running-operations)
  - [Dry-Run Mode for Write Operations](#dry-run-mode-for-write-operations)
- [Development](#development)

## Features

TrueNAS MCP provides comprehensive management capabilities through natural language:

### Core Categories
- 📊 **Monitoring** - System info, health, alerts, performance metrics
- 💾 **Storage** - Pools, datasets, snapshots, shares (SMB/NFS)
- 🖥️ **Virtualization** - VM management and status
- 🔐 **Directory Services** - Active Directory, LDAP, FreeIPA integration and health monitoring
- 📈 **Capacity Planning** - Utilization analysis and trend projections
- 🔄 **Maintenance** - System updates, boot environments, pool scrubs
- 📦 **Applications** - Catalog search, guided installation with storage setup, app management and upgrades
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
│  - Tool registry + safety  │
│  - Output redaction        │
│  - JSON-RPC 2.0 client     │
└────────┬───────────────────┘
         │ Secure WebSocket (wss://), optionally through an
         │ HTTP CONNECT or SOCKS5 proxy (see Proxies)
         │ + TrueNAS API key auth (auth.login_ex)
┌────────▼───────────────────┐
│  TrueNAS Middleware        │
│  - JSON-RPC 2.0 endpoint   │
│    /api/current            │
│  - Port 443 (wss)          │
└────────────────────────────┘
```

**Key Benefits:**
- ✅ No deployment to TrueNAS required
- ✅ Runs entirely on your desktop
- ✅ Secure WebSocket connection (wss://) to TrueNAS middleware, directly or through an HTTP/SOCKS5 proxy
- ✅ Self-signed certificate support via `--tls-ca` (works with TrueNAS defaults)
- ✅ Cross-platform support (macOS, Linux, Windows)
- ✅ Simple configuration with hostname or full WebSocket URL
- ✅ API key protection (requires encrypted connections)

## Building

**Requirements:** Go 1.27.3 or later (the `go` directive in `go.mod`), and `make` for the targets below. With the default `GOTOOLCHAIN=auto`, a Go command that supports toolchain selection downloads the required toolchain automatically; with `GOTOOLCHAIN=local`, an older Go refuses to build.

```bash
git clone https://github.com/truenas/truenas-mcp.git
cd truenas-mcp

# Download dependencies
go mod download

# Build for the local platform: writes ./truenas-mcp
make build

# Build for all platforms: writes truenas-mcp-darwin-arm64,
# truenas-mcp-darwin-amd64, truenas-mcp-linux-amd64 and
# truenas-mcp-windows-amd64.exe to the repository root
make build-all
```

Binaries report their version with `--version`. `make` takes the version from `git describe`; override it with `make build VERSION=1.2.3`. Without `make`, `go build -o truenas-mcp ./cmd/truenas-mcp` produces the same binary, with version `dev`. `make clean` removes the binaries.

## Installation

**Requirements:** TrueNAS 25.04 or later. The server uses the JSON-RPC 2.0
endpoint (`/api/current`) and `auth.login_ex`, both introduced in 25.04; there
is no fallback to the legacy `/websocket` protocol, so TrueNAS 24.10 and older
are not supported. An older server answers the handshake with HTTP 404, which
is reported as "TrueNAS 25.04 or later required".

### Step 1: Download or Build Binary

Download the archive for your platform from the
[Releases page](https://github.com/truenas/truenas-mcp/releases), together with
`checksums.txt`:

| Platform | Release asset |
|----------|---------------|
| macOS (Apple Silicon) | `truenas-mcp-darwin-arm64.tar.gz` |
| Linux (x86-64) | `truenas-mcp-linux-amd64.tar.gz` |
| Windows (x86-64) | `truenas-mcp-windows-amd64.zip` |

There is no release asset for macOS on Intel; [build it from source](#building)
(`make build`). Each archive contains one binary named `truenas-mcp-<os>-<arch>`
(with `.exe` on Windows). Verify the download, unpack it and put the binary on
your `PATH` as `truenas-mcp`:

**macOS (Apple Silicon):**
```bash
shasum -a 256 -c checksums.txt --ignore-missing
tar -xzf truenas-mcp-darwin-arm64.tar.gz
sudo cp truenas-mcp-darwin-arm64 /usr/local/bin/truenas-mcp
sudo chmod +x /usr/local/bin/truenas-mcp
```

**macOS (Intel), built from source:**
```bash
make build
sudo cp truenas-mcp /usr/local/bin/truenas-mcp
```

**Linux:**
```bash
sha256sum -c checksums.txt --ignore-missing
tar -xzf truenas-mcp-linux-amd64.tar.gz
sudo cp truenas-mcp-linux-amd64 /usr/local/bin/truenas-mcp
sudo chmod +x /usr/local/bin/truenas-mcp
```

**Windows:**
```powershell
Expand-Archive truenas-mcp-windows-amd64.zip -DestinationPath .
New-Item -ItemType Directory -Force "$env:USERPROFILE\bin" | Out-Null
copy truenas-mcp-windows-amd64.exe "$env:USERPROFILE\bin\truenas-mcp.exe"
```
Add `%USERPROFILE%\bin` to your `PATH`, or use the full path to `truenas-mcp.exe`
as the `command` in your MCP client configuration. Compare the zip's SHA-256
(`Get-FileHash`) with `checksums.txt`.

### Step 2: Get TrueNAS API Key

1. Log into your TrueNAS web interface
2. Go to **System Settings → API Keys**
3. Click **Add** to create a new API key
4. Give it a name (e.g., "Claude Desktop MCP")
5. Make sure it has appropriate permissions (admin recommended)
6. **Copy the API key** - you'll need it for configuration
7. Note the **user that owns the key** (e.g. `truenas_admin`) - it is required as `--api-user` / `TRUENAS_API_USER`

### Step 3: Configure Your MCP Client

#### Claude Desktop

Edit your Claude Desktop configuration file (a template is in [`claude_desktop_config.example.json`](claude_desktop_config.example.json)):

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
        "--api-key", "your-api-key-here",
        "--api-user", "your-api-user"
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
  "--api-key", "your-api-key-here",
  "--api-user", "your-api-user"
]
```

**Option 2: Using environment variables:**
```json
{
  "mcpServers": {
    "truenas": {
      "command": "truenas-mcp",
      "env": {
        "TRUENAS_URL": "192.168.0.31",
        "TRUENAS_API_KEY": "your-api-key-here",
        "TRUENAS_API_USER": "your-api-user"
      }
    }
  }
}
```

**TLS with a self-signed certificate:** TrueNAS ships with a self-signed
certificate by default, and certificate verification is on by default. Either
export your server's certificate once and trust it (recommended):

```json
"env": {
  "TRUENAS_URL": "192.168.0.31",
  "TRUENAS_API_KEY": "your-api-key-here",
  "TRUENAS_API_USER": "your-api-user",
  "TRUENAS_TLS_CA": "/path/to/truenas-cert.pem"
}
```

or explicitly disable verification (not recommended - allows man-in-the-middle
attacks on your network):

```json
"env": { "TRUENAS_INSECURE": "1" }
```

To export the certificate: TrueNAS UI → System Settings → Certificates, or
`openssl s_client -connect 192.168.0.31:443 </dev/null 2>/dev/null | openssl x509 > truenas-cert.pem`

#### Claude Code

Claude Code uses the `claude mcp` command to configure MCP servers:

**Add TrueNAS MCP server:**
```bash
claude mcp add truenas -- truenas-mcp --truenas-url 192.168.0.31 --api-key your-api-key-here --api-user your-api-user
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
claude mcp add truenas -- truenas-mcp --truenas-url 192.168.0.31 --api-key new-api-key --api-user your-api-user
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

- `--truenas-url` - TrueNAS hostname (required, or use `TRUENAS_URL` env var)
  - Examples: `truenas.local`, `192.168.0.31` or `fd00::10` (automatically uses `wss://<host>:443/api/current`), a host with a port such as `truenas.local:8443` or `[2001:db8::10]:8443`, or a full `wss://host[:port]/api/current` URL
  - Credentials go in `--api-key` and `--api-user`; a URL containing `user:password@` is refused
  - A bare host cannot include a path; use a full `wss://` URL for that
  - A `wss://` URL ending in the legacy `/websocket` (or `/websocket/`) path is rewritten to `/api/current` with a one-time warning; the legacy endpoint is deprecated and planned for removal in TrueNAS 28
  - ⚠️ **Note**: `ws://` (unencrypted) is **not allowed** - TrueNAS will revoke API keys used over unencrypted connections
- `--api-key` - TrueNAS API key for authentication (required, or use `TRUENAS_API_KEY` env var)
- `--api-user` - The user that owns the API key, sent to `auth.login_ex` (required, or use `TRUENAS_API_USER`), e.g. `truenas_admin`. There is no default; the server exits at startup if it is missing
- `--tls-ca` - Path to a PEM certificate to trust, e.g. the TrueNAS self-signed certificate (or use `TRUENAS_TLS_CA` env var)
- `--insecure` - Disable TLS certificate verification entirely (or set `TRUENAS_INSECURE=1`) - **unsafe**: allows man-in-the-middle attacks; prefer `--tls-ca`
- `--proxy` - Proxy for the TrueNAS connection, overriding `HTTPS_PROXY` and `ALL_PROXY` (or use `TRUENAS_PROXY` env var); `none` ignores proxy environment variables; see [Proxies](#proxies)
- `--debug` - Enable debug logging (or set `TRUENAS_MCP_DEBUG=1`)
- `--read-only` - Hide and refuse every tool that changes the system (or set `TRUENAS_MCP_READ_ONLY=1`); see [Safety Controls](#safety-controls)
- `--allow-tools` - Comma-separated list of the only tools to expose (or use `TRUENAS_MCP_ALLOW_TOOLS`)
- `--deny-tools` - Comma-separated list of tools to hide and refuse (or use `TRUENAS_MCP_DENY_TOOLS`)
- `--version` - Print version and exit

### Debug Logging

By default, logs written to stderr contain only metadata (method names, request
IDs, response sizes) - never request or response bodies. Set `TRUENAS_MCP_DEBUG=1`
(or pass `--debug`) to log full request/response bodies for troubleshooting.

Even in debug mode, credentials are never logged: API keys and other
credential-bearing values (`password`, `bindpw`, `secret`, `token`, etc.) are
replaced with `[REDACTED]`, so debug logs are safe to share in bug reports.

### Examples

```bash
# Basic usage
./truenas-mcp --truenas-url 192.168.0.31 --api-key your-api-key --api-user your-api-user

# Using environment variables
export TRUENAS_URL=192.168.0.31
export TRUENAS_API_KEY=your-api-key
export TRUENAS_API_USER=your-api-user
./truenas-mcp

# With debug logging (full request/response bodies, credentials redacted)
./truenas-mcp --truenas-url 192.168.0.31 --api-key your-api-key --api-user your-api-user --debug
# or: TRUENAS_MCP_DEBUG=1 ./truenas-mcp --truenas-url 192.168.0.31 --api-key your-api-key --api-user your-api-user
```

## Connection Details

### How It Works

The binary connects directly to TrueNAS middleware's WebSocket endpoint:

1. **Uses secure WebSocket (wss://)**: Connects to the JSON-RPC 2.0 endpoint `wss://your-truenas:443/api/current` (TrueNAS 25.04 or later; no `/websocket` fallback)
2. **Verifies certificates by default**: trust the TrueNAS self-signed certificate with `--tls-ca` (see [TLS with a self-signed certificate](#step-3-configure-your-mcp-client))
3. **Authenticates via API key**: Logs in with `auth.login_ex` (`API_KEY_PLAIN` mechanism, user from `--api-user`)

The connection and login happen once, at startup, before the server reads any MCP request. If TrueNAS cannot be reached, or the login fails, the server logs the reason to stderr (`Could not connect to TrueNAS (no login was attempted): ...` when the connection fails, `Failed to authenticate with TrueNAS: ...` when the login is rejected, or `invalid TrueNAS host or URL: ...` for an unusable host setting) and exits with a non-zero status instead of starting without a working connection. This is intentional: the MCP client then shows the server as failed, and its log holds that message. Fix the cause (host, port, proxy, certificate or API key, see [Troubleshooting](#troubleshooting)) and restart the client.

### ⚠️ Security Requirement

**IMPORTANT**: TrueNAS **requires** encrypted connections (`wss://`) for API key authentication. Using unencrypted `ws://` will cause your API key to be **revoked** as a security measure. This binary defaults to `wss://` to protect your credentials.

### Proxies

The connection to TrueNAS honours the standard proxy environment variables,
so the server works behind an HTTP or SOCKS proxy, for example when TrueNAS is
only reachable through userspace-networking Tailscale (`tailscaled
--tun=userspace-networking --socks5-server=localhost:1055
--outbound-http-proxy-listen=localhost:1056`).

The proxy is chosen in this order:

1. `--proxy` / `TRUENAS_PROXY`
2. `HTTPS_PROXY` / `https_proxy`
3. `ALL_PROXY` / `all_proxy`

`HTTP_PROXY` is not used: the connection is `wss://`, which is HTTPS traffic.
`NO_PROXY` / `no_proxy` (comma-separated hosts, domains such as
`.example.com`, IPs, CIDRs, or `*`) applies to all of them. The exact host name
`localhost` and loopback IP addresses are always reached directly; other
spellings such as `LOCALHOST`, `localhost.` or `*.localhost` go through the proxy
unless `NO_PROXY` lists them.

To ignore inherited proxy variables, pass `--proxy none` (or set
`TRUENAS_PROXY=none`). An empty `--proxy` / `TRUENAS_PROXY` means "not set",
so the environment variables still apply. To bypass an inherited
`HTTPS_PROXY` / `ALL_PROXY`, use `--proxy none` (or `TRUENAS_PROXY=none`), which
ignores all proxy variables, or list the TrueNAS host in `NO_PROXY`, which
exempts only that host. `none` is only an opt-out in those
two settings: `HTTPS_PROXY=none` or `ALL_PROXY=none` is refused at startup
(it would name a proxy host called "none"), with a message pointing to
`TRUENAS_PROXY=none`.

A proxy setting is only checked when it applies to the TrueNAS host: an
unusable inherited value (say an `https://` proxy) does not matter if
`NO_PROXY` covers the host. If it does apply, the server refuses to start
and names the setting at fault, without echoing its value.

Supported proxy URLs:

- `http://host:port` - HTTP `CONNECT` tunnel; the port defaults to 80, and a URL without a scheme is treated as `http://`
- `socks5h://host:port` or `socks5://host:port` - SOCKS5; the port defaults to 1080; with either, host names are resolved by the proxy, so names such as Tailscale MagicDNS names work
- Credentials may be included: `http://user:pass@host:port` sends HTTP Basic `Proxy-Authorization` (`http://token@host:port` sends the token with an empty password), and SOCKS5 uses username/password authentication. Logs show the proxy only as `scheme://redacted@host:port`, and errors never echo the proxy URL

`https://` proxy URLs are not supported.

TLS runs end to end inside the tunnel: the proxy only sees encrypted traffic,
and the TrueNAS certificate is verified as usual (`--tls-ca` / `--insecure`
behave the same with or without a proxy).

```bash
# Through a SOCKS5 proxy, e.g. userspace Tailscale
ALL_PROXY=socks5h://localhost:1055 ./truenas-mcp --truenas-url truenas.tailnet-name.ts.net --api-key your-api-key --api-user your-api-user

# Through an HTTP proxy, bypassing it for the local network
HTTPS_PROXY=http://localhost:1056 NO_PROXY=192.0.2.0/24 ./truenas-mcp --truenas-url truenas.tailnet-name.ts.net --api-key your-api-key --api-user your-api-user

# Or set it explicitly, overriding the environment
./truenas-mcp --proxy socks5h://localhost:1055 --truenas-url truenas.tailnet-name.ts.net --api-key your-api-key --api-user your-api-user
```

Desktop MCP clients usually do not inherit your shell environment, so set the
proxy in the client's `env` block (or pass `--proxy` in `args`):

```json
{
  "mcpServers": {
    "truenas": {
      "command": "truenas-mcp",
      "args": ["--truenas-url", "truenas.tailnet-name.ts.net", "--api-user", "your-api-user"],
      "env": {
        "TRUENAS_API_KEY": "your-api-key",
        "TRUENAS_PROXY": "socks5h://localhost:1055"
      }
    }
  }
}
```

### Troubleshooting

**Connection Issues:**
- "invalid TrueNAS host or URL: ...": `--truenas-url` / `TRUENAS_URL` is unusable (bad port, user info in the URL, `https://`, ...); the message says why. Use the host name alone (`truenas.local`, `truenas.local:8443`, `[fd00::1]:8443`) or a `wss://host[:port]` URL
- "Could not connect to TrueNAS (no login was attempted)" means the TCP, proxy, TLS or WebSocket step failed; "Failed to authenticate with TrueNAS" means the connection worked and the login was rejected. If the connection drops during login and reconnecting fails, the message says so ("the connection was lost during login, and reconnecting failed") instead. The URL is logged without any query, fragment or user info, and error messages never quote them (at most they quote the host name or a short URL scheme)
- Verify TrueNAS is accessible from your machine
- Check firewall allows port 443 (wss), or the port you configured
- `HTTP 404` / "TrueNAS 25.04 or later required": the server predates the JSON-RPC API; upgrade TrueNAS
- Verify API key is valid and has admin permissions
- "connecting via proxy ... proxy unreachable": the proxy named in the message (and the variable it came from) is not listening; check it, or bypass it with `NO_PROXY` or `--proxy none`

**Certificate Verification Errors** (`x509: certificate signed by unknown authority` or similar):
- TrueNAS uses a self-signed certificate by default; export it and pass `--tls-ca truenas-cert.pem` (recommended)
- Or disable verification with `--insecure` / `TRUENAS_INSECURE=1` (unsafe on untrusted networks)

**Name mismatch** (`x509: certificate is valid for ..., not ...` or `doesn't contain any IP SANs`):
- The certificate does not list the host name or IP address you connected to (e.g. connecting to `192.0.2.10` when the certificate only names `truenas.example`)
- Connect using a host name listed in the certificate, or install a TrueNAS certificate whose subject alternative names include that name or IP
- `--tls-ca` cannot fix a name mismatch; `--insecure` is the last resort

**Authentication Failures:**
- Generate a new API key in TrueNAS System Settings → API Keys
- Ensure the key has appropriate permissions
- Check that the key wasn't accidentally truncated when copying
- Check that `--api-user` / `TRUENAS_API_USER` names the user that owns the key

## Security

- **Authentication**: TrueNAS API key required for all operations
- **TLS/SSL**: Only supports wss:// (encrypted) - ws:// is rejected for security
- **Certificate verification**: On by default; pin a self-signed certificate with `--tls-ca`, or opt out explicitly with `--insecure`
- **Network**: Client-only (no listening ports, all connections outbound)
- **API Key Storage**: Recommend using environment variables instead of command-line args
- **Log Safety**: Credentials are redacted from all log output and error messages; full request/response bodies are only logged in debug mode (see [Debug Logging](#debug-logging))
- **Tool Output Redaction**: Secrets are always removed from tool results and error messages before they reach the MCP client (and the model), with no opt-out. Values under credential-bearing names (`password`, `DB_PASS`, `adminPwd`, `bindpw`, `secret`, `token`, `api_key`, `private_key`, `passphrase`, `credential`, `pin`, `otp`, `cookie`, `authorization`, password hashes, a key named `key` or ending in `_key`, ...) are replaced with `[REDACTED]`; the key is kept so the model knows the field exists, and a whole object or list under such a name is replaced. `{name, value}` environment entries and `NAME=VALUE` strings are redacted by name. Free text, including strings inside JSON results such as job errors, is also scrubbed of PEM private key blocks, `key=value`/`"key": "value"` pairs, Bearer tokens and passwords in URLs. This is a safety net, not a substitute for restricting the API key's permissions

**Known gaps of output redaction.** Redaction works on names and on common text shapes, so it cannot catch everything:

- A secret stored under a name that does not look sensitive (for example a free-form `description` or `notes` field) is not recognised.
- Command lines: `-p` of programs other than `sshpass` and the MySQL/MariaDB clients is left alone (it often means port or parents), and so is Samba's `net -U user%password`. A password passed as a plain positional argument (`["admin", "secret"]`, `tool user secret`) is not detected; in free text this includes the positional passwords of `smbpasswd`, `net user`, `pdbedit -t` and `chpasswd`.
- Command-line scanning is heuristic: where a command ends on a line is worked out from separators (`|`, `;`, `&`) and quotes, so unusual quoting (a separator inside a quoted argument, an unbalanced quote) can leave a later password option visible or redact the rest of the line. A quote in the middle of an unquoted option value (`--password ab"c d"`) ends that value. An escaped quote (`\"`) that is never closed swallows the separators after it, so a later option of another program on the same line (`ssh -p 2222`) can be redacted as well; this is deliberate, because the same text can be a truncated quoted argument. A doubled-escaped quote idiom written twice in one command can still hide the second password option from the scanner.
- Text right after a closing quote of a quoted value (`password='x' y`, `'token_ttl': '5'X`) is not part of the value and stays visible.
- URLs whose password contains a raw `/`, `?`, `#`, quote or whitespace, or whose user name contains `@`, are only partly redacted. The host name stays visible by design.
- Plain multi-line YAML scalars are not handled; single-line `key: value` pairs, block scalars (`|`, `>`) under a sensitive key and Kubernetes-style `name`/`value` entries are.
- In JSON results, keys such as `password_file`, `token_file` or `secret_path` stay sensitive and their values are redacted, although the path itself is not a secret. Only command-line options of that shape (`--password-file /path`) are left visible.
- Keys ending in `private` or `priv` (for example `wg_private`, `tls_private`) are redacted unless the value is a boolean. A few names that denote a flag or a network are exempt (`is_private`, `network_private`, `net_private`, ...), but another `*_private` key that holds a non-secret value, such as a network name, is redacted too.
- Keys that name an algorithm (`*_algorithm`, `*_alg`, `*_algo`, for example `jwt_secret_algorithm`) show their value only when it looks like an identifier (letters, digits and `_ . + / -`, up to 40 characters, such as `HS256`). Any other non-empty string (a longer one, or text with spaces), object or array is redacted. Booleans, `null`, numbers, empty strings and empty objects or arrays stay visible.
- Redaction can also hide more than the secret: a bare `key` field is always redacted (including non-secret alert keys), `password: changed` in prose loses the word after the colon, and a sensitive-looking option in prose (`use --password to ...`) loses the next word. Objects under a sensitive name that have no `id` or `name` (for example directory service `credential` objects) are replaced as a whole.
- Only tool results, tool errors and logs are covered. Tool arguments the model sends are not redacted before they go to TrueNAS (they are redacted in logs).

### Security Best Practices

1. **Always use secure WebSocket (wss://)** - enforced by default, ws:// is rejected
2. **Generate dedicated API key** for MCP use only
3. **Use environment variables** for API keys in Claude Desktop config
4. **Restrict API key permissions** to minimum required
5. **Rotate API keys periodically**
6. **Start in `--read-only` mode** and enable write tools only where you need them (see [Safety Controls](#safety-controls))

### Safety Controls

Every tool is classified as **read**, **write** or **destructive** in one table
(`tools/safety.go`). The server uses it for three controls:

**Read-only mode.** `--read-only` (or `TRUENAS_MCP_READ_ONLY` set to `1`, `true`,
`yes`, `on`, ...) removes every write and destructive tool from `tools/list`,
and `tools/call` refuses them with an error, even if a client calls a tool it
was never shown. Dry runs of write tools are refused too.

**Tool allowlist and denylist.** `--allow-tools` / `TRUENAS_MCP_ALLOW_TOOLS`
takes a comma-separated list; only those tools are listed and callable.
`--deny-tools` / `TRUENAS_MCP_DENY_TOOLS` hides and refuses the listed tools.
A flag overrides its environment variable. The server refuses to start if a
list names an unknown tool, if `--allow-tools` is set but names no tools (for
example `","`), or if `--allow-tools` names a write tool while `--read-only`
is set.

```bash
# Monitoring only
./truenas-mcp --read-only

# Monitoring plus app restarts (stop_app still needs confirm=true)
TRUENAS_MCP_ALLOW_TOOLS=query_apps,start_app,stop_app,tasks_get ./truenas-mcp
```

**Confirmation for destructive tools.** These tools need `"confirm": true` to execute:

| Tool | Why it is destructive |
|------|-----------------------|
| `system_reboot` | Interrupts every service and client connection. `delay` is capped at 86400 seconds; a pending reboot cannot be cancelled through this server |
| `apply_update` | Replaces the system software and may reboot |
| `delete_boot_environment` | Permanently removes a rollback target |
| `delete_app` | Removes an application and its configuration |
| `stop_app` | Takes a running service offline |
| `upgrade_app` | Takes the app offline; upgrades can run one-way migrations and there is no rollback tool |
| `configure_directory_service` | Disabling or re-pointing a joined service cuts off domain users, like leaving |
| `leave_directory_service` | Leaves the domain; domain users lose access to shares |
| `delete_scrub_schedule` | Removes automatic data-integrity checks |

Called without `confirm`, a destructive tool changes nothing and returns a
normal (non-error) result with `"status": "confirmation_required"` and the
tool's dry-run preview. The assistant should show the preview, get the user's
explicit approval, and call again with the same arguments plus
`"confirm": true`. If the preview itself fails, the call returns an error and
nothing runs; resolve the problem before confirming. `"dry_run": true` still
works without `confirm` (only on tools that declare `dry_run`, which all
destructive tools do). `confirm` and `dry_run` must be JSON booleans.

> **Breaking change:** clients or scripts that called these nine tools without
> `confirm` now get a preview instead of the operation. Add `"confirm": true`
> to execute them.

**Tool annotations.** Every tool in `tools/list` carries MCP
[tool annotations](https://modelcontextprotocol.io/specification/2025-03-26/server/tools)
(`readOnlyHint`, `destructiveHint`, `idempotentHint`, `openWorldHint: false`)
derived from the same table, so clients can apply their own approval prompts.
`destructiveHint` is true for the nine destructive tools and for `update_app`,
which replaces app configuration in place but does not need `confirm`.
Annotations were added in MCP 2025-03-26; this server still reports protocol
`2024-11-05`, and older clients ignore the extra field.

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
4. `upgrade_app` only reports success once the app is seen RUNNING after the upgrade job finishes. It waits `health_timeout_seconds` (an integer from 0 to 600, default 60; other values are rejected). If the app is merely still starting when the wait ends, the task fails with a message saying the upgrade itself succeeded and the app was slow to start; raise `health_timeout_seconds` for such apps. TrueNAS always stops the app during an upgrade and snapshots its ix-volumes; `snapshot_hostpaths` only controls whether host paths are also snapshotted
5. Job output is summarized: each job summary (the `job` of `tasks_get`, each entry of `query_jobs`) is at most 4 KiB, with `*_truncated` flags when text was cut
6. Tasks update automatically - no manual polling needed

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
Add `"dry_run": true` to any write operation to preview what would happen without making changes. Destructive tools return this preview automatically when called without `"confirm": true` (see [Safety Controls](#safety-controls)).

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

## Development

Go 1.27.3 or later is required (see [Building](#building)).

```bash
# Run linters (go vet, and a gofmt check)
make lint

# Run tests
make test

# Run tests with the race detector (recommended before sending a change)
go test -race ./...

# Check dependencies for known vulnerabilities (version used in CI)
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# Clean build artifacts
make clean
```

The "Build and Package" workflow runs the linters, the tests, `govulncheck`
and builds for macOS (Apple Silicon), Linux and Windows on every pull request
and on every push to `main`. The release workflow, which runs for version
tags, runs the linters and tests again and publishes those three platforms. A test in
`cmd/truenas-mcp` checks that the flags documented in this README match the
flags the binary registers, and that the Go version above matches `go.mod`.
