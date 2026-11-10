# Historical read admission and CrowdSec follow-up

## Incident evidence

The supplied November 7 and 8 Nginx samples contain 618 and 1,417 historical reads, peaking at 8 and 23 historical requests per second. The second incident includes 590 HTTP 499 responses and 53 HTTP 429 responses among historical reads. These samples contain short request bursts. The November 9 sample contains no historical-read endpoint requests. The supplied resource snapshot describes a one-vCPU host with approximately 967 MiB RAM, swap pressure and CPU steal. Storage compaction, retention and backup work require separate investigation.

## Implemented

- [x] Install API credential authentication in the normal application router. Reject revoked credentials before public-route fallback. Cover Bootstrap and BuildRouter in a regression test.
- [x] Separate request budgets by verified Agent, account, API Token ID or anonymous source. Ignore untrusted forwarding headers for budget and RPC metadata identity.
- [x] Charge stream openings once. Separate probe, ingestion, control and browser read budgets.
- [x] Limit historical reads to 2 requests per second with burst 5 when request limiting is enabled. Permit at most 2 in-flight requests per principal and clamp global HTTP concurrency to 2 times GOMAXPROCS, between 2 and 8.
- [x] Charge the five legacy history RPC methods individually, including batches and WebSocket messages. Preserve plugin-owned RPC methods and legacy JSON-RPC error envelopes.
- [x] Fail fast when native physical read capacity is exhausted, with HTTP 429 and Retry-After. Keep recent and historical lanes separate. Light capacity is clamp(GOMAXPROCS, 1, 4), heavy weighted capacity is clamp(GOMAXPROCS, 1, 2). A long-window request consumes up to the entire heavy capacity.
- [x] Apply a 15-second execution deadline to native metric and Ping summary reads and the outer administration dashboard handler. Slow reads taking at least 2 seconds impose a same-lane cooldown of up to 15 seconds. Admission is independent of the chosen storage engine.
- [x] Share successful Ping and dashboard messages through the existing 5-second, 32-MiB bounded metric response cache. Allocate fresh per-call Connect response headers. Preserve identity-scoped keys.
- [x] Cancel native metric work with the initiating request and allow coalesced followers to stop waiting independently. Internal administration dashboard modules retain their existing shared load semantics and maximum 30-second budget. Traffic trends retain their separate 8-second budget.
- [x] Serialize first-party historical reads per page, with at least 1 second between starts, at most 32 queued reads and 30 seconds queue lifetime. Abort obsolete jobs before network work.
- [x] Honor Retry-After on 429/503, cool down the page queue and retry at most once. Back off for at least 10 seconds on timeout without automatically retrying that request.
- [x] Cancel chart reads while the page is hidden, wait for metric definitions before querying and stabilize selected metric dependencies across chart layout changes. Login, terminal input, files and streams bypass the historical queue.
- [x] Provide deploy/crowdsec with exact machine-route generation, dedicated server-derived logs, parser rules, migration instructions and isolated verification fixtures.

## Verification

- [x] Frontend TypeScript build and 196 unit tests pass.
- [x] Frontend lint completes with zero errors and 38 existing/general warnings.
- [x] Eighteen real Chromium cases pass, including the production MiniPingChart/public adapter/transport queue under synthetic fetch and visibility changes. Hiding aborts the active read and drops its queued summary. Showing resumes reads.
- [x] Real Nginx syntax validation and seven HTTP routing checks pass with a test-only mock bouncer and synthetic downstream credential validation.
- [x] Official CrowdSec v1.8.2 with its Nginx collection parses seven synthetic log classes. Valid RPC/business traffic and 429 are whitelisted. Unknown RPC, webpage authentication failures and path scans still reach scenarios.
- [x] Affected backend race suites pass for security, API authentication, legacy JSON-RPC, native Connect and normal application startup. Agent history uses the history budget independently of reporting.
- [x] All 12 hosted Web workflows and 18 hosted Chromium cases pass at 4031c487c63a90ca94ae0a44e970aa7f3298bac0. Both system UI and default public theme use that successful direct-head CI artifact.
- [x] Single-CPU admission/cancellation regressions pass with GOMAXPROCS=1 and a 512-MiB Go memory budget.
- [x] Complete Linux backend builds with both current CI frontend bundles embedded.
- [ ] Hosted backend checks for the final PR revision.
- [ ] Deploy and validate real Nginx/Xray, CrowdSec LAPI/bouncer and enrolled Agent traffic, including shared NAT and a selectively added test decision.
- [ ] Replay the incident database or measure CPU and ingestion latency on the affected host. The supplied logs alone cannot establish a production CPU reduction.

## Deployment choices

SSRF protection defaults to enabled, preserving the existing address checks. Disabling it skips address restrictions for the supported market/theme download operations. Windows basic GPU collection defaults to enabled and uses the ordinary Windows runtime toggle, independently of privileged Linux GPU grants. These preceding sequential changes have their own tests and remain in the same open PR series.

Request-budget limiting remains controlled by the existing setting. Native physical-read admission, deadlines and first-party scheduling remain active independently. This layer returns backpressure while the storage engine remains replaceable. It does not impose a process-wide CPU percentage cap. Database retention, compaction scheduling, backup pressure and the planned compressed-block engine require their own acceptance measurements.

CrowdSec uses precise machine routes to bypass its Nginx access hook before backend authentication. Invalid credentials are still rejected by Komari. Known RPC authentication and business failures are delegated to Komari budgets and authentication controls. Unknown routes and ordinary webpages retain the bouncer. Deployments using AppSec must validate machine-lane rules separately. Only restored loopback sources receive local bypass. Existing public-IP bans must be reviewed individually.

No production configuration has been applied from this workspace.
