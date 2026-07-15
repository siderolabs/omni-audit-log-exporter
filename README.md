# omni-audit-log-exporter

An audit log exporter for [Omni](https://github.com/siderolabs/omni).

It connects to the Omni API using a service account, follows the audit log and writes each event to stdout as a line of JSON.
Across reconnects and restarts it resumes from where it left off.
Logs go to stderr, so stdout carries nothing but the events and can be piped into any log shipper or file.

It requires an Omni version with audit log follow support in the management API, and the audit log must be enabled on the instance.

## Quick start

Create a service account on your Omni instance:

```sh
omnictl serviceaccount create --use-user-role=false --role=Admin audit-log-exporter
```

The account must have the `Admin` role, since reading the audit log is an administrative operation.
Note that `--use-user-role` defaults to true, which would clone the role of the user running the command instead.

The command prints an `OMNI_SERVICE_ACCOUNT_KEY`.
Run the exporter with it:

```sh
docker run -d \
  -v audit-log-exporter-state:/state \
  -e OMNI_ENDPOINT=https://<account>.omni.siderolabs.io \
  -e OMNI_SERVICE_ACCOUNT_KEY=<key> \
  ghcr.io/siderolabs/omni-audit-log-exporter:latest --state-file=/state/position
```

The events appear on the container's stdout, one JSON object per line.
The first run exports everything the instance still retains, then keeps following.
The key can alternatively be read from a file via `--omni-service-account-key-file`, e.g. for Kubernetes secret mounts.
See `--help` for all flags.

## Resuming and the state file

The exporter tracks its position in the audit log and persists it to the state file: periodically while events flow, and once more on every reconnect and on shutdown.
On a restart it resumes exactly after the last persisted position.
The file holds a single number managed by the exporter, treat it as opaque.

Without `--state-file` nothing is persisted and every start begins at `--start-from`, which is useful for kicking the tires and little else.

`--start-from` positions the export when there is no state yet:

- `beginning` (the default): everything the server still retains, so the first run exports the full retained history.
- `now`: new events only.
- an RFC3339 time, e.g. `2026-07-01T00:00:00Z`: events from that time on.

Once a position exists, `--start-from` is ignored.

## Delivery semantics

Events are delivered at least once and in insertion order within a connection:

- Reconnects, including the periodic ones initiated by the server, resume exactly where they left off, without repeating or skipping events.
- A restart resumes from the persisted position: a hard crash can re-emit the events processed after the last checkpoint, at most a few hundred, so downstream consumers should tolerate duplicates.
- A stopped or slow exporter does not stall Omni: the audit log retention keeps deleting old events, and events that age out before the exporter reads them are gone.
  Keep the exporter running, and use `--start-from beginning` on a fresh state to re-export everything that still exists.

Two failures end the exporter instead of being retried, because retrying will not fix them:

- The server does not support following the audit log (an older Omni version).
- The server no longer knows the resume position, e.g. because its database was restored from a backup.
  Recover by removing the state file: the next start positions itself with `--start-from`.

Everything else, including an unreachable server and authentication failures, is retried with backoff forever.
Once the server is back, the export resumes without loss.

## License

Mozilla Public License 2.0, see [LICENSE](LICENSE).
