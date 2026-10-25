# Feature audit on GitHub Actions

Run these checks on GitHub-hosted runners. Do not install dependencies, compile,
launch browsers or run the audit suite on the 1 CPU / 1 GB panel VPS.
`run-audit-check.sh` rejects direct execution outside GitHub Actions.

The audit runs on pull requests, main, `ci/feature-audit-*` branches, and manual
workflow dispatch. Independent jobs use `fail-fast: false`. Independent steps
continue after a failed check where their dependencies remain available.
Failures stay red; they are never converted into a passing result.

Each check records the actual checkout and recursive submodule commits, command,
start time and exit code. stdout and stderr are retained in full. Go checks also
produce JSON test events, readable output, compiler stderr and a list of failed
tests/packages. A failed or timed-out command is visible in the Actions job
summary. Artifacts are uploaded with `if: always()` and retained for 30 days.
Logs from runner-level termination are limited to what Actions could flush.

No deployment, service restart, production credentials, real agents, notification
providers or production file operations are part of this workflow. All new file
regressions use `t.TempDir()`. The browser harness mounts the real TerminalSession
component with an isolated synthetic transport; it is not a full production
panel/Agent end-to-end acceptance test.

The application source and protocol gitlinks stay at the synced revisions.
These branches add verification only. Existing product defects may intentionally
make the new regression cases fail until the application is repaired.

Backend areas cover file leases/transfers and 2FA, terminal/settings, plugins,
all JS runtime subpackages, built-in notification channels, RPC ACL and GPU
mapping. Tests run with the Go race detector. JS compatibility tests exercise
legacy `sendMessage(message, title)`, async Promises and event JSON field names.
