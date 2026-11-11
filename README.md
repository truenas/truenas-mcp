# TrueNAS MCP Server

> **⚠️ Early Access**
> This project is in early access. Destructive operations require explicit confirmation, but tools, arguments and outputs may still change between releases. Review each confirmation carefully and use caution in production environments.

An [MCP](https://modelcontextprotocol.io) server for TrueNAS. It runs on your computer, connects to the TrueNAS API, and lets an AI assistant such as Claude query and manage the system in plain language.

## Features

- **Monitoring:** system info and health, alerts, jobs, CPU, memory, disk, network, ARC and UPS metrics.
- **Storage:** pools, datasets, snapshots, and SMB and NFS shares.
- **Apps:** catalog search, install, start, stop, upgrade, update and delete.
- **Maintenance:** system updates, boot environments and pool scrubs.
- **Directory services:** Active Directory, LDAP and FreeIPA status and setup.
- **Capacity planning:** usage analysis and growth trends.
- **Virtual machines:** VM status.

Safety: `--read-only` hides every tool that changes the system. Destructive actions need your explicit confirmation, and secrets are redacted from everything sent back to the assistant.

See [docs/full-features.md](docs/full-features.md) for every tool and [docs/examples.md](docs/examples.md) for example questions.

## Install

You need TrueNAS 25.10 or later.

### 1. Get the binary

Download the archive for your platform from the [Releases page](https://github.com/truenas/truenas-mcp/releases): macOS (Apple Silicon), Linux (x86-64) or Windows (x86-64). Put the binary on your `PATH` as `truenas-mcp`:

```bash
tar -xzf truenas-mcp-linux-amd64.tar.gz
sudo install truenas-mcp-linux-amd64 /usr/local/bin/truenas-mcp
```

On Windows, unzip `truenas-mcp-windows-amd64.zip` and use the full path to the `.exe` as the `command` in your client config. Each release has a `checksums.txt` to verify the download.

To build it yourself (needed on Intel Macs), you need Go 1.27.3 or later:

```bash
git clone https://github.com/truenas/truenas-mcp.git
cd truenas-mcp
make build    # writes ./truenas-mcp
```

### 2. Create a TrueNAS API key

In the TrueNAS web UI, open the user menu in the top-right corner and choose **My API Keys**, or go to **Credentials → Users**, select the user, and click **View API Keys**. Note the user that owns it (for example `truenas_admin`). The key has that user's privileges, so use the narrowest role that covers what you need: Read-Only Administrator is enough for `--read-only`.

### 3. Configure your MCP client

The basic settings are:

| Setting | Environment variable | Flag |
|---------|----------------------|------|
| TrueNAS host or `wss://` URL | `TRUENAS_URL` | `--truenas-url` |
| API key owner | `TRUENAS_API_USER` | `--api-user` |
| API key | `TRUENAS_API_KEY`, or a file in `TRUENAS_API_KEY_FILE` | `--api-key-file` |
| Read-only mode | `TRUENAS_MCP_READ_ONLY=1` | `--read-only` |

Put the key in the environment or a key file, not in `args`, where it shows up in the process list. Write tools are available unless you turn on read-only mode; `--allow-tools` and `--deny-tools` narrow the set further.

TrueNAS uses a self-signed certificate by default. Point `TRUENAS_TLS_CA` at a copy of it, or set `TRUENAS_INSECURE=1` if you accept the risk.

**Claude Desktop:** edit `claude_desktop_config.json` (on macOS in `~/Library/Application Support/Claude/`, on Windows in `%APPDATA%\Claude\`, on Linux in `~/.config/Claude/`), then restart Claude Desktop:

```json
{
  "mcpServers": {
    "truenas": {
      "command": "truenas-mcp",
      "args": ["--read-only"],
      "env": {
        "TRUENAS_URL": "192.168.0.31",
        "TRUENAS_API_USER": "truenas_admin",
        "TRUENAS_API_KEY": "your-api-key"
      }
    }
  }
}
```

Leave out `--read-only` to allow changes.

**Claude Code:**

```bash
claude mcp add truenas \
  --env TRUENAS_URL=192.168.0.31 \
  --env TRUENAS_API_USER=truenas_admin \
  --env TRUENAS_API_KEY=your-api-key \
  -- truenas-mcp --read-only
```

Then ask something like "What version of TrueNAS is running?"

**`.local` names on Linux:** the static Linux release binary can't resolve `.local` (mDNS) names such as `truenas.local`. Use the IP address, an `/etc/hosts` entry or a DNS name, or build with `CGO_ENABLED=1 go build ./cmd/truenas-mcp`. macOS and Windows aren't affected.

[docs/configuration.md](docs/configuration.md) lists every flag and covers proxies, TLS, debug logging and troubleshooting. [docs/safety.md](docs/safety.md) covers read-only mode, confirmation and dry runs, redaction, and protocol details for client authors.

## Development

`make lint` runs the linters and `make test` runs the tests with the race detector.

## License

GPL-3.0. See [LICENSE](LICENSE).
