# Interactive workspace authorization

Only a real administrator cookie session can unlock a workspace via the
explicit Connect WorkspaceService. Fresh TOTP is consumed once to mint an
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
logout/session expiry is rechecked by the server sweeper (at most one second).

Account changes revoke the previous grant for this session and machine. No
workspace token is an authorization for account settings, general execution,
other machines, or other RPC2 methods. No deployment or E2E is part of these PRs.
