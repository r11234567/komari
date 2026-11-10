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

Legacy writer entry points are excluded from the normal build. Compatibility
decoders, schema preparation and explicit administrative deletion still support
offline migration and retained source data. The old reference engine exists
only behind `metric_legacy_fixture`; full reference regressions use
`scripts/ci/run-go-tests.sh`. Production telemetry CI also runs the complete
storage suite without that tag, and checks the selected build files. Release
and real E2E compile without the fixture tag.

- [x] Independent traffic state, hourly/daily/cycle ledgers and calibration.
- [x] Authorized bounded chart routing, actual resolution and missing-data status.
- [x] Bounded hot/cold CSV, archive ownership, deletion and file leases.
- [x] Consistent SQLite full backup/restore, resumable migration and data rollback.
- [x] Settings and initial backlog diagnostics, protocol compatibility.
- [x] Isolate legacy migration readers; prevent obsolete application writers.
- [x] Component CI, authenticated real E2E, restore and migration verification.
- [x] Exclude obsolete automatic migration and compaction entry points from
  normal builds. Historical behavior remains in the explicit
  `metric_legacy_fixture` reference build, never in release or real E2E builds.

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

Historical-read admission follow-up: the original HTTP limit of two concurrent
reads per identity and the browser's single read per second prevented ordinary
chart/Ping fan-out even after storage scans became inexpensive. HTTP history now
allows 32 concurrent requests per identity, 64–256 server-wide, and 32 requests
per second with a burst of 96. Scoped API tokens use the same history budget.
Physical metric work remains separately bounded: recent reads have 32–128 slots;
longer reads have 24–64 weighted slots (two for day queries, three for longer
windows). Completed slow queries no longer suspend either lane. SQLite pool and
storage scan bounds remain independent of HTTP admission.

The browser starts up to 16 chart reads and reserves a separate four-slot Ping
queue, without a one-second launch delay. Pending queues remain bounded and
cancelable; explicit 429/503 responses retain Retry-After backoff and at most
one retry. A single chart timeout does not pause unrelated charts or Ping.
Regressions cover concurrent chart/Ping admission, finite global capacity,
Agent report/probe isolation, canceled-slot reuse and the removed slow-read
cooldown. The real E2E adds 32 distinct concurrent chart/Ping requests through
HTTPS with rate limiting enabled for the remaining scenarios. These limits are
admission budgets, not a measured maximum sustainable request rate.

- [x] All 25 component checks pass on final executable revision `ca5f02ac`,
  including both actual external-SQL build variants.
- [x] Real browser/server/Agent scenarios pass on that executable revision:
  [final E2E 42820507567](https://github.com/r11234567/komari/actions/runs/42820507567).
- [x] Incident-sized consistent data, mixed ingestion/query/archive/export load,
  migration interrupt/resume, backup restore and rollback.
- [x] Fixed 1 vCPU / 1 GiB, 15-minute mixed-load manifest and measurements.
- [ ] Full-day stability, isolated archive RSS and matched old/new load comparison.
- [x] Accelerated complete retention/archive-cycle and scoped authorization,
  workspace revocation, API Token, plugin and root-permission regressions.
- [ ] Complete deployment-environment retention/archive-cycle soak.
- [x] Evidence links, limitations and unchecked deployment-only items recorded.

Final verification (2026-11-10): the disposable incident migration
imported all 11,583,271 logical raw points in 15m9.871s, then verified six exact
raw windows and the unchanged original logical point count. Its durable fixture
is retained outside the repository; no production database was opened for write.
The reference-format race suite passed again after build isolation (`pkg/metric`
72.400s), including metricstore, trafficledger, Connect and application startup.
The normal-build full metric and metricstore suites pass independently. The
production package selector rejects any retired fixture entry points.

All 25 final component checks passed on `ca5f02ac`. The
[final external SQL workflow](https://github.com/r11234567/komari/actions/runs/42820511980)
passed the complete historical-reference race suite with real PostgreSQL 17,
MySQL 8.4 and MariaDB 11.4, followed by the complete normal-build storage race
suite and a package-selection guard excluding all retired writer controllers.
The normal-build package results were 14.173s for metric, 4.173s for metricstore
and 6.307s for trafficledger. The other component checks cover exact pinned Web
builds, terminal/files, credential lifecycle, scoped API security, plugins,
privileged execution, workspace lifecycle, traffic, notifications and archives.

The final [real E2E](https://github.com/r11234567/komari/actions/runs/42820507567)
also passed on `ca5f02ac`: built pinned Web, HTTPS/Nginx, an installed nonroot
Agent, fresh human verification, terminal/clipboard/file round trips and
workspace revocation. Actual Agent reports produced 504 summary rows, one
complete cold archive, zero legacy hot rows or compression blocks, 13 durable
task rows and zero task failures. History queries and the real CSV download
passed. The downloaded full backup restored 79 CPU raw points matching the
chart sample count and sum; traffic ownership was 79 through accepted 79.
Process restart recovery and explicit legacy-compatible rollback also passed.
Subsequent evidence-only documentation commits do not change the executable
revision; they do not imply another measured runtime or deployment soak.

The resource fixture fixes ten nodes reporting every five seconds, two scoped
day/week/month chart workers every five seconds, real cold export, cancellation,
snapshot backup and the production background scheduler. The kernel enforces
one CPU and 1 GiB with swap disabled. Setup/migration is excluded from measured
CPU, and the source SQL and immutable archives are never modified. This is WSL
storage-call acceptance, not a claim of measured Debian Docker HTTP latency or
a completed 24-hour soak. Per-operation timings and backlog snapshots are
retained in the report produced by `scripts/ci/telemetry-resource.sh`.
The [normalized resource report](evidence/telemetry-resource-2026-11-10.json)
retains individual hot-byte samples, all timing summaries, the workload manifest
and source report/log SHA-256 values. Only repetitive CPU-info and host interrupt
statistics are omitted; the CPU model remains recorded. The measured working
tree was not recorded as a pristine commit, so this report makes no such claim.

The completed 900.04-second run on the durable incident fixture passed:

| Measurement | Result |
| --- | --- |
| Mean process CPU | 0.08184 cores |
| Sampled peak process RSS | 44.80 MiB |
| Report commits | 1,792; p95 22.83 ms, p99 62.82 ms; max 3,729.17 ms |
| Scoped day/week/month charts | 120 each; p95 61.38 / 70.93 / 69.36 ms |
| Cold export | 117.79 ms |
| Full backup during ingestion | 42.81 s; includes 12,620 immutable source files |
| End summary lag | One accepted batch; no pending task failure |
| Hot logical-byte range | 5,347–1,727,076; ended at 1,448,166 bytes |

The report contains no foreground errors. One initial maintenance slice hit its
deadline; its completion-based retry recovered and all final task attempt counts
returned to zero. The maximum report latency is deliberately retained even though
the design's p95/p99 gates passed. RSS is sampled once per second and is a mixed
process measurement, not a kernel high-water mark or isolated archive memory.
The cancellation timing in this load report is a pre-cancelled request; active
export cancellation and lease cleanup are verified by separate fault/race tests.
The added active-cancellation regression stops after 128 decoded cold records,
requires cancellation within one second and zero remaining leases, then verifies
that another complete cold export still succeeds; three race repetitions pass.
The source fixture was migrated before build isolation; the measured application
already selected the identical retired-storage/new-Telemetry path. A post-cleanup
smoke and final production-build CI separately validate that build selection.

Prior PR #75 verification is baseline evidence only. It does not validate this
new storage design or any unchecked item above. Do not merge, tag or deploy the
branch as part of these checkpoints.
