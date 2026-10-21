# Stage 0–2 implementation and rollout status

This branch supplies protocol-v2 server support and regression infrastructure.
It does not install a host tool, convert a running service, reset an old node,
or enable privileged delivery for an old agent. Use a v2-capable agent only
after later installation/local-policy/platform stages have been implemented.

- Monitoring and existing ordinary configuration remain available to old agents.
- New privileged saves require protocol 2 in the request and prior authenticated
  agent negotiation. Negotiation alone is not approval or baseline application.
- Legacy terminal reports and CompleteManualUpgrade are rejected. Legacy receipt
  reports never change approval. Old pending tasks must be superseded, not
  repaired by manufacturing confirmed_at or delivered.
- Existing revision history is retained. The new applied pointer starts unknown
  for legacy data; old claims are not promoted to verified v2 observations.
- Schema migration is additive. ClientPrivilegedState is initialized lazily under
  a transaction; startup AutoMigrate creates the three companion tables.
- Same-identity grants require signed 2FA approval. Service identity changes and
  helper installation/removal additionally require bounded host operations.
- Bound application failures retain the previous applied pointer and cannot regress a delivered revision.
- Host LOCAL_APPLIED is not delivered. The daemon reports the matching revision,
  hash, generation, service, actual identity/capabilities and fresh startup ID.
- Pending revocations block new server-side execution/WebSSH scheduling; grants
  only become authorized after the matching daemon report.
- A self-report is authenticated telemetry, not remote kernel attestation. The
  later administrator-owned host tool must enforce fresh OS authentication,
  local policy, signature checks and safe service conversion. The server never
  treats nonce possession or a caller-provided method string as that proof.

Run `scripts/ci/run-go-tests.sh` with the desired Go package list. GitHub workflow
`Privileged delivery regression` runs the relevant database, Connect, execution,
terminal and legacy adapter suites. On all outcomes it retains JSONL events,
human logs, compiler stderr, exit code and a failure index for 30 days. Test
fixtures use temporary databases and synthetic credentials only.

The proto dependency is pinned to an immutable development commit, not a local
replace directive. Publish/merge the proto change before releasing a consumer.
The protocol workflow separately checks breaking changes, Go and TS clients,
regeneration consistency, and shared hash vectors.
