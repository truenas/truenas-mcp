# Live TrueNAS integration tests

An opt-in suite talks to a real TrueNAS system (25.10 or 27) using the same
environment variables as the server: `TRUENAS_URL`, `TRUENAS_API_KEY`, and
`TRUENAS_API_USER`. It is **not** part of `make test` or the pull-request
checks. Run it against both 25.10 and 27 before a release.

## Run

```bash
export TRUENAS_URL=truenas.example.com
export TRUENAS_API_USER=truenas_admin
export TRUENAS_API_KEY=your-api-key
# Self-signed certificate: trust it, or (less safely) skip verification
export TRUENAS_TLS_CA=/path/to/truenas-cert.pem
# export TRUENAS_INSECURE=1

make integration
# or: go test -tags integration -count=1 -timeout 30m ./tools/ -run 'TestIntegration'
```

If the three required variables are missing, the tests skip. Optional:

| Variable | Purpose |
| --- | --- |
| `TRUENAS_TLS_CA` | PEM file to trust instead of the system store |
| `TRUENAS_INSECURE=1` | Disable TLS verification (last resort) |
| `TRUENAS_PROXY` | Proxy for the connection, or `none` |
| `TRUENAS_INTEGRATION_POOL` | Pool for the scratch dataset (default: first pool) |
| `TRUENAS_INTEGRATION_WRITES=1` | Enable the scratch write cycle (create a dataset; optional app cycle) |
| `TRUENAS_INTEGRATION_APP` | Installed app for stop / start (and delete if allowed) |
| `TRUENAS_INTEGRATION_ALLOW_APP_DELETE=1` | Allow `delete_app` on `TRUENAS_INTEGRATION_APP` after confirmation |
| `TRUENAS_INTEGRATION_DUMP` | Directory to write each tool's JSON result as a fixture |

Use a lab or otherwise disposable system. The default run only calls read
tools and every write tool with `dry_run: true`, so it should not change the
NAS. The scratch write cycle is separate and requires `TRUENAS_INTEGRATION_WRITES=1`.

## What it covers

- Every read tool, with required arguments filled from a short discovery
  pass (`query_pools`, `query_apps`, `list_alerts`, and so on).
- Every write and destructive tool's dry run, including the confirmation
  preview for destructive tools. Dummy values are used when the system has
  no matching object (for example a fake alert UUID); a "not found" or
  "cannot proceed" error still counts as exercising the handler.
- A scratch write cycle, when enabled: create a uniquely named dataset
  under the chosen pool, then delete it through `pool.dataset.delete`
  (there is no `delete_dataset` tool). If `TRUENAS_INTEGRATION_APP` is set,
  the cycle also stops and starts that app through the confirmation-token
  flow. Delete is only attempted when `TRUENAS_INTEGRATION_ALLOW_APP_DELETE=1`.

Installing a new catalog app is not done automatically: values and host
paths are site-specific. Dry-run `install_app` with a placeholder is
still part of the dry-run pass.

## Fixtures

Set `TRUENAS_INTEGRATION_DUMP` to a directory and the suite writes each
tool result (or error) as JSON. Use those files to replace hand-written
unit-test shapes when a live response does not match what the fakes
assumed. Commit fixtures only after checking they contain no host names,
API keys, or other private data.
