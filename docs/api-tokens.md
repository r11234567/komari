# Scoped API tokens

Login settings now manage multiple per-purpose credentials, following the
resource/permission model described by [Cloudflare](https://developers.cloudflare.com/fundamentals/api/how-to/create-via-api/)
and the expiry/revocation guidance for [GitHub fine-grained tokens](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens).

Each token has a public ID, a name, an explicit expiry, permissions, and either
specific node IDs or an explicitly selected all-nodes policy (including future
nodes). Optional IPv4/IPv6 CIDRs restrict its source. The UI defaults to 30 days;
the server requires a future expiry within one year. One owner may have at most
50 active tokens. Metadata is paginated in groups of 25.

The current reviewed permission catalogue is deliberately limited:

| Permission | Typed HTTP procedures |
| --- | --- |
| `nodes:read` | `BrowserService/ListAgents`, `BrowserService/GetAgent` |
| `metrics:read` | `MetricsService/QueryMetrics`, `MetricsService/GetPingStats`, `MetricsService/ListMetricDefinitions` |

Services use their canonical `/komari.browser.v1...` and `/komari.metrics.v1...`
paths. Node information includes the selected nodes' private metadata. Node
filters apply server-side even when the caller omits `agent_ids`; explicitly
requesting another node fails. Metric caches are separated by token ID. New
procedures, legacy RPC/REST adapters, plugins, terminal, execution, files and
credential management are denied to scoped tokens by default. No admin role or
browser user/session is inferred from a scoped token.

For example, send `POST /komari.browser.v1.BrowserService/ListAgents` with
`Content-Type: application/json`, `Connect-Protocol-Version: 1`, an
`Authorization: Bearer <token>` header, and body `{}`. Do not put the token in
query strings, URLs or logs.

Creation, rotation, revocation and legacy-key shutdown require a real human
session plus a fresh single-use proof bound to the exact action. Creation binds
the name, permissions, nodes, expiry, source restrictions and replaced token ID.
No API token or plugin principal can supply that proof. Rotation atomically
creates a new credential and revokes the old one; if validation or insertion
fails, the old credential stays valid. For overlap during migration, create a
separate token, update the caller, then revoke the old token.

Secrets have a `komari_pat_` prefix, public UUID, and 256 random bits. Only their
SHA-256 verifier is persisted. Verification compares fixed-size digests in
constant time. The secret appears in its creation response only, with no-store
caching; list APIs return metadata only. The UI keeps the one-time result in
memory, never local storage. Losing that response requires issuing another
credential and revoking the inaccessible one.

Revocation and expiry are checked on every authenticated request. Password or
second-factor security resets revoke the account's scoped tokens in the same
credential transaction. Account deletion prevents further authentication.
In-flight reads already authorized before revocation may finish within the
normal request deadline; scoped tokens cannot open long-lived streams.

The independent failed-authentication budget permits ten initial failures per
source and replenishes one attempt per six seconds. Successful API use does
not consume that budget. Its table is bounded. Valid enrolled device identities
are verified separately and remain usable when this budget is exhausted. When
`KOMARI_TRUSTED_PROXIES` is unset, authentication budgets and CIDR restrictions
use the transport peer instead of trusting arbitrary forwarded headers. Declare
the actual proxy hosts/CIDRs when deploying behind a proxy.

Successful scoped-token requests additionally have an always-on per-token
budget: 10 requests/second with burst 30; history reads share 1 request/second
with burst 4. Exhaustion returns 429 and Retry-After. Rotating source addresses
does not reset that budget. Existing query concurrency limits still apply.

The old global API key remains a separate compatibility credential with its
old privileges until explicitly disabled after migration. Both old comparison
sites use the same fixed-size constant-time comparison. The new UI shows its
enabled status and requires fresh verification to disable it. Do not describe
this compatibility credential as a scoped token. Plugin-owned routes retain
their own foreign Bearer authentication; namespaced Komari tokens still obey
Komari's restrictions there.

The new token table is additive. No production credentials are rotated or
revoked by the schema migration itself.

## Existing quota data

`traffic_limit` writes now require a non-negative int64 at the shared save
boundary. To inspect older data, run
`python3 scripts/audit-traffic-limits.py /path/to/komari.db` against the existing
database or a consistent backup. It opens SQLite in read-only/query-only mode,
reports node IDs and storage classes, and does not print stored payloads. Exit
codes are 0 for no anomalies, 1 for anomalies, and 2 for an inspection error.
Non-integer/NULL values are review candidates, not proof of an attack. Corrections
require a separately reviewed value from a backup/provider record; this tool
does not silently replace quotas with zero.
