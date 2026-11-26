# Safety, confirmation and protocol details

How the server decides what a tool may do, how destructive calls are confirmed, what is redacted, and how previews and tasks behave. Written mainly for MCP client authors and for anyone who wants to know exactly what the server does. For setup see the [README](../README.md); for flags see [configuration.md](configuration.md).

## MCP protocol

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
  [Safety controls](#safety-controls) is refused with a tool error
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

## Safety controls

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

**Confirmation for destructive tools.** These fifteen tools execute only after
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
| `run_snapshot_task` | Starts the replication tasks bound to the snapshot task, whose retention can delete target snapshots; the task's own retention can remove old snapshots |
| `run_replication` | Retention (`SOURCE` or `CUSTOM`) deletes target snapshots after the transfer, and `allow_from_scratch` can destroy the target dataset's data when no common snapshot exists |
| `update_network_config` | Gateways, nameservers and hostname apply at once with no automatic rollback; a wrong gateway can cut off every remote client, including this server |
| `update_network_interface` | Reconfigures interfaces and can cut off clients; TrueNAS rolls it back unless it is checked in (see [Network check-in window](#network-check-in-window)) |
| `release_snapshot` | Removes the `truenas` hold, so the snapshot is deletable again: a manual delete, or periodic snapshot task or replication retention, can then remove it. Refused if the snapshot has any other hold |

No other tool needs a token: the other write tools (`create_dataset`,
`create_smb_share`, `create_nfs_share`, `install_app`, `start_app`,
`download_update`, `create_scrub_schedule`, `run_scrub`, `create_snapshot`,
`clone_snapshot`, `hold_snapshot`, `dismiss_alert`, `restore_alert`, `refresh_directory_cache`,
`checkin_network_changes`, `rollback_network_changes`) run directly and keep their
optional `dry_run`, and read tools never take one.

**Elicitation (primary path).** If the client declared the MCP form
elicitation capability, a destructive call without a valid token runs the
preview and then sends an elicitation whose message names the tool, the
target, a short headline of what will happen, and the key facts a human
needs to approve it (for example current and new versions and whether a
reboot will follow for `apply_update`; the keys and storage paths
`update_app` will change; the object name and state for delete and stop
tools). Those facts are redacted with the same rules as other tool output
and are cut at 2 KiB with a "more changes not shown" line when the change
set is larger. The message also includes the main warnings, and has one
required boolean field,
`approved`. The server uses the official Go MCP SDK for this: mid-call
`elicitation/create` on protocol revisions 2025-06-18 and 2025-11-25, and a
tool result with `inputRequests` (the client retries with the answer) on
2026-07-28. The call executes only on `accept` with `approved` true, and
only the plan that was previewed. The prompt is valid for 5 minutes. A
declined, cancelled, failed or expired prompt executes nothing and does not
fall back to a confirmation token. A blocked preview (the operation cannot
proceed) is an error, issues no prompt, and executes nothing. Dry-run
`planned_actions` for `apply_update` and `download_update` list the
middleware call and the versions read from the system, not invented
connect, stop, or verify steps.


`--require-human-confirm` (or `TRUENAS_MCP_REQUIRE_HUMAN_CONFIRM`) never
issues a confirmation token and never redeems one. Destructive tools on
clients that did not declare form elicitation are refused; a client that
declared it is prompted, even if the call carried a `confirmation_token`.
Dry runs and `--read-only` are unchanged.

**Token flow (fallback).** Clients that do not declare elicitation use a
confirmation token:

1. Call the tool without a token. Nothing is executed. The reply is a
   normal (non-error) result with `"status": "confirmation_required"`,
   `"executed": false`, the tool's dry-run `preview` (including a
   `confirm_summary` with the same headline and key facts as the
   elicitation message, already redacted), a `confirmation_token`
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
`apply_update`, `configure_directory_service`, `leave_directory_service`,
`run_snapshot_task`, `run_replication`, `update_network_config`,
`update_network_interface`, `release_snapshot`), the token is also bound to
a hash of the planned middleware method and parameters, so it cannot
execute a different planned operation. For `release_snapshot` the hash also
covers the previewed hold tags, hold count (`userrefs`) and the snapshot's
`createtxg`, so a hold added after the preview, or the snapshot being
recreated under the same name, rejects the token. The token store does not hold those
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
`destructiveHint` is true for the fifteen destructive tools (the ones that need a
confirmation token, `update_app` included) and false for every other tool.
Annotations were added in MCP 2025-03-26; clients that negotiate
`2024-11-05` ignore the extra field.

### Network check-in window

`update_network_interface` uses the TrueNAS test-changes flow. After the
change is approved it stages it (`interface.update`) and applies it
(`interface.commit` with `rollback: true`). TrueNAS then waits
`checkin_timeout_seconds` (default 60, 15 to 600) for a check-in and rolls
the change back by itself if none arrives. The window starts after approval,
so the confirmation prompt does not use it up.

The reply says `status: "awaiting_checkin"` with `remaining_seconds`,
`rollback_at` and the addresses now on the interface. Verify with
`query_network_interfaces` or `get_pending_network_changes`, then call
`checkin_network_changes` to keep the change or `rollback_network_changes`
to undo it. Those two need no token: they only end a change that was already
approved, and a token round trip would eat into the window.
`get_pending_network_changes` reports `outcome` for the last change:
`checked_in`, `rolled_back`, `changed_elsewhere` or `unknown`.

If the connection drops during the commit, the server reconnects for up to
20 seconds. When the change is waiting for check-in, it reports
`awaiting_checkin` with `reconnected: true`. TrueNAS keeps applying the
commit after the connection drops and arms the check-in timer only when the
apply finishes, so while changes are pending without a timer the server
keeps checking until the timer appears. If it does not appear within those
20 seconds, the server reports `unknown` and rolls nothing back; call
`get_pending_network_changes` to see the state. When nothing is pending any
more, it reports the outcome it finds (for example `rolled_back`). If it
cannot reconnect, it returns `connection_lost` with the rollback time:
TrueNAS reverts the change without a check-in.

`checkin_network_changes` only checks in a change this server committed. A
change waiting for check-in from elsewhere (for example the web UI) is
refused: check it in where it was made, or revert it with
`rollback_network_changes`.

The change is refused when interface changes are already pending (for
example from the web UI), when that cannot be checked, and when it would
remove the address this server is connected through. Only one
`update_network_interface` call runs at a time. If the commit fails, the staged change is discarded.

`update_network_config` has no such window. TrueNAS applies gateways,
nameservers and the hostname immediately and never rolls them back, so the
preview shows the previous values to restore by hand. It is refused while
interface changes are pending (or when that cannot be checked), because an interface rollback also restores
the IPv4 gateway. The interface tools need an API key with the
`NETWORK_INTERFACE_WRITE` role.

## Security and output redaction

- **Authentication**: TrueNAS API key required for all operations; `truenas-mcp setup` creates one from a one-time password login (see [Setup security notes](#setup-security-notes))
- **TLS/SSL**: Only supports wss:// (encrypted) - ws:// is rejected for security
- **Certificate verification**: On by default; trust a self-signed certificate with `--tls-ca`, pin its fingerprint with `--tls-pin` / `TRUENAS_TLS_PIN`, or opt out explicitly with `--insecure`
- **Network**: Client-only (no listening ports, all connections outbound)
- **API Key Storage**: Prefer `TRUENAS_API_KEY` in the client's `env` block, or `--api-key-file` / `TRUENAS_API_KEY_FILE` pointing at a mode `0600` file. `--api-key` is supported but visible in the process list
- **Log Safety**: Credentials are redacted from all log output and error messages; full request/response bodies are only logged in debug mode (see [Debug logging](configuration.md#debug-logging))
- **Tool Output Redaction**: Secrets are always removed from tool results and error messages before they reach the MCP client (and the model), with no opt-out. Values under credential-bearing names (`password`, `DB_PASS`, `adminPwd`, `bindpw`, `secret`, `token`, `api_key`, `private_key`, `passphrase`, `credential`, `pin`, `otp`, `cookie`, `authorization`, password hashes, a key named `key` or ending in `_key`, ...) are replaced with `[REDACTED]`; the key is kept so the model knows the field exists, and a whole object or list under such a name is replaced. `{name, value}` environment entries and `NAME=VALUE` strings are redacted by name. Free text, including strings inside JSON results such as job errors, is also scrubbed of PEM private key blocks, `key=value`/`"key": "value"` pairs, Bearer tokens and passwords in URLs. This is a safety net, not a substitute for restricting the API key's permissions
- **Allow-listed tool output**: `query_pools`, `query_shares`, `list_alerts`, `check_updates`, `query_replications`, `query_snapshot_tasks`, `query_users` and `query_groups` project each middleware record onto a fixed field list before that safety net runs, so unlisted keys (including surprise secret-looking fields, untrimmed pool vdev stats, and SMB `auxsmbconf`) never reach the client. `query_replications` reduces the SSH connection to its keychain id and leaves out SSH host, user, key and host key details, encryption keys and key locations, and netcat addresses. `query_pools` keeps leaf `disk` and `device` names, a stats object trimmed to read/write/checksum errors, and RAIDZ expansion progress, so a degraded pool can still be mapped to physical disks. `dismiss_alert` and `restore_alert` dry-run previews use the same alert field list, so raw `mail` and `key` values are not passed through in `current_state`. `get_app_config` still returns the full values object so it can be edited and passed to `update_app`; redaction remains the backstop there. Secrets in that object come back as `[REDACTED]`; `update_app` puts the stored value back for those fields, `configure_directory_service` does the same for secrets copied from `query_directory_services`, and `install_app` refuses the marker (see [Dry runs and previews](#dry-runs-and-previews)). Use a least-privilege API key role as well (see [Security Best Practices](#security-best-practices))
- **Tool Output Size**: Tool results and errors sent to the MCP client are capped at 100 KiB. A larger value is redacted first, then cut on a rune boundary, and a notice is appended saying the output was truncated and to narrow the query with `limit`, `select`, or filters. Redacting first prevents a secret from surviving as a fragment at the cut. Middleware error traces (Python tracebacks) are never sent to the client; with `--debug` they are logged to stderr, still redacted

### Setup security notes

`truenas-mcp setup` handles a password, so it is stricter than the server:

- **No password on the command line.** There is no password or OTP flag. Values come from a hidden prompt, from `TRUENAS_SETUP_PASSWORD` / `TRUENAS_SETUP_OTP`, or from stdin. The variables are read once and removed from setup's environment at once, so child processes do not inherit them. On Linux, though, `/proc/<pid>/environ` keeps showing the values setup was started with to the same user and to root for as long as setup runs (a few seconds); where that matters, use the hidden prompt or `--password-stdin` / `--otp-stdin`. A value given to `--password-stdin=` or `--otp-stdin=` is refused without being echoed, and an empty password or code line is refused locally. Agent tools can pass a value from a masked form this way without it entering the chat transcript.
- **Never written or logged.** The password and code are not written to disk, the config, logs, error text or `--debug` output. If TrueNAS echoes one back in an error, it is replaced with `[REDACTED]`. The new key is written only to the key file, never printed, and the client snippet references the file.
- **Memory is best effort.** Input buffers are zeroed after use, but Go copies strings (into the request sent to TrueNAS, and the environment value itself), and those copies cannot be cleared. They live until the process exits, which is a few seconds.
- **Certificates.** Verification is on and the connection is always `wss://`. An untrusted certificate stops setup before anything is sent; you trust it by typing `yes` after checking its fingerprint, or with `--trust-fingerprint`. `--insecure` needs a typed confirmation word, and the printed configuration then sets `TRUENAS_INSECURE=1`; prefer the pin.
- **Least privilege.** A key inherits its owner's roles. Setup warns about a `FULL_ADMIN` owner and needs a typed confirmation or `--yes-full-admin`. Use `--key-owner` with a dedicated user that has a limited role. Keys expire after one year by default.
- **No orphaned keys.** If the key file cannot be saved, the key is deleted from TrueNAS (after `--rotate`, the whole key record, and the error says so). If the reply to the create or reset call is lost, the error names the key that may exist without anyone holding it, so you can check and delete it in the web UI. `--rotate` does not replace a key file that holds a different key, or a key file that is a symlink (it would replace the link, not its target), unless `--force` is given. If the new key then fails to log in, setup says so and keeps the file so you can revoke the key.

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

1. **Generate a dedicated API key** for MCP use only, owned by a user whose privilege is no broader than the tools you enable
2. **Use environment variables or `--api-key-file`** for API keys; do not pass `--api-key` on the command line (it is visible in the process list)
3. **Restrict the API key's privileges** to the minimum required: [Read-Only Administrator](https://www.truenas.com/docs/scale/25.10/scaletutorials/credentials/adminroles/) (`READONLY_ADMIN`) for `--read-only` deployments, so TrueNAS itself rejects writes, otherwise the narrowest role that covers the enabled write tools
4. **Rotate API keys periodically**
5. **Start in `--read-only` mode** and enable write tools only where you need them (see [Safety controls](#safety-controls))

## Tasks for long-running operations

Operations that take time to complete (like app upgrades) are tracked as tasks. These are ordinary tools (`tasks_list` and `tasks_get`) backed by the server's own background polling of TrueNAS jobs; the server does not implement the MCP tasks extension, which the Go MCP SDK does not support yet.

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

## Dry runs and previews

Add `"dry_run": true` to any write operation to preview what would happen without making changes. Destructive tools return this preview automatically, with a confirmation token, when called without a valid `confirmation_token` (see [Safety controls](#safety-controls)).

> **Behaviour change:** `list_alerts` called without arguments now lists only
> active alerts; dismissed alerts are no longer included. Pass
> `"include_dismissed": true` to list all alerts as before, or
> `"dismissed": true` for dismissed alerts only. `dismissed` and
> `include_dismissed` must be JSON booleans; any other value is rejected.

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

`delete_app` and `delete_scrub_schedule` use the same shared plan for preview and the real call (`app.delete` and `pool.scrub.delete`). Preview `details` are derived from those parameters. Host-path datasets are not deleted with the app. The `delete_app` preview loads the app's current values with `app.config` (the 25.10 user-specified values object) and lists host-path locations in `current_state.storage_paths` and in the preserved-path warnings: storage slots and `additional_storage` (`host_path_config.path`, or `acl.path` when `acl_enable` is set, plus bare host-path entries), and Compose service `volumes` bind mounts (short-syntax strings and long-syntax entries with `type: bind` and a `source`). Other arrays, such as `command` or `entrypoint`, are not treated as mounts. `ix_volume` slots are not host paths and are omitted; this tool does not send `remove_ix_volumes`. The confirmation token remains bound only to the planned method and parameters, so filling `storage_paths` changes the preview contract but not the plan hash.

`upgrade_app` and `update_app` use the same shared plan for preview and the real call (`app.upgrade` and `app.update`). The upgrade preview still describes the stop, snapshot, upgrade, start and verify steps (or redeploy and verify for a custom app or image-only update) that a single `app.upgrade` performs; those middleware parameters are what the confirmed call sends. An `update_app` dry run now includes `"dry_run": true, "executed": false` like the other plan-backed previews. Storage may be omitted on `update_app` to leave current storage unchanged, including existing `ix_volume` entries. If a partial storage object is sent, omitted slots are filled from the current config before `app.update` is called, so they do not revert to schema defaults. When a provided slot keeps the same type and location, omitted top-level keys (such as `read_only`) and omitted fields of `host_path_config`, `nfs_config`, and `cifs_config` are also filled from the current slot. New storage entries, and type or location changes that are not a same-type NFS or CIFS server/path change, must be `host_path` with a cleaned path under `/mnt/<pool>/` on an existing dataset; unchanged existing entries are left as they are. Changing the type or location of an existing slot, including an NFS or CIFS server or path, requires `allow_storage_change=true`, and the preview and confirmation prompt name each changed slot, its old and new location, and state that data is not migrated. A change to `read_only` or ACL settings (`acl_enable` or `acl`) on a slot whose type and location are unchanged does not need the flag, but is listed as a plan warning with the old and new values, so the preview and confirmation prompt show it. Missing host-path datasets stay a preview warning; the real call still refuses until they exist. When `upgrade_app` is called with `version` omitted or `"latest"`, the confirmation token is also bound to the catalog version `app.upgrade_summary` resolved in the preview (`upgrade_version`). If a newer release is published between preview and confirm, the token is refused and nothing is upgraded; the `app.upgrade` call still sends `"latest"`. Preview `details` and `current_state` on these plans (and on any plan that uses custom preview actions) are masked the same way as other plan previews.

`app.update` replaces each top-level value as a whole, so changing one field of a nested object means sending the whole object, including any secret in it that `get_app_config` showed as `[REDACTED]`. For example, an app with a `network` object holding `auth_key` and `hostname`: to change `network.hostname`, send `{"network": {"auth_key": "[REDACTED]", "hostname": "new-name"}}`. Before the call, `update_app` replaces every string that is exactly `[REDACTED]` with the value stored at the same path in the current config (object keys by name; list items by index, only when the stored list has the same length), so the stored secret is kept instead of being overwritten with the marker. A path is kept only when it is safe: the stored value there is a non-empty string that `get_app_config` itself redacts, the outgoing values still redact it at that path (so renaming or reordering an entry whose name makes its value secret, such as an environment variable, cannot move the secret where it would be shown in clear), every enclosing entry keeps its `name`, `key`, `variable`, `username`, `principal`, `binddn` and `credential_type` (so renaming `DB_PASSWORD` to `API_TOKEN` is refused even though both names are masked: the app would get the secret under a different variable; this fails closed, so a legitimate rename of an entry that keeps its secret is refused too, and the real value must be sent instead), and no enclosing storage entry changes its type or location, whether the stored or the sent entry has the storage shape (so a stored share password is never sent to a different server or path, even with `allow_storage_change=true`). Otherwise the update is refused with an error naming the path; send the real value instead. A string that contains `[REDACTED]` inside a longer value (for example `NAME=[REDACTED]` in an environment list) cannot be restored and is refused unless it is unchanged from the stored value. The preview, its `kept_values` list, its warnings and the confirmation prompt name each path kept at its stored value, never the value itself; the stored task arguments keep the marker as sent.

The `update_app` confirmation prompt lists the top-level keys that change and then one `path: old -> new` fact per changed leaf field, so an unchanged field of a resent nested object (including a kept secret) is not shown as part of the change. Values are shown after redaction in context (an environment entry's value stays masked by its name); a changed secret whose old and new values are both masked is shown as `path: changed (secret, not shown)`. Kept paths are listed in a separate fact.

`install_app` refuses any value that is exactly `[REDACTED]` or contains it, naming the path: a new app has no stored value to keep, so the marker would otherwise be installed as the literal value.

> **Behaviour change:** `update_app` used to send `[REDACTED]` to `app.update` verbatim, replacing the stored secret with that string. It now keeps the stored value, or refuses as described above. `install_app` and `configure_directory_service` used to send the marker verbatim too; `install_app` now refuses it and `configure_directory_service` keeps the stored value or refuses (see below).

> **Behaviour change:** `update_app` no longer requires every call to include the full storage section as `host_path`, and it no longer refuses to update an app that already uses `ix_volume` storage. Omitting storage, or sending storage entries that match the current config, leaves those entries unchanged. A type or location change on an existing slot (including an NFS or CIFS server or path) needs `allow_storage_change=true` and does not migrate data. A `read_only` or ACL-only change does not need the flag, but is warned in the preview and confirmation prompt.

`system_reboot` and `apply_update` use the same shared plan for preview and the real call (`system.reboot` and `update.run`). A reboot delay of 0 is omitted from the middleware payload, as before. Their dry runs now include `"dry_run": true, "executed": false` like the other plan-backed previews. A confirmed call re-reads `system.info` (and, for `apply_update`, `update.status`) before writing, and fails closed if those reads fail. The confirmation token for `apply_update` is also bound to `update.status`'s `status.new_version.version` (the 25.10 `UpdateStatus` shape: `new_version` is nested under `status`, not at the top level). If that version changes between preview and confirm, the token is refused and `update.run` is not sent; the `update.run` payload is unchanged (`{"reboot": ...}` only). When `update.status` reports `code: "ERROR"` or a null `status`, the preview is blocked with that reason.

> **Breaking change:** an explicit `dry_run` or a call without a token of `apply_update` when `update.status` has no `new_version` (or reports `code: "ERROR"` or a null `status`) now returns an error result with `"status": "blocked"` and the preview, instead of a successful preview. Clients that treated that case as a normal result need to read the error result instead. A confirmed call is also refused; nothing is applied.

`configure_directory_service` and `leave_directory_service` use the same shared plan for preview and the real call (`directoryservices.update` and `directoryservices.leave`). The middleware payloads are unchanged. Credentials such as bind passwords, and LDAP `auxiliary_parameters` (free text that can contain secrets such as `ldap_default_authtok`), are masked in the preview and are never written back in the result or stored task arguments. The same exact-key mask is also applied to read-only directory config output from `query_directory_services`, so `auxiliary_parameters` is hidden there too. A confirmed `configure_directory_service` now also reads `directoryservices.status` (as the preview already did) and fails closed if that read fails. Their dry runs now include `"dry_run": true, "executed": false` like the other plan-backed previews.

`query_directory_services` shows the stored credential and configuration with secrets as `[REDACTED]` (`credential.password`, `credential.bindpw`, `configuration.auxiliary_parameters`, idmap `ldap_user_dn_password`, ...). When a `configure_directory_service` payload contains the marker, the plan reads `directoryservices.config` and replaces every string that is exactly `[REDACTED]` with the value stored at the same path, so resending that object keeps the stored secret instead of storing the marker. A path is kept only when the stored value there is a non-empty string that `query_directory_services` redacts and the outgoing payload still redacts it; the object holding it (unless that is the configuration itself) keeps every non-secret text field, such as `credential_type` and `username`, `principal` or `binddn` of a credential, or the name, URL, bind DN and base DN of an idmap backend; `service_type`, plus `kerberos_realm`, `configuration.domain`, `configuration.target_server`, `configuration.server_urls` and `configuration.auxiliary_parameters` when sent, match the stored config (LDAP auxiliary parameters are written into the same SSSD domain section as the bind credential, so a line such as `ldap_backup_uri` or `ldap_uri` could otherwise send the kept password to another server); and no `validate_certificates` or `starttls` flag, including an idmap backend's `validate_certificates`, is turned off where the stored config has it on (turning one on is fine). Because `directoryservices.update` merges only top-level keys, a flag left out of a sent object takes its middleware default: an omitted `starttls` (default false) counts as turning it off and is refused, while an omitted `validate_certificates` (default true) is allowed. Otherwise the call is refused with an error naming the path; send the real value instead. A string that contains the marker inside a longer value is refused unless it is unchanged from the stored value. The preview (`kept_values` and a warning) and the confirmation prompt name each kept path, never the value; the stored secret is only in the `directoryservices.update` call, and the stored task arguments keep the marker as sent. A payload without the marker does not read `directoryservices.config`.

`leave_directory_service` refuses a credential value that is or contains `[REDACTED]`, naming the path, before any middleware call: there is no stored credential to keep, so the marker would only be tried as the real password against the domain account.

`download_update`, `dismiss_alert`, `restore_alert`, `refresh_directory_cache`, `create_scrub_schedule`, and `run_scrub` use the same shared plan for preview and the real call. Their dry runs now include `"dry_run": true, "executed": false` like the other plan-backed previews. Jobs started by `download_update` and `run_scrub` are tracked with `WithExpectedMethod` (`update.download` and `pool.scrub.scrub`).

`run_snapshot_task`, `run_replication` and `create_snapshot` use the same shared plan for preview and the real call (`pool.snapshottask.run`, `replication.run` with only the task id, and `pool.snapshot.create` with `dataset`, `name` and `recursive`). A preview that says the run cannot proceed (a disabled or missing snapshot task, a replication task that is running or on hold or, on 25.10, disabled, a snapshot name that already exists) is an error and issues no token. `run_replication` returns the job id at once and tracks it with `WithExpectedMethod` (`replication.run`); when the job finishes, `tasks_get` reports whether anything was transferred, read from the job's final progress text. Only a missing or empty progress text is reported as "nothing transferred"; text it cannot parse is reported as "could not determine what was transferred". `pool.snapshottask.run` creates no job (it queues the task and returns null), so `run_snapshot_task` follows the task's own state for up to two minutes, then reads the task's `last_snapshot` and the snapshots on the covered datasets back from the middleware; if the run has not finished by then, it says the run was queued and points to `query_snapshot_tasks` instead of reporting success or failure. A disabled replication task is refused on 25.10, where `replication.run` refuses it too.

`clone_snapshot`, `hold_snapshot` and `release_snapshot` use the same shared plan for preview and the real call (`pool.snapshot.clone` with `snapshot` and `dataset_dst`, and `pool.snapshot.hold` and `pool.snapshot.release` with the snapshot id and `{"recursive": false}`). None of them takes a `recursive` argument: only the named snapshot is held, released or cloned. `clone_snapshot` is refused (as an error, on the dry run too) unless the snapshot exists and is not of a hidden dataset such as `.system`, the target is a new dataset name (not a `/mnt` path) in the same pool, below the pool root and outside hidden datasets such as `.system` and the legacy `ix-applications` dataset, and its parent already exists, is an unlocked filesystem and is mounted at or below `/mnt/<pool>`; a locked source dataset is refused too. The preview shows the source snapshot, the target and the mountpoint the clone will get. `hold_snapshot` shows the snapshot and its current holds; if the snapshot already has the `truenas` tag it sends nothing and reports `already_held: true`, so repeating it is safe (ZFS itself would fail a second hold with the same tag). `release_snapshot` is destructive: its preview and confirmation say that the snapshot becomes deletable again, name the hold and show any retention that schedules the snapshot's removal (`extra.retention`). `pool.snapshot.release` removes every hold on the snapshot, not only `truenas`, and `pool.snapshot.query` may list only the `truenas` tag (TrueNAS 27 always does). So the tool also reads the native `userrefs` property (the total hold count) and releases only when `truenas` is provably the only hold: it is refused (no token) if any other tag is listed, if `userrefs` is greater than the number of tags listed, or if `userrefs` cannot be read. Other holds must be managed outside this tool, where they were set. A snapshot with no holds is refused too, and so is a confirmed call whose hold state or `createtxg` changed after the preview.

`download_update` passes optional `train` and `version` through to `update.download`. Both must be set, or both omitted (matching 25.10's `UpdateDownloadArgs` and `plugins/update_/download.py`). When both are omitted, or both are blank or whitespace, they are treated as omitted and the middleware call still has empty parameters (`params: []`) and downloads the update offered by the configured profile, as before. When both are set, the call is `update.download` with those two positional arguments. A request for only one of them is refused. A completed download (`update_download_progress.percent == 100`) is not treated as already done if the caller asked for a different `version`.

> **Behaviour change:** `download_update` no longer ignores `train` and `version`. Clients that sent those fields expecting the configured train must now omit both.

A dry run of `refresh_directory_cache` now returns an error when no directory service is configured or the service is not enabled, matching the real call, instead of a successful preview with a warning. A dry run of `create_scrub_schedule` now errors when a schedule already exists, matching the real call. A dry run of `run_scrub` now matches the real call: if a finished scrub is still within the threshold, the preview reports that no scrub will start (the real call already returned `scrub_started: false`). If a scrub is already running, or the pool cannot be scrubbed, the dry run returns an error instead of a successful preview with a warning.

`install_app`, `create_dataset`, `create_smb_share`, and `create_nfs_share` use the same shared plan for preview and the real call (`app.create`, `pool.dataset.create`, `sharing.smb.create`, and `sharing.nfs.create`). `create_smb_share` and `create_nfs_share` reject a path that contains a `..` segment after cleaning, or that resolves outside `/mnt/<pool>` (the pool is the first component after `/mnt/` on the original path). The middleware payloads are unchanged: `install_app` still sends `version` as `"latest"` when it was omitted, and dataset and share bodies keep the same fields and defaults. Jobs started by `install_app` are tracked with `WithExpectedMethod` (`app.create`). Secrets inside `install_app` `values` (passwords, tokens, and similar, including nested ones) are masked in the preview, in rejection and error text, in stored task arguments, and in debug logs; they are still sent to the middleware on the real call. Their dry runs now include `"dry_run": true, "executed": false` like the other plan-backed previews. Dataset ancestor lists and share security warnings appear in the plan preview (`current_state` / `warnings`) rather than as custom top-level fields. A dry run of `install_app` now matches the real call: a missing host-path dataset or an existing instance name is an error on both paths, not a successful preview with a warning, and a catalog version with no schema is still refused on both. `create_dataset` preflight (the pool must exist and the name must be free) now also runs on the real call.

> **Behaviour change:** dry-run previews for `create_dataset`, `create_smb_share`, and `create_nfs_share` now use the shared plan shape (`current_state`, `planned_actions`, `warnings`, `"dry_run": true, "executed": false`) instead of a custom top-level `payload` / `security_warnings` object. Clients that read those old top-level fields should use the plan preview. A dry run of `install_app` now returns an error when a host-path dataset is missing or the instance name is already in use, matching the real call, instead of a successful preview with a warning.

`refresh_directory_cache` reports `status: "submitted"` (with `task_id` / `job_id` when TrueNAS returns a job id, tracked with `WithExpectedMethod` for `directoryservices.cache_refresh`) instead of `cache_refreshed: true` as soon as the job is submitted. A first Active Directory cache fill can take a significant amount of time; track the task for the result.

> **Behaviour change:** `refresh_directory_cache` no longer reports `cache_refreshed: true` when the `directoryservices.cache_refresh` job is only submitted. Clients should treat `status: "submitted"` like the other directory-service tools and poll the task.

`get_app_catalog_details` returns the app schema (groups and questions, including each question's defaults) and, when the schema defines them, a `defaults` object with run-as user/group and resource limits taken from `catalog.get_app_details`. Resource defaults keep the schema nesting (`defaults.resources.limits.cpus` and `defaults.resources.limits.memory`). Only numeric defaults are passed through. When the schema has `run_as_custom_user` and its default is false, `run_as` is omitted because those values only apply when that flag is true. When the schema has `enable_resource_limits` and its default is false, `defaults.resources.limits` is omitted for the same reason. Those defaults are omitted when the schema has none. Workflow guidance for installation (review schema groups, query pools, create datasets, use `host_path`, use schema defaults when present) lives in the `get_app_catalog_details` and `install_app` tool descriptions, worded so any MCP client can follow it.

> **Behaviour change:** `get_app_catalog_details` no longer includes a `wizard_guidance` object with client-specific instructions (for example a named tool of one MCP client) or fixed run-as and resource defaults. Clients that read `wizard_guidance` should use the schema and the `defaults` field (when present) instead, and follow the tool descriptions for the install workflow.
