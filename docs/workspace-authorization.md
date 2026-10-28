# Interactive workspace authorization

Only a real administrator cookie session can unlock a workspace via the
explicit Connect WorkspaceService. A fresh trusted-credential authorization is consumed once to mint an
in-memory grant bound to that session, user, machine and selected OS username.
API keys, Agents, plugin system calls and account-page verification cannot
unlock it. RPC2's compatibility allowlist is unchanged.

The absolute deadline is 30 minutes. The idle deadline is 3 minutes after the
last foreground activity reported by the UI. Status requests, terminal output,
acknowledgements and file transfers do not renew either deadline. Touch refuses
already expired grants and cannot extend the absolute deadline. Like every
browser inactivity lock, foreground activity is a UI signal, not proof of a
physical human; the server independently enforces the deadlines.

All terminal methods and file operations require the grant. Terminal sessions
are bound to it and queued input is discarded on lock. File assignments include
the exact account and a distinct workspace ID for the Agent's isolated worker.
Old Agents fail the identity-version handshake. Legacy WebSocket terminal and
embedded terminal file commands are disabled.

For image/video/download links, a HttpOnly, SameSite=Strict cookie is scoped to
that machine's `/api/admin/client/<id>/file` data plane. It still requires the
original login session and the same grant. Upload sessions bind to the grant;
resuming them under another account or after server restart is refused. Office
preview tokens retain their exact-file scope and become invalid as soon as the
underlying workspace locks. Ongoing file calls/streams are cancelled on lock;
logout/session expiry and remote-control capability revocation are rechecked
by the server sweeper (at most one second). Normal terminal closure removes its
grant callback so closed output buffers are not retained until grant expiry.

Account changes revoke the previous grant for this session and machine. No
workspace token is an authorization for account settings, general execution,
other machines, or other RPC2 methods.

## Machine-local lifecycle

Each machine has independent activity, cancellation and prompt state. Expiry
silently locks its surfaces without opening a credential dialog. A visible
surface has an explicit unlock button; Escape/cancel affects only that machine
and never drains a queue of background prompts. There is no global bottom-right
workspace account/lock control and no whole-page inert overlay.

Only trusted events on a visible, focused machine surface renew its idle
deadline, at most once per ten seconds per window/machine. Background output,
presence announcements and status requests never renew it. A stalled SJC
renewal cannot hold FRA's renewal state. Renewals update existing session grant
references and cannot resurrect a locally canceled grant.

Terminal and file surfaces for the same machine share a grant. Separate
same-origin browser windows use BroadcastChannel for in-memory grant/renewal
and consumer presence; tokens are not persisted in browser storage. Every
request still checks the browser login, machine, OS account and server-side
deadline. Peer presence is announced every 15 seconds locally, expires after
45 seconds, and creates no network polling. A delayed lock carries the old
token and cannot lock a newer grant. Revoked tokens are rejected on late replay.

Locking disables input and cancels server/Agent work, while retaining the Xterm
renderer and the file browser's current view. Reverification within the absolute
window restores the **screen**, not the old privileged shell: revocation closes
that PTY and unlock starts a new shell. Running processes and current shell
directory are not guaranteed to resume. Old terminal text is cleared at its
screen deadline or when the selected OS account changes. Renderer disposal
happens when the surface closes. No original-authority extension is implied.

The final local consumer starts a 60-second cleanup grace; reopening cancels
it. Another live window prevents revocation of its machine grant. Independently,
the server counts authenticated terminal watchers and closes an orphan PTY
after 60 seconds without a browser watcher, including broken connections. Agent
heartbeats/output do not count as browser consumers. Closed sessions release
commands, events, output buffers and grant callbacks; bounded tombstones are
separate from the live-session limit.

## Rescue delivery and read-path changes

The rescue helper uses the existing DPoP-authenticated privileged-delivery
stream, opting in with `X-Komari-Rescue-Watch: v1`. Committed per-machine changes
wake the stream immediately. Fresh signed heartbeats are sent every second;
the opening access credential is revalidated on each heartbeat. Ordinary
privileged watches retain a 20-second reconciliation fallback. No new RPC or
unsigned authorization cache is introduced.

The helper verifies the existing signature, schema, machine and configuration
binding, and then requires increasing issue times, non-regressing sequences
and bounded freshness. A repeated state may be a fresh signed heartbeat; a
replayed signed payload may not. EOF, malformed/replayed authorization, local
policy denial or two seconds without a sufficiently fresh signed frame revokes
the temporary SSH lease. The watchdog checks every 100 ms; scheduler delays
can add to that bound. Local policy is still checked every 500 ms. Reconnects
back off from 500 ms to 30 seconds and reload identity credentials; startup
never restores authority from a status file. Status-file publication is reduced
to every two seconds, within its existing ten-second stale limit.

Stable privileged-state reads perform SQL reads without initialization INSERTs
or a write transaction. A cursor reread detects concurrent state changes;
missing-state initialization and expired approvals retain transactional handling.
Login/policy validation no longer holds the global workspace state mutex.
Already-authenticated terminal output uses local liveness, with the existing
one-second revalidation sweep; new commands still perform full authorization.
Database waiting can still delay authentication and sweeps, so this is not an
availability or wall-clock revocation guarantee during a blocked database.

## Integration and rollout

The backend PR pins Web through its submodule and Agent through
`scripts/ci/workspace-agent-revision.txt`. Companion commits are on the shared
`fix/workspace-lifecycle-performance` integration ref. The Agent pin makes CI
reviewable; merging the backend does **not** merge or publish Agent main.

Deploy the updated backend before upgrading the Agent/rescue helper. A new
helper against an old backend without heartbeat acknowledgement remains denied;
this is deliberate fail-closed incompatibility, not transparent backward
compatibility. Hosts and panel need synchronized clocks: frames older than two
seconds or over one second in the future are refused. Network buffering/stalls
can disable rescue until a fresh valid stream recovers. Existing host policy,
PAM authentication, ceilings and pending approval requirements remain in force.

All validation is hosted CI. No production deployment, load test, local build
or browser E2E is part of this PR. See
[diagnostic evidence and limitations](workspace-diagnostics.md).
