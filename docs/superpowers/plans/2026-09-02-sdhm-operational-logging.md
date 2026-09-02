# SDHM Operational Logging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add bounded, journal-visible failure, recovery, safety, readiness, and shutdown transitions without restoring per-event or per-update log noise.

**Architecture:** The existing daemon remains the logging policy owner. Hosts preparation returns one typed recovery outcome, the coordinator keeps only last-failure transition state, and `cmd/sdhm` owns a private systemd-aware `slog.Handler` that maps levels to native journal priorities while preserving current interactive text output.

**Tech Stack:** Go 1.27.0, `log/slog`, standard `testing`, `testing/synctest`, systemd journal streams, Cobra, Make, Saltbox VM MCP.

**Spec:** `docs/superpowers/specs/2026-09-02-sdhm-operational-logging-design.md`

**Implementation status:** The logging source contract is implemented through `2eeef4cdd234da4c9bc3258b124f5496ba2711bc`. Its canonical messages, fields, transition suppression, readiness and clean-stop meanings, and priority mapping are recorded in the linked design. Task 5 Steps 1--3 record that local contract; independent review, Linux artifact construction, and VM journal acceptance remain pending and must not be inferred from local tests.

## Global Constraints

- Log state transitions only; do not log individual Docker events, container names, unmonitored networks, successful no-op reconciliations, or every successful hosts update.
- Keep health state/history authoritative and byte-compatible.
- Do not log and return the same terminal error; terminal errors remain one process-boundary record.
- Recovery logs require observed success, never elapsed time or a reconnect attempt.
- Repeated identical failures produce one transition record; a changed exact error may produce another.
- Caller cancellation must not create false failure or recovery logs.
- Outside journald, retain the current unprefixed `slog.TextHandler` format.
- Add no third-party logging or journald dependency and no new CLI flag.
- Preserve historical Saltbox flags, unit dependencies, version parsing, and artifact names without requiring a Saltbox repository change.
- Use `apply_patch` for edits, TDD for every behavior change, `testing/synctest` or channels instead of timing sleeps, and Conventional Commits for every commit.

## Final File Map

```text
cmd/sdhm/logging.go                 process logger, journal priority handler, terminal error writer
cmd/sdhm/logging_test.go            level mapping, attrs/groups, concurrency, interactive output
cmd/sdhm/main.go                    one priority-aware terminal error boundary
cmd/sdhm/run.go                     logger construction, startup fields, clean-stop record
cmd/sdhm/run_test.go                startup/stop logging assertions

daemon/types.go                     PrepareResult and revised HostStore contract
daemon/daemon.go                    preparation recovery and ready records
daemon/loop.go                      reconciliation and event-stream transition records
daemon/daemon_test.go               preparation, readiness, and cancellation logging tests
daemon/loop_test.go                 deterministic failure suppression and recovery tests
daemon/logging_test.go              synchronized slog record collector shared by daemon tests

hosts/store.go                      preparation outcome and explicit rollback-success context
hosts/store_test.go                 outcome and rollback message tests

README.md                           operator-visible logging contract and journal examples
docs/superpowers/specs/2026-09-01-sdhm-go-1.27-rewrite-design.md
                                     amended daemon/hosts logging contract
docs/superpowers/reviews/2026-09-02-sdhm-go-1.27-rewrite-review.md
                                     logging-review resolution link
```

---

## Execution Preflight

After the user approves this spec and plan, commit the planning artifacts before changing production code:

```bash
git add docs/superpowers/specs/2026-09-02-sdhm-operational-logging-design.md docs/superpowers/plans/2026-09-02-sdhm-operational-logging.md
git commit -m "docs(logging): plan operational transitions"
```

Create an isolated implementation worktree at execution time. Re-run `make check TEST_FLAGS='-count=1'` there before Task 1 and stop if the baseline is not green.

---

### Task 1: Preserve native journal priorities at the process boundary

**Files:**
- Create: `cmd/sdhm/logging.go`
- Create: `cmd/sdhm/logging_test.go`
- Modify: `cmd/sdhm/main.go:15-23`
- Modify: `cmd/sdhm/run.go:36-47,114-129`

**Interfaces:**
- Consumes: `slog.Handler`, `slog.TextHandler`, `JOURNAL_STREAM`, stdout, and stderr.
- Produces: `newProcessLogger(io.Writer, bool) *slog.Logger`, private `newProcessLoggerWithOptions(io.Writer, bool, *slog.HandlerOptions) *slog.Logger`, `journalStreamPresent() bool`, and `writeProcessError(io.Writer, bool, error)`.

- [ ] **Step 1: Write failing process-logger tests**

Add table tests in `cmd/sdhm/logging_test.go` that invoke real `slog.Logger` methods:

```go
func TestProcessLoggerMapsNativeJournalPriorities(t *testing.T) {
    tests := []struct {
        name       string
        log        func(*slog.Logger)
        wantPrefix string
    }{
        {"debug", func(l *slog.Logger) { l.Debug("debug") }, "<7>"},
        {"info", func(l *slog.Logger) { l.Info("info") }, "<6>"},
        {"warn", func(l *slog.Logger) { l.Warn("warn") }, "<4>"},
        {"error", func(l *slog.Logger) { l.Error("error") }, "<3>"},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            var output bytes.Buffer
            logger := newProcessLoggerWithOptions(&output, true, &slog.HandlerOptions{Level: slog.LevelDebug})
            tt.log(logger.With("component", "daemon"))
            if got := output.String(); !strings.HasPrefix(got, tt.wantPrefix) || !strings.Contains(got, "component=daemon") {
                t.Fatalf("journal output = %q, want prefix %q and retained attrs", got, tt.wantPrefix)
            }
        })
    }
}
```

Also require:

- journal-disabled output has no `<N>` prefix and otherwise matches standard text output;
- `WithAttrs` and `WithGroup` retain their fields;
- 100 concurrent mixed-level records produce 100 complete, non-interleaved lines with the correct prefix;
- `writeProcessError` emits `<3>sentinel\n` only in journal mode and `sentinel\n` interactively;
- `journalStreamPresent` distinguishes an absent versus non-empty `JOURNAL_STREAM` using `t.Setenv`.

- [ ] **Step 2: Run the tests and observe RED**

Run:

```bash
go test ./cmd/sdhm -run 'Test(ProcessLogger|WriteProcessError|JournalStreamPresent)' -count=1
```

Expected: compile failure because the logger helpers and handler do not exist.

- [ ] **Step 3: Implement the minimal priority-aware handler**

Create `cmd/sdhm/logging.go` with a wrapper that delegates formatting to one standard text handler per journal priority:

```go
type priorityTextHandler struct {
    debug slog.Handler
    info  slog.Handler
    warn  slog.Handler
    err   slog.Handler
}

type priorityWriter struct {
    out    io.Writer
    mu     *sync.Mutex
    prefix string
}

func (w *priorityWriter) Write(data []byte) (int, error) {
    w.mu.Lock()
    defer w.mu.Unlock()
    if _, err := io.WriteString(w.out, w.prefix); err != nil {
        return 0, err
    }
    return w.out.Write(data)
}

func (h *priorityTextHandler) handler(level slog.Level) slog.Handler {
    switch {
    case level >= slog.LevelError:
        return h.err
    case level >= slog.LevelWarn:
        return h.warn
    case level >= slog.LevelInfo:
        return h.info
    default:
        return h.debug
    }
}

func (h *priorityTextHandler) Enabled(ctx context.Context, level slog.Level) bool {
    return h.handler(level).Enabled(ctx, level)
}

func (h *priorityTextHandler) Handle(ctx context.Context, record slog.Record) error {
    return h.handler(record.Level).Handle(ctx, record)
}

func (h *priorityTextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
    return &priorityTextHandler{
        debug: h.debug.WithAttrs(attrs),
        info:  h.info.WithAttrs(attrs),
        warn:  h.warn.WithAttrs(attrs),
        err:   h.err.WithAttrs(attrs),
    }
}

func (h *priorityTextHandler) WithGroup(name string) slog.Handler {
    return &priorityTextHandler{
        debug: h.debug.WithGroup(name),
        info:  h.info.WithGroup(name),
        warn:  h.warn.WithGroup(name),
        err:   h.err.WithGroup(name),
    }
}
```

`newProcessLoggerWithOptions` creates the four handlers over writers sharing one mutex and prefixes `<7>`, `<6>`, `<4>`, and `<3>`. `newProcessLogger` passes default options and uses an ordinary `slog.TextHandler` when journal mode is false.

Change both logger construction sites in `run.go` to call:

```go
logger := newProcessLogger(os.Stdout, journalStreamPresent())
```

Change the terminal error boundary in `main.go` to `writeProcessError(os.Stderr, journalStreamPresent(), err)`. This function is deliberately best-effort because the process is already terminating.

- [ ] **Step 4: Verify GREEN and race behavior**

Run:

```bash
go test ./cmd/sdhm -run 'Test(ProcessLogger|WriteProcessError|JournalStreamPresent)' -count=20
go test -race ./cmd/sdhm -count=1
```

Expected: all tests pass; concurrent records remain intact.

- [ ] **Step 5: Review and commit Task 1**

Run `git diff --check` and inspect that non-journal output remains unchanged.

```bash
git add cmd/sdhm/logging.go cmd/sdhm/logging_test.go cmd/sdhm/main.go cmd/sdhm/run.go
git commit -m "feat(logging): preserve journal priorities"
```

---

### Task 2: Report automatic hosts recovery and rollback safety

**Files:**
- Modify: `daemon/types.go:39-43`
- Modify: `daemon/daemon.go:123-145`
- Create: `daemon/logging_test.go`
- Modify: `daemon/daemon_test.go` fake `HostStore` and preparation tests
- Modify: `daemon/loop_test.go` fake `HostStore`
- Modify: `daemon/types_test.go` interface fake
- Modify: `cmd/sdhm/run_test.go` interface fakes
- Modify: `hosts/store.go:56-89,179-205`
- Modify: `hosts/store_test.go` preparation and late-failure tests

**Interfaces:**
- Consumes: the existing deep `HostStore` boundary and daemon logger.
- Produces: `daemon.PrepareResult`, revised `HostStore.Prepare(context.Context) (PrepareResult, error)`, one recovery warning, and explicit rollback outcome text.

- [ ] **Step 1: Write failing hosts outcome tests**

Extend the `Store.Prepare` state table so every case asserts a `daemon.PrepareResult`. The corrupt-target/valid-backup case must require:

```go
if !result.RecoveredFromBackup {
    t.Fatal("Prepare() did not report validated backup recovery")
}
```

Every valid, no-marker, missing-target, and invalid-backup case must receive the zero result. Add a late target-failure test that injects a post-rename readback failure, permits rollback to succeed, and requires the returned error to contain `target restored from retained snapshot`. The existing rollback-failure case must still expose both failures through `errors.Is` and contain `restore target after replacement failure`.

- [ ] **Step 2: Write a failing daemon recovery-log test**

Create a synchronized test handler in `daemon/logging_test.go` that clones `slog.Record` values under a mutex and supports `WithAttrs`/`WithGroup`. Use it in a daemon test whose fake store returns:

```go
PrepareResult{RecoveredFromBackup: true}, nil
```

Cancel during the initial snapshot after preparation, then require exactly one WARN record named `hosts file recovered from validated backup`. A normal preparation result must produce no such record.

- [ ] **Step 3: Run RED**

Run:

```bash
go test ./hosts ./daemon ./cmd/sdhm -run 'Test(StorePrepare|StoreApplyLate|RunLogsValidatedBackupRecovery)' -count=1
```

Expected: compile failures from the old `Prepare(context.Context) error` signature and missing recovery record; rollback text assertion also fails.

- [ ] **Step 4: Implement the typed preparation result**

Add to `daemon/types.go`:

```go
type PrepareResult struct {
    RecoveredFromBackup bool
}

type HostStore interface {
    Prepare(context.Context) (PrepareResult, error)
    Apply(context.Context, []Endpoint) error
}
```

Return `PrepareResult{RecoveredFromBackup: true}` only after corrupt markers were restored successfully. Return the zero result for valid/no-marker success and every error path. Update every compile fake to the exact signature.

In `Daemon.Run`, log the warning after successful preparation and before the recovery concern is cleared:

```go
prepareResult, err := d.store.Prepare(ctx)
// existing error handling
if prepareResult.RecoveredFromBackup {
    d.logger.Warn("hosts file recovered from validated backup")
}
```

In `applyReplacement`, distinguish successful and failed rollback:

```go
rollbackErr := s.rollbackTarget(ctx, s.hostsPath, current, targetMetadata)
if rollbackErr == nil {
    return fmt.Errorf("%w; target restored from retained snapshot", primaryErr)
}
return errors.Join(
    primaryErr,
    fmt.Errorf("restore target after replacement failure: %w", rollbackErr),
)
```

- [ ] **Step 5: Verify GREEN and all interface consumers**

Run:

```bash
go test ./hosts ./daemon ./cmd/sdhm -count=1
go test -race ./hosts ./daemon ./cmd/sdhm -count=1
```

Expected: outcomes, error identity, recovery warning, and all wiring contracts pass.

- [ ] **Step 6: Review and commit Task 2**

```bash
git add daemon/types.go daemon/types_test.go daemon/daemon.go daemon/daemon_test.go daemon/loop_test.go daemon/logging_test.go cmd/sdhm/run_test.go hosts/store.go hosts/store_test.go
git commit -m "feat(hosts): report recovery outcomes"
```

---

### Task 3: Log readiness, reconciliation recovery, and clean stop transitions

**Files:**
- Modify: `cmd/sdhm/run.go:36-47,76-112`
- Modify: `cmd/sdhm/run_test.go`
- Modify: `daemon/daemon.go:136-151`
- Modify: `daemon/daemon_test.go` startup/readiness tests
- Modify: `daemon/loop.go:14-30,101-120`
- Modify: `daemon/loop_test.go` initial failure, retry, and recovery tests
- Reuse: `daemon/logging_test.go`

**Interfaces:**
- Consumes: daemon logger, existing retry state, `initialReconcileErr`.
- Produces: bounded `SDHM ready`, changed-error reconciliation warnings, one recovery record, richer startup fields, and `SDHM stopped`.

- [ ] **Step 1: Write failing readiness and startup-field tests**

Use the daemon recording handler to require:

- successful initial reconciliation emits exactly one INFO `SDHM ready` after `Apply` returns and readiness becomes true;
- initial failure emits WARN `reconciliation failed` with `phase=initial`, `err`, and `retry_in=1s`, but no ready record;
- cancellation during initial reconciliation emits neither ready nor failure;
- startup record includes `interval`, `health_addr`, existing network/default fields, and version.

Factor startup logging into a private helper so `run_test.go` can test exact attributes without invoking production Docker:

```go
func logStartup(logger *slog.Logger, cfg command.Config) {
    logger.Info("starting SDHM",
        "version", version,
        "networks", cfg.Networks,
        "default_network", cfg.DefaultNetwork,
        "interval", cfg.PeriodicInterval,
        "health_addr", net.JoinHostPort(cfg.HealthAddr, strconv.Itoa(cfg.HealthPort)),
    )
}
```

- [ ] **Step 2: Write failing reconciliation transition tests**

Change the loop harness logger to the recording handler and add deterministic cases:

1. First failed attempt logs one warning with `retry_in`.
2. An identical retry failure creates health history but no duplicate warning.
3. A different error creates a second warning.
4. The next complete success logs one INFO `reconciliation recovered`.
5. Later healthy attempts do not repeat the recovery record.
6. Caller cancellation while an attempt is in flight produces no failure/recovery record.

Pass the exact initial error into `loop`, not only a boolean, so the loop begins with the already-logged failure string and can emit recovery without duplicating the initial warning:

```go
func (d *Daemon) loop(ctx context.Context, initialReconcileErr error) (error, bool)
```

- [ ] **Step 3: Write a failing clean-stop test**

In `cmd/sdhm/run_test.go`, make the fake runner return nil and require one INFO `SDHM stopped`. When the fake returns a sentinel error, require no stopped record and preserve the exact returned error for the process boundary.

- [ ] **Step 4: Run RED**

```bash
go test ./daemon ./cmd/sdhm -run 'Test(RunLogs|LoopLogs|LogStartup|RunDaemonWithLogsCleanStop)' -count=1
```

Expected: missing records, duplicate warnings, and the old boolean loop signature fail the tests.

- [ ] **Step 5: Implement transition-gated reconciliation logging**

Use one local string in `loop`:

```go
lastReconcileFailure := ""
if initialReconcileErr != nil {
    lastReconcileFailure = initialReconcileErr.Error()
}
```

On failure, warn only when `reconcileErr.Error()` differs, then update the string. Include the delay used to reset the retry timer. On success, emit `reconciliation recovered` only when the string was non-empty, then clear it. Keep health failure recording on every observed failure.

After a successful initial reconciliation and `CompleteInitialization`, log `SDHM ready`. Replace the old initial-only message with the contract's stable `reconciliation failed` message and add `phase=initial` plus `retry_in=d.timing.retryInitialDelay`.

In `runDaemonWith`, log `SDHM stopped` only after `runner.Run(ctx)` returns nil. Return non-nil errors without logging them.

- [ ] **Step 6: Verify GREEN repeatedly and under race detection**

```bash
go test ./daemon ./cmd/sdhm -run 'Test(RunLogs|LoopLogs|LogStartup|RunDaemonWithLogsCleanStop)' -count=50
go test -race ./daemon ./cmd/sdhm -count=1
```

- [ ] **Step 7: Review and commit Task 3**

```bash
git add cmd/sdhm/run.go cmd/sdhm/run_test.go daemon/daemon.go daemon/daemon_test.go daemon/loop.go daemon/loop_test.go daemon/logging_test.go
git commit -m "feat(daemon): log lifecycle transitions"
```

---

### Task 4: Log Docker event-stream failure and recovery transitions

**Files:**
- Modify: `daemon/loop.go:32-85,132-185`
- Modify: `daemon/loop_test.go` stream failure/recovery/cancellation cases
- Reuse: `daemon/logging_test.go`

**Interfaces:**
- Consumes: existing stream error/reconnect/stability state and daemon logger.
- Produces: one changed-error warning per unavailable state and one observed-success recovery record.

- [ ] **Step 1: Write failing stream transition tests**

Using `testing/synctest`, require:

- first exact stream error logs WARN `Docker event stream unavailable` with the error and `retry_in=1s`;
- a reconnect that fails with the same exact error does not duplicate the warning;
- a changed exact error logs a new warning with the current backoff;
- a valid event logs one INFO `Docker event stream recovered` with `evidence=event`;
- thirty stable seconds logs one recovery with `evidence=stable`;
- later events/stability do not repeat recovery;
- generic channel closure uses the existing `Docker event stream closed` reason;
- caller cancellation and health-server termination do not log a false stream loss.

- [ ] **Step 2: Run RED**

```bash
go test ./daemon -run '^TestLoopLogsEventStream' -count=1
```

Expected: no matching records exist.

- [ ] **Step 3: Implement local stream logging state**

Add `lastStreamFailure := ""` beside reconnect state. In `disconnectStream`, first retain the existing stream cancellation, channel clearing, and stability-timer cleanup; after that cleanup, return without health/log/reconnect mutation if `ctx.Err() != nil`. Build the exact message, log only when it differs from `lastStreamFailure`, include the retry delay about to be scheduled, then store it and retain existing tracker/backoff behavior.

Replace direct recovery calls with:

```go
recoverStream := func(evidence string) {
    reconnectDelay = d.timing.retryInitialDelay
    stabilityTimer = stopLoopTimer(stabilityTimer)
    d.tracker.Recover(health.ConcernDockerEvents)
    if lastStreamFailure != "" {
        d.logger.Info("Docker event stream recovered", "evidence", evidence)
        lastStreamFailure = ""
    }
}
```

Call it with `event` from `observeEvent` and `stable` from the stability branch. Do not log initial connection or reconnect attempts independently.

- [ ] **Step 4: Verify GREEN and existing scheduling behavior**

```bash
go test ./daemon -run '^TestLoopLogsEventStream' -count=50
go test -race ./daemon -count=1
```

Then run all existing loop tests to prove no timer, health, or backoff behavior changed:

```bash
go test ./daemon -run '^TestLoop' -count=20
```

- [ ] **Step 5: Review and commit Task 4**

```bash
git add daemon/loop.go daemon/loop_test.go daemon/logging_test.go
git commit -m "feat(daemon): log event stream transitions"
```

---

### Task 5: Document, qualify, and review the final logging contract

**Files:**
- Modify: `README.md` logging/service sections
- Modify: `docs/superpowers/specs/2026-09-01-sdhm-go-1.27-rewrite-design.md`
- Modify: `docs/superpowers/specs/2026-09-02-sdhm-operational-logging-design.md`
- Modify: `docs/superpowers/plans/2026-09-02-sdhm-operational-logging.md`
- Modify: `docs/superpowers/reviews/2026-09-02-sdhm-go-1.27-rewrite-review.md`
- Retain evidence under: `.superpowers/sdd/2026-09-01-sdhm-go-1.27-rewrite/`

**Interfaces:**
- Consumes: completed logging behavior and Saltbox VM lifecycle.
- Produces: operator contract, exact committed-head local/VM evidence, and final review verdict.

**Phase A status (2026-09-02):** Steps 1--3 are recorded by the documentation-contract commit. They establish the local operator contract only. Step 4 remains controller-owned; Steps 5--10 remain pending, including all live `PRIORITY` and Saltbox VM assertions.

- [x] **Step 1: Update operator and design documentation**

Document the exact transition table from the logging spec, including:

- `journalctl -u saltbox_managed_docker_update_hosts.service` for the complete history;
- native priority filtering with `journalctl -p warning -u ...`;
- health as current state versus logs as transition history;
- intentionally suppressed per-event/update noise;
- successful automatic recovery warning and rollback wording;
- readiness and clean-stop meanings.

Update the original rewrite design's logging paragraph so it points to the operational logging spec. Set the logging design status to `Implemented; acceptance pending`. Add a short resolution entry to the written-spec review rather than rewriting its historical findings.

- [x] **Step 2: Run the complete local gate**

```bash
make check TEST_FLAGS='-count=1'
make test-coverage
git diff --check
git status --short
```

Expected: all package/race/vet/format/tidy/build gates pass and coverage is reported without a percentage gate.

- [x] **Step 3: Commit the documentation contract**

```bash
git add README.md docs/superpowers/specs/2026-09-01-sdhm-go-1.27-rewrite-design.md docs/superpowers/reviews/2026-09-02-sdhm-go-1.27-rewrite-review.md docs/superpowers/specs/2026-09-02-sdhm-operational-logging-design.md docs/superpowers/plans/2026-09-02-sdhm-operational-logging.md
git commit -m "docs(logging): define operational transitions"
```

- [ ] **Step 4: Request independent code and spec review**

Review from the pre-Task-1 base through the documentation commit along separate Standards and Spec axes. Require explicit closure of:

- event stream journal visibility;
- reconciliation recovery closure;
- automatic marker repair and rollback outcome;
- readiness/clean-stop lifecycle records;
- native journal priorities;
- bounded volume and cancellation suppression;
- no Saltbox template or CLI compatibility change.

Fix every Critical or Important finding test-first and repeat review until both axes approve.

- [ ] **Step 5: Build the exact acceptance artifact**

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 make build
sha256sum build/sdhm
file build/sdhm
./build/sdhm --version
```

Record the commit, SHA-256, static ELF result, and exact three-token version.

- [ ] **Step 6: Inspect and prepare the VM lifecycle**

As root coordinator, read the installed Saltbox VM skill, call `inspect` with `target: system`, and stop on unhealthy checks or unexpected ownership. Prepare one Ubuntu 26.04 `minimal` ordinary slot and retain its exact `guest_instance_id` for every guest and finish call. Do not use core.

- [ ] **Step 7: Run logging-specific VM acceptance**

Install Docker on the minimal guest, transfer the exact binary, and use only temporary hosts/backup files and isolated synthetic networks. Run the candidate under transient systemd units and query journal entries with verbose fields.

Before asserting priorities, require `systemctl show <unit> --property=SyslogLevelPrefix` to report `yes`; do not add a Saltbox template dependency on an explicitly configured value that historical units already receive by default.

Require all of the following:

1. `starting SDHM` and `SDHM ready` have `PRIORITY=6`.
2. With the unit active and topology unchanged, `SIGSTOP` dockerd until the fixed snapshot timeout produces `reconciliation failed` with `PRIORITY=4`; `SIGCONT` produces one `reconciliation recovered` with `PRIORITY=6` and health returns 200.
3. A valid backup plus reversed target markers produces exactly one `hosts file recovered from validated backup` with `PRIORITY=4`, then ready/HTTP 200.
4. A logging-only standalone unit without Docker binding observes Docker event-stream loss as one priority-4 warning, then one priority-6 recovery after Docker returns; no individual event records appear.
5. A separate invalid invocation produces one concise terminal record with `PRIORITY=3` and no usage dump.
6. SIGTERM produces `SDHM stopped`, exits zero within five seconds, and no later hosts mutation occurs.

Install PID-specific idempotent `SIGCONT` cleanup before pausing dockerd. Retain the harness and raw journal evidence with hashes.

- [ ] **Step 8: Clean and finish the VM**

Remove transient units, containers, networks, temporary files, and payload. Verify Docker active and the assigned slot healthy on the same guest fence. Call `finish` with `passed`, require final state absent, then inspect the system and verify `test-a`, `test-b`, and `proxmox-runner` absent. On failure, finish with `failed` and preserve diagnostic state while needed.

- [ ] **Step 9: Record and commit qualification evidence**

Update the logging design status to `Implemented and qualified 2026-09-02`. Append the exact reviewed commit, binary SHA-256, journal priority/message assertions, helper operation IDs, evidence hashes, and final absent slot state to the written review and ignored SDD ledger.

```bash
git add docs/superpowers/specs/2026-09-02-sdhm-operational-logging-design.md docs/superpowers/reviews/2026-09-02-sdhm-go-1.27-rewrite-review.md
git commit -m "test(logging): qualify journal transitions"
```

- [ ] **Step 10: Run final committed-head verification**

```bash
make check TEST_FLAGS='-count=1'
make test-coverage
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 make build
sha256sum build/sdhm
git diff --check
git status --short --branch
```

Inspect every new commit subject for Conventional Commit compliance. Record final coverage, artifact hash, review verdicts, VM operation IDs, evidence hashes, and slot state in the ignored SDD ledger.

Suggested final state: local `main` clean, no VM retained, and no push or release tag unless separately authorized.
