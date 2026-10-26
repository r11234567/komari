# Monthly traffic cycles and provider calibration

Reference implementation: `nuomiiiii/Lite` commit
`a73311d35f06d5f8f7a33937856dc485b88bfc80` (`pkg/trafficreset` and
`database/trafficledger`), under the repository's MIT license. The local port
retains Komari's Connect API, authorization boundaries and Agent raw counters.

Each node can set a reset day (1–31), clock and timezone. A day beyond the end
of a month uses that month's last day. Existing configurations continue to use
Asia/Shanghai midnight. Fixed UTC offsets accept `UTC+08:00`, `UTC-05:30`, etc.
between UTC-12:00 and UTC+14:00; these do not observe daylight saving time.
IANA zones remain accepted for compatibility with Lite.

The panel computes configured cycles from raw traffic deltas and the independent
daily ledger. Existing Agents do not need a new runtime protocol or a restart.
The raw Agent counter may still reset according to its existing local settings;
the panel's cycle does not reset or rewrite that counter. Browser Connect
responses, RPC2 compatibility views and traffic alerts use the same cycle totals.
Raw metric history and instantaneous network speeds retain their original meaning.

Calibration accepts either the provider's current-period upload/download totals
or a single billable total using the node's sum/max/min/up/down counting rule.
The administrator interface uses decimal GB (1 GB = 1,000,000,000 bytes), with
at most two decimal places. The API retains integer-byte fields `target_up` and
`target_down`, or an exclusive `target_total`. The effective total equals raw cycle usage plus a signed correction;
later real usage continues accumulating. Recalibration computes a replacement
correction against the already effective total, rather than adding it twice.
Corrections and their operator/time/targets are retained separately for audit.
They stop applying at the next cycle boundary. No Agent data files are rewritten.

For total entry, the fixed correction applies to raw **billable** usage, even
when upload and download cross over under max/min accounting. Directional values
are accounting allocations, explicitly labelled as such, not provider-measured
upload/download. Sum allocates proportionally; other rules retain the uncharged
direction unless bounding it is necessary. Raw data remains available separately.
The total target, offset, counting rule, operator and time are auditable. Changing
the node's counting rule deactivates an old total calibration and requires a new
provider observation. Directional calibration replaces total mode explicitly.
Current cycle display is cached for at most 15 seconds (never across a reset),
with concurrent loads collapsed and cache invalidation protected against stale
in-flight loads.

Partial first days are stored independently so a custom reset clock can outlive
raw metric retention. If the first day's history has already expired before the
upgrade, the dialog reports incomplete history and allows an explicit provider
calibration to establish the current total. It must not describe missing history
as a measured zero. A different reset policy creates a distinct cycle key, so
old corrections are not silently reused for a different billing period.

Validation runs in GitHub Actions only: `traffic-cycle.yml` covers timezone and
month boundaries, real SQLite persistence, metric retention, positive/negative
and repeated/concurrent corrections, and isolation of raw counters from browser
display values, both input modes, max/min crossover, changed counting rules and
cache invalidation. Complete output and failure context are retained for 30 days.
No browser E2E or local VPS builds/application tests are required.
