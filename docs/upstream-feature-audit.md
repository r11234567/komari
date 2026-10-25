# Upstream references and CI repairs — 2026-10-25

Source-only shallow checkouts were fetched from the following repositories.
No dependencies, builds, tests or browsers ran on the panel VPS during this
follow-up. All execution belongs on GitHub-hosted runners.

| Repository | Branch | Reference commit |
| --- | --- | --- |
| `komari-monitor/komari` | `main` | `9e5f9a3a71daadc488e063193f383a3a464e8c48` |
| `komari-monitor/komari-web` | `radix` | `eed29f344af608eb364426643da9124af863afff` |
| `komari-monitor/komari-agent` | `main` | `828afafe7fb5b1b58c1232094e1e46320719016e` |
| `nuomiiiii/Lite` | `main` | `a4fc0bf349c8eb9f26facc4044e3098ba32302bf` |
| `nuomiiiii/Lite-web` | `main` | `9df343c0be525fbf48b5d4069f9ff5d49f7f9394` |
| `nuomiiiii/Lite-agent` | `main` | `8c4331a747b19b6c3790aeec6cb3fd3f741a862e` |

These are reference revisions, not a claim that this fork has merged every
change in all six repositories. Keep the local Connect protocol, capability
checks, fresh 2FA and protocol gitlinks when adapting upstream features.

Official Agent `server/files.go` still ignores local-protocol overwrite and
recursive flags and replaces symlinks on save. Lite Agent
`terminal/file_manager.go` provides explicit recursive confirmation,
non-overwrite checks and regular-file upload validation. The local adaptation
keeps the official stream architecture and adds those safeguards. Copies are
staged before publication; failed Windows replacement no longer deletes the
old target. Unix saves preserve mode/ownership. ACLs, xattrs and hard-link inode
identity are not preserved by atomic replacement; do not use the editor for
files requiring those properties. Non-overwriting directory moves fail safely
on Unix platforms other than Linux until a platform-specific primitive exists.

Lite Web `src/utils/clipboard.ts` and its tests are ported from the pinned
reference: legacy copy stays inside the user gesture, followed by write/read
confirmation. An unreadable clipboard is reported as unconfirmed.
Official Web `TerminalSession.tsx` provides the reference terminal lifecycle;
the local session transport stays on Connect. OTP confirmation now starts the
session, and persisted XtermJS settings reach the workbench without reconnecting
existing sessions. GPU history uses device identity rather than model name.
Account verification fails closed on missing endpoints and never reuses its
consumed OTP for password changes. Microsoft Office preview requires per-file
consent before a preview token is issued.

Backend upload initialization now requires fresh 2FA before creating an opaque,
path-bound upload session. Individual Connect file mutations also prompt for a
fresh code. Chunk/merge requests use the initialized session; OTPs are not cached
or replayed. Bulk operations currently require separate codes and may need the
next authenticator interval. This deliberately preserves existing authorization.

The JS race was reproduced in official `pkg/jsruntime/node_test.go` too: tests
read bytes.Buffer concurrently with asynchronous console writes. The fixture is
now synchronized without removing its error/close assertions. The latest
official runtime has additionally migrated from Goja to MoeJS alpha. This audit
repair keeps the fork's current Goja runtime; the engine migration and latest WS
plugin features are **not** included and require a separate compatibility port.

CI fixes: explicitly start Vite from the repository root; enumerate generated
TypeScript files using portable find rather than assuming ripgrep exists.
Regression failures remain failures, with complete logs and browser evidence.

Deploy only a coherently pinned server/Web/Agent set after the GitHub checks
pass. These tests use synthetic transports and temporary files; they do not
replace a staging end-to-end acceptance check. Production is unchanged.
