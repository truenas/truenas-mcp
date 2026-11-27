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

**Confirmation for destructive tools.** These forty-three tools execute only after
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
| `rollback_snapshot` | Discards everything written to the dataset since the snapshot; rolling back to an older snapshot also destroys every newer snapshot and, with `destroy_clones`, their clones |
| `delete_snapshot` | Permanently removes a snapshot (with `recursive`, the same snapshot on every child dataset); its data can no longer be restored or replicated from it |
| `control_service` | STOP and RESTART take a service offline and disconnect its clients (SMB, NFS, iSCSI, NVMe-oF, UPS), and RELOAD of a service that cannot reload is a restart. One tool covers every action, so START and RELOAD need confirmation too |
| `create_user` | Adds an account that can sign in (with a random or supplied password) and joins it to groups that grant access to shares. Another user's primary group is refused as its group or in `add_groups`. Confirmation is bound to the absence of the user, the groups named and the new home directory |
| `update_user` | Changes an account in place: its password, groups, lock state, shell or SMB access. Another user's primary group is refused in `group` and `add_groups`. Confirmation is bound to the user's current settings, including when its password last changed, and to the groups involved |
| `create_group` | Adds a group and its members, which can grant access to shares. Confirmation is bound to the members' current settings |
| `update_group` | Changes a group's members or SMB flag in place. Adding a user to another user's primary group (the group created with that user, for example) is refused, at the preview and again when the confirmation re-plans. Confirmation is bound to the group's current settings and the members named |
| `delete_user` | Removes an account; files it owns keep its numeric uid, which a later account with that uid inherits. Its primary group and home directory are kept. Confirmation is bound to the user's current settings, its groups, the SMB guest account and whether `<home>/.ssh` exists |
| `delete_group` | Removes a group; members lose the access it grants and files keep its numeric gid. No user is deleted, and a group that is still some user's primary group is refused. Confirmation is bound to the group's current settings, its members and the users that have it as primary group |
| `update_smb_share` | A rename breaks client paths and mapped drives, a new path serves a different directory, and `readonly`, `enabled` or host lists change who can reach the data. Confirmation is bound to the share's current settings |
| `delete_smb_share` | Disconnects clients and removes their access (only the share configuration is removed; files are kept). Confirmation is bound to the share's current settings |
| `update_nfs_share` | Changes which hosts can mount the export, how root and other users are mapped, or which directory is exported. Confirmation is bound to the share's current settings |
| `delete_nfs_share` | Clients with the export mounted lose access (only the export is removed; files are kept). Confirmation is bound to the share's current settings |
| `set_share_acl` | Replaces the share-level ACL as a whole and can lock users, administrators included, out of the share (directory service accounts are refused). Confirmation is bound to the share and its current share ACL |
| `set_filesystem_acl` | Replaces a path's filesystem ACL as a whole and can lock users out or widen access; with `recursive`, every ACL below the path in its dataset is replaced. Confirmation is bound to the path's identity, its dataset, its current ACL and the principals named |
| `offline_disk` | Reduces the redundancy of the disk's vdev until the disk is back online. Refused unless the vdev keeps at least one disk's worth of redundancy. Confirmation is bound to the pool topology and the disk |
| `online_disk` | Starts a resilver of the data written while the disk was offline. Confirmation is bound to the pool topology and the disk |
| `replace_disk` | Destroys everything on the replacement disk and starts a full resilver. Confirmation is bound to the pool topology, the disk being replaced, and the replacement disk's identity, size and unused state |
| `upload_file` | Writes a file of up to 64 GiB onto a dataset; with `overwrite`, an existing file is truncated and rewritten in place, and a failed upload leaves the target partly written. `source_path` (a file on this host) is off unless `--upload-source-dirs` is set (see [Local sources](#upload_file-local-sources)). Confirmation is bound to the source, the target directory and dataset, and the existing file's inode, size and modification time |
| `create_vm` | Creates a VM (`vm.create`, always with `autostart: false` and `ensure_display_device: false`), attaches every listed device (`vm.device.create`), and only then enables a requested autostart (`vm.update`), potentially creating new zvols or exposing a display console. A failed attach deletes the new VM again (`vm.delete`, zvols kept and listed). Refuses unsupported virtualization, host CPU/physical-memory limit violations, name collisions, a missing display password on 25.10, display ports used by another VM, invalid disk sector sizes, managed/pool-root/symlink paths, in-use or existing zvols, and wildcard display binds (`0.0.0.0`/`::`) unless `allow_wildcard_display_bind=true`. Display passwords are never echoed. Confirmation is bound to the planned VM, every device (password hashed), host limits, device choices, and zvol/ISO dataset rows and file stats |
| `start_vm` | Starts a stopped VM (`vm.start`), allocating host RAM and CPUs. Refuses if already running or suspended, if virtualization is unsupported, or if memory exceeds available VM memory. Confirmation is bound to the VM id, current status, and `overcommit` |
| `stop_vm` | Stops a running VM (`vm.stop`, tracked job; graceful ACPI shutdown by default, or forced when `force`/`force_after_timeout` is set). Refuses if already stopped. Confirmation is bound to the VM id, current status, and stop options |
| `poweroff_vm` | Immediately cuts power to a VM (`vm.poweroff`) without guest OS shutdown; unsaved guest data may be lost. Refuses if already stopped. Confirmation is bound to the VM id and current status |
| `restart_vm` | Gracefully stops and restarts a running VM (`vm.restart`, tracked job; the middleware restarts with `overcommit: true`, so no memory check). Refuses if stopped. Confirmation is bound to the VM id and current status |
| `reset_vm` | Immediately hardware-resets a running VM (`vm.reset`, TrueNAS 28.0+ / master only) without guest OS shutdown; unsaved guest data may be lost. Refuses unless running. Confirmation is bound to the VM id and current status |
| `update_vm_device` | Modifies a VM device (`vm.device.update`), such as changing boot order, swapping a CDROM ISO, or updating DISK, NIC, or DISPLAY attributes. Refuses while the VM is not `STOPPED` unless `force=true` (updates stored config only; takes effect after stop and start); a DISK `path` change always needs the VM `STOPPED`. Enforces per-dtype field allowlists (`dtype` immutable, DISK `serial` read-only), mirrors middleware device validators (display password rules by version including strict unknown-version handling, display port/web_port clashes and reserved port 6000, disk sector rules, CDROM path/libvirt readability, NIC attach choices), and never echoes display passwords. Confirmation is bound to the device row, VM status, sibling orders, and validated targets |
| `delete_vm_device` | Removes a VM device (`vm.device.delete`; also how installer CDROMs are ejected while keeping the ISO on disk). Backing zvols and raw files are kept by default; `zvol=true` (DISK only) or `raw_file=true` (RAW only) permanently destroys them and shows the size in the preview. Always refuses while the VM is `RUNNING` or `SUSPENDED` (TrueNAS rejects active VMs even with `force=true`), refuses `ERROR` without `force=true`, refuses non-managed or shared zvol/raw paths, a zvol that has snapshots (TrueNAS destroys it non-recursively) and a zvol used by an iSCSI extent, NVMe-oF namespace or (before 27.0) Incus instance, reports afterwards whether the device and its storage are gone, and binds confirmation to the device row, VM status, and backing storage |
| `delete_vm` | Deletes a VM (`vm.delete`). Refuses while the VM is not `STOPPED` unless `force=true` (which forcibly powers off a running VM and ignores cleanup errors). Backing zvols are kept by default; `zvols=true` recursively destroys every attached `/dev/zvol/` DISK volume (and its snapshots), lists each zvol with its size and snapshot count in the preview, and refuses if any zvol is shared with another VM or another service (iSCSI, NVMe-oF, or an Incus instance before 27.0), has a snapshot with clones or holds (which a recursive destroy would destroy or release), or fails managed-dataset guards. Afterwards it reports which of the VM and its zvols are still present. Confirmation is bound to the VM row, live status, full device list (passwords stripped), and each zvol's dataset, snapshots, clones and holds |

No other tool needs a token: the other write tools (`create_dataset`,
`create_smb_share`, `create_nfs_share`, `install_app`, `start_app`,
`download_update`, `create_scrub_schedule`, `run_scrub`, `create_snapshot`,
`clone_snapshot`, `hold_snapshot`, `set_service_enabled`, `dismiss_alert`, `restore_alert`, `refresh_directory_cache`,
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
`update_network_interface`, `release_snapshot`, `rollback_snapshot`,
`delete_snapshot`, `control_service`, `create_user`, `update_user`,
`create_group`, `update_group`, `update_smb_share`, `delete_smb_share`,
`update_nfs_share`, `delete_nfs_share`, `set_share_acl`, `offline_disk`, `online_disk`,
`replace_disk`, `delete_user`, `delete_group`, `set_filesystem_acl`, `upload_file`, `create_vm`, `start_vm`, `stop_vm`, `poweroff_vm`, `restart_vm`, `reset_vm`, `update_vm_device`, `delete_vm_device`, `delete_vm`), the token is also bound to
a hash of the planned middleware method and parameters, so it cannot
execute a different planned operation. For `release_snapshot` the hash also
covers the previewed hold tags, hold count (`userrefs`) and the snapshot's
`createtxg`, so a hold added after the preview, or the snapshot being
recreated under the same name, rejects the token. For `rollback_snapshot` it
covers the target and every snapshot of the dataset newer than it (name,
`createtxg`, holds and clones), so a newer snapshot taken, deleted or
recreated, the target recreated, or a hold or clone added to a newer
snapshot, after the preview rejects the token. For `delete_snapshot` it covers the `createtxg`, holds and
clones of every snapshot the call deletes. For `control_service` it
covers the service's previewed state, boot setting and id and
`allow_protected_service`, so a service started, stopped or re-enabled after
the preview rejects the token. For `update_user` it covers the user's
current settings (uid, names, groups, home, shell, lock, password and SMB
flags, roles, sudo, and when the password last changed) and the groups
named; for `create_user` the absence of the user, the groups named and the
state of the new home directory; for `update_group` and `create_group` the
group's settings and the members named. For the share tools it covers the
share's current settings as read from the system (and, for `set_share_acl`,
its current share ACL and the principals named; for a new path, the
directory's inode and device), so a share edited, recreated or locked after
the preview, or an ACL changed in the meantime, rejects the token and
nothing is written. For `offline_disk`, `online_disk` and `replace_disk` it
covers the pool's status, whether a resilver is running, and its whole
topology (every vdev's type, guid and state, and every device's guid, disk
name and state), plus the target device's serial and size; for
`replace_disk` also the replacement disk's name, serial, identifier (from
both `disk.query` and `disk.details`), partitions and size, whether TrueNAS reports it unused, the pool it belongs to, and any
running `pool.replace` job. A disk failing, going offline or coming back,
a resilver starting, or a different disk under the same name after the
preview rejects the token and nothing is written. For `delete_user` it covers
the user's settings (as for `update_user`), its groups, the SMB guest account
and whether `<home>/.ssh` exists; for `delete_group` the group's settings,
its members and the users that have it as primary group. For
`set_filesystem_acl` it covers the path's realpath, type, inode, device,
mount id, owner, group and mode, the dataset and mount it is on
(`filesystem.statfs` source, mountpoint, filesystem id and flags, and the
dataset's lock state), its current ACL (type, entries, flags, owner), the
local users and groups named, and, for a recursive change, the child
datasets below the path. The entry and object counts shown in a recursive
preview are not bound. For `upload_file` it covers the source (the URL and
SHA-256, or the local file's resolved path, device and inode, size,
modification time and SHA-256 if given), the target directory's realpath, inode, device and mount
id, the target file's state (absent, or its realpath, type, inode, device,
mount id, size, modification time, owner, group and mode), and the dataset
and mount it is on, so a file created, replaced or changed at the target, or
a local source changed, after the preview rejects the token. The free space
and a URL's reported size are shown but not bound. For `create_vm` it covers
the VM payload, every planned device (with any display password replaced by its
SHA-256 hash), host virtualization support and static CPU/memory limits, the
queried device choices (`vm.device.disk_choices`, `vm.device.nic_attach_choices`,
`vm.device.bind_choices`), the display ports other VMs use, and the zvol dataset rows or CDROM ISO file stats,
filesystem info and dataset rows; available memory is shown and warned about
but neither hashed nor blocking. For `start_vm`, `stop_vm`,
`poweroff_vm`, `restart_vm` and `reset_vm` it covers the VM id, name, UUID,
live `vm.status` (`state`, `pid`, `domain_state`) and tool options. For
`update_vm_device` it covers the sanitized device row, owning VM id/name/UUID
and live status, sibling device orders, `force`, the sanitized update payload
(display password replaced by its SHA-256 hash), and any queried choices,
display ports in use, or dataset/file stats. For `delete_vm_device` it covers
the sanitized device row, owning VM id/name/UUID and live status, delete
options (`zvol`, `raw_file`, `force`), and the backing zvol dataset or raw file
state and cross-VM references. For `delete_vm` it covers the VM id, name, UUID,
memory, autostart, live status, full sanitized device list, every attached
zvol's dataset row and cross-VM references, with `zvols=true` every snapshot
of each zvol with its clones, hold tags and hold count, and `zvols`/`force`. The token store does not hold those
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

The only other exemption is the top-level `generated_password` of a
successful `create_user` or `update_user` result with `password_mode`
`random`. The password is generated from a cryptographically secure random
source only when the call executes, after confirmation, so it is never in a
preview, an elicitation message or a confirmation token. The server lets it
through once: the registry keeps only a SHA-256 of it, for at most a
minute, and forgets it when the result is sent, so the same value in any
later output is redacted. A password supplied with `password_mode` `set` is
never returned; it is redacted in debug logs like any other `password`
value and removed from TrueNAS error messages. Like every other argument
it is part of the argument hash a confirmation token is bound to, so the
confirming call must repeat it unchanged.

**Passwords pass through the assistant.** Not returning a password again
does not keep it private: a password supplied with `password_mode` `set`
is an argument the AI assistant writes, and a `random` password is in the
result the assistant reads. Both pass through the assistant and appear in
the MCP client's chat transcript, and in whatever the client or its
provider keeps of it. Treat such a password as known to anyone who can read
that transcript; use `password_mode` `disabled` and set the password in
the TrueNAS web UI when that is not acceptable, or have the user change it
after first sign-in.

**A lost random password.** The account is created (or its password
changed) before the result is returned. If that result never reaches the
client, for example because the call was cancelled, timed out or the
connection dropped, or because the result is sent after the one-minute
window and the password is redacted, the account exists with a password
nobody saw. It cannot be recovered; set a new one with `update_user`
(`password_mode` `random` or `set`) or in the TrueNAS web UI.

**Which accounts `update_user` can change.** Apart from its refusals
(root, the built-in administrator accounts, other built-in accounts, the
account this server signs in as, directory service users, and users with
TrueNAS roles, sudo or an administrative group), any local account can be
changed, including service accounts such as the users that replication or
rsync tasks sign in as. That includes resetting its password, turning on
SSH password login and changing its shell. Limit the tool with the tool
allowlist or denylist, or read-only mode, where that is too broad.

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
(`readOnlyHint`, `destructiveHint`, `idempotentHint`, `openWorldHint`)
derived from the same table, so clients can apply their own approval prompts.
`destructiveHint` is true for the forty-three destructive tools (the ones that need a
confirmation token, `update_app` included) and false for every other tool.
`openWorldHint` is true only for `upload_file`, which downloads arbitrary
URLs; every other tool talks only to the TrueNAS system.
Annotations were added in MCP 2025-03-26; clients that negotiate
`2024-11-05` ignore the extra field.

### upload_file local sources

`source_path` reads a file on the host running this server. It is off by default: unless the operator lists the directories it may read with `--upload-source-dirs` (or `TRUENAS_MCP_UPLOAD_SOURCE_DIRS`), a `source_path` is refused before the path is even looked at, and only `source_url` works. Without this, a client that does not support form elicitation gets the confirmation token back in the tool reply, so no person needs to approve the call, and text injected into the conversation could copy SSH keys, cloud credentials or this server's own API key onto a dataset that share users can read. When the setting is used:

- The directories are separated by the OS path list separator (`:`, or `;` on Windows). Each must be an absolute path of an existing directory that is not a filesystem root and whose real path has no dot-prefixed component; otherwise the server exits at startup. List only directories that hold files meant for upload, such as a downloads folder for installer images, never a home directory.
- The `source_path` is resolved to its real path (symlinks followed), which must be inside one of the directories. A path outside them is refused without saying whether it exists.
- A real path with any dot-prefixed (hidden) component, such as `.ssh`, `.aws`, `.config`, `.gnupg` or `.kube`, is refused, even inside an allowed directory and even through a symlink.
- This server's own API key file (`--api-key-file` / `TRUENAS_API_KEY_FILE`), CA file (`--tls-ca` / `TRUENAS_TLS_CA`) and `setup`'s configuration directory (`truenas-mcp` in the user configuration directory) are refused, as are MCP client configuration files (`claude_desktop_config.json`, `mcp.json`), which hold the API key, and `.key`, `.pem` and `.pin` files.
- The preview shows the resolved path. The confirmation is bound to the file's device and inode, size and modification time. The confirmed upload opens the file once, checks that the opened file is that same file (device and inode, size and modification time), and then hashes and sends from that one descriptor, so a file swapped in at the same path after the preview is refused.

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

`clone_snapshot`, `hold_snapshot` and `release_snapshot` use the same shared plan for preview and the real call (`pool.snapshot.clone` with `snapshot` and `dataset_dst`, and `pool.snapshot.hold` and `pool.snapshot.release` with the snapshot id and `{"recursive": false}`). None of them takes a `recursive` argument: only the named snapshot is held, released or cloned. `clone_snapshot` is refused (as an error, on the dry run too) unless the snapshot exists, neither the snapshot's dataset nor the target is inside a dataset TrueNAS manages itself (the same rule as `rollback_snapshot` and `delete_snapshot`: every dataset of the boot pool, `boot-pool` or `freenas-boot`, any name containing `/.system`, `/ix-applications`, `/ix-apps` or `/.ix-virt`, and any other component below the pool root that starts with `.`), the target is a new dataset name (not a `/mnt` path) in the same pool and below the pool root, and its parent already exists, is an unlocked filesystem and is mounted at or below `/mnt/<pool>`; a locked source dataset is refused too. The preview shows the source snapshot, the target and the mountpoint the clone will get. `hold_snapshot` shows the snapshot and its current holds; if the snapshot already has the `truenas` tag it sends nothing and reports `already_held: true`, so repeating it is safe (ZFS itself would fail a second hold with the same tag). `release_snapshot` is destructive: its preview and confirmation say that the snapshot becomes deletable again, name the hold and show any retention that schedules the snapshot's removal (`extra.retention`). `pool.snapshot.release` removes every hold on the snapshot, not only `truenas`, and `pool.snapshot.query` may list only the `truenas` tag (TrueNAS 27 always does). So the tool also reads the native `userrefs` property (the total hold count) and releases only when `truenas` is provably the only hold: it is refused (no token) if any other tag is listed, if `userrefs` is greater than the number of tags listed, or if `userrefs` cannot be read. Other holds must be managed outside this tool, where they were set. A snapshot with no holds is refused too, and so is a confirmed call whose hold state or `createtxg` changed after the preview. The retention check runs before the final hold read, so the release is the only call between that read and the change; a hold added in that short gap is still released with the rest, because `pool.snapshot.release` has no per-tag option.

`rollback_snapshot` and `delete_snapshot` are destructive and share their plan between preview and the real call too. Both read each snapshot they act on from `pool.snapshot.query` with a single `id =` filter, holds and the native `createtxg`, `userrefs` and `clones` properties. On 25.10 any other filter with holds or properties lists every snapshot on the system, so the lists of candidate snapshots come from the query's names-only form instead (`select: ["name"]` with only `dataset` or `pool` and `name` filters), which iterates only the named dataset (`dataset =`) or pool (`pool =`); for a rollback it also passes `extra.min_txg` (the target's `createtxg`) so ZFS skips older snapshots. Every list is filtered again client-side. Retention is read the same way, one `id =` query per snapshot. They find clones from both each dataset's `origin` (`pool.dataset.query`) and the snapshot's `clones` property, which also lists hidden datasets. A snapshot counts as held when it has any hold tag, when `userrefs` is greater than zero, or when `userrefs` cannot be read; a snapshot whose `createtxg` cannot be read is refused, because the confirmation could not be bound to it. Every refusal is a blocked preview: an error with no token and no prompt.

`rollback_snapshot` sends `pool.snapshot.rollback` with the snapshot id and all four options set explicitly. By default the target must be the dataset's newest snapshot, so nothing but data written since the snapshot is lost. An older target needs `allow_destroy_newer: true`; it maps to `recursive` (ZFS `rollback -r`), which destroys the newer snapshots and bookmarks, and the preview lists those snapshots. Held newer snapshots are refused: ZFS cannot destroy a held snapshot, so the rollback would fail, possibly after destroying some of the other newer snapshots. Newer snapshots with clones are refused unless `destroy_clones` is also set; that maps to `recursive_clones` (`rollback -R`), and the preview lists the clones. A clone that has snapshots or child datasets of its own, or that `query_datasets` does not list, is refused even with `destroy_clones`, so the rollback never destroys more than the preview names. `force` (which unmounts the dataset) and `recursive_rollback` (which rolls back child datasets too) are never sent and cannot be requested: only the named dataset is rolled back, and a busy dataset fails rather than being unmounted under its clients. A pool's root dataset is refused, as an error on the dry run too: its mountpoint `/mnt/<pool>` is the path to every other dataset in the pool, and a rollback reverts the root's ownership, permissions and ACL, which can lock users out of the whole pool. Roll back a child dataset instead, or clone the snapshot to recover files. Datasets TrueNAS manages itself are refused too, as an error on the dry run. The rule copies the 25.10 middleware's `is_internal_dataset()` (`BOOT_POOL_NAME_VALID` in `middlewared/utils/__init__.py` and `INTERNAL_DATASETS` in `middlewared/plugins/pool_/dataset_query_utils.py`, branch `stable/goldeye`): every dataset of the boot pool (`boot-pool` or `freenas-boot`, so boot environments are left to the boot environment tools), and any name containing `/.system`, `/ix-applications`, `/ix-apps` (the apps dataset since 24.10) or `/.ix-virt`. Any other component starting with `.` is refused as well. `pool.snapshot.query` lists snapshots of all of these even though `pool.dataset.query` hides the datasets, so the tool also requires `pool.dataset.query` to list the target dataset and refuses otherwise; that keeps following the middleware if its hidden list grows. A flag the rollback does not need (`allow_destroy_newer` on the newest snapshot, `destroy_clones` with no clones) is refused rather than ignored. The preview warns that the data is lost, that child datasets are not rolled back, that clients see the contents change, and, when newer snapshots are destroyed, that bookmarks go with them and replication may need the target rolled back as well. A bookmark newer than the target is not listed (the middleware has no bookmark query), and without `recursive` ZFS refuses the rollback when one exists. The tool sends `recursive` only when newer snapshots are destroyed, so rolling back to the newest snapshot past a newer bookmark fails; the error then says that a newer bookmark (or a snapshot taken since the preview) is blocking it and that this tool does not destroy bookmarks for the newest snapshot. After the call, the tool reads the target and the snapshots newer than it back and reports whether the target is now the newest snapshot.

`delete_snapshot` sends `pool.snapshot.delete` with the snapshot id; by default only that snapshot is deleted. `recursive: true` also deletes the snapshot of the same name on every child dataset. The preview lists every affected snapshot and checks each for holds and clones. The snapshot is refused if its dataset is one TrueNAS manages itself (the same middleware rule as for `rollback_snapshot`) or is not listed by `pool.dataset.query`, and a recursive delete is refused if any child snapshot it would reach is on such a dataset. A recursive delete from a pool root that would reach `<pool>/ix-apps@name` is therefore refused, and the other snapshots can be deleted without `recursive`. `recursive` is sent only when a child snapshot exists. Held snapshots are always refused; ZFS refuses to delete them anyway, and release_snapshot or the tool that set the hold has to release it first. Snapshots with clones are refused by default. The middleware's `defer` option (ZFS `destroy -d`) is exposed only as the explicit `defer_if_cloned: true`, and is sent only when an affected snapshot has clones. Then the snapshot is not deleted at once: ZFS marks it and deletes it when its last clone is destroyed. The preview lists those clones and warns about the deferral. After the call the tool reads every affected snapshot back, with its clones: one that still has clones is reported as `pending_deletion`, and one that still exists without clones is reported under `remaining` with a note (if it is held, for example by a hold added after the preview, ZFS deletes it only when its last hold is released). `defer` is not sent otherwise because it changes what the call means. On a held snapshot it succeeds without deleting anything, and the snapshot disappears later when the last hold is released, which a preview cannot show. Held snapshots are refused before that can happen. The preview shows any periodic snapshot task retention that manages an affected snapshot (from `pool.snapshot.query` with `retention`), warns if that could not be checked, and warns that deleting the last snapshot shared with a replication target can force a full resend.

Both tools re-plan against fresh reads right before the call and refuse a token whose bound state changed, but a small window remains between that final re-plan and the middleware call, and it cannot be closed client-side because `pool.snapshot.rollback` and `pool.snapshot.delete` have no compare-and-swap. A snapshot created in that window on the dataset (by a periodic snapshot task or an incoming replication, for example) is destroyed by a rollback with `allow_destroy_newer` (`rollback -r`) without having been previewed, and a clone of a newer snapshot created in that window is destroyed by a rollback with `destroy_clones` (`rollback -R`). Likewise a child snapshot of the same name created in that window is deleted by a recursive `delete_snapshot`. Without those options ZFS itself refuses instead. The tool descriptions say this too.

`control_service` and `set_service_enabled` use the same shared plan for preview and the real call (`service.control` with the verb, the service name and `{"silent": false}`, and `service.update` with the name and `{"enable": ...}`). Both read the service from `service.query` first and refuse a name it does not list, so internal service objects that `service.control` would otherwise accept (on 25.10 these include `sssd`, `idmap`, `nscd`, `keepalived` and the `http` web server) cannot be reached by name. `http` is refused outright for every action and override, because the TrueNAS API this server talks to is served through it. Services that provide access or authentication (on 25.10 only `ssh` is listed by `service.query`; the server also protects `sssd`, `idmap`, `nscd` and `keepalived` in case a release lists them) are refused, as a blocked preview with no token, for STOP and RESTART and for a RELOAD that TrueNAS performs as a restart, unless `allow_protected_service` is true; the flag is part of the argument hash and of the plan binding, and the preview and confirmation show it. `set_service_enabled` refuses to disable them at boot with no override at all: it is a write tool with no confirmation step, so an override there would be granted by the same call that uses it. The refusal points to the TrueNAS web UI. STOP and RESTART of `cifs`, `nfs`, `iscsitarget`, `nvmet` and `ups` warn that connected clients are disrupted. `control_service` is refused when it would change nothing (START of a RUNNING service, STOP or RELOAD of a STOPPED one). It waits for the job: a `FAILED` or `ABORTED` job, or a `SUCCESS` whose result is `false`, is an error; a job that does not finish within 150 seconds is reported as `running` (with a `task_id` when task tracking is on), and a job that is no longer listed, runs a different method, or can no longer be identified after a reconnect is reported as `unknown` (with no task, since its id may now belong to another job), rather than as success or failure. `service.control` is sent without `ha_propagate`, so the 25.10 default (`true`) applies: on an HA system with healthy failover, the active controller also runs the action on the standby for services that may run there. `set_service_enabled` reads the setting back and fails if it does not match.

`update_smb_share`, `delete_smb_share`, `update_nfs_share`, `delete_nfs_share` and `set_share_acl` use the same shared plan for preview and the real call (`sharing.smb.update` and `sharing.nfs.update` with the share id and only the changed fields, `sharing.smb.delete` and `sharing.nfs.delete` with the share id, and `sharing.smb.setacl` with the share name and the full new ACL). Each reads the share by id (or, for SMB, by name) first and works on exactly one share per call. A share's current and new path must be a normalised directory below `/mnt/<pool>`, reached without symlinks, and not a pool root, `/mnt` itself, an EXTERNAL DFS proxy or a managed dataset (`boot-pool`, `freenas-boot`, `.system`, `ix-applications`, `ix-apps`, `.ix-virt` or any dot-prefixed component, matched in any letter case); a share that is already on such a path is refused for every action, deletion included, and is left to the TrueNAS web UI. Any other dataset folder is allowed: a share can be pointed at any ordinary folder below `/mnt/<pool>`, including other users' or teams' data and home directories, because the tools do not check who owns the files there. The preview of a path change says so. `hostsallow` and `hostsdeny` are changed only on `LEGACY_SHARE` shares. Reserved and administrative SMB share names (`global`, `printers`, `homes`, `IPC$`, `ADMIN$`, `PRINT$`, drive shares such as `C$`) and the homes share are refused as a target and as a new name, and a share whose `locked` flag is true, or not reported, is refused. A delete removes only the share configuration and never the directory or its files; its preview warns that clients lose access. `set_share_acl` changes only the share ACL (`sharing.smb.getacl` / `setacl`), not the filesystem ACL of the path, and refuses an empty ACL (which TrueNAS would reset to its default) and an ACL with no ALLOWED FULL entry, with a DENIED entry for everyone, or with a DENIED entry for an administrative group (`builtin_administrators`, another administrative group name, or a group that grants TrueNAS roles or whose roles cannot be read), unless `allow_no_full_control` is true; the flag is part of the argument hash and shown in the preview and confirmation. Principals are resolved to SIDs from `user.query` and `group.query`, filtered to local accounts and checked row by row, so only local accounts with SMB enabled (and everyone, `S-1-1-0`) are accepted; directory service users and groups, and rows without a `local` flag of true, are refused by name and by SID. Each tool reads the share back afterwards and reports a mismatch. The global SMB and NFS service settings are not changed.

`update_nfs_share` can map every client to root (`mapall_user` root), keep a client's root as root (`no_root_squash`), or remove every network and host limit, in one call and without an extra flag, as `create_nfs_share` can. Each of these gives an explicit `WARNING:` in the preview's warnings and in the confirmation facts, so the approver sees it before anything is sent.

The share tools are not limited to 25.10, but the option shapes they send (for example the SMB `options` block sent back with the host lists) were checked only against 25.10. On any other version the preview adds a warning saying so; check the result with `query_shares`.

`delete_user` and `delete_group` use the same shared plan for preview and the real call: `user.delete` with the user id and `{"delete_group": false}`, and `group.delete` with the group id and `{"delete_users": false}`. On 25.10 `delete_group` defaults to true for `user.delete`, which would delete the user's primary group when no other user has it; it is always sent as false, and `delete_users` is sent as false although it is the default. Neither tool accepts either option. Both read the account with `user.query` / `group.query` filtered to local accounts and apply the refusal rules of `update_user` and `update_group`. `user.delete` itself has side effects these tools cannot turn off, so `delete_user` refuses the cases where they would touch more than the account: it always removes `<home>/.ssh` (checked with `filesystem.stat`; a user whose home directory has one is refused), and it resets the SMB service's guest account to `nobody` when it names the user (read from `smb.config`; that user is refused, and an unreadable `smb.config` fails the plan). It also removes the user's SMB password entry, its web UI settings and its group memberships, and reloads the SSH and user services; the preview lists them. `group.delete` moves users whose primary group it is to `nogroup`, so `delete_group` refuses such a group; it also deletes the group's SMB group mapping, which the preview says for an SMB group. TrueNAS refuses to delete a group that a privilege uses; `delete_group` refuses groups that grant roles before that. Both read the account back afterwards and report it if it is still listed.

`set_filesystem_acl` uses the same shared plan for preview and the real call: `filesystem.setacl` with one argument, `{"path", "dacl", "options": {"stripacl": false, "recursive": <recursive>, "traverse": false, "canonicalize": true, "validate_effective_acl": true}, "uid": -1, "gid": -1, "acltype": <current type>}`. It walks the path one component at a time as `get_filesystem_acl` does and refuses a path that is not its own real path. It reads the dataset the path is on from `filesystem.statfs` (refusing a non-ZFS filesystem, a dataset of another pool, a managed dataset by name, and a read-only mount) and its lock state, and the lock state of every dataset below it, from `pool.dataset.query`; a path inside a locked dataset, or one whose lock state is not reported, is refused. The current ACL is read with `filesystem.getacl` (not simplified); the new entries must be in the same ACL type, and type `DISABLED` is refused. `USER` and `GROUP` entries name local accounts, resolved to their uid or gid with `user.query` and `group.query` filtered to local accounts, so directory service principals and unknown names are refused; `who` is not sent. POSIX ACLs need exactly one `USER_OBJ`, `GROUP_OBJ` and `OTHER` entry, and a `MASK` when they name users or groups, for the access ACL and for the default ACL if any default entry is given. An NFSv4 ACL with no `ALLOW` entry that grants `FULL_CONTROL` or `WRITE_ACL` (`DENY` entries do not count) is allowed with a warning; the file's owner and root can still change the ACL afterwards. A recursive change gives everything below the path the ACL inherited from the new one, so it is refused when nothing is inheritable: no NFSv4 entry with `FILE_INHERIT` or `DIRECTORY_INHERIT`, or no POSIX default entry (TrueNAS refuses the POSIX case too). A recursive change reads `filesystem.listdir` with `count: true` for the entries directly in the path and shows `files - free_files` from `filesystem.statfs` as the objects in use in the dataset; if the count cannot be read, the change is refused. `filesystem.setacl` is a job; its id is tracked as a `tasks_get` task (`set_filesystem_acl`, 24 hours). As with the other destructive tools, a small window remains between the final re-plan and the middleware call, which `filesystem.setacl` cannot close because it has no compare-and-swap; TrueNAS's own path checks (paths under `/mnt` only, no pool mountpoints, no locked or read-only datasets) still apply. The request shape was checked only against 25.10; on any other version the preview adds a warning saying so; check the result with `get_filesystem_acl`.

`upload_file` uses the same shared plan for preview and the real call: `filesystem.put` with `[path, {"append": false}]`, sent to the `/_upload` HTTP endpoint as a multipart body (a `data` part with `{"method", "params"}`, then the `file` part) with `Authorization: Bearer <API key>`; `mode` is not sent. TrueNAS creates the job, copies the file part into its input and replies with the job id. The `tasks_get` task (`upload_file`, kept 13 hours) is created when the call is confirmed and reports the download and upload progress; the job id is attached to it once the upload has been sent, and the task then follows the job. The request shape is the same on 25.10 and master. On 25.10 `filesystem.put` checks nothing about the path: it creates missing directories and follows symlinks, so every rule is enforced by this server. The plan validates the path as `set_filesystem_acl` does, stats the parent directory one component at a time and refuses a parent that is not its own real path, and stats the target (`ENOENT` means a new file; a symlink, a non-file or a realpath that differs is refused). It reads the dataset from `filesystem.statfs` (refusing a non-ZFS filesystem, a dataset of another pool, the pool's root dataset, a managed dataset by name and a read-only mount, and reading `avail_bytes`) and its lock state, and that of every dataset below it, from `pool.dataset.query`. A URL source is downloaded to a private temporary file (mode 0600) first, so a SHA-256 mismatch, a failed download or a stall never reaches TrueNAS, which writes the file as it receives it: streaming the source straight through would leave a corrupt file on any failure. Once the file is local, the plan is run again and its hash compared with the confirmed one; a change uploads nothing. A small window remains between that check and the write, which `filesystem.put` cannot close; it has no compare-and-swap and no way to write to a temporary name and rename. On 25.10 that window matters because the write runs as root and follows symlinks: anyone with write access to the target directory (for example through a share) who replaces the target file, or the directory itself or one of its parents, with a symlink between the final check and the `filesystem.put` call makes TrueNAS write the uploaded file wherever the symlink points, anywhere on the system, and create missing directories there. This server cannot close that gap; upload only into directories that untrusted users cannot write to. On master, `filesystem.put` opens the final path with `safe_open`, which refuses a symlink at the last component.

`create_vm`, `start_vm`, `stop_vm`, `poweroff_vm`, `restart_vm` and `reset_vm` use the same shared plan for preview and execution. Because TrueNAS's `vm.create` excludes `devices` on both 25.10 and master, `create_vm` calls `vm.create` (always sending `autostart: false` and `ensure_display_device: false` so middleware never adds an unpreviewed display device) followed by `vm.device.create` for each planned device, and then, only if `autostart: true` was requested, `vm.update` with `{"autostart": true}`. If a device attach fails, the VM is deleted with `vm.delete(id, {"zvols": false, "force": false})` and the error lists the zvols this call created (kept, for the operator to reuse or remove); if the rollback fails, the error says the VM still exists, gives its id, says autostart is off and lists the zvols. If only the final `vm.update` fails, the complete VM is kept with autostart off and the error says so. The middleware device validators that run only inside `vm.device.create` are mirrored up front so they cannot fail after `vm.create`: a non-blank DISPLAY password on 25.10 (or an unknown version), DISPLAY `port`/`web_port` not used by another VM's display (`vm.device.query` on `attributes.dtype = DISPLAY`) and not the reserved port 6000, `logical_sectorsize` not above `physical_sectorsize`, and on 25.10 `physical_sectorsize` only with `logical_sectorsize`. The DISK `serial` is not accepted, because TrueNAS generates it on create. The middleware also checks that the libvirt user can read a CDROM ISO, which cannot be checked from outside, so the ISO's permissions/ACL must allow it. Preflight reads `vm.virtualization_details`, `vm.maximum_supported_vcpus`, `system.info`, `vm.get_available_memory(false)`, `vm.query`, `vm.device.disk_choices`, `vm.device.nic_attach_choices` (handling both 25.10's flat map and 27.0+/master's `BRIDGE`/`MACVLAN` object), `vm.device.bind_choices`, `vm.device.query` (DISPLAY devices), `pool.dataset.query`, and `filesystem.stat`/`filesystem.statfs` for CDROM ISOs. Existing zvols must be in `vm.device.disk_choices`, not attached to any existing VM, unlocked, writable, and outside TrueNAS-managed datasets; `create_zvol` refuses existing zvols and verifies the parent filesystem dataset exists, is unlocked, writable, and has enough free space; CDROM ISOs must be regular files (no symlinks) inside an unlocked, non-managed child dataset below `/mnt/<pool>`. Display devices default to `bind: "127.0.0.1"`, refuse `0.0.0.0` and `::` unless `allow_wildcard_display_bind: true` is set, and never echo passwords in previews, summaries, errors or responses. `stop_vm` (`vm.stop`) and `restart_vm` (`vm.restart`) are tracked through `tasks_get`. `reset_vm` (`vm.reset`) is version-gated to TrueNAS 28.0+ (master) in `tools/version.go` because `vm.reset` does not exist on 25.10 or 27.x.

`update_vm_device` (`vm.device.update`), `delete_vm_device` (`vm.device.delete`), and `delete_vm` (`vm.delete`) also use the same shared plan for preview and execution and work on both TrueNAS 25.10 and master. `update_vm_device` enforces per-dtype attribute allowlists (rejecting `dtype` changes, read-only DISK `serial`, create-only DISK fields, and cross-dtype/unknown keys), refuses non-`STOPPED` VMs unless `force=true` (an MCP-side guard not sent to middleware, since middleware only updates stored config), and mirrors the middleware device validators on the effective merged attributes before confirmation: display password rules by version (strict when version is `< 27.0` or unknown), VNC `web=true` refusal below 27.0/unknown, display `port`/`web_port` clashes with other VMs' displays (excluding the device itself) plus reserved port 6000 and self-clashes, DISK `logical_sectorsize > physical_sectorsize` everywhere and `physical_sectorsize` without `logical_sectorsize` below 27.0/unknown, CDROM path checks (regular non-symlink file inside an unlocked, non-root, non-managed ZFS dataset, plus POSIX `libvirt-qemu` read/traverse checks when no ACL is present), and NIC attach choices (both 25.10 flat map and 27.0+/master `BRIDGE`/`MACVLAN`). `delete_vm_device` always refuses `RUNNING` or `SUSPENDED` VMs even with `force=true` because middleware rejects active VMs unconditionally (`options.force` only ignores backing storage deletion errors); with `zvol=true` or `raw_file=true` it validates managed-dataset guards, refuses any zvol or raw file shared with another VM device, and shows the size in the preview. `delete_vm` refuses non-`STOPPED` VMs unless `force=true` (protecting 25.10, where middleware otherwise powers off running VMs silently, and matching master), accepts both 25.10's `bool` and master's `null` return value, and with `zvols=true` checks every attached DISK zvol against managed-dataset guards and other VMs' attachments while listing every zvol with its size in the preview.

`offline_disk`, `online_disk` and `replace_disk` use the same shared plan for preview and the real call: `pool.offline` and `pool.online` with the pool id and `{"label": <device guid>}`, and `pool.replace` with the pool id and `{"label": <device guid>, "disk": <replacement disk identifier>, "force": false}`. The `disk` value is the replacement disk's `identifier` from the `disk.details` row the checks used (for example `{serial_lunid}...`), not its name: 25.10 `pool.replace` looks the disk up by identifier in `disk.details`. No tool accepts or sends a force option. Each reads the boot pool (`boot.get_state`), the named pool (`pool.query` with its topology and scan state) and the disk inventory (`disk.query`); a disk is named by its disk name or, for a device that is no longer present, by the ZFS guid `get_disk_health` shows. The boot pool is refused by name (`boot-pool`, `freenas-boot`, or the name `boot.get_state` reports), and so is a boot disk named as the target; if the boot pool cannot be read, the call is refused. Cache and spare devices, and vdevs with a replacement or hot spare already in progress (a `replacing-N` or `spare-N` child), are refused by every tool. Only mirror and RAIDZ vdevs are reasoned about: dRAID and other vdev types are refused. Special, dedup and log vdevs follow the same rules as data vdevs. A device whose ZFS guid is missing, empty or not a string is refused, since the device is named to TrueNAS only by its guid. Only leaf devices of the named pool match: a top-level vdev guid or a device of another pool is refused. Every state-dependent refusal is a blocked preview that still shows the pool, the vdev layout and the disk: an error with no token and no prompt. The disk name and serial shown for the target come from the pool's device path, which can be stale on older pools; every preview says so and that the call names the device by its guid.

`offline_disk` takes a disk offline only when the pool is `ONLINE`, reported healthy (`healthy: true`; a pool that reports read, write or checksum errors, or no health at all, is refused) and has no resilver running, the disk and every other disk of its vdev are `ONLINE`, and the vdev still tolerates at least one more failed disk afterwards (a mirror of three or more disks, or RAIDZ2 or RAIDZ3). A 2-way mirror, a RAIDZ1 vdev and a single-disk (stripe) vdev are refused. The preview shows the vdev layout before and after and how many more disk failures the vdev tolerates. `online_disk` brings back only a device whose ZFS state is `OFFLINE` and that is matched to a present disk; FAULTED, REMOVED, UNAVAIL or DEGRADED devices are left to `replace_disk`. Its preview says that ZFS resilvers only the data written while the disk was offline. Both read the device back afterwards and report its state, with a note if it is not what was asked for.

`replace_disk` checks the replacement disk against `disk.details` (with `join_partitions: true`): it must be in the unused list, in no imported or exported pool, not a boot disk, in the TrueNAS disk database, without a duplicate serial, with no partitions (25.10 `pool.replace` without force refuses a disk that is not clean, using the same partition list), and with a known size at least that of the disk it replaces (whose size must be known too). Its `disk.details` identifier must be set and equal to its `disk.query` identifier, which the token binds. Swap, self-encrypting drive locking and other open use of the replacement disk are not checked; the preview says so. A running resilver means a `RESILVER` scan in state `SCANNING`; a running scrub or a finished or cancelled resilver does not count. The disk being replaced must be in a mirror or RAIDZ vdev. If any other disk of that vdev is not `ONLINE`, only a failed or offline disk can be replaced; replacing a healthy disk is refused while any device of the pool is not `ONLINE`, and a vdev with more failed disks than it tolerates is refused. It is also refused while a resilver or any other `pool.replace` job is running (`core.get_jobs`). The preview shows both disks with their serials and sizes, the vdev layout, and that the replacement disk's contents are destroyed. `pool.replace` is a job; its id is tracked as a `tasks_get` task (`replace_disk`, 24 hours). The job finishes once ZFS has started the replacement; the resilver continues afterwards and is followed with `get_disk_health` or `query_pools`. The old disk stays in the vdev until the resilver completes. As with the other destructive tools, a small window remains between the final re-plan and the middleware call, which `pool.offline`, `pool.online` and `pool.replace` cannot close because they have no compare-and-swap; TrueNAS's own checks on those calls still apply.

`download_update` passes optional `train` and `version` through to `update.download`. Both must be set, or both omitted (matching 25.10's `UpdateDownloadArgs` and `plugins/update_/download.py`). When both are omitted, or both are blank or whitespace, they are treated as omitted and the middleware call still has empty parameters (`params: []`) and downloads the update offered by the configured profile, as before. When both are set, the call is `update.download` with those two positional arguments. A request for only one of them is refused. A completed download (`update_download_progress.percent == 100`) is not treated as already done if the caller asked for a different `version`.

> **Behaviour change:** `download_update` no longer ignores `train` and `version`. Clients that sent those fields expecting the configured train must now omit both.

A dry run of `refresh_directory_cache` now returns an error when no directory service is configured or the service is not enabled, matching the real call, instead of a successful preview with a warning. A dry run of `create_scrub_schedule` now errors when a schedule already exists, matching the real call. A dry run of `run_scrub` now matches the real call: if a finished scrub is still within the threshold, the preview reports that no scrub will start (the real call already returned `scrub_started: false`). If a scrub is already running, or the pool cannot be scrubbed, the dry run returns an error instead of a successful preview with a warning.

`install_app`, `create_dataset`, `create_smb_share`, and `create_nfs_share` use the same shared plan for preview and the real call (`app.create`, `pool.dataset.create`, `sharing.smb.create`, and `sharing.nfs.create`). `create_smb_share` and `create_nfs_share` reject a path that contains a `..` segment after cleaning, or that resolves outside `/mnt/<pool>` (the pool is the first component after `/mnt/` on the original path). The middleware payloads are unchanged: `install_app` still sends `version` as `"latest"` when it was omitted, and dataset and share bodies keep the same fields and defaults. Jobs started by `install_app` are tracked with `WithExpectedMethod` (`app.create`). Secrets inside `install_app` `values` (passwords, tokens, and similar, including nested ones) are masked in the preview, in rejection and error text, in stored task arguments, and in debug logs; they are still sent to the middleware on the real call. Their dry runs now include `"dry_run": true, "executed": false` like the other plan-backed previews. Dataset ancestor lists and share security warnings appear in the plan preview (`current_state` / `warnings`) rather than as custom top-level fields. A dry run of `install_app` now matches the real call: a missing host-path dataset or an existing instance name is an error on both paths, not a successful preview with a warning, and a catalog version with no schema is still refused on both. `create_dataset` preflight (the pool must exist and the name must be free) now also runs on the real call.

> **Behaviour change:** dry-run previews for `create_dataset`, `create_smb_share`, and `create_nfs_share` now use the shared plan shape (`current_state`, `planned_actions`, `warnings`, `"dry_run": true, "executed": false`) instead of a custom top-level `payload` / `security_warnings` object. Clients that read those old top-level fields should use the plan preview. A dry run of `install_app` now returns an error when a host-path dataset is missing or the instance name is already in use, matching the real call, instead of a successful preview with a warning.

`refresh_directory_cache` reports `status: "submitted"` (with `task_id` / `job_id` when TrueNAS returns a job id, tracked with `WithExpectedMethod` for `directoryservices.cache_refresh`) instead of `cache_refreshed: true` as soon as the job is submitted. A first Active Directory cache fill can take a significant amount of time; track the task for the result.

> **Behaviour change:** `refresh_directory_cache` no longer reports `cache_refreshed: true` when the `directoryservices.cache_refresh` job is only submitted. Clients should treat `status: "submitted"` like the other directory-service tools and poll the task.

`get_app_catalog_details` returns the app schema (groups and questions, including each question's defaults) and, when the schema defines them, a `defaults` object with run-as user/group and resource limits taken from `catalog.get_app_details`. Resource defaults keep the schema nesting (`defaults.resources.limits.cpus` and `defaults.resources.limits.memory`). Only numeric defaults are passed through. When the schema has `run_as_custom_user` and its default is false, `run_as` is omitted because those values only apply when that flag is true. When the schema has `enable_resource_limits` and its default is false, `defaults.resources.limits` is omitted for the same reason. Those defaults are omitted when the schema has none. Workflow guidance for installation (review schema groups, query pools, create datasets, use `host_path`, use schema defaults when present) lives in the `get_app_catalog_details` and `install_app` tool descriptions, worded so any MCP client can follow it.

> **Behaviour change:** `get_app_catalog_details` no longer includes a `wizard_guidance` object with client-specific instructions (for example a named tool of one MCP client) or fixed run-as and resource defaults. Clients that read `wizard_guidance` should use the schema and the `defaults` field (when present) instead, and follow the tool descriptions for the install workflow.
