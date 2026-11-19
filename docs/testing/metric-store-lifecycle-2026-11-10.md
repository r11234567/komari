# Metric Store lifecycle repair

## Behavior

Runtime metric readers now acquire a shared Store lease before obtaining the
active pointer. Reload, close and exclusive physical maintenance wait for these
leases. Readers release after their SQL rows/streams and Store calls finish;
waiting acquisitions respect cancellation. Release is idempotent so scoped
cleanup and an explicit early release cannot consume the gate twice.

Coverage includes native and legacy history/definitions, Ping summaries,
administrative dashboard reads, CSV export, record reconstruction, traffic
baselines, notifications, retention updates and asynchronous disabled-metric
deletion. Existing ingestion/maintenance leases and startup-only pointers retain
their ownership rules. `GetStore` documents its restricted unleased use.

Traffic baseline fallback passes its already-leased Store to the helper. Traffic
ledger backfill runs outside the caller's lease and retention updates reacquire
afterward. This avoids recursive shared acquisition while an exclusive operation
is waiting. Notification network delivery does not retain a Store lease.

No metric schema, compression format, maintenance interval, retry schedule,
retention policy or historical-result point semantics changes in this repair.
The storage-engine redesign and maintenance scheduling remain deferred together,
as requested. A lease protects lifetime; it is not a multi-query SQL snapshot or
an implementation of cross-engine data migration.

## Regressions

- `TestStoreLeaseBlocksReloadAndClose`: active reader keeps the instance open;
  waiting reload/close respects its deadline; release permits close; repeated
  release is safe.
- `TestStoreLeaseWaitCancellationAndMissingStore`: canceled/missing-store
  acquisition returns no pointer or lease and leaves the operation gate usable.
- `TestTrafficBaselineReusesLeaseWhileCloseWaits`: a real SQLite read is stopped
  at the connection pool, close queues, then traffic reconstruction reaches its
  missing-counter fallback and completes without acquiring a nested lease.
- `TestDualEnd_MetricReadsSurviveReload`: real HTTP QueryMetrics,
  ListMetricDefinitions and GetPingStats stop at a deliberately occupied SQLite
  read pool. Reload waits while the old Store remains open. Releasing the pool
  allows reads, reload and authenticated DPoP SubmitMetrics to finish. An
  uncached post-reload HTTP query verifies the exact retained sample value.

The HTTP regression was also run with the pre-repair `metrics_query.go` restored
through a read-only Go overlay. It failed because reload completed before the
HTTP reader finished. This negative control verifies that the test detects the
original defect, rather than merely exercising successful sequential reloads.

## CI and end-to-end verification

The `metric-store-lifecycle` feature-audit matrix runs complete race suites for
`database/metricstore`, `database/notification`, `database/trafficledger`,
`utils/notifier`, `web/connect`, `web/rpc/jsonrpc`, `web/api/admin` and `cmd`.
The normal CI retains full repository tests, formatting, external SQL integration,
frontend checks and complete embedded server builds.

The manually invoked `feature-e2e.yml` pins Agent
`f6d3504cbb4306b2dba40193ba1a7572791edd71`, checks protocol provenance and builds
the actual pinned Web/server/Agent. On a disposable GitHub-hosted Linux runner,
it installs a nonroot Agent behind HTTPS Nginx, enrolls normally and waits for
real CPU history. Six rounds of historical/definition/Ping reads run concurrently
with authenticated policy-API reloads. Reads after the response-cache TTL verify
subsequent instances. The original browser authorization, terminal and file
round-trip checks remain part of the scenario. Raw logs and browser artifacts
are retained by the workflow; no production service or database is targeted.

Hosted results are recorded in PR #75 after the corresponding revision completes.
Local evidence is retained in the workspace under `.cache/backend-p2-*.log`.
Passing this fixture does not establish performance on the incident database or
CPU reduction from a future compression engine.
