# Workspace lifecycle and performance diagnostics

All five stages are tracked in one PR, in implementation order. The initial
diagnostic baseline was followed by machine isolation, session cleanup,
signed rescue streaming and read-path optimization.

1. Diagnostics and evidence-producing hosted CI.
2. Per-machine activity, prompts and request cancellation.
3. Locked-session restoration and final-consumer cleanup.
4. Event-driven rescue authorization with bounded fail-closed revocation.
5. Read-path and Agent background-work optimization against the stage-1 baseline.

See [workspace authorization](workspace-authorization.md) for the resulting
behavior, dependency revisions and upgrade order. No production deployment,
local builds, dependency installation, load testing or browser E2E is part of
this work. Source changes and hosted regressions are not a production profile
or proof that every source of the reported stall has been eliminated.

## Read the snapshot

`GET /api/admin/diagnostics` uses the existing administrator authorization
boundary and returns `Cache-Control: no-store`. It does not introduce a public
metrics port, plugin RPC, new token type or authentication exception. An existing
administrator session or already entitled administrator API credential is
required, just as for the existing diagnostic endpoints. Do not paste the
credential into reports. The normal JSON response contains:

| Field | Meaning |
| --- | --- |
| `requests.routes` | Started/completed/in-flight/peak request counts; completion duration sum/max/histogram; first response duration sum/count/max/histogram; HTTP classes and Connect outcome counts. |
| `requests.recent_slow_or_failed` | A bounded ring of the last 64 slow unary or failed/canceled requests, containing only route templates, time, status and duration. |
| `database.primary_pool` | Primary database open/in-use/idle connections and cumulative pool wait count/duration. Sampling does not acquire a connection. |
| `database.primary_queries` | Cumulative GORM statement duration and errors, including a separate record-not-found count. No SQL or query values are retained. |
| `database.metric_pools` | Separate write/read pool counters where configured. No DSN is returned. |
| `report_queue` | Actual channel depth/capacity, pending/writing records, accepted/full-rejected reports, flush count/errors and duration. Counters belong to the current worker. |
| `workspace` / `verification` | Aggregate stored-grant/challenge/busy counts. If a state mutex is occupied the snapshot reports `snapshot_busy: 1` rather than waiting. No user, machine, system account, token or challenge identifiers are returned. |
| `runtime` | Selected Go runtime CPU-class, goroutine, GC and memory counters. CPU-class seconds are **not** host utilization or a replacement for a CPU profile. |

Counts are cumulative since process/worker start, with approximately simultaneous
atomic reads. Compare two snapshots from the **same** `requests.started_at` to
calculate rates; do not divide lifetime counts by an arbitrary recent window.
An occasional scrape is an administrator action, not a new background polling
loop. No history is persisted to the database.

Histogram bounds are nanoseconds, ascending and inclusive. Buckets are
**disjoint**, with a final unbounded bucket. HTTP status classes are indexed 0–5;
index 0 means no normal HTTP status. Connect code 0 means success; HTTP-only
or pre-Connect authentication rejections have no RPC outcome. In recent events
`rpc_code: -1` means the request did not reach a Connect outcome interceptor.

Canonical Connect labels come only from mounted protobuf service descriptors.
Unknown methods share an unknown-method template; arbitrary URLs/query values
cannot allocate per-URL labels. Other HTTP labels use Gin's server-side route
template and normalized method. At most 512 labels and 64 recent events are
retained. Terminal output, files, headers, cookies, OTPs, proofs, principals,
request parameters and raw error messages are not diagnostic labels.

## Interpret a stall without confusing it with normal streaming

* `CONNECT ... unary`: completion duration includes server-side authentication,
  policy checks, database waiting and handler work inside Gin.
* `CONNECT ... stream`: completion duration is the **connection lifetime**, not
  interactive response latency. Inspect first-response counters, in-flight
  streams and the actual Connect end-stream code. An HTTP 200 stream can fail.
* HTTP file transfers, WebSockets and legacy streams still have full-handler
  duration; do not call a long transfer a slow authorization operation.
* Pool `wait_duration_ns` is time waiting for a database/sql connection. It is
  not SQLite lock waiting inside a statement. Query elapsed time includes
  statement work/wait, but does not individually instrument transaction begin
  and commit. Do not add these overlapping times as independent CPU cost.
* HTTP-first-response time measures a server write/flush, not arrival at the
  browser. Reverse-proxy buffering, TLS, DNS and network failures require proxy
  observations. Outer plugin hooks run before Gin and are not covered here.
* Authentication remains unchanged and may itself wait for the database. The
  **snapshot handler** does not query SQL, but its authenticated HTTP endpoint
  cannot promise availability while authentication is blocked. Existing
  bounded administrator CPU/goroutine profiles remain available after access
  recovers. No unauthenticated diagnostic bypass is added.

The instrumentation adds no periodic profiler, SQL tracing, per-request log writing or
unbounded request history. A slow/error ring is diagnostic context, not a
durable audit log. Cancellation counts are server-observed client context
cancellation, not a claim that every browser-side `Failed to fetch` reaches
the server.

## Hosted CI evidence and limitations

`Workspace lifecycle and diagnostics` runs on GitHub-hosted runners with CGO
enabled. No tests use real production accounts, credentials or traffic.

* Real `httptest.Server` HTTP requests verify in-flight visibility, actual client
  cancellation and an early flushed response before a blocked stream completes.
* Actual Connect clients/handlers verify successful and rejected authentication,
  DPoP rejection before the RPC handler, recovered panics, and an HTTP-200 stream
  whose actual outcome is `resource_exhausted`. A real WebSocket upgrade and echo
  verifies that HTTP hijacking is preserved; a server-side RPC deadline is
  distinguished from client cancellation.
* Real temporary SQLite with a held single database connection forces a query
  to wait and expire. Tests assert pool wait counters, query cancellation and
  that the canceled INSERT did not execute.
* Actual report enqueue/overflow/drain operations verify the queue snapshot.
  Occupied workspace/verification locks verify non-blocking, aggregate-only
  snapshots. Unknown URLs and concurrent requests verify cardinality and
  accounting under the race detector.
* Negative controls deliberately run one exit-23 command and one genuine failing
  Go assertion in an isolated synthetic package. The check passes only if the
  runners return nonzero and retain full stdout, stderr, test JSON, human-readable
  failure output, context and original exit codes. These expected failures are
  labeled as negative controls and excluded from positive test evidence.
* A result validator requires every named positive assertion to have actually
  passed. Missing JSON, skipped tests, compilation failures or missing repeated
  benchmarks fail validation. It also requires the actual workspace deadline,
  grant cancellation, final-browser cleanup, bounded buffer, signed watch,
  committed wake, cross-machine mutex and SQLite read-only tests from stages
  2–5. A successful command without these named assertions is insufficient.
* Repeated real Gin-handler benchmarks compare instrumentation enabled/disabled.
  Fixed guardrails are added median time ≤25 microseconds/request, bytes ≤2048
  and allocations ≤12. Full measurements and deltas are uploaded. These detect
  gross diagnostic overhead; they are **not** production throughput claims,
  fleet-capacity certification or proof that the original stall is fixed.

All steps stream complete output to the Actions log, and retain raw JSON,
build stderr, command context, coverage, benchmark samples, validation evidence
and exit codes for 30 days even on failure. The displayed summary may be short;
the uploaded artifact is not truncated. No test is marked `continue-on-error`.

The companion jobs check out the exact Agent revision in
`scripts/ci/workspace-agent-revision.txt` and the exact Web submodule commit.
Real Node BroadcastChannels exercise cross-window sharing and delayed lock
messages. Node state tests exercise silent expiry, independent cancellation,
single-flight renewal and final-consumer timing. React component units execute
the actual boundary and terminal effects using JSDOM; only network/PTY and
canvas peripherals are substituted. They check renderer retention, cancellation,
session-token binding and failed-create retry suppression. These are component
units, not an actual browser, authenticator, Nginx or production OS shell.

Agent tests use a real temporary Connect stream with synthetic signed payloads,
including replay/tamper/disconnect rejection. Root/systemd integration cases
may skip on an unprivileged hosted runner; their inclusion in the package does
not mean privileged host operations were exercised. Production UI, reverse-proxy
and deployment behavior still require a separately authorized staged rollout.
