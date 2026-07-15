# omni-audit-log-exporter — agent guide

This is the ever-growing knowledge base for the project.
Maintain it as you go: whenever you learn something durable about how this repo works, add it here.
It is not append-only, so fix or delete anything that becomes wrong or outdated.
Only capture timeless, project-general knowledge here, never in-flight work or an individual's local setup.
Prefer pointers over copies and invariants over inventories: flag lists, constants and version numbers live in the code and rot here.

Markdown note: this file is linted (`markdownlint` with the `sentences-per-line` rule), so keep one sentence per line and the first line as the single top-level heading.

`CLAUDE.md` and `GEMINI.md` just import this file, so this is the one place to edit.

## What this repo is

A daemon that follows the audit log of an [Omni](https://github.com/siderolabs/omni) instance and writes each event to stdout as a line of JSON, resuming where it left off across reconnects and restarts.
Logs go to stderr, so stdout carries nothing but the events and can be piped into any log shipper or file.
There is deliberately no HTTP surface, no output plugins and no filtering: the tool does one thing, and downstream tooling composes with it through stdout.

It authenticates with an Omni service account that must have the `Admin` role, because reading the audit log is an administrative operation.
It requires an Omni version whose management API supports following the audit log, and the audit log must be enabled on the instance.
A disabled audit log surfaces as an endless retry loop rather than a distinct failure, because the server reports it as a plain error.

## The cast and where the logic lives

- Omni stores the audit log and serves the follow mode of its management API `ReadAuditLog` RPC.
  The storage, retention cleanup, stream lease and follow protocol all live there, not here.
- The Omni client library (`omni/client/pkg/client/management`) provides `FollowAuditLog`, an iterator that owns the follow protocol details: it sets the follow flag, verifies the acknowledgment handshake, reconnects transparently when the server ends a stream cleanly at its lifetime bound, and advances the resume position.
  Any error ends the iteration, and the caller decides the retry policy.
- This exporter is the thin remainder: position persistence, retry-with-backoff, fatal-versus-retryable classification, and writing the events out.
  Keeping the reconnect logic in the client library was a deliberate choice, so that other consumers (such as `omnictl audit-log --follow`) share it.

## The follow protocol, as this repo depends on it

The protocol semantics are defined and tested on the Omni side, and the contract of record is the doc comment on `FollowAuditLog` in the client library.
The exporter's correctness leans on a few of its invariants:

- Every follow stream starts with an event-less acknowledgment response carrying the resolved start position as its id.
  The acknowledgments are yielded to the consumer like events, so persisting their ids gives an exact recovery baseline even when a stream dies before delivering any event.
- Positions are storage ids, monotonically increasing, and a resume reads strictly above the given position, so it is exact: everything newer, each event exactly once.
- Omni's cleanup never deletes the newest event, so ordinary retention can never invalidate a position: a lost position means a replaced database, not retention at work.
- Timestamps position only the very first contact, when no id is known yet: the full retained history, the current tail, or a point in time.
  Once any response arrives, ids take over.
- The server bounds every stream's lifetime and ends it cleanly, forcing periodic re-authentication.
  The client library reconnects on these clean ends itself, opening each new stream with a fresh acknowledgment, so an error reaching the exporter is always a real failure.
- A resume position the server no longer knows (in practice, a database restored from a backup) fails the stream with a gRPC `FailedPrecondition`.
  There is no safe automatic recovery, so this is surfaced to the operator instead of guessed around.

## Delivery semantics and the state file

The contract is at least once and in insertion order: reconnects and clean restarts with a state file resume exactly, while a hard crash replays from the last durable checkpoint.

- The state file holds a single number, the id of the last processed response, written atomically and durably (temp file, fsync, rename, directory fsync).
  It is checkpointed on every acknowledgment, after a bounded number of events, and whenever a stream ends, including at shutdown.
  There is no per-event fsync by design: a hard crash (kill -9, power loss) re-emits at most the events since the last checkpoint, bounded by the event threshold.
- The in-memory position never advances past an event that failed to write to the output, so a write failure can never lose an event.
- A persisted position always wins over the start-position flag, and a corrupt state file is a startup error, never silently a fresh start.
- Loss requires the retention cleanup on the Omni side outrunning a stopped or extremely slow exporter: events it deleted are gone.
  Recovery from any position mishap is a fresh state plus the start-from-beginning flag.

## Fatal versus retryable

Everything transient (an unreachable server, authentication failures at runtime) is retried forever with jittered backoff, resuming from the last position.
The doubling applies to the already jittered delay, so the wait grows by about half per attempt and reaches the ceiling later than a plain doubling would.
The backoff resets only when an actual event was delivered, not on a bare acknowledgment: a server that fails right after every handshake would otherwise be hammered at the backoff floor forever.

Four kinds of failure end the process instead, because retrying cannot fix them: a server without follow support (also the verdict when a stream ends cleanly before ever acknowledging), a lost resume position (`FailedPrecondition`), an identity not allowed to read the audit log (`PermissionDenied`), and failures to write an event or the state file.
Missing permission ends the run because no amount of waiting will grant it.
A rejected signature is retried instead, because that can be transient.
Permission taken away mid-export ends the process within the stream lease, not at the next restart, because every reconnect re-authorizes.
A canceled context is a clean shutdown after a final checkpoint, and only a failure of that checkpoint makes it exit non-zero.
The shutdown logs the position it stopped at, which is the only thing that distinguishes it from a crash in the log.

## Code map

- `cmd/omni-audit-log-exporter`: the cobra entrypoint, following the layout of the other siderolabs single-binary projects.
  Thin by design: flags, logger (stderr only), client construction with fail-fast key validation, then hand-off.
- `internal/exporter`: everything real.
  `New` parses the start position and loads the persisted state.
  `Run` loops follow iterations with backoff and the fatal-versus-retryable classification.
  The dependency on the client is the single-method `AuditLogFollower` interface, which the management client satisfies, so tests script a fake follower instead of a gRPC stack.
- The test convention: a scripted fake serves one canned iteration per connection attempt, and running out of scripts cancels the run context, which is the clean-shutdown path.
  End-to-end coverage of the real client against the real server lives in the Omni repo, next to the protocol.

## Verifying changes

Gates, in order, using the make targets, which are authoritative: `make lint-fmt`, `make lint` (includes govulncheck and markdownlint) and `make unit-tests`.

## Generated files and rekres

Files generated by kres must not be hand-edited, and a generated file identifies itself with a generated-file comment near its top.
To change them, edit `.kres.yaml` and run `make rekres`.
kres scans the tracked source tree at generation time: it wires root-level `.md` files into the markdown-lint stage, and writes Dockerfile `COPY` lines only for source directories that exist, so re-run rekres after adding a top-level source directory.

## Dependency bumps

Bump direct deps only: `go list -m -f '{{if and (not .Main) (not .Indirect)}}{{.Path}}@upgrade{{end}}' all | xargs go get`, then `go mod tidy`.
Never use `-u` or `all`, since those drag indirect deps past what direct deps require.
The `omni/client` module is versioned with Omni releases, so following support in a released client is the floor for this repo's dependency.
When `go.mod` carries a `replace` for `omni/client`, it is a temporary pin and the bump does not apply to it.

## Release process

Releases are cut with the repo's own tooling (`hack/release.sh` and `hack/release.toml`) in two phases.
First a release PR sets `hack/release.toml` `previous` to the latest tag, regenerates version files, runs `hack/release.sh changelog vX` to update the changelog, and creates the `release(vX): prepare release` commit via `hack/release.sh commit vX` (the script adds a DCO sign-off, and commits are GPG-signed when git is configured to sign).
Then, after merge, a signed tag is pushed, which triggers CI to build and push the release image and draft a GitHub release.
A deps bump and a release are separate PRs.

## Commit and PR conventions

Siderolabs PRs often contain a single commit, but multiple commits are fine for separate atomic logical changes (conform sets `maximumOfOneCommit: false`).
A single commit is usually still preferable, since the PR title and body then equal the commit title and body minus the DCO trailer.
Either way, keep PR titles and bodies simple like a commit message, without fancy markdown, and not long like a documentation page.
The commit title follows the conform rules: a `type[(scope)]: imperative summary` line, with types such as `feat`, `fix`, `chore`, `refactor`, `test`, `docs` and `release`.
Commits carry a DCO `Signed-off-by` trailer (`git commit -s`) and are GPG-signed.
Reference issues inline as sentences, for example `Closes siderolabs/omni#<n>.`

## Related projects

- `siderolabs/omni` is the server this exporter follows, and also the home of the client library, the follow protocol, and its end-to-end tests.
- `siderolabs/omni_exporter` is the metrics sibling: it exposes the current state of an Omni instance as Prometheus metrics, while this repo streams the audit event history.
  State versus events is the line between them.
- `omnictl audit-log --follow` is the interactive consumer of the same client API, useful for eyeballing what the exporter would receive.
