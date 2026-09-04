# SDHM Technical-Debt Remediation Design

Date: 2026-09-04
Status: Approved for implementation

## Purpose

Harden the unreleased Go 1.27 rewrite before producing a commit-addressed
candidate. The work closes the current dependency and CI gaps, prevents
competing SDHM processes from writing the same hosts file, makes the persistent
backup a current validated recovery image, bounds Docker inspection
concurrency, and reduces event-loop complexity without changing its behavior.

Publication is outside this work. No push, tag, GitHub release, or other release
mutation is authorized. The README continues to describe `main`.

## Dependency and CI Contract

- `make check` runs format, module-tidiness, staticcheck, govulncheck,
  race-enabled tests, vet, and build gates.
- Staticcheck and govulncheck are pinned Go tool dependencies.
- Pull requests run the same checks and Linux build matrix as pushes.
- Renovate proposes dependency and GitHub Actions updates without automerge;
  major releases stay separate.

## Hosts Ownership Contract

One SDHM process may own a hosts path at a time. Both daemon execution and
`sdhm regenerate` acquire a nonblocking exclusive lock before constructing
Docker or hosts resources. The lock path is the absolute hosts path plus
`.sdhm.lock`. The mode-`0600` regular lock file persists across runs and is
never unlinked; closing its descriptor releases ownership.

The lock coordinates SDHM processes. A final no-follow content and metadata
comparison immediately before rename detects most changes by non-cooperating
writers. A changed destination aborts without rename and is retried through the
existing reconciliation policy. Advisory locking cannot eliminate the final
kernel scheduling window for writers that ignore the lock.

## Backup and Replacement Contract

After successful `Prepare`, `Apply`, or `Regenerate`, the backup bytes equal the
fully validated target bytes. Existing backup metadata is retained; a missing
backup uses the existing target/default metadata policy. Equal current and
backup content causes no write.

During replacement, the validated target preimage remains in memory for bounded
rollback. The old target may temporarily be persisted as crash-recovery state,
but a successful target commit is followed by a current-mirror refresh. A
post-commit backup failure does not roll back the correct target; it returns an
error and the next preparation or reconciliation repairs the mirror.

Temporary files remain adjacent to their destination and retain the existing
mode restriction, ownership, sync, close, rename, parent-sync, no-follow
readback, cleanup, cancellation, and joined-error guarantees. A cross-filesystem
copy fallback remains forbidden because these are replacement semantics;
`fileflow` conflict-renaming semantics do not match this contract.

## Docker and Event-Loop Contract

Container inspections run with a maximum concurrency of eight under the
existing ten-second snapshot context. Results are stored by Docker list index
and interpreted afterward in list order, preserving deterministic first-error,
not-found omission, all-or-nothing snapshot, and output-order behavior.

A private `loopState` owns all timers, stream channels, stream cancellation,
pending work, retry delays, and failure-transition strings. It is used only by
the existing single coordinator goroutine. Public interfaces, timings, health
state, logs, retry priority, cancellation, and shutdown behavior do not change.

## Saltbox Compatibility

Saltbox adopts a separate exact `docker_dns_version` pin, initially `v1.0.4`,
while SDC continues to follow its latest release. Saltbox derives the SDHM tag
API URL from that value and verifies the returned release tag. Renovate may
propose exact SDHM pin changes, but every change remains a reviewed Saltbox
commit; breaking SDHM releases use semantic-version major changes and are never
deployed merely because GitHub marks them latest.

## Acceptance Boundary

Local acceptance requires the complete SDHM gate, shuffled repetitions,
coverage, module verification, and clean diffs. Standalone behavior is qualified
on an Ubuntu 26.04 toolbox guest with isolated Docker networks and temporary
hosts files. The Saltbox exact-pin path is qualified separately on an Ubuntu
26.04 core guest. The handoff is an exact source commit, five Linux artifacts,
their SHA-256 manifest, and an evidence record; it is not a release.
