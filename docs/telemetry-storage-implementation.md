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

Core implementation is independently callable through
`Store.OpenTelemetry`; application ingest/query/traffic routing is intentionally
not changed until checkpoint 3. New-schema acceptance commits receipts, immutable
rows, fixed segment membership and a traffic outbox together. SQLite requires
FULL durability on replacement connections. Summary deltas and their durable
cursor commit together; cold files use bounded JSONL/zstd, exclusive attempts,
cross-process publication fencing, checksums, fsync and resumable row deletion.
No component-only result marks this checkpoint complete.

Local evidence includes the full storage race suites and repeated core
contracts. All 25 component PR checks passed on `802e296b`, including the
[actual PostgreSQL 17 / MySQL 8.4 / MariaDB 11.4 matrix](https://github.com/r11234567/komari/actions/runs/42791005961).
The matrix caught PostgreSQL's padding of an empty CHAR staging identity;
the corrected scanner and complete virtual Ping projections passed the rerun.
The [real HTTPS / built Web / installed Agent E2E](https://github.com/r11234567/komari/actions/runs/42791050067)
passed on `802e296b`, including the multi-process crash/restart archive test.
The final local full race rerun passed (`pkg/metric`: 68.569s;
`database/metricstore`: 5.505s). Application integration starts after this gate.

- [x] Core durable batch identity, receipts and integer traffic outbox.
  Agent identity mapping and the metadata consumer remain checkpoint 3 work.
- [x] New indexed hot tables, bounded summary processing and durable progress.
- [x] Mergeable weighted statistics, variance, percentiles and Ping semantics.
- [x] Streaming JSONL-zstd, fixed segment membership, checksums and crash-safe
  publication/deletion state machine; finite memory and cancellation.
- [x] Core precision retention and bounded hot reads with explicit raw errors.
  HTTP query routing and archive expiration remain checkpoint 3 work.
- [x] SQLite / supported external-driver integration, fault injection and race CI.
- [x] Real E2E on this checkpoint revision.

## 3. Adapt and connect the surrounding features

The checkpoint implementation now selects durable ingestion and scoped summary
queries at startup, drains the idempotent integer traffic outbox into independent
metadata ledgers, and routes explicit CSV through pinned hot/cold readers. New
retention controls replace the retired compression controls in Web `22511e9`.
Live SQL/archive identity changes are rejected; existing history requires the
explicit stopped-service `telemetry-migrate --confirm-offline` command. Old
tables remain unchanged. The offline reader uses deterministic keyset batches,
decodes each source block once per window, and refuses ambiguous overlaps.

Full SQLite backups freeze this panel's lifecycle and archive publication,
snapshot the drained metadata/SQL boundary and hash-copy immutable files. Restore
validates SQL integrity, the manifest boundary and every catalogued file before
replacing data. The supported deployment is one panel writer; sharing a database
between multiple running panels is not a coordinated full-backup mode. Portable
external-SQL backups and live database relocation are explicitly unavailable.

Local Linux race suites passed on 2026-11-10: `pkg/metric` (70.232s),
`database/metricstore`, `database/trafficledger`, `database/enrollment`,
`database/dbcore`, `web/connect`, `web/rpc/jsonrpc`, `web/api/admin`, `web/backup`
and `cmd`. Compressed V4/V6, summary-only history, hot corrections and interrupted
migration passed. The full Web production build, 196 tests and lint passed
(zero lint errors, 39 existing warnings). A full regression also fixed expired
previous-token handoffs by comparing decoded instants instead of SQLite text
timestamps with different UTC offsets.

All 25 component checks, including actual external-driver services, passed on
`a9f6675e`. The [real HTTPS / built Web / installed Agent E2E](https://github.com/r11234567/komari/actions/runs/42806682373)
passed on that revision: real reports rotate to cold files, charts survive raw
deletion, CSV downloads, and the downloaded full backup restores and replays
into a separate legacy-readable database. Rollback preserves the metadata and
calibration ledger; known unavailable raw fails explicitly. Already-compressed
archive members use ZIP Store to avoid recompression. The final local relevant
race suites passed again (`pkg/metric`: 70.723s).

Legacy writer APIs are unreachable from the active application Store. Physical
library deletion remains deferred until the final full-cycle/migration checks;
compatibility codecs are still needed by offline migration and their tests.
Incident-sized migration and fixed-resource verification remain checkpoint 5.

- [x] Independent traffic state, hourly/daily/cycle ledgers and calibration.
- [x] Authorized bounded chart routing, actual resolution and missing-data status.
- [x] Bounded hot/cold CSV, archive ownership, deletion and file leases.
- [x] Consistent SQLite full backup/restore, resumable migration and data rollback.
- [x] Settings and initial backlog diagnostics, protocol compatibility.
- [x] Isolate legacy migration readers; prevent obsolete application writers.
- [x] Component CI, authenticated real E2E, restore and migration verification.
- [ ] Physically remove obsolete library writers after final compatibility checks.

## 4. Replace the maintenance scheduler

- [x] Due-work queues, short commits, persistent per-task progress and fairness.
- [x] Failure-completion-based minute backoff with jitter, bounded worker count.
- [x] No periodic full COUNT, recompression, scan or VACUUM on idle data.
- [x] Independent checkpoint/space reclamation, backlog and disk backpressure.
- [x] Scheduler race/fault CI and real E2E on this checkpoint revision.

Checkpoint `00245cb4` passed all 25 component checks, including the live external
SQL matrix, and [real E2E run 42811775927](https://github.com/r11234567/komari/actions/runs/42811775927).
The running fixture requires all 13 durable task rows and zero task failures;
it also validates real archives/raw deletion, restart, restore and rollback.
Local full race checks passed for metric, metricstore, trafficledger, Connect
and application startup (2026-11-10). The first browser run failed because a
zero-delay Chromium EditContext typing burst dropped characters before save;
typing is now paced and the visible buffer is asserted before the unchanged
UTF-8/zero-byte save and download assertions. No assertion was removed.

The former 30-second compaction cron is replaced by one due-work worker with
durable deadlines/cursors, independent tasks, completion-based minute backoff,
input wakeups and bounded work slices. Archive verification releases the ingest
gate before paced decompression and rechecks deletion eligibility afterwards.
Disk reserve backpressure protects newly acknowledged data and prioritizes
ingestion/accounting over export/backup; it is not a capacity guarantee.

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
