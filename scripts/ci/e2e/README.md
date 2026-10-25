# Real feature integration on GitHub-hosted runners

Run **Feature audit - real end-to-end** (`feature-e2e.yml`) on the audit branch.
Never execute these scripts on the panel VPS. Both shell and browser entry
points require a GitHub-hosted Linux runner. There is no production deployment,
repository write permission, production secret or external test target.

The workflow records the server, Web, Agent and protocol revisions, verifies
that all protocol gitlinks agree, builds the pinned Web with the existing
release composite action and embeds it in a fresh server binary. The Agent
revision is deliberately immutable in the workflow; update it explicitly when
testing a new Agent candidate.

The fixture is an isolated SQLite panel behind loopback-only HTTPS Nginx. A
temporary CA is trusted by the runner and Chromium. Nginx forwards the Agent's
bidirectional Connect streams over HTTP/2 to the panel's h2c listener. The real
host installer creates a systemd service under the runner's nonroot identity.
The test uses first-run setup, login, actual Agent enrollment, a short-lived
administrator bind ticket, locally installed panel public-key trust, protocol
v2 negotiation, a signed capability grant and normal fresh 2FA confirmation.
Execution and rescue-helper capabilities stay disabled. No private keys,
signed configuration or replay state are injected or edited.

The single sequential Playwright scenario tests:

- Wrong OTP rejection and account gate success without reusing the proof for
  password changes.
- Actual xterm input/output and text export; search, system clipboard copy and
  a paste checked against the Agent's filesystem to catch duplicate sends.
- Tab rename, color, duplicate, pointer drag and bulk close, including actual
  CreateSession/CloseSession responses.
- Real file browsing; a >2 MiB binary uploaded in at least three 1 MiB chunks
  through the browser, then downloaded and compared using SHA256.
- Monaco saves of UTF-8 text and an empty file, checked both on disk and by
  downloading through the Agent.
- Missing/replayed file proofs, overwrite=false and recursive=false failures
  with assertions that existing files survive.
- Create (including the requested Unix permission bits), copy, move, chmod,
  name search and delete, followed by an Agent
  restart with the same identity, applied revision and working file channel.

Each sensitive action waits for a genuinely new 30-second TOTP window. No
future codes, clock changes, replay resets or mock transports are used. This
adds several minutes; the scenario timeout is 15 minutes and the job timeout
45 minutes. Dependencies, compilation and browsers run only on Actions.

Every failure retains the named Playwright step, screenshot, video, trace,
HTML/JUnit/JSON reports, browser console/network events, Nginx access/error
logs, server logs, enrollment log and Agent journal. Build and setup command
output is also captured by the existing audit wrapper. Artifacts exclude
databases, private keys, credentials files and Agent state. Browser traces
contain **disposable CI account/session data** and expire after seven days.
The always-run cleanup stops the fixture and destroys its state and secrets.

A green run proves this pinned Linux/Chromium/loopback proxy scenario. It does
not prove every production proxy configuration, Windows/macOS behavior, real
GPU driver telemetry, plugin compatibility or absence of all defects. Keep
the existing component/race/platform CI jobs alongside this workflow.
