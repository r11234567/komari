# Credential lifecycle staged delivery

One ongoing PR per repository. No local dependency installation, build, test or E2E on the panel VPS. Every stage must pass GitHub Actions before dependent implementation starts. No release or production deployment is part of this change.

| Stage | Backend | Agent | Web | Gate |
| --- | --- | --- | --- | --- |
| 1: transactional rotation, generations, bounded recovery | CI passed: 30 checks at 7d9ffbd2 | Not started | Not started | Race-enabled real SQLite transactions, actual signed Connect requests, legacy compatibility, repeated concurrent writers and deliberately broken recovery negative control |
| 2: proactive renewal and rescue handoff | CI passed: backend 31 checks at d26e6170 | CI passed: Agent 30 checks at 35e44b7 | Not required | Actual persistence, lost responses, restart recovery, renewal concurrency and rescue handoff |
| 3: idle and absolute deadlines, migration, admin visibility | Dedicated CI passed; final pinned integration running | Dedicated CI passed at 78010f6 | CI passed at 8e7bed7 | Deadline boundaries, migration, reapproval, access controls and component assertions |

## Stage 1 protocol

Existing AT lifetime remains one hour and the legacy RT expiry policy is unchanged in stage 1. Stage 3 introduces idle and non-sliding absolute deadlines after compatibility is proven.

V2 uses the existing signed refresh envelope and JSON payload with protocol=2, agent_id, refresh_token, UUID request_id, optional bootstrap generation, timestamp and a fresh UUID nonce for every network attempt. The same persisted request_id is reused after a lost response. A successful response signs protocol, agent_id, request_id, generation and SHA-256 of the deterministic complete AgentCredentials protobuf. No protobuf schema change is required.

A database transaction compares the current generation and RT hash before updating. Repeated same-request attempts return the same encrypted response for 60 seconds, not another credential rotation. Conflicting requests within that window fail without revoking the machine. Authenticated reuse of a consumed RT beyond recovery revokes the chain; invalid proofs cannot revoke it. V1 compatibility recovers the same pair for its existing ten-minute window and cannot downgrade a V2 machine.

The previous AT remains valid only until the earlier of its original expiry and 60 seconds after rotation. Revocation rejects both generations immediately. Recovery payload is AES-GCM encrypted with the panel secret. Consumed tokens are represented only by hashes. Stage 3 must expire stored recovery ciphertext and prune exhausted history.

CI retains raw go-test JSON, readable output, build stderr, command context, exit statuses and test-evidence manifests for 30 days. A negative control disables recovery and must fail the intended runtime assertion; compilation failure or skipped tests are not acceptable evidence.

## Stage 3 policy and operations

Defaults: seven days idle refresh validity, 180 days fixed human authorization, a once-only 30-day existing-node migration grace, and fourteen days advance UI warning. Idle may be 1–30 days; absolute authorization 30–365; migration grace 1–90; warning 1–30. Policy updates affect future issuance/refreshes, never extend a stored absolute deadline or repeat migration. Revoked and already-expired RTs are not resurrected by migration. ATs cannot outlive the absolute deadline.

An interactive administrator must complete a fresh, single-use, resource-bound trusted-credential challenge for policy changes or explicit reapproval. Reapproval compares current generation and key fingerprint, refuses revoked identities and in-flight recovery, records the actor and renews the human-approved deadline without changing machine keys/tokens. Restart the Agent after manual reapproval to immediately retry; a lost RT or revoked identity needs fresh device enrollment. No API-key/plugin approval path exists.

Recovery ciphertext is cleared by a minute-based, cancellable local database maintenance task; consumed RT hashes remain through their chain's fixed deadline for authenticated reuse detection, then expire. There is no additional Agent network polling for lifecycle administration. The UI uses manual refresh and paginated status reads and displays no tokens, token hashes, public keys, encrypted responses or private keys.

## Final composition gate

The backend pins Agent 78010f6c7a392e31f5d47e16d815cf6f38cabe10 for both actual renewal integration and workspace/rescue regression checks, and Web 8e7bed7 for the shipped UI. Its exact-Web gate also requires the new credential-lifecycle workflow. The Agent final-horizon test proves zero early exchanges when the AT and RT share the fixed deadline, while ordinary short ATs still renew proactively. Permanent denial stops the proactive worker and reactive consumers share a 15-minute cooldown.

Web administration remains independently reachable after the login-settings account verification even if the OAuth provider catalogue is unavailable. No production deployment, merge or release is included.
