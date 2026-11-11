# Configuration

How to build the server, every flag and environment variable, proxies, and troubleshooting. For setup basics see the [README](../README.md).

## Building from source

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

## Flags

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
- `--read-only` - Hide and refuse every tool that changes the system (or set `TRUENAS_MCP_READ_ONLY=1`); see [Safety controls](safety.md#safety-controls)
- `--allow-tools` - Comma-separated list of the only tools to expose (or use `TRUENAS_MCP_ALLOW_TOOLS`)
- `--deny-tools` - Comma-separated list of tools to hide and refuse (or use `TRUENAS_MCP_DENY_TOOLS`)
- `--require-human-confirm` - Refuse destructive tools on clients that do not support MCP elicitation, instead of falling back to a confirmation token (or set `TRUENAS_MCP_REQUIRE_HUMAN_CONFIRM=1`); see [Safety controls](safety.md#safety-controls)
- `--version` - Print version and exit

Examples:

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

## API key

The key inherits its owner's privileges. Give that user the narrowest TrueNAS role that covers the tools you will enable, not a full administrator account by default. For a `--read-only` deployment, assign the [Read-Only Administrator](https://www.truenas.com/docs/scale/25.10/scaletutorials/credentials/adminroles/) privilege (`READONLY_ADMIN`) so TrueNAS itself rejects writes, not only this binary. Otherwise use the narrowest built-in or custom privilege that covers the write and destructive tools you enable (for example Sharing Administrator if you only expose share tools). See [Using Administrator Logins](https://www.truenas.com/docs/scale/25.10/scaletutorials/credentials/adminroles/) and [Managing API Keys](https://www.truenas.com/docs/scale/25.10/scaletutorials/toptoolbar/managingapikeys/).

Pass the key in one of these ways:

- `TRUENAS_API_KEY` in the MCP client's `env` block (recommended).
- `TRUENAS_API_KEY_FILE` (or `--api-key-file`) pointing at a file that contains the key. On Unix the file must
  not be readable by group or others (mode `0600`). One trailing newline is
  trimmed. Do not also set `TRUENAS_API_KEY` or `--api-key`.
- `--api-key` in `args` is still accepted, but the key is visible in the
  process list.

## TLS with a self-signed certificate

TrueNAS ships with a self-signed
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

To export the certificate: TrueNAS UI → Credentials → Certificates, or
`openssl s_client -connect 192.168.0.31:443 </dev/null 2>/dev/null | openssl x509 > truenas-cert.pem`

## Startup and connection

The binary connects directly to TrueNAS middleware's WebSocket endpoint:

1. **Uses secure WebSocket (wss://)**: Connects to the JSON-RPC 2.0 endpoint `wss://your-truenas:443/api/current` (TrueNAS 25.10 or later; no `/websocket` fallback)
2. **Verifies certificates by default**: trust the TrueNAS self-signed certificate with `--tls-ca` (see [TLS with a self-signed certificate](#tls-with-a-self-signed-certificate))
3. **Authenticates via API key**: Logs in with `auth.login_ex` (`API_KEY_PLAIN` mechanism, user from `--api-user`)

The connection, login, and version check happen once, at startup, before the server reads any MCP request. If TrueNAS cannot be reached, the login fails, or the release is older than 25.10, the server logs the reason to stderr (`Could not connect to TrueNAS (no login was attempted): ...` when the connection fails, `Failed to authenticate with TrueNAS: ...` when the login is rejected, `TrueNAS 25.04.2 is not supported; TrueNAS 25.10 or later is required` when the version is too old, or `invalid TrueNAS host or URL: ...` for an unusable host setting) and exits with a non-zero status instead of starting without a working connection. This is intentional: the MCP client then shows the server as failed, and its log holds that message. Fix the cause (host, port, proxy, certificate, API key, or TrueNAS version, see [Troubleshooting](#troubleshooting)) and restart the client. After a reconnect mid-session (for example after a reboot), the version is read again and tool calls are refused if the new release is below 25.10.

## Debug logging

By default, logs written to stderr contain only metadata (method names, request
IDs, response sizes) - never request or response bodies. Set `TRUENAS_MCP_DEBUG=1`
(or pass `--debug`) to log full request/response bodies for troubleshooting,
and to log middleware error traces (Python tracebacks) to stderr. Those traces
are not included in tool errors sent to the MCP client.

Even in debug mode, credentials are never logged: API keys and other
credential-bearing values (`password`, `bindpw`, `secret`, `token`, etc.) are
replaced with `[REDACTED]`, so debug logs are safe to share in bug reports.

## Proxies

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

## Troubleshooting

**Connection Issues:**
- "invalid TrueNAS host or URL: ...": `--truenas-url` / `TRUENAS_URL` is unusable (bad port, user info in the URL, `https://`, ...); the message says why. Use the host name alone (`192.168.0.31`, `truenas.example.com:8443`, `[fd00::1]:8443`) or a `wss://host[:port]` URL
- A `.local` name such as `truenas.local` does not resolve on Linux with the static release binary (or `make build`), because that binary does not use mDNS/nss-mdns. Use the IP address, an `/etc/hosts` entry, or a DNS name, or build from source with cgo (`CGO_ENABLED=1 go build`). macOS and Windows builds are unaffected, because cgo was already off for those cross-builds
- "Could not connect to TrueNAS (no login was attempted)" means the TCP, proxy, TLS or WebSocket step failed; "Failed to authenticate with TrueNAS" means the connection worked and the login was rejected. If the connection drops during login and reconnecting fails, the message says so ("the connection was lost during login, and reconnecting failed") instead. The URL is logged without any query, fragment or user info, and error messages never quote them (at most they quote the host name or a short URL scheme)
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
- Create a new API key (user menu → **My API Keys**, or **Credentials → Users** → **View API Keys**)
- Check that the key wasn't accidentally truncated when copying
- Check that `--api-user` / `TRUENAS_API_USER` names the user that owns the key

## TrueNAS version compatibility

> **Breaking change:** the minimum supported TrueNAS release is now 25.10.
> Earlier releases, including 25.04, are refused at startup. Clients that
> pointed this server at a 25.04 system need to upgrade TrueNAS (or use an
> older server build) before it will start.

The server stays on `/api/current` and records the connected release after
login. Tools the connected release cannot run are omitted from `tools/list`
and refused if called, with a message that names the tool and the version
it needs, and states that nothing was executed. The per-tool range lives
next to the safety table in `tools/version.go`. All current tools run on
TrueNAS 25.10 and later.
