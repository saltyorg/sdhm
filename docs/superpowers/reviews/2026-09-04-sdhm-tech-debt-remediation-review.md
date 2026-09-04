# SDHM Technical-Debt Remediation Review

Date: 2026-09-04
Status: Implementation complete; local, toolbox, and Saltbox minimal acceptance passed

## Scope and Commits

- SDHM baseline: `30a6c873e9c8c5826cfb5ce143bf0116302f961a`
- Qualified SDHM source: `8bd1a5b74f7772e85fe1e144b634c3c2ce11ed3f`
- Saltbox baseline after rebase: `1689d856f2c58a89a56281173d04e4ef57a6a95e`
- Saltbox candidate: `4dbb2ad3544429c13e2621db139fb5fb023498ca`
- Design: `docs/superpowers/specs/2026-09-04-sdhm-tech-debt-remediation-design.md`

No branch was pushed, no tag or GitHub release was created, and no Renovate
configuration was activated remotely.

## Implemented Results

- Updated Moby and OpenTelemetry dependencies; pinned staticcheck and
  govulncheck as Go tools and added both to `make check`.
- Added pull-request CI, updated GitHub Actions, and added validated Renovate
  configuration without automerge.
- Added a persistent mode-`0600`, no-follow, nonblocking process-lifetime hosts
  lock for daemon and regeneration commands.
- Added content/existence/mode/UID/GID comparison immediately before every
  production rename derived from prior path state.
- Made the persistent backup a byte-identical current recovery mirror after
  every successful preparation, reconciliation, and regeneration; equal
  preimage backups are not rewritten.
- Split hosts staging from commit/durability validation, preserving adjacent
  temporary files, atomic replacement, fsync, readback, cleanup, and bounded
  guarded rollback.
- Bounded Docker inspections to eight concurrent calls while retaining Docker
  list-order error selection and deterministic output.
- Extracted private single-owner event-loop state without changing timers,
  retry priority, health, logs, cancellation, or shutdown behavior.
- Saltbox alone now pins `docker_dns_version` to `v1.0.4`, validates the API tag,
  and exposes the pin to Renovate; SDC remains on `releases/latest`.

## Local Evidence

- `make check TEST_FLAGS='-count=1'`: PASS, including format, tidy, staticcheck,
  govulncheck, race tests, vet, and build.
- `make test TEST_FLAGS='-shuffle=on -count=20'`: PASS.
- Coverage: 90.0% of statements.
- `go mod verify`: PASS.
- Advisory complexity check: `Daemon.loop` and hosts replacement are below 20;
  only `Daemon.Run` remains above at 22.
- Saltbox linter: 92 role defaults, 334 task files, and one inventory passed.
- Targeted Docker-role ansible-lint: zero failures and zero warnings.
- SDHM and Saltbox Renovate configurations passed Renovate 44.61.6 strict
  validation. The Saltbox dry run extracted `saltyorg/sdhm` `v1.0.4`.
- Live Saltbox preflight rejected requested `v1.0.4` with returned `v1.0.3`,
  accepted returned `v1.0.4`, and retained release-asset SHA-256 validation.
- Independent SDHM and Saltbox re-reviews returned zero Critical, Important,
  or Minor findings after fixes.

## Candidate Artifacts

Version: `0.0.0-dev+g8bd1a5b74f77`; Go: `go1.27.0`; `CGO_ENABLED=0`.

| Artifact | SHA-256 |
|---|---|
| `sdhm_linux_amd64` | `c660afbd6d4fe4e802bdd2d90db918b1b4d0044121de94f2fddccbdea4635976` |
| `sdhm_linux_arm64` | `33293589b516000bfa294afcfff966097690e857e681303978f7f40e73d8825b` |
| `sdhm_linux_armv5` | `c0f2387a67807503974905bced1dff5a86bda1170042725e7267850e0323cb8c` |
| `sdhm_linux_armv6` | `eac36b5ffb40aaf407ca22cc61841de0270e9e97f89f3686ec9a73227977d648` |
| `sdhm_linux_armv7` | `1ad3aae0fb7b2b74d7060fed6d666219bf2e18a7c189fa974fb260bd7fa73384` |

## Toolbox Acceptance

- Slot/profile: `test-a`, Ubuntu 26.04 `toolbox`.
- Guest instance: `9f604ca9bbea68790c6c889c7d72d9baa53cf4faccf943d4a35003715d6b9eb7`.
- Docker Engine: 29.3.1.
- Exact guest artifact hash matched the local amd64 SHA-256.
- Seventeen isolated Alpine containers reconciled with all aliases present.
- A stale valid backup was healed during an unchanged periodic reconciliation.
- A second daemon and `regenerate` both failed immediately on the same lock.
- A seventeenth container converged through an event and left target/backup
  byte-identical.
- Corrupt target markers restored the latest validated mirror, including the
  unmanaged suffix.
- Docker outage created snapshot and event-stream health concerns; both
  recovered through observed success and health returned HTTP 200.
- Both SIGTERM stops exited cleanly, the persistent lock remained mode `0600`,
  and final target/backup SHA-256 values matched.
- Downloaded evidence-tree digest: `d633b088f35c012468a7dd9a5101b83602b9ade4ce5cd448ed058288a8fe0dcf`.
- Finish operation `0094c19d-9307-41b3-9da9-eb3c1903c286` destroyed the passed
  guest and returned `test-a` absent.

## Saltbox Minimal Acceptance

- Slot/profile: `test-a`, Ubuntu 26.04 `minimal`.
- Guest instance: `8f63d41f4376757a1b8adf52175d17727ddcf52a86bb6f7ae09a87992f70c34d`.
- Prepare operation: `b62411d6-be8b-4325-b233-2b706aa8f8a2`.
- The clean minimal image had no Saltbox checkout, Ansible, or Docker. Ansible
  2.20.1 was installed, and only the exact committed Docker defaults,
  GitHub-binary preflight/install tasks, and shared GitHub API request task were
  staged under `/tmp`.
- Preflight resolved SDHM exactly to `v1.0.4`, selected
  `sdhm_linux_amd64`, and required the release-provided
  `sha256:76d91545e701d0c5f936964214774f2d54d982638dc34ccfff4c5a067127d999`.
- The installed binary reported `sdhm version v1.0.4` and its local checksum
  matched the release digest.
- A second preflight reported no update required, and a second install skipped
  the download as unchanged.
- Deliberately returning `v1.0.3` metadata against the `v1.0.4` request failed
  closed with the exact tag-mismatch error and was captured by the acceptance
  playbook's rescue assertion.
- SDC remained on its floating path and resolved to the current `v1.0.6` without
  an expected-version constraint.
- The play completed with 104 successful tasks, one expected rescued mismatch,
  zero unhandled failures, and one initial download change.
- Finish operation `3a101f99-93ca-479d-a208-3d198bf31e2d` destroyed the passed
  guest and returned `test-a` absent.

Initial minimal prepare operation `0a774190-b668-4529-a5d3-b226d8f19a31`
was rejected before guest creation while the helper's background catalog was
reconciling. After catalog attempt 1834 reported ready and a fresh system
inspection was healthy, the successful minimal preparation above was submitted.
Earlier core-profile attempts were a profile-selection error and are not part of
the acceptance evidence.
