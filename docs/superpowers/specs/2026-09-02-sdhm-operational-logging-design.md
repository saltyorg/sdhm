# SDHM Operational Logging Design

Date: 2026-09-02
Status: Proposed for review

## Purpose

Make every actionable SDHM failure, recovery, and lifecycle transition visible in `journalctl` without restoring v1.0.4's per-event and per-update noise. Health remains the authoritative current-state API; logs become a durable transition history that explains when health changed and when an automatic safety action occurred.

## Current Findings

- Saltbox's unit captures stdout and stderr in journald. Terminal construction, configuration, Docker Ping, listener, preparation, health-server, shutdown, and Docker-close failures already reach one process-boundary stderr line.
- Snapshot and hosts-Apply failures reach both health and a `WARN` log.
- Docker event-stream loss and recovery are health-only.
- Successful reconciliation after a failure is health-only, so a warning has no journal closure record.
- Successful recovery of corrupt markers from a validated backup is silent and creates no health history.
- Successful target rollback after a late replacement failure is implicit in the returned primary error.
- The only application lifecycle record is emitted before Docker construction and initial reconciliation.
- Plain `slog` text written to the journal stream arrives with native journal priority `6`; `level=WARN` exists only inside `MESSAGE`.

The live v1.0.4 journal also demonstrates what must not return: unmonitored bridge events, container-name lookup solely for logging, every monitored event, and every successful hosts update created high-volume noise.

## Logging Contract

Logs describe transitions, not every attempt or event.

| Transition | Level | Message | Required fields | Suppression |
|---|---|---|---|---|
| Process invocation | INFO | `starting SDHM` | `version`, `networks`, `default_network`, `interval`, `health_addr` | Once per process |
| Initial reconciliation succeeds | INFO | `SDHM ready` | None | Once per process |
| Initial or later reconciliation fails | WARN | `reconciliation failed` | `err`, `retry_in` when retrying | First failure and when exact error changes |
| Reconciliation succeeds after failure | INFO | `reconciliation recovered` | None | Once per failed-to-healthy transition |
| Event stream becomes unavailable | WARN | `Docker event stream unavailable` | `err`, `retry_in` | First failure and when exact error changes |
| Event stream becomes healthy | INFO | `Docker event stream recovered` | `evidence=event` or `evidence=stable` | Once per failed-to-healthy transition |
| Corrupt target restored from backup | WARN | `hosts file recovered from validated backup` | None | Once during preparation |
| Late target replacement rolls back successfully | WARN via reconciliation error | Existing replacement error plus explicit `target restored from retained snapshot` | Included in `err` | Once per distinct reconciliation failure |
| Clean `Run` completion | INFO | `SDHM stopped` | None | Once per process |
| Terminal process error | ERROR/native priority 3 | Existing concise error text | None | Once at process boundary |

The following remain silent:

- individual Docker events, container names, and unmonitored networks;
- successful periodic no-op reconciliations;
- ordinary successful hosts mutations;
- reconnect attempts that repeat the same failure;
- health-response writes abandoned by the client;
- endpoint omission for intentionally unpublishable containers.

Clean shutdown already has systemd stop/deactivation records, but one final application record confirms that SDHM joined its listener, event mapper, and Docker client successfully.

## State Ownership

The daemon remains the only logging policy owner.

- `hosts.Store` returns a typed preparation outcome instead of receiving a logger.
- `PrepareResult.RecoveredFromBackup` tells the daemon that a material repair succeeded.
- A successful late rollback is added to the contextual error string at the storage boundary, so health and the daemon warning carry the same safety result.
- Reconciliation and event-stream transition state remains local to the existing coordinator loop. It stores only the last active error string; identical retry failures do not generate duplicate records.
- Recovery logs occur only when that stored failure state is cleared by observed success.

The consumed interface becomes:

```go
type PrepareResult struct {
    RecoveredFromBackup bool
}

type HostStore interface {
    Prepare(context.Context) (PrepareResult, error)
    Apply(context.Context, []Endpoint) error
}
```

No public CLI option or dynamic log-level configuration is added.

## Native Journal Priority

`cmd/sdhm` owns a private process logger. Outside journald it retains the current `slog.TextHandler` output exactly. When `JOURNAL_STREAM` is present, a private handler prefixes each complete text record with the syslog priority prefix recognized by systemd's default `SyslogLevelPrefix=yes` stream parser:

- DEBUG: `<7>`
- INFO: `<6>`
- WARN: `<4>`
- ERROR: `<3>`

The handler delegates formatting, attributes, groups, and minimum-level behavior to standard `slog.TextHandler` instances. A shared writer mutex keeps the prefix and complete record atomic across levels. `WithAttrs` and `WithGroup` return cloned wrappers around the corresponding delegated handlers.

The process-boundary terminal error writer uses `<3>` only when `JOURNAL_STREAM` is present. Interactive execution keeps the current unprefixed output. This requires no third-party dependency and no Saltbox template change; historical systemd units retain the default prefix parser.

Implementation must follow the official Go `slog.Handler` contract and its concurrency rules: <https://pkg.go.dev/golang.org/x/example/slog-handler-guide>. Systemd stream behavior is defined by `SyslogLevel=` and `SyslogLevelPrefix=` in the official execution manual: <https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html>.

## Error and Recovery Semantics

- Do not log and return the same terminal error. Continued failures log inside the daemon; terminal errors return to the single process boundary.
- Health concern/history semantics do not change.
- A recovery record must be based on observed success, never elapsed time or a reconnect attempt.
- Caller cancellation must not create false stream-loss or reconciliation warnings.
- If a rollback fails, the existing joined primary and rollback errors remain intact. Only successful rollback gains explicit text.
- Logging failures remain best-effort and must not alter daemon control flow or health.

## Testing

- Process logger unit tests cover journal/non-journal formatting, all four priority mappings, terminal error prefixes, `WithAttrs`, `WithGroup`, and concurrent record integrity.
- Hosts tests cover every preparation outcome and explicit rollback-success/failure text.
- Daemon tests use a synchronized recording handler to assert exact level, message, and fields.
- `testing/synctest` cases prove identical failure suppression, changed-error logging, one recovery record, reconnect recovery through event/stability, and no false warning on cancellation.
- Existing health, retry, timer, file-safety, and shutdown behavior must remain byte- and state-compatible.
- `make check TEST_FLAGS='-count=1'` and coverage remain required.

## Acceptance

Use an Ubuntu 26.04 `minimal` Saltbox VM with isolated synthetic networks and temporary hosts files. The exact committed static binary must demonstrate:

1. `starting SDHM` and `SDHM ready` at native priority 6.
2. A topology-stable Docker API pause producing `reconciliation failed` at priority 4, followed by `reconciliation recovered` at priority 6.
3. Validated marker restoration producing one recovery warning at priority 4.
4. Event-stream loss/recovery producing one warning and one recovery record without per-event logs.
5. A deliberately invalid invocation producing one terminal priority-3 journal record.
6. Clean SIGTERM producing `SDHM stopped`, no later file mutation, and no remaining process.

All fixtures are removed and the VM is destroyed after success. This logging-specific qualification supplements rather than relabels the existing composite Saltbox acceptance evidence.

## Compatibility

- Historical Saltbox command lines, version parsing, artifact names, and systemd dependencies remain unchanged.
- The health schema and current-state transitions remain unchanged.
- Normal interactive text logs remain unprefixed and human-readable.
- Journal volume stays bounded to failures, changed failure reasons, recoveries, material automatic repair, and lifecycle boundaries.
