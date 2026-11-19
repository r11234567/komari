# Terminal UI and transport resumption feedback

## Completed implementation

- [x] Keep terminal menus in the nearest Radix theme, including portalled submenu content. Move submenu content out of the scrolling parent so colors and close actions remain reachable.
- [x] Add a per-machine workspace lock to the tab context menu, using the existing revocation path without a new 2FA ceremony.
- [x] Compact the unlock dialog to a maximum 400 px width, use the terminal's dark appearance and provide all new unlock/startup messages in the five supported languages.
- [x] Fit the actual content area after outer padding, bound the page by the dynamic viewport and schedule ResizeObserver fits outside its current delivery cycle.
- [x] Resume the existing PTY after transient Agent transport loss using accepted-output checkpoints and a retained command cursor. Do not re-lease an attached session or silently create another shell.
- [x] Replay browser watches with the same session/output cursor and retry ambiguous input using identical command sequence/bytes. Stop retries on lock, revocation or non-transport errors.
- [x] Bound Agent replay by 32 frames of at most 32 KiB, and offline retention by the backend heartbeat lease, current idle grant and fixed absolute authorization deadline. Backend state loss fails closed.

## Local verification

- [x] 183 Web unit tests, TypeScript build and ESLint passed (warnings remain; no lint errors).
- [x] 17 real Chromium browser cases passed, including actual color/close actions, server-picker contrast, machine lock, English/Chinese dialogs, 1280x720 / 800x500 / 430x700 terminal geometry, clipboard, OTP, adapter replay and lock-during-command-retry.
- [x] Linux Go race tests passed for Agent clientcore and terminal.
- [x] Ten repetitions of resumption cases passed, including a real `/bin/sh` PTY retaining its PID, working directory and output generated during the offline interval. Replayed input is not re-executed.
- [x] Backend remote-management, workspace and complete Connect race suites passed. The Connect regression uses a real local TLS/HTTP2 server and production handlers/stores, with a synthetic authenticated Agent peer. It verifies checkpoints, output deduplication, command resume and prompt revocation.
- [x] Run the workspace component checks with the real English locale after translating the terminal UI; all three portal/provider/collected-lock cases passed. Keep the existing negative controls enabled.
- [x] Pin the backend workspace CI to the paired Agent revision and require all four named PTY-resumption regressions in addition to its existing signed-stream evidence.

These are separate component/integration layers. They do not constitute a full
deployed browser → Nginx → panel → Agent acceptance test. The source changes
require paired new backend/Agent builds; the older published dev tags do not
contain them. Agent/panel process restart ends the original shell. Browser-only
disconnect grace is 60 seconds; Agent transport retention is at most 30 seconds
after the last checkpoint and never extends human authorization.

## Delivery verification

- [x] All 12 required Web workflows passed at `2e8c14f42a756d258468a8ee583f38535e69f3a3`, including the explicitly dispatched browser job (run `41816218966`). The workspace component positive and existing negative controls passed.
- [x] All 30 Agent PR checks passed at `f140b0d6dc8b4cda2c9b2d21de4f312f266e65c0`; all eight protocol checks passed at `e0c08a1ef9cd30e65071e9941a134a55cb42dddc`.
- [x] Embed and hash-verify all 605 files from the exact Web CI system-build artifact. Record source/protocol/CI provenance and the canonical Git lockfile SHA-256, avoiding Windows checkout line-ending differences.
- [x] Verify the expanded Agent evidence gate accepts complete results and rejects missing, skipped, wrong-package and failed results. The backend workspace job now uses the paired Agent revision and requires the real PTY case by name.

## Remaining acceptance

- [ ] Verify the repaired UI and both network-loss paths on the deployed Debian/Docker/Nginx environment.
- [ ] Reproduce sufficiently cold startup and compare fast/slow nodes; short repeated openings do not establish a cold baseline.
- [ ] Test historical queries, reporting, traffic and configuration delivery on real nodes.
- [ ] Complete the untested file-management and transfer steps; do not infer resumable file transfers from terminal resumption.

See [the shared real-Agent checklist](dev-real-agent-checklist.md). These unchecked
items are deployment acceptance, not completed test claims. No merge or new dev
release is part of this feedback repair.
