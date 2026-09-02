# SDHM Operational Logging Design

Date: 2026-09-02
Status: Implemented and qualified 2026-09-02

## Purpose

Make every actionable SDHM failure, recovery, and lifecycle transition visible in `journalctl` without restoring v1.0.4's per-event and per-update noise. Health remains the authoritative current-state API; logs become a durable transition history that explains when health changed and when an automatic safety action occurred.

## Implementation Status

The implementation through reviewed head `1e725d1283a05c8776415ce8422b8025f8a79cc5` supplies the process, daemon, and storage transitions defined below. Local tests cover record levels, messages, fields, suppression, rollback wording, cancellation behavior, and process priority-prefix formatting.

The native-priority mapping and transition contract were qualified on 2026-09-02 in an Ubuntu 26.04 `minimal` guest using isolated synthetic networks and temporary hosts files. This is not a real Saltbox deployment or a `core`-profile qualification; the evidence and its exact boundary are recorded below.

The live v1.0.4 journal established the noise that must not return: unmonitored bridge events, container-name lookup solely for logging, every monitored event, and every successful hosts update.

## Logging Contract

Logs describe transitions, not every attempt or event.

| Transition | Level | Message | Required fields | Suppression |
|---|---|---|---|---|
| Process invocation | INFO | `starting SDHM` | `version`, `networks`, `default_network`, `interval`, `health_addr` (address and port) | Once per process |
| Initial reconciliation succeeds | INFO | `SDHM ready` | None | Once per process |
| Initial reconciliation fails | WARN | `reconciliation failed` | `phase=initial`, `err`, `retry_in` | Once after the first failed reconciliation |
| Later reconciliation fails | WARN | `reconciliation failed` | `err`, `retry_in` | First failure and when exact error changes |
| Reconciliation succeeds after failure | INFO | `reconciliation recovered` | None | Once per failed-to-healthy transition |
| Event stream becomes unavailable | WARN | `Docker event stream unavailable` | `err`, `retry_in` | First failure and when exact error changes |
| Event stream becomes healthy | INFO | `Docker event stream recovered` | `evidence=event` or `evidence=stable` | Once per failed-to-healthy transition |
| Corrupt target restored from backup | WARN | `hosts file recovered from validated backup` | None | Once during preparation |
| Late target replacement rolls back successfully | WARN via reconciliation error | Existing replacement error plus explicit `target restored from retained snapshot` | Included in `err` | Once per distinct reconciliation failure |
| Clean `Run` completion | INFO | `SDHM stopped` | None | Once per clean process completion; omitted after a terminal daemon error |
| Terminal process error | ERROR/native priority 3 | One concise command error string | None | Once at process boundary; invalid configuration has no usage dump |

The following remain silent:

- individual Docker events, container names, and unmonitored networks;
- successful periodic no-op reconciliations;
- ordinary successful hosts mutations;
- reconnect attempts that repeat the same failure;
- health-response writes abandoned by the client;
- endpoint omission for intentionally unpublishable containers.

`SDHM ready` means the health listener was started, hosts preparation completed, and the first reconciliation succeeded. `SDHM stopped` means `Run` returned cleanly after its shutdown cleanup; it is not a record for a failing daemon run. Health is therefore current state, while the journal preserves transition history even after health has recovered.

`sdhm regenerate` records `regenerating hosts file` at INFO with `path`; either command warns `SDHM should run as root to modify the hosts file` when its effective UID is not root. These command records do not alter the daemon transition policy.

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

The process-boundary terminal error writer uses `<3>` only when `JOURNAL_STREAM` is present. Interactive execution keeps the current unprefixed output. This requires no third-party dependency and no Saltbox template change; historical systemd units retain the default prefix parser. Local tests cover the prefix formatter, and the qualification below demonstrates the parser configuration and resulting `PRIORITY` values.

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

## Qualification Evidence

- Reviewed head: `1e725d1283a05c8776415ce8422b8025f8a79cc5`; static stripped linux/amd64 artifact SHA-256: `3a2dcbd7ada7c9d07751be6609af3af9e42c81e5b4e0022d5c100a3599dbb76a`; exact version: `sdhm version 0.0.0-dev`.
- Environment: Ubuntu 26.04 `minimal` guest `49dcbf99c8a46e0a6e2ac3831ee2cc2e30975231f4e58e1ae7468fa261aaf040`, isolated synthetic `saltbox` and `sdhm-extra` networks, and temporary hosts/backup files. Each transient unit reported `SyslogLevelPrefix=yes` without an injected property.
- PASS assertions: one startup and ready record at `PRIORITY=6`; a topology-stable fixed-timeout Docker pause produced one reconciliation warning at `PRIORITY=4`, then one recovery at `PRIORITY=6` after PID-fenced resume with unchanged hosts and topology; validated marker repair produced one warning at `PRIORITY=4`, then ready/HTTP 200; standalone stream loss produced one warning at `PRIORITY=4` and one stable-evidence recovery at `PRIORITY=6` with no per-event or container-name record; invalid `--health-port 0` produced one priority-3 record without usage; and direct SIGTERM produced `SDHM stopped` at priority 6, exit zero in 4 ms, and no later hosts mutation.
- Retained harness SHA-256: `8bbe3840ca13527a5bbedb2b388022cb4e22686b9d857027502f57eb0c5fb657`. Final evidence comprises a verified 71-file manifest, deterministic tree SHA-256 `1aa5aaa95cecc230876fd98bfc066d227a00340dc101af12064ab380c2d34f53`, and report SHA-256 `3c1ffb6be1a177ea81bd0d6b5f5aaef098ff8f8e94e7516419751e76ae55e967`.
- Attempt 1 (tree SHA-256 `99b417698efcf3d80bf664d023c22daf9c6fe0376a0662e1ea61aa612a135f96`) over-counted the legitimate reconciliation transitions that followed Docker restart; attempt 2 (tree SHA-256 `b99f38f8c316c185cd33d8b5aed4a9a85bcd64c15b65ca19fcac8489904cf976`) misread nanosecond fractional output as milliseconds and lacked JSONL slurping in two diagnostic predicates. Both were harness-only outcomes, cleaned before the PASS run, and do not represent product failures.
- Root coordinator destroy operation `594a1bb4-37c6-4593-8855-5d482ce58f03` removed the guest. Final helper inspection was overall healthy with `test-a`, `test-b`, and `proxmox-runner` absent.

This qualifies the documented logging transitions in the stated minimal synthetic environment only. It does not claim a real Saltbox service run, a `core`-profile run, a Saltbox template change, or broader release qualification.

## Compatibility

- Historical Saltbox command lines, version parsing, artifact names, and systemd dependencies remain unchanged.
- The health schema and current-state transitions remain unchanged.
- Normal interactive text logs remain unprefixed and human-readable.
- Journal volume stays bounded to failures, changed failure reasons, recoveries, material automatic repair, and lifecycle boundaries.
