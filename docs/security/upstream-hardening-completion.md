# Selected upstream hardening: implementation and evidence

This completes the remaining software work from the R1–R5 selection. Real-node
acceptance is a separate joint test, described in
[the Debian/Docker/Nginx checklist](../testing/dev-real-agent-checklist.md).
Nothing here changes production configuration, quota data, or merge status.

## Terminal pressure and cleanup

The Agent previously put output and command acknowledgments in one 32-entry
queue. When a slow upstream receiver filled it, acknowledging the first input
could block processing subsequent input or resize commands. The replacement
coalesces acknowledgments independently and gives them priority between output
frames. Only one goroutine writes to the Connect stream; output sequences remain
ordered. It cannot interrupt a network write already in progress, but cancelling
the stream interrupts that write and closes the PTY.

Additional corrections:

- A send failure cancels a quiet receive rather than leaving the shell alive.
- Normal completion waits for the PTY reader before enqueuing its close event.
- Cancellation closes the PTY and joins the sender, reader, and wait workers.
- Unix `Wait` and `Close` share one process reaper. The PTY descriptor closes even
  after natural process exit, releasing blocked I/O.
- Acknowledged backend commands/events clear discarded slice references.
- Backend cancellation is checked before delivering an existing backlog.
- A full replay buffer fails the session, releases its retained data, notifies
  the browser with a failed-close event and stops further dispatch. This is an
  explicit overload failure, not lossless infinite scrollback.

The tests fill the actual Agent sender queue with 32 KiB chunks, stall transport
writes, issue interrupts and resize, then exercise cancellation, send failure,
local capability revocation, output ordering and final close. Backend tests cover
workspace revocation while output and old commands are retained. Real Unix PTYs
cover concurrent wait/close and descriptor release.

## Resource review

Limits below are separate controls, not a claim of a process-wide heap ceiling.
Serialized-size accounting excludes Go object/allocator overhead.

| Path | Limits / behavior | Disposition |
| --- | --- | --- |
| Agent terminal sender | 32 output chunks of at most 32 KiB, one in-flight send, one reader chunk; one coalesced ACK | Existing output bound retained; ACK blockage and cancellation fixed |
| Backend terminal input | 1,024 queued commands and 2 MiB serialized command bytes per session | Byte budget added; acknowledgments/close release it |
| Backend terminal replay | 8 MiB output payload, 16 MiB serialized events, 8,192 events per session | Metadata/tiny-frame bounds added; overload closes the session |
| Session population | 64 active sessions; closed retention and total-entry pruning remain in place | Existing limits retained; aggregate memory can still be substantial with many authorized sessions |
| Historical query cache | 512 entries, 32 MiB total protobuf size, 4 MiB per response, 5-second TTL | Size budgets added; oversized results are returned without caching |
| Historical reads | Existing light/heavy lanes, short admission wait and storage gates | Retained; a cancelled singleflight follower now returns promptly |
| Backend file data plane | 32 transfers, 512 KiB copy buffer, `io.Pipe` backpressure, exact-size checking, 2-minute transfer ticket and 30-minute call bound | Existing implementation reviewed; full-body buffering was not introduced |
| Agent file streams | 8 stream operations, 512 KiB copy buffers, 30-minute HTTP timeout, explicit upload cancellation | Existing implementation reviewed; isolated worker and transfer regressions retained |
| Plugin HTTP / hooks / WebSocket / logs | Existing body/output buffers, JS turn deadlines, bounded logs, WS queue of 64 frames / 16 MiB and write deadlines | Reviewed without changing official plugin APIs |

Approved plugins remain trusted code in the panel process. JS turn deadlines and
individual I/O buffers are not a hard per-plugin OS memory/process quota. In
particular, Node-capable code, host callbacks, and same-origin HTML integration
must not be described as a malicious-plugin sandbox. Imposing a new sandbox or
removing official runtime APIs is not part of this compatible update.

Revocation checks now cover online pressure and idle local cancellation in
addition to the existing fixed/idle grant deadline, scope and replay tests.
Actual offline expiry, container/Agent restart and reconnect behavior remains on
the deployment checklist; those scenarios are not inferred from a queue test.

## Historical-query selection

The reviewed official changes were `48af63ca` (query summaries and bounded state
reuse) and `3cf8445a` (field projection and read-path work). The local store already
has batched V4 block reads, shared aggregation views, average-only dashboard
reads and conditional digest decoding. The official summary representation also
does not carry all local merged-Ping state such as `lossCount`. Copying it over
would require a separate storage/aggregation adaptation.

This delivery instead removes a measured adapter cost: the old Connect path
scanned the entire physical result and reserved an entire-result slice again for
each selected node. Results are now indexed once by node using compact integer
positions instead of copying large point structs. Only requested/visible node
groups are indexed for response assembly. Raw and downsampled
responses keep their existing contracts. There is no cross-request mutable
bucket pool, schema/index migration, new percentile algorithm or raw/rollup
handoff change.

Run the reproducible measurements on a development machine or disposable CI
runner, never on the panel VPS:

```sh
bash scripts/ci/measure-history.sh ci-artifacts/history
```

The benchmarks compare the retained previous implementation with the current
implementation on identical ordered samples, then on one file-backed SQLite V4
fixture: four nodes, three tagged metrics, five-minute samples across 30 days,
1h/24h/30d windows, warm database state and no response cache. Separate load
processes run two readers, twelve mixed-window queries and concurrent ingestion,
recording query/ingestion p50/p95, peak sampled Go heap, process RSS high water and
cancel/recovery behavior. RSS includes fixture setup and cannot be interpreted
as the query's isolated memory cost. Timing remains machine/load dependent.

Raw results accompany this document under `../benchmarks/2026-11-03/` and are also
uploaded by the terminal-workbench CI job. Retain the measured allocation saving;
do not advertise a general database speedup. The complete upstream query-plan
rewrite, arena cache and index migrations remain deferred, with no claimed
benefit or unimplemented promise in this batch.

The broader metric tests exposed an existing fixture timing dependency:
materialization could leave the sealing cursor partway through a sweep. The
separate full-sweep test now explicitly resets its starting cursor and retains
all multi-pass/progress assertions; production compaction was not changed.

## Seven-advisory regression contract

```sh
python3 scripts/ci/run-security-advisories.py ci-artifacts/security-advisories
```

The checked-in JSON matrix names required tests and their packages. The runner
retains raw Go JSON, readable logs and a per-advisory report. A missing test,
skip, duplicate/conflicting outcome, package failure or nonzero Go exit fails
the contract. The matrix checker itself has negative regression tests.

| Advisory | Required coverage |
| --- | --- |
| GHSA-3hqg-6m62-cgxf | Fixed-size legacy comparison, independent failure budget, proxy spoofing, enrolled-device isolation and scoped-token revocation |
| GHSA-74qc-gfcj-9273 | Theme-name rejection, real extraction traversal/symlink rejection and failed-update preservation |
| GHSA-hxjg-93wc-h8p8 | REST/Connect Origin controls, real cross-site reduction request and removal of migrated admin REST bridges |
| GHSA-h8jh-mv4w-3gm5 | Shared strict quota boundary, real SQLite atomic rejection and valid numeric callers; unused Web evaluator was already removed |
| GHSA-j2jg-mq93-mqg4 | Blocked IPv4/IPv6 ranges, repeated DNS validation and numeric-only dialing, actual redirect callbacks and bounded redirect chain |
| GHSA-q355-h244-969h | Actual strict terminal WS handshakes, default-disabled legacy routes, explicit compatibility routing and single-use browser tickets |
| GHSA-jhmr-57cj-q6g9 | Real legacy and native password/TOTP flows reject missing, malformed and replayed factors; scoped single-use authorization and cancellation remain enforced |

Redirect DNS now uses the request context; the standalone preflight resolver has
a deadline. The DNS-rebinding test supplies synthetic resolver/dial functions;
the redirect test supplies a synthetic HTTP transport. They make no requests to
metadata services or other private infrastructure. The WS handshake is a real
local connection. These are repeatable component/integration regressions, not a
new production penetration test or a complete audit of every configuration.

The optional mobile remote-browser origin rule still allows opaque origins only
with its separate authenticated, single-use ticket flow. That exception is not
the strict terminal handshake rule. Disabling Origin protection, widening
allowlists, retaining the privileged legacy key, or exposing compatibility routes
changes the deployment assessment.

## Remaining joint acceptance

Only environment-dependent work remains in this selected batch: inspect a
consistent production quota-data backup, validate actual Origin/trusted-proxy
settings, migrate callers and disable the old global key when ready, and run the
combined real-node checklist. No production repair, new authority model, merge
or deployment is implicitly approved by a passing local test.

For the stronger threat model where the panel process itself is compromised,
see [the independent-authorization design boundary](control-plane-trust.md).
