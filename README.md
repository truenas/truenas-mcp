# TrueNAS MCP Server

> **⚠️ Early Access**
> This project is in early access. It is data safe, but tools, arguments and outputs may still change between releases. Use caution in production environments.

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
  - [MCP Protocol](#mcp-protocol)
  - [Proxies](#proxies)
- [Security](#security)
  - [Safety Controls](#safety-controls)
- [Example Usage](#example-usage)
- [Advanced Features](#advanced-features)
  - [Tasks for Long-Running Operations](#tasks-for-long-running-operations)
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
│  - MCP server (Go MCP SDK) │
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

Binaries report their version with `--version`. `make` takes the version from `git describe`; override it with `make build VERSION=1.2.3`. The `build` and `build-all` targets set `CGO_ENABLED=0`, so the Linux binary is statically linked. Without `make`, `CGO_ENABLED=0 go build -o truenas-mcp ./cmd/truenas-mcp` produces the same binary; omit `CGO_ENABLED=0` and a Linux build may be dynamically linked if a C compiler is present. The binary's version is the module version Go records from the git checkout (or from `go install ...@vX.Y.Z`), or `dev` when none is available. `make clean` removes the binaries.

## Installation

**Requirements:** TrueNAS 25.10 or later. The server uses the JSON-RPC 2.0
endpoint (`/api/current`) and `auth.login_ex`. After login it reads
`system.info` and exits if the connected release is older than 25.10, with
a message that names the version it found and the 25.10 minimum. There is
no fallback to the legacy `/websocket` protocol, so TrueNAS 24.10 and older
are not supported. A server that predates the JSON-RPC API answers the
handshake with HTTP 404, which is reported as "TrueNAS 25.10 or later
required". The endpoint is kept at `/api/current` (not pinned to a
versioned path such as `/api/v25.10`); tools that a given release cannot
run are hidden and refused, as listed in [TrueNAS version compatibility](#truenas-version-compatibility).

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
5. The key inherits its owner's privileges. Give that user the narrowest TrueNAS role that covers the tools you will enable, not a full administrator account by default. For a `--read-only` deployment, assign the [Read-Only Administrator](https://www.truenas.com/docs/scale/25.10/scaletutorials/credentials/adminroles/) privilege (`READONLY_ADMIN`) so TrueNAS itself rejects writes, not only this binary. Otherwise use the narrowest built-in or custom privilege that covers the write and destructive tools you enable (for example Sharing Administrator if you only expose share tools). See [Using Administrator Logins](https://www.truenas.com/docs/scale/25.10/scaletutorials/credentials/adminroles/) and [Managing API Keys](https://www.truenas.com/docs/scale/25.10/scaletutorials/toptoolbar/managingapikeys/).
6. **Copy the API key** - you'll need it for configuration. Prefer `TRUENAS_API_KEY` or `--api-key-file`; see [Flags](#flags)
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

Add the TrueNAS MCP server configuration. Put the API key in the client's
`env` block (`TRUENAS_API_KEY`), not in `args`. A key passed with `--api-key`
is visible in the process list.

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

On Linux, the static release binary cannot resolve `.local` (mDNS) names:
Go's pure resolver uses `files,dns` and does not use nss-mdns. Use the IP
address, an `/etc/hosts` entry, or a DNS name (`truenas.example.com`)
instead, or build from source with cgo (`CGO_ENABLED=1 go build`). macOS
and Windows builds are unaffected, because cgo was already off for those
cross-builds.

**Configuration options:**

**Option 1: Environment variables (recommended):**
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

**Option 2: Hostname as a flag, key still in the environment:**
```json
{
  "command": "truenas-mcp",
  "args": [
    "--truenas-url", "192.168.0.31",
    "--api-user", "your-api-user"
  ],
  "env": {
    "TRUENAS_API_KEY": "your-api-key-here"
  }
}
```

**Option 3: Key from a file:** set `TRUENAS_API_KEY_FILE` (or pass
`--api-key-file`) to a file that contains the key. On Unix the file must
not be readable by group or others (mode `0600`). One trailing newline is
trimmed. Do not also set `TRUENAS_API_KEY` or `--api-key`.

`--api-key` in `args` is still accepted, but the key is visible in the
process list; prefer `TRUENAS_API_KEY` or `--api-key-file`.

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

**Add TrueNAS MCP server** (the key is stored in the server's environment, not as a process argument):
```bash
claude mcp add truenas \
  --env TRUENAS_URL=192.168.0.31 \
  --env TRUENAS_API_KEY=your-api-key-here \
  --env TRUENAS_API_USER=your-api-user \
  -- truenas-mcp
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
claude mcp add truenas \
  --env TRUENAS_URL=192.168.0.31 \
  --env TRUENAS_API_KEY=new-api-key \
  --env TRUENAS_API_USER=your-api-user \
  -- truenas-mcp
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
  - Examples: `192.168.0.31`, `truenas.example.com` or `fd00::10` (automatically uses `wss://<host>:443/api/current`), a host with a port such as `192.168.0.31:8443` or `[2001:db8::10]:8443`, or a full `wss://host[:port]/api/current` URL. A `.local` name such as `truenas.local` is accepted, but on Linux the static release binary cannot resolve it via mDNS (use the IP, an `/etc/hosts` entry, or a DNS name, or build from source with `CGO_ENABLED=1 go build`); macOS and Windows builds are unaffected
  - Credentials go in `TRUENAS_API_KEY` (or `--api-key-file` / `--api-key`) and `--api-user`; a URL containing `user:password@` is refused
  - A bare host cannot include a path; use a full `wss://` URL for that
  - A `wss://` URL ending in the legacy `/websocket` (or `/websocket/`) path is rewritten to `/api/current` with a one-time warning; the legacy endpoint is deprecated and planned for removal in TrueNAS 28
  - ⚠️ **Note**: `ws://` (unencrypted) is **not allowed** - TrueNAS will revoke API keys used over unencrypted connections
- `--api-key` - TrueNAS API key for authentication (or use `TRUENAS_API_KEY`). The key is visible in the process list; prefer the environment variable or `--api-key-file`
- `--api-key-file` - Path to a file containing the API key (or use `TRUENAS_API_KEY_FILE`). The file is read at startup, one trailing newline is trimmed, and on Unix a file readable by group or others is refused (use mode `0600`). The file contents are never logged. Provide only one of `--api-key` / `TRUENAS_API_KEY` or `--api-key-file` / `TRUENAS_API_KEY_FILE`; a flag wins over the environment
- `--api-user` - The user that owns the API key, sent to `auth.login_ex` (required, or use `TRUENAS_API_USER`), e.g. `truenas_admin`. There is no default; the server exits at startup if it is missing
- `--tls-ca` - Path to a PEM certificate to trust, e.g. the TrueNAS self-signed certificate (or use `TRUENAS_TLS_CA` env var)
- `--insecure` - Disable TLS certificate verification entirely (or set `TRUENAS_INSECURE=1`) - **unsafe**: allows man-in-the-middle attacks; prefer `--tls-ca`
- `--proxy` - Proxy for the TrueNAS connection, overriding `HTTPS_PROXY` and `ALL_PROXY` (or use `TRUENAS_PROXY` env var); `none` ignores proxy environment variables; see [Proxies](#proxies)
- `--debug` - Enable debug logging (or set `TRUENAS_MCP_DEBUG=1`)
- `--read-only` - Hide and refuse every tool that changes the system (or set `TRUENAS_MCP_READ_ONLY=1`); see [Safety Controls](#safety-controls)
- `--allow-tools` - Comma-separated list of the only tools to expose (or use `TRUENAS_MCP_ALLOW_TOOLS`)
- `--deny-tools` - Comma-separated list of tools to hide and refuse (or use `TRUENAS_MCP_DENY_TOOLS`)
- `--require-human-confirm` - Refuse destructive tools on clients that do not support MCP elicitation, instead of falling back to a confirmation token (or set `TRUENAS_MCP_REQUIRE_HUMAN_CONFIRM=1`); see [Safety Controls](#safety-controls)
- `--version` - Print version and exit

### Debug Logging

By default, logs written to stderr contain only metadata (method names, request
IDs, response sizes) - never request or response bodies. Set `TRUENAS_MCP_DEBUG=1`
(or pass `--debug`) to log full request/response bodies for troubleshooting,
and to log middleware error traces (Python tracebacks) to stderr. Those traces
are not included in tool errors sent to the MCP client.

Even in debug mode, credentials are never logged: API keys and other
credential-bearing values (`password`, `bindpw`, `secret`, `token`, etc.) are
replaced with `[REDACTED]`, so debug logs are safe to share in bug reports.

### Examples

```bash
# Recommended: environment variables (the key is not visible in the process list)
export TRUENAS_URL=192.168.0.31
export TRUENAS_API_KEY=your-api-key
export TRUENAS_API_USER=your-api-user
./truenas-mcp

# Or read the key from a mode 0600 file
export TRUENAS_URL=192.168.0.31
export TRUENAS_API_KEY_FILE=/path/to/truenas-api-key
export TRUENAS_API_USER=your-api-user
./truenas-mcp

# --api-key works, but the key is visible in the process list
./truenas-mcp --truenas-url 192.168.0.31 --api-key your-api-key --api-user your-api-user

# With debug logging (full request/response bodies, credentials redacted)
./truenas-mcp --debug
# or: TRUENAS_MCP_DEBUG=1 ./truenas-mcp
```

## Connection Details

### How It Works

The binary connects directly to TrueNAS middleware's WebSocket endpoint:

1. **Uses secure WebSocket (wss://)**: Connects to the JSON-RPC 2.0 endpoint `wss://your-truenas:443/api/current` (TrueNAS 25.10 or later; no `/websocket` fallback)
2. **Verifies certificates by default**: trust the TrueNAS self-signed certificate with `--tls-ca` (see [TLS with a self-signed certificate](#step-3-configure-your-mcp-client))
3. **Authenticates via API key**: Logs in with `auth.login_ex` (`API_KEY_PLAIN` mechanism, user from `--api-user`)

The connection, login, and version check happen once, at startup, before the server reads any MCP request. If TrueNAS cannot be reached, the login fails, or the release is older than 25.10, the server logs the reason to stderr (`Could not connect to TrueNAS (no login was attempted): ...` when the connection fails, `Failed to authenticate with TrueNAS: ...` when the login is rejected, `TrueNAS 25.04.2 is not supported; TrueNAS 25.10 or later is required` when the version is too old, or `invalid TrueNAS host or URL: ...` for an unusable host setting) and exits with a non-zero status instead of starting without a working connection. This is intentional: the MCP client then shows the server as failed, and its log holds that message. Fix the cause (host, port, proxy, certificate, API key, or TrueNAS version, see [Troubleshooting](#troubleshooting)) and restart the client. After a reconnect mid-session (for example after a reboot), the version is read again and tool calls are refused if the new release is below 25.10.

### MCP Protocol

The MCP side is served by the official
[Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk) (v1.8.0) over stdio:
newline-delimited JSON-RPC 2.0 on stdin and stdout, with logs on stderr only.

- **Protocol versions:** `initialize` negotiates `2024-11-05`, `2025-03-26`,
  `2025-06-18` or `2025-11-25` (the version the client asks for, or
  `2025-11-25` if the server does not support it). Clients on `2026-07-28`
  use that version's per-request handshake (`server/discover`) instead.
- **Concurrency:** requests are handled concurrently, so a slow tool call
  does not hold up `ping`, `tools/list` or other tool calls. At most 8 tool
  calls run at once; further calls wait for a slot.
- **Cancellation and deadlines:** a `notifications/cancelled` for a running
  call, or the client closing stdin, cancels the call and the server stops
  waiting for it, including any in-flight TrueNAS request; a cancelled call,
  and a call still in flight when stdin closes, gets no reply. The
  concurrency slot is released once the handler returns, which happens
  promptly when it observes the cancellation. A tool call that has not
  finished after 10 minutes gets a tool error. The TrueNAS request a
  cancelled or timed-out tool already sent is not withdrawn, so the
  operation may still complete on the NAS: check the system state before
  retrying.
- **Errors:** an unknown tool name, or `params` or `arguments` that are not
  objects (including array params), are JSON-RPC errors (`-32602`) that
  keep the request's id. A tool hidden by
  [Safety Controls](#safety-controls) is refused with a tool error
  (`isError`) that says why. Requests other than `ping` are rejected until
  the client has sent `initialize`, which must carry its `params`.
- **Limits:** a message may be up to 64 MiB. A longer line, or one that is
  not JSON, gets an error reply (`-32600` or `-32700`, `"id": null`) and the
  session goes on. JSON-RPC batches are not supported and get a `-32600`
  error.

> **Breaking change:** older releases had a hand-written transport that
> always answered `protocolVersion` `2024-11-05`, served requests one at a
> time and in order, answered unknown tools with a tool error, accepted
> `tools/list` and `tools/call` before `initialize`, and still answered
> calls that were in flight when stdin closed. Array params now get
> `-32602` with the request id; a batch gets `-32600`; calls still in
> flight when stdin closes get no reply. A cancelled call stops waiting
> for TrueNAS and releases its concurrency slot once the handler returns.
> Clients that follow the
> MCP specification are not affected; tool names, schemas and output are
> unchanged.

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
export TRUENAS_URL=truenas.tailnet-name.ts.net
export TRUENAS_API_KEY=your-api-key
export TRUENAS_API_USER=your-api-user

# Through a SOCKS5 proxy, e.g. userspace Tailscale
ALL_PROXY=socks5h://localhost:1055 ./truenas-mcp

# Through an HTTP proxy, bypassing it for the local network
HTTPS_PROXY=http://localhost:1056 NO_PROXY=192.0.2.0/24 ./truenas-mcp

# Or set the proxy explicitly, overriding the environment
./truenas-mcp --proxy socks5h://localhost:1055
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
- "invalid TrueNAS host or URL: ...": `--truenas-url` / `TRUENAS_URL` is unusable (bad port, user info in the URL, `https://`, ...); the message says why. Use the host name alone (`192.168.0.31`, `truenas.example.com:8443`, `[fd00::1]:8443`) or a `wss://host[:port]` URL
- A `.local` name such as `truenas.local` does not resolve on Linux with the static release binary (or `make build`), because that binary does not use mDNS/nss-mdns. Use the IP address, an `/etc/hosts` entry, or a DNS name, or build from source with cgo (`CGO_ENABLED=1 go build`). macOS and Windows builds are unaffected, because cgo was already off for those cross-builds
- "Could not connect to TrueNAS (no login was attempted)" means the TCP, proxy, TLS or WebSocket step failed; "Failed to authenticate with TrueNAS" means the connection worked and the login was rejected. If the connection drops during login and reconnecting fails, the message says so ("the connection was lost during login, and reconnecting failed") instead. The URL is logged without any query, fragment or user info, and error messages never quote them (at most they quote the host name or a short URL scheme)
- Verify TrueNAS is accessible from your machine
- Check firewall allows port 443 (wss), or the port you configured
- `HTTP 404` / "TrueNAS 25.10 or later required": the JSON-RPC endpoint was not found (the server predates that API, which was introduced in 25.04); upgrade TrueNAS to 25.10 or later
- "TrueNAS … is not supported; TrueNAS 25.10 or later is required": the server connected and logged in, but `system.info` reported a release older than 25.10 (including 25.04); upgrade TrueNAS
- Verify the API key is valid and its owner has the privileges required for the tools you enable (see [Using Administrator Logins](https://www.truenas.com/docs/scale/25.10/scaletutorials/credentials/adminroles/))
- "connecting via proxy ... proxy unreachable": the proxy named in the message (and the variable it came from) is not listening; check it, or bypass it with `NO_PROXY` or `--proxy none`
- "connecting via proxy ... the proxy is reachable but could not connect to the TrueNAS host ...": the SOCKS5 proxy answered but could not open a connection to TrueNAS (the SOCKS5 reply, such as "general failure" or "host unreachable", is shown at the end); check the TrueNAS host and port, that the system is up, and that the proxy's network (for example your tailnet or VPN) can route to it
- "request too large for the TrueNAS API": TrueNAS accepts at most 64 KiB per API message, so a larger request (for example a very long value in a tool argument) is refused before it is sent and nothing runs; reduce the size of the input. If TrueNAS itself closes the connection for an oversized message (WebSocket close code 1009), the error says so and the next request reconnects
- "the TrueNAS response was larger than 10 MiB": a single API response exceeded this client's read limit, so the connection is dropped and every in-flight request on it fails with that message; narrow the query with filters or a limit. The next request reconnects

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
- **API Key Storage**: Prefer `TRUENAS_API_KEY` in the client's `env` block, or `--api-key-file` / `TRUENAS_API_KEY_FILE` pointing at a mode `0600` file. `--api-key` is supported but visible in the process list
- **Log Safety**: Credentials are redacted from all log output and error messages; full request/response bodies are only logged in debug mode (see [Debug Logging](#debug-logging))
- **Tool Output Redaction**: Secrets are always removed from tool results and error messages before they reach the MCP client (and the model), with no opt-out. Values under credential-bearing names (`password`, `DB_PASS`, `adminPwd`, `bindpw`, `secret`, `token`, `api_key`, `private_key`, `passphrase`, `credential`, `pin`, `otp`, `cookie`, `authorization`, password hashes, a key named `key` or ending in `_key`, ...) are replaced with `[REDACTED]`; the key is kept so the model knows the field exists, and a whole object or list under such a name is replaced. `{name, value}` environment entries and `NAME=VALUE` strings are redacted by name. Free text, including strings inside JSON results such as job errors, is also scrubbed of PEM private key blocks, `key=value`/`"key": "value"` pairs, Bearer tokens and passwords in URLs. This is a safety net, not a substitute for restricting the API key's permissions
- **Allow-listed tool output**: `query_pools`, `query_shares`, `list_alerts`, and `check_updates` project each middleware record onto a fixed field list before that safety net runs, so unlisted keys (including surprise secret-looking fields, untrimmed pool vdev stats, and SMB `auxsmbconf`) never reach the client. `query_pools` keeps leaf `disk` and `device` names, a stats object trimmed to read/write/checksum errors, and RAIDZ expansion progress, so a degraded pool can still be mapped to physical disks. `dismiss_alert` and `restore_alert` dry-run previews use the same alert field list, so raw `mail` and `key` values are not passed through in `current_state`. `get_app_config` still returns the full values object so it can be edited and passed to `update_app`; redaction remains the backstop there. Use a least-privilege API key role as well (see [Security Best Practices](#security-best-practices))
- **Tool Output Size**: Tool results and errors sent to the MCP client are capped at 100 KiB. A larger value is redacted first, then cut on a rune boundary, and a notice is appended saying the output was truncated and to narrow the query with `limit`, `select`, or filters. Redacting first prevents a secret from surviving as a fragment at the cut. Middleware error traces (Python tracebacks) are never sent to the client; with `--debug` they are logged to stderr, still redacted

**Known gaps of output redaction.** Redaction works on names and on common text shapes, so it cannot catch everything. The allow lists above shrink what those heuristics have to cover for the listed tools, but they do not replace the backstop, and `get_app_config` is still a values passthrough:

- A secret stored under a name that does not look sensitive (for example a free-form `description` or `notes` field) is not recognised.
- Command lines: `-p` of programs other than `sshpass` and the MySQL/MariaDB clients is left alone (it often means port or parents), and so is Samba's `net -U user%password`. A password passed as a plain positional argument (`["admin", "secret"]`, `tool user secret`) is not detected; in free text this includes the positional passwords of `smbpasswd`, `net user`, `pdbedit -t` and `chpasswd`.
- Command-line scanning is heuristic: where a command ends on a line is worked out from separators (`|`, `;`, `&`) and quotes, so unusual quoting (a separator inside a quoted argument, an unbalanced quote) can leave a later password option visible or redact the rest of the line. A separator glued to an unquoted password value (`sshpass -p ab&cd`, `mysql -pab;mysql -pcd`) does not end it, because it may be part of the password: the value is redacted to the end of the word, including the separator and anything glued to it, quoted parts too. A key or another program glued after the separator (`mysql -pab;password=cd`, `mysql -pab|ipmitool -P cd`) is still redacted as on its own first, and is then covered by the same marker. Quote the value, or put white space before the separator, to keep the next command visible. A quote in the middle of an unquoted option value (`--password ab"c d"`) ends that value. An escaped quote (`\"`) that is never closed swallows the separators after it, so a later option of another program on the same line (`ssh -p 2222`) can be redacted as well; this is deliberate, because the same text can be a truncated quoted argument. A doubled-escaped quote idiom written twice in one command can still hide the second password option from the scanner.
- Text right after a closing quote of a quoted value (`password='x' y`, `'token_ttl': '5'X`) is not part of the value and stays visible.
- Free-text redaction fails closed. An unquoted `key=value` or `key: value` value of a sensitive key is redacted to the end of the line, together with any later pairs on it (`password=S3CR3T user=bob` becomes `password=[REDACTED]`), because its end cannot be proven: `password=ab cd=ef` may be the single secret `ab cd=ef`. This also applies to backslash-escaped spaces, tabs and vertical tabs, and to `&name=` anywhere on the line, not only inside a URL query string (`?token=S3CR3T&page=2` becomes `?token=[REDACTED]`), because a secret can itself contain `&name=`. Quote the value to keep the rest of the line visible: a quoted value ends at its closing quote (`password="a b" user=bob` becomes `password="[REDACTED]" user=bob`). In a query string, put a sensitive parameter last to keep the others visible. A whole string of the form `NAME=VALUE` (an environment entry) is redacted to its end.
- URLs whose password contains a raw `/`, `?`, `#`, quote or whitespace, or whose user name contains `@`, are only partly redacted. The host name stays visible by design.
- Plain multi-line YAML scalars are not handled; single-line `key: value` pairs, block scalars (`|`, `>`) under a sensitive key and Kubernetes-style `name`/`value` entries are.
- In JSON results, keys such as `password_file`, `token_file` or `secret_path` stay sensitive and their values are redacted, although the path itself is not a secret. Only command-line options of that shape (`--password-file /path`) are left visible.
- Keys ending in `private` or `priv` (for example `wg_private`, `tls_private`) are redacted unless the value is a boolean. A few names that denote a flag or a network are exempt (`is_private`, `network_private`, `net_private`, ...), but another `*_private` key that holds a non-secret value, such as a network name, is redacted too.
- Keys that name an algorithm (`*_algorithm`, `*_alg`, `*_algo`, for example `jwt_secret_algorithm`) show their value only when it looks like an identifier (letters, digits and `_ . + / -`, up to 40 characters, such as `HS256`). Any other non-empty string (a longer one, or text with spaces), object or array is redacted. Booleans, `null`, numbers, empty strings and empty objects or arrays stay visible.
- Redaction can also hide more than the secret: a bare `key` field is always redacted, `password: changed` in prose loses the word after the colon, and a sensitive-looking option in prose (`use --password to ...`) loses the next word. `list_alerts` therefore omits the middleware alert `key` (a non-secret dedup id) and keeps `uuid` as the identifier for `dismiss_alert` and `restore_alert`. Objects under a sensitive name that have no `id` or `name` (for example directory service `credential` objects) are replaced as a whole.
- Only tool results, tool errors and logs are covered. Tool arguments the model sends are not redacted before they go to TrueNAS (they are redacted in logs).

### Security Best Practices

1. **Always use secure WebSocket (wss://)** - enforced by default, ws:// is rejected
2. **Generate a dedicated API key** for MCP use only, owned by a user whose privilege is no broader than the tools you enable
3. **Use environment variables or `--api-key-file`** for API keys; do not pass `--api-key` on the command line (it is visible in the process list)
4. **Restrict the API key's privileges** to the minimum required: [Read-Only Administrator](https://www.truenas.com/docs/scale/25.10/scaletutorials/credentials/adminroles/) (`READONLY_ADMIN`) for `--read-only` deployments, so TrueNAS itself rejects writes, otherwise the narrowest role that covers the enabled write tools
5. **Rotate API keys periodically**
6. **Start in `--read-only` mode** and enable write tools only where you need them (see [Safety Controls](#safety-controls))

### Safety Controls

Every tool is classified as **read**, **write** or **destructive** in one table
(`tools/safety.go`). The server uses it for three controls:

**Read-only mode.** `--read-only` (or `TRUENAS_MCP_READ_ONLY` set to `1`, `true`,
`yes`, `on`, ...) removes every write and destructive tool from `tools/list`,
and `tools/call` refuses them with an error, even if a client calls a tool it
was never shown. Dry runs of write tools are refused too, with one explicit
exception: `upgrade_app` stays listed for dry runs only, so you can preview an
upgrade (current version, target version, changelog, planned steps) from a
read-only server. In read-only mode its schema requires `"dry_run": true`, its
description says only dry runs are allowed, and the preview only queries the
system (`app.query`, `app.upgrade_summary`). Any other call (`dry_run` false
or absent, with or without a `confirmation_token`) is refused with a
read-only error and nothing is executed. Read-only mode never issues
confirmation tokens.

**Tool allowlist and denylist.** `--allow-tools` / `TRUENAS_MCP_ALLOW_TOOLS`
takes a comma-separated list; only those tools are listed and callable.
`--deny-tools` / `TRUENAS_MCP_DENY_TOOLS` hides and refuses the listed tools.
A flag overrides its environment variable. The server refuses to start if a
list names an unknown tool, if `--allow-tools` is set but names no tools (for
example `","`), or if `--allow-tools` names a write tool while `--read-only`
is set (`upgrade_app` may be named; it stays limited to dry runs).

```bash
# Monitoring only
./truenas-mcp --read-only

# Monitoring plus app restarts (stop_app still needs a confirmation token)
TRUENAS_MCP_ALLOW_TOOLS=query_apps,start_app,stop_app,tasks_get ./truenas-mcp
```

**Confirmation for destructive tools.** These ten tools execute only after
confirmation. When the MCP client supports form elicitation, the server asks
the user to approve a short preview in the client and executes only if they
accept. Otherwise the tools execute only with a `confirmation_token` from a
preview of the same call:

| Tool | Why it is destructive |
|------|-----------------------|
| `system_reboot` | Interrupts every service and client connection. `delay` is capped at 86400 seconds; a pending reboot cannot be cancelled through this server |
| `apply_update` | Replaces the system software and may reboot |
| `delete_boot_environment` | Permanently removes a rollback target |
| `delete_app` | Removes an application and its configuration |
| `stop_app` | Takes a running service offline |
| `upgrade_app` | Takes the app offline; upgrades can run one-way migrations and there is no rollback tool |
| `update_app` | Replaces the app's configuration in place and restarts the app |
| `configure_directory_service` | Disabling or re-pointing a joined service cuts off domain users, like leaving |
| `leave_directory_service` | Leaves the domain; domain users lose access to shares |
| `delete_scrub_schedule` | Removes automatic data-integrity checks |

No other tool needs a token: the other write tools (`create_dataset`,
`create_smb_share`, `create_nfs_share`, `install_app`, `start_app`,
`download_update`, `create_scrub_schedule`, `run_scrub`, `dismiss_alert`,
`restore_alert`, `refresh_directory_cache`) run directly and keep their
optional `dry_run`, and read tools never take one.

**Elicitation (primary path).** If the client declared the MCP form
elicitation capability, a destructive call without a valid token runs the
preview and then sends an elicitation whose message is a short summary of
that preview (tool, target, main warnings) with one required boolean field,
`approved`. The server uses the official Go MCP SDK for this: mid-call
`elicitation/create` on protocol revisions 2025-06-18 and 2025-11-25, and a
tool result with `inputRequests` (the client retries with the answer) on
2026-07-28. The call executes only on `accept` with `approved` true, and
only the plan that was previewed. The prompt is valid for 5 minutes. A
declined, cancelled, failed or expired prompt executes nothing and does not
fall back to a confirmation token. A blocked preview (the operation cannot
proceed) is an error, issues no prompt, and executes nothing.

`--require-human-confirm` (or `TRUENAS_MCP_REQUIRE_HUMAN_CONFIRM`) never
issues a confirmation token and never redeems one. Destructive tools on
clients that did not declare form elicitation are refused; a client that
declared it is prompted, even if the call carried a `confirmation_token`.
Dry runs and `--read-only` are unchanged.

**Token flow (fallback).** Clients that do not declare elicitation use a
confirmation token:

1. Call the tool without a token. Nothing is executed. The reply is a
   normal (non-error) result with `"status": "confirmation_required"`,
   `"executed": false`, the tool's dry-run `preview`, a `confirmation_token`
   and `expires_in_seconds` (300).
2. Show the preview to the user and get their explicit approval.
3. Call the tool again with exactly the same arguments plus
   `"confirmation_token": "<token>"`. The call executes once.

When `--require-human-confirm` is not set, a call that already carries a
valid token is executed without elicitation (no double prompt). A rejected
token on a client that declared elicitation re-prompts instead of issuing
a new token. A call approved through elicitation does not need a token.

The token is 128 bits from a cryptographically secure random source, single
use, valid for 5 minutes, and held in the server's memory only (a restart
invalidates every token). It is bound to the tool and to a SHA-256 hash of
the canonical JSON of the arguments (keys sorted; `confirmation_token`,
`dry_run` and the old `confirm` are left out; an optional argument sent as
`null` counts as omitted). For tools that share a plan between preview and
execute (`stop_app`, `delete_boot_environment`, `delete_app`,
`delete_scrub_schedule`, `upgrade_app`, `update_app`, `system_reboot`,
`apply_update`, `configure_directory_service`, `leave_directory_service`),
the token is also bound to
a hash of the planned middleware method and parameters, so it cannot
execute a different planned operation. The token store does not hold those
parameters. A token that is unknown, expired, already used,
issued for another tool or issued for different arguments is rejected with
an error that says why and that nothing was executed; that error result
carries a fresh preview and a fresh token. A token presented for another
tool or other arguments is used up by that attempt.

If the preview reports that the operation cannot proceed (for example
`delete_boot_environment` on the running, next-boot or protected boot
environment), the call returns an error-shaped result with
`"status": "blocked"` and the preview, and no token is issued. If the
preview itself fails (for example `upgrade_app` when no upgrade is
available), the call returns an error and no token is issued; resolve the
problem first. `"dry_run": true` still returns only the preview and issues
no token (a token sent with `dry_run: true` is not used up). `dry_run` must
be a JSON boolean (`true` or `false`); a string such as `"true"` or a number
is rejected and nothing is executed.

**What confirmation does and does not prove.** A confirmation token
guarantees that a preview of these exact arguments was produced by this
server within the last 5 minutes and that the executed arguments match it.
It does **not** prove that a human saw or approved the preview: the client
holds the token and can send it straight back. MCP elicitation raises the
bar by asking the client to show the preview and return the user's
decision, but the capability is self-declared: a headless or auto-approving
host can answer `accept` without a person. Elicitation is not proof that a
human approved. Hard enforcement is `--read-only`, `--allow-tools` /
`--deny-tools`, `--require-human-confirm`, and the TrueNAS role of the API
key.

The token is returned to the client in the tool result. Debug logs
(`--debug`) never show it: it is redacted like any other value under a
`token` key, both in the request that presents it and in the reply that
returns it. In tool results only the top-level `confirmation_token` of a
reply, and only while it is a live token issued by this server, is exempt
from redaction. The token is the first key of the reply, so the 100 KiB
output cap never cuts it; if a cut ever would, the call returns an error
without the token instead (nothing is executed).

> **Breaking change (confirmation tokens):** `"confirm": true` no longer
> executes a destructive tool. A call that sends it without a token gets the
> preview, a token and a message explaining the new flow; repeat the call
> with the returned `confirmation_token` instead. The `confirm` parameter is
> gone from the schemas of the destructive tools (it is still accepted, and
> must still be a JSON boolean, so clients with a cached schema get the
> explanation rather than an unknown-argument error). `update_app` is now
> destructive and needs a token too. Scripts must make two calls: the
> preview, then the call with the token.
>
> **Earlier breaking change:** clients or scripts that called the
> destructive tools without `confirm` get a preview instead of the
> operation.
>
> **Breaking change (directory services):** `configure_directory_service`
> and `leave_directory_service` now return `status: "submitted"` and a
> `task_id` for the accepted job, not `success: true`. The job has been
> submitted, not completed; track it with `tasks_get`. Clients that keyed
> on `success` need to use `status` and the task id instead.

**Argument validation.** Before anything runs (including a dry run, the
confirmation preview or the check of a confirmation token), every `tools/call` is checked against the tool's own
input schema, as listed in `tools/list`:

- an argument the schema does not declare is rejected (`unknown argument`);
- a missing required argument is rejected (`... is required`);
- a value of the wrong JSON type is rejected, not ignored: a string
  `"true"` or a number for a boolean, a string `"10G"` for an integer such
  as `quota`, a fraction such as `1.5` for an integer, and so on. Integers
  are accepted as any whole JSON number (`5` or `5.0`);
- a value outside the schema's `enum`, `minimum`/`maximum` or `pattern`
  is rejected (for example a negative `quota` or a `limit` of 0), and nested
  objects and array items are checked the same way;
- `null` on an optional argument means the argument was omitted, at any
  level. `null` for a required argument is reported as missing, and `null`
  for `dry_run` or `confirm` is rejected (they must be `true` or `false`).

The error names the argument and what it must be, never the value, and ends
with `nothing was executed`. Inside app `values` (which have no fixed
schema), `host_path_config.acl_enable` must also be a JSON boolean.

> **Behaviour change:** what is stricter now is wrong JSON types,
> out-of-range numbers and values outside an enum. These used to be dropped
> silently and the call ran with the default (for example `"readonly":
> "true"` created a read-write dataset, `"quota": "10G"` created one with no
> quota, and `"share_type": "bogus"` returned an empty list); they now fail
> with an error. Missing required arguments were already refused; only the
> error text changes (destructive tools called with `{}` used to answer
> `Preview failed: ... is required`). `system_reboot`,
> which has no required arguments, still returns a confirmation preview for
> `{}`. `apply_update` also has no required arguments, but when
> `update.status` has no `new_version` the preview is blocked rather than a
> confirmation preview.

**Tool annotations.** Every tool in `tools/list` carries MCP
[tool annotations](https://modelcontextprotocol.io/specification/2025-03-26/server/tools)
(`readOnlyHint`, `destructiveHint`, `idempotentHint`, `openWorldHint: false`)
derived from the same table, so clients can apply their own approval prompts.
`destructiveHint` is true for the ten destructive tools (the ones that need a
confirmation token, `update_app` included) and false for every other tool.
Annotations were added in MCP 2025-03-26; clients that negotiate
`2024-11-05` ignore the extra field.

## TrueNAS version compatibility

> **Breaking change:** the minimum supported TrueNAS release is now 25.10.
> Earlier releases, including 25.04, are refused at startup. Clients that
> pointed this server at a 25.04 system need to upgrade TrueNAS (or use an
> older server build) before it will start.

The server stays on `/api/current` and records the connected release after
login. Tools the connected release cannot run are omitted from `tools/list`
and refused if called, with a message that names the tool and the version
it needs, and states that nothing was executed. The per-tool range lives
next to the safety table in `tools/version.go`.

All current tools are available on TrueNAS 25.10 and later, with no upper
bound:

| Tools | TrueNAS versions |
|-------|------------------|
| Every tool listed in [docs/full-features.md](docs/full-features.md) | 25.10 and later |

A later tool that exists only on 26, 27, or a single train will be added
to the table with a min or max version and will be hidden on releases
outside that range.

## Example Usage

Once connected via an MCP client, ask questions in natural language:

### Quick Start Examples

**Monitoring:**
- "What version of TrueNAS is running?"
- "Are there any system alerts?" (`list_alerts` lists active alerts; dismissed ones need `include_dismissed: true`)
- "Show me CPU and memory usage over the past day"

> **Behaviour change:** `list_alerts` called without arguments now lists only
> active alerts; dismissed alerts are no longer included. Pass
> `"include_dismissed": true` to list all alerts as before, or
> `"dismissed": true` for dismissed alerts only. `dismissed` and
> `include_dismissed` must be JSON booleans; any other value is rejected.

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

### Tasks for Long-Running Operations

Operations that take time to complete (like app upgrades) are tracked as tasks. These are ordinary tools (`tasks_list` and `tasks_get`) backed by the server's own background polling of TrueNAS jobs; the server does not implement the MCP tasks extension, which the Go MCP SDK does not support yet.

**How it works:**
1. Write operations (like `upgrade_app`) return a `task_id` instead of blocking. TrueNAS job methods return a job id as a bare integer. The tools also accept an array whose first element is the id, and a missing or invalid id is an error. Directory-service tools (`configure_directory_service`, `leave_directory_service`, `refresh_directory_cache`) still report `status: "submitted"` and add a warning when no job id is returned, so progress cannot be tracked
2. Tasks are automatically tracked in the background
3. Use `tasks_get` with the task ID to check progress
4. `upgrade_app` only reports success once the app is seen RUNNING after the upgrade job finishes. It waits `health_timeout_seconds` (an integer from 0 to 600, default 60; other values are rejected). If the app is merely still starting when the wait ends, the task fails with a message saying the upgrade itself succeeded and the app was slow to start; raise `health_timeout_seconds` for such apps. For a version upgrade TrueNAS stops the app and snapshots its ix-volumes dataset (if it has one); `snapshot_hostpaths` only controls whether host paths are also snapshotted. Custom apps and image-only updates are instead redeployed with updated images, without a stop or any snapshot, and the preview shows a single `redeploy` step saying so. While it waits, and once it is done, the task's `job.app` is the app as read from `app.query` (`app_source: "app.query"`, `app_checked_at`), not the copy embedded in the job result, so it agrees with `statusMessage`. A job state of `SUCCESS` (in `job.state` or `query_jobs`) only means the job finished, not that the app is healthy. Task IDs are held in memory; after a server restart, list jobs with `query_jobs` (`method: "app.upgrade"` narrows the list) and find the entry whose `id` matches the returned `job_id`.
5. Job output is summarized: each job summary (the `job` of `tasks_get`, each entry of `query_jobs`) is at most 4 KiB, with `*_truncated` flags when text was cut
6. Tasks update automatically - no manual polling needed
7. Each task shows the tool that started it (`tool`), the TrueNAS job it tracks (`job_id`) and, once the job succeeded, its `result`. The result is at most 4 KiB; a larger one is cut to a preview (an app entry is summarized) with `result_truncated: true` and `result_size_bytes`. `upgrade_app` tasks report the result in `job` instead. Results and error text are redacted before they are cut, so a secret cannot survive as a fragment at the cut. Task arguments are never returned.
8. TrueNAS job ids restart at 1 when the middleware restarts or the system reboots, so the server records each job's method and start time when the task is created and checks them on every poll. The job must also run the method the tool starts (for example `app.upgrade` for `upgrade_app`). If the job is gone (no row for 3 consecutive polls) or its id now belongs to a different job, the task fails with a message saying TrueNAS restarted and which tool shows the current state. For `apply_update` a reboot is expected; the message points to `system_info` and `update_status`.
9. If reading the job fails repeatedly (3 consecutive polls), `statusMessage` says so and the failure is logged to stderr; the task keeps working.
10. `ttl` is how long a task is tracked while it is working, in seconds from `createdAt`. A task still working when it runs out fails with "tracking timed out" (the operation may still be running on TrueNAS). Finished tasks (completed, failed or cancelled) stay readable for 24 hours after they finish; at most 500 are kept, and beyond that the ones that finished first are removed early. Tasks are held in memory and are lost when the server restarts.

**Example:**
```
User: "Install the web app"
→ Returns: {"task_id": "abc-123", "task_status": "working", "job_id": 42, ...}

User: "Check task abc-123"
→ Returns: {"taskId": "abc-123", "status": "completed", "tool": "install_app", "job_id": 42,
            "statusMessage": "Job completed successfully", "result": {...}, ...}
```

**Task States:**
- `working` - Operation in progress
- `completed` - Operation finished successfully
- `failed` - Operation encountered an error
- `cancelled` - The TrueNAS job was aborted

### Dry-Run Mode for Write Operations

Write operations support previewing changes before execution:

**How to use:**
Add `"dry_run": true` to any write operation to preview what would happen without making changes. Destructive tools return this preview automatically, with a confirmation token, when called without a valid `confirmation_token` (see [Safety Controls](#safety-controls)).

**Example:**
```
Tool: upgrade_app
Args: {"app_name": "plex", "dry_run": true, "health_timeout_seconds": 300}

Returns:
{
  "tool": "upgrade_app",
  "dry_run": true,
  "executed": false,
  "current_state": {
    "name": "plex",
    "version": "1.40.0_1.2.0",
    "state": "RUNNING"
  },
  "planned_actions": [
    {"step": 1, "operation": "stop", "description": "Stop application containers", ...},
    {"step": 2, "operation": "snapshot", "description": "Snapshot the app's ix-volumes dataset (if it has one) and its host paths (those that are ZFS datasets) before the upgrade, for rollback", ...},
    {"step": 3, "operation": "upgrade", "description": "Upgrade from 1.40.0_1.2.0 to 1.41.0_1.2.1 (catalog version 1.2.1, latest)", "details": {...upgrade_summary...}},
    {"step": 4, "operation": "start", "description": "Start application with new version", ...},
    {"step": 5, "operation": "verify", "description": "Verify the app reports RUNNING after the job finishes (waits up to 300s); ...", "details": {"health_timeout_seconds": 300}}
  ],
  "estimated_time": {
    "min_seconds": 30,
    "max_seconds": 600,
    "note": "Time varies based on image size and network speed: up to 300s for the upgrade job, plus up to 300s waiting for the app to report RUNNING (health_timeout_seconds)"
  }
}
```

`upgrade_app` previews carry `"dry_run": true, "executed": false` (also on the `confirmation_required` reply returned without a `confirmation_token`); a started upgrade carries `"dry_run": false, "executed": true`. When no `version` is given, the upgrade step names the version the middleware resolved "latest" to. For a custom app, or a catalog app whose only update is new images, the plan is instead `redeploy` (pull updated images and redeploy; no snapshot is taken) followed by `verify`, with a warning that there is no rollback point. A preview of a STOPPED app warns that TrueNAS rejects upgrading it. The changelog and version details come from the middleware's `app.upgrade_summary` and may be empty.

`start_app` builds one plan and uses it for both the dry-run preview and the real call, so the preview lists only the `app.start` middleware call that will be made. Preview `details` are derived from the same parameters as the real call and use named fields (masked for secrets) rather than raw positional parameters; the real call still sends those parameters. A dry run of a STOPPED app no longer warns that the app "must be STOPPED". If the app is already RUNNING, or is DEPLOYING or STOPPING, both the preview and the real call return an error and do not start it (DEPLOYING and STOPPING say to retry when the app settles). A CRASHED app can still be started.

`stop_app` uses the same shared plan for its preview and the real `app.stop` call. A malformed app query or a non-string app state is a parse error, not a misleading "app not found" or a warning that prints the raw value. Stopping an already STOPPED app is not refused.

`delete_boot_environment` also uses the shared plan. Preview `details` come from the same parameters as the real `boot.environment.destroy` call. A boot environment that is running, selected for the next reboot, or protected is listed in the plan's `blocked` field. An explicit dry run, a call without a token, and a confirmed call all return an error-shaped result with `"status": "blocked"` and the preview (with `deletion_allowed` false and no planned action); nothing is destroyed and no token is issued. `start_app` uses the same blocked dry-run shape when the app is RUNNING, DEPLOYING, or STOPPING.

> **Breaking change:** an explicit `dry_run` of a blocked `delete_boot_environment` (the running, next-boot, or protected environment) now returns an error result with `"status": "blocked"` and the preview, instead of a successful preview. Clients that treated a blocked dry run as a normal result need to read the error result instead.

`delete_app` and `delete_scrub_schedule` use the same shared plan for preview and the real call (`app.delete` and `pool.scrub.delete`). Preview `details` are derived from those parameters. Host-path datasets are not deleted with the app; the preview warns that data paths are preserved.

`upgrade_app` and `update_app` use the same shared plan for preview and the real call (`app.upgrade` and `app.update`). The upgrade preview still describes the stop, snapshot, upgrade, start and verify steps (or redeploy and verify for a custom app or image-only update) that a single `app.upgrade` performs; those middleware parameters are what the confirmed call sends. An `update_app` dry run now includes `"dry_run": true, "executed": false` like the other plan-backed previews. Missing host-path datasets stay a preview warning; the real call still refuses until they exist. When `upgrade_app` is called with `version` omitted or `"latest"`, the confirmation token is also bound to the catalog version `app.upgrade_summary` resolved in the preview (`upgrade_version`). If a newer release is published between preview and confirm, the token is refused and nothing is upgraded; the `app.upgrade` call still sends `"latest"`. Preview `details` and `current_state` on these plans (and on any plan that uses custom preview actions) are masked the same way as other plan previews.

`system_reboot` and `apply_update` use the same shared plan for preview and the real call (`system.reboot` and `update.run`). A reboot delay of 0 is omitted from the middleware payload, as before. Their dry runs now include `"dry_run": true, "executed": false` like the other plan-backed previews. A confirmed call re-reads `system.info` (and, for `apply_update`, `update.status`) before writing, and fails closed if those reads fail. The confirmation token for `apply_update` is also bound to `update.status`'s `status.new_version.version` (the 25.10 `UpdateStatus` shape: `new_version` is nested under `status`, not at the top level). If that version changes between preview and confirm, the token is refused and `update.run` is not sent; the `update.run` payload is unchanged (`{"reboot": ...}` only). When `update.status` reports `code: "ERROR"` or a null `status`, the preview is blocked with that reason.

> **Breaking change:** an explicit `dry_run` or a call without a token of `apply_update` when `update.status` has no `new_version` (or reports `code: "ERROR"` or a null `status`) now returns an error result with `"status": "blocked"` and the preview, instead of a successful preview. Clients that treated that case as a normal result need to read the error result instead. A confirmed call is also refused; nothing is applied.

`configure_directory_service` and `leave_directory_service` use the same shared plan for preview and the real call (`directoryservices.update` and `directoryservices.leave`). The middleware payloads are unchanged. Credentials such as bind passwords, and LDAP `auxiliary_parameters` (free text that can contain secrets such as `ldap_default_authtok`), are masked in the preview and are never written back in the result or stored task arguments. The same exact-key mask is also applied to read-only directory config output from `query_directory_services`, so `auxiliary_parameters` is hidden there too. A confirmed `configure_directory_service` now also reads `directoryservices.status` (as the preview already did) and fails closed if that read fails. Their dry runs now include `"dry_run": true, "executed": false` like the other plan-backed previews.

`download_update`, `dismiss_alert`, `restore_alert`, `refresh_directory_cache`, `create_scrub_schedule`, and `run_scrub` use the same shared plan for preview and the real call. Their dry runs now include `"dry_run": true, "executed": false` like the other plan-backed previews. Jobs started by `download_update` and `run_scrub` are tracked with `WithExpectedMethod` (`update.download` and `pool.scrub.scrub`).

`download_update` passes optional `train` and `version` through to `update.download`. Both must be set, or both omitted (matching 25.10's `UpdateDownloadArgs` and `plugins/update_/download.py`). When both are omitted, or both are blank or whitespace, they are treated as omitted and the middleware call still has empty parameters (`params: []`) and downloads the update offered by the configured profile, as before. When both are set, the call is `update.download` with those two positional arguments. A request for only one of them is refused. A completed download (`update_download_progress.percent == 100`) is not treated as already done if the caller asked for a different `version`.

> **Behaviour change:** `download_update` no longer ignores `train` and `version`. Clients that sent those fields expecting the configured train must now omit both.

A dry run of `refresh_directory_cache` now returns an error when no directory service is configured or the service is not enabled, matching the real call, instead of a successful preview with a warning. A dry run of `create_scrub_schedule` now errors when a schedule already exists, matching the real call. A dry run of `run_scrub` now matches the real call: if a finished scrub is still within the threshold, the preview reports that no scrub will start (the real call already returned `scrub_started: false`). If a scrub is already running, or the pool cannot be scrubbed, the dry run returns an error instead of a successful preview with a warning.

`install_app`, `create_dataset`, `create_smb_share`, and `create_nfs_share` use the same shared plan for preview and the real call (`app.create`, `pool.dataset.create`, `sharing.smb.create`, and `sharing.nfs.create`). `create_smb_share` and `create_nfs_share` reject a path that contains a `..` segment after cleaning, or that resolves outside `/mnt/<pool>` (the pool is the first component after `/mnt/` on the original path). The middleware payloads are unchanged: `install_app` still sends `version` as `"latest"` when it was omitted, and dataset and share bodies keep the same fields and defaults. Jobs started by `install_app` are tracked with `WithExpectedMethod` (`app.create`). Secrets inside `install_app` `values` (passwords, tokens, and similar, including nested ones) are masked in the preview, in rejection and error text, in stored task arguments, and in debug logs; they are still sent to the middleware on the real call. Their dry runs now include `"dry_run": true, "executed": false` like the other plan-backed previews. Dataset ancestor lists and share security warnings appear in the plan preview (`current_state` / `warnings`) rather than as custom top-level fields. A dry run of `install_app` now matches the real call: a missing host-path dataset or an existing instance name is an error on both paths, not a successful preview with a warning, and a catalog version with no schema is still refused on both. `create_dataset` preflight (the pool must exist and the name must be free) now also runs on the real call.

> **Behaviour change:** dry-run previews for `create_dataset`, `create_smb_share`, and `create_nfs_share` now use the shared plan shape (`current_state`, `planned_actions`, `warnings`, `"dry_run": true, "executed": false`) instead of a custom top-level `payload` / `security_warnings` object. Clients that read those old top-level fields should use the plan preview. A dry run of `install_app` now returns an error when a host-path dataset is missing or the instance name is already in use, matching the real call, instead of a successful preview with a warning.

`refresh_directory_cache` reports `status: "submitted"` (with `task_id` / `job_id` when TrueNAS returns a job id, tracked with `WithExpectedMethod` for `directoryservices.cache_refresh`) instead of `cache_refreshed: true` as soon as the job is submitted. A first Active Directory cache fill can take a significant amount of time; track the task for the result.

> **Behaviour change:** `refresh_directory_cache` no longer reports `cache_refreshed: true` when the `directoryservices.cache_refresh` job is only submitted. Clients should treat `status: "submitted"` like the other directory-service tools and poll the task.

`get_app_catalog_details` returns the app schema (groups and questions, including each question's defaults) and, when the schema defines them, a `defaults` object with run-as user/group and resource limits taken from `catalog.get_app_details`. Resource defaults keep the schema nesting (`defaults.resources.limits.cpus` and `defaults.resources.limits.memory`). Only numeric defaults are passed through. When the schema has `run_as_custom_user` and its default is false, `run_as` is omitted because those values only apply when that flag is true. When the schema has `enable_resource_limits` and its default is false, `defaults.resources.limits` is omitted for the same reason. Those defaults are omitted when the schema has none. Workflow guidance for installation (review schema groups, query pools, create datasets, use `host_path`, use schema defaults when present) lives in the `get_app_catalog_details` and `install_app` tool descriptions, worded so any MCP client can follow it.

> **Behaviour change:** `get_app_catalog_details` no longer includes a `wizard_guidance` object with client-specific instructions (for example a named tool of one MCP client) or fixed run-as and resource defaults. Clients that read `wizard_guidance` should use the schema and the `defaults` field (when present) instead, and follow the tool descriptions for the install workflow.

**Benefits:**
- See exactly what will change before committing
- Understand prerequisites and warnings
- Get time estimates for operations
- Build confidence before making changes

## Development

Go 1.27.3 or later is required (see [Building](#building)).

```bash
# Run linters (go vet, gofmt, staticcheck v0.8.2, and errcheck v1.30.0)
make lint

# Run tests (includes the race detector)
make test

# Run tests without the race detector (faster)
make test RACE=

# Check dependencies for known vulnerabilities (version used in CI)
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

# Clean build artifacts
make clean
```

The "Build and Package" workflow runs the linters (`make lint`, including
staticcheck and errcheck), a single test step with the
race detector and coverage (`go test -race -coverprofile=coverage.out -covermode=atomic ./...`), a non-race `go test -count=1 ./redact` so the randomized and linear redaction tests run at full strength, `govulncheck`
and builds for macOS (Apple Silicon), Linux and Windows on every pull request
and on every push to `main`. Those binary builds, and the release workflow's
builds, set `CGO_ENABLED=0`. After the Linux binary is built, CI runs `file`
and `ldd` and fails if it is not statically linked. Test steps leave cgo
enabled, because the race detector requires it. The release workflow, which runs for version
tags, runs the linters and tests again (with the race detector) and publishes those three platforms, with SLSA build provenance attestations for the release archives. A test in
`cmd/truenas-mcp` checks that the flags documented in this README match the
flags the binary registers, and that the Go version above matches `go.mod`.
