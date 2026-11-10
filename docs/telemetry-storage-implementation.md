# Telemetry storage replacement: implementation checkpoints

The accepted design is [SQL hot data, summaries and JSONL-zstd archives](telemetry-storage-design.md).
This branch follows the requested order. Each checkpoint must pass component CI
and the real HTTPS browser/server/installed-Agent E2E before the next production
checkpoint starts. The final checkpoint adds restore, migration, mixed-load and
resource verification. No intermediate checkpoint is a release candidate.

## 1. Retire legacy compression and active resource benchmarks

- [x] Remove the unused CPU/random-write benchmark package.
- [x] Remove automatic block encoding, axis/digest conversion and old retention
  jobs from the application Store path. Existing blocks remain readable.
- [x] Keep new samples in ordinary SQL rows during the transition. Explicit
  administrative deletion remains available.
- [x] Add SQLite mutation-denial regression tests, real application configuration
  coverage and a real E2E database assertion.
- [x] Pass the complete local Linux `pkg/metric` and `database/metricstore` race
  suites, plus three runs of the retirement regressions (2026-11-10).
- [x] Pass all 24 component PR checks on checkpoint `e0451aa0`.
- [x] Pass real HTTPS / built Web / installed Agent E2E on `e0451aa0`:
  [run 42785003349](https://github.com/r11234567/komari/actions/runs/42785003349).

The first real run failed because the browser typed while xterm was mounted but
Agent attachment still disabled input. Trace and service logs showed no input
request and a subsequent real shell prompt. The repaired test waits for that
prompt, then exercises unchanged remote-output/export/clipboard assertions.
No terminal assertion was skipped. Local race suites, three focused repetitions,
a negative control rejecting the old mutating startup path, and byte-identical
legacy block/axis hashes on a copy containing 11,583,271 logical raw points
provide additional evidence. These checks do not establish new-engine speed.

The retained library implementation exists only to exercise old formats and
prepare the explicit migration reader. It is not selected by the application.
Physical removal of obsolete writers follows the replacement integration.
Until archive ownership is available, automatic expiration is paused rather
than deleting unarchived or unsummarized source data. This temporary state can
grow SQL storage, so it must not be shipped as the completed engine.

## 2. Implement hot SQL, incremental summaries and immutable cold archives

Core implementation is in progress, independently callable through
`Store.OpenTelemetry`; application ingest/query/traffic routing is intentionally
not changed until checkpoint 3. New-schema acceptance commits receipts, immutable
rows, fixed segment membership and a traffic outbox together. SQLite requires
FULL durability on replacement connections. Summary deltas and their durable
cursor commit together; cold files use bounded JSONL/zstd, exclusive attempts,
cross-process publication fencing, checksums, fsync and resumable row deletion.
No component-only result marks this checkpoint complete.

Local evidence currently includes the full storage race suites and repeated
core contracts. The dedicated workflow also runs against actual PostgreSQL,
MySQL and MariaDB services; all remote results remain pending for this core.

- [ ] Durable batch identity, receipts and traffic outbox; compatible Agent
  restart/epoch rules and integer counter handling.
- [ ] New indexed hot tables, bounded summary processing and durable progress.
- [ ] Mergeable weighted statistics, variance, percentiles and Ping semantics.
- [ ] Streaming JSONL-zstd, fixed segment membership, checksums and crash-safe
  publication/deletion state machine; finite memory and cancellation.
- [ ] Separate chart precision and raw archive retention; explicit raw API errors.
- [ ] SQLite / supported external-driver integration, fault injection and race CI.
- [ ] Real E2E on this checkpoint revision.

## 3. Adapt and connect the surrounding features

- [ ] Independent traffic state, hourly/daily/cycle ledgers and calibration.
- [ ] Authorized bounded chart routing, actual resolution and missing-data status.
- [ ] Streaming hot/cold CSV, archive ownership, deletion and file leases.
- [ ] Consistent full backup/restore, resumable legacy migration and data rollback.
- [ ] Settings and capacity/backlog diagnostics, protocol compatibility.
- [ ] Remove obsolete runtime writers after the migration reader is isolated.
- [ ] Component CI, authenticated real E2E, restore and migration verification.

## 4. Replace the maintenance scheduler

- [ ] Due-work queues, short commits, persistent per-task progress and fairness.
- [ ] Failure-completion-based minute backoff with jitter, bounded worker count.
- [ ] No periodic full COUNT, recompression, scan or VACUUM on idle data.
- [ ] Independent checkpoint/space reclamation, backlog and disk backpressure.
- [ ] Scheduler race/fault CI and real E2E on this checkpoint revision.

## 5. Final integrated verification

- [ ] All CI checks and real browser/server/Agent scenarios pass on final HEAD.
- [ ] Incident-sized consistent data, mixed ingestion/query/archive/export load,
  migration interrupt/resume, backup restore and rollback.
- [ ] Fixed 1 vCPU / 1 GiB benchmark manifest; record CPU/RSS, ingest and chart
  latency, cancellation and sustained backlog reduction against the design.
- [ ] Complete retention/archive-cycle soak; permission/security regressions.
- [ ] Evidence links, limitations and unchecked deployment-only items recorded.

Prior PR #75 verification is baseline evidence only. It does not validate this
new storage design or any unchecked item above. Do not merge, tag or deploy the
branch as part of these checkpoints.
