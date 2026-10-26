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

Calibration accepts the provider's current-period upload and download totals.
The administrator interface uses decimal GB (1 GB = 1,000,000,000 bytes), with
at most two decimal places. The API retains integer-byte fields `target_up` and
`target_down`. The effective total equals raw cycle usage plus a signed correction;
later real usage continues accumulating. Recalibration computes a replacement
correction against the already effective total, rather than adding it twice.
Corrections and their operator/time/targets are retained separately for audit.
They stop applying at the next cycle boundary. No Agent data files are rewritten.

Partial first days are stored independently so a custom reset clock can outlive
raw metric retention. If the first day's history has already expired before the
upgrade, the dialog reports incomplete history and allows an explicit provider
calibration to establish the current total. It must not describe missing history
as a measured zero. A different reset policy creates a distinct cycle key, so
old corrections are not silently reused for a different billing period.

Validation runs in GitHub Actions only: `traffic-cycle.yml` covers timezone and
month boundaries, real SQLite persistence, metric retention, positive/negative
and repeated/concurrent corrections, and isolation of raw counters from browser
display values. Complete output and failure context are retained for 30 days.
No browser E2E or local VPS builds/application tests are required.
