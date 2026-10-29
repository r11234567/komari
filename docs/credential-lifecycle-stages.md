# Credential lifecycle staged delivery

One ongoing PR per repository. No local dependency installation, build, test or E2E on the panel VPS. Every stage must pass GitHub Actions before dependent implementation starts. No release or production deployment is part of this change.

| Stage | Backend | Agent | Web | Gate |
| --- | --- | --- | --- | --- |
| 1: transactional rotation, generations, bounded recovery | CI passed: 30 checks at 7d9ffbd2 | Not started | Not started | Race-enabled real SQLite transactions, actual signed Connect requests, legacy compatibility, repeated concurrent writers and deliberately broken recovery negative control |
| 2: proactive renewal and rescue handoff | Pinned real Agent integration added; awaiting CI | Agent PR in progress | Not required | Actual persistence, lost responses, restart recovery, renewal concurrency and rescue handoff |
| 3: idle and absolute deadlines, migration, admin visibility | Not started | Depends on stage 2 | Not started | Deadline boundaries, migration, reapproval, access controls and component assertions |

## Stage 1 protocol

Existing AT lifetime remains one hour and the legacy RT expiry policy is unchanged in stage 1. Stage 3 introduces idle and non-sliding absolute deadlines after compatibility is proven.

V2 uses the existing signed refresh envelope and JSON payload with protocol=2, agent_id, refresh_token, UUID request_id, optional bootstrap generation, timestamp and a fresh UUID nonce for every network attempt. The same persisted request_id is reused after a lost response. A successful response signs protocol, agent_id, request_id, generation and SHA-256 of the deterministic complete AgentCredentials protobuf. No protobuf schema change is required.

A database transaction compares the current generation and RT hash before updating. Repeated same-request attempts return the same encrypted response for 60 seconds, not another credential rotation. Conflicting requests within that window fail without revoking the machine. Authenticated reuse of a consumed RT beyond recovery revokes the chain; invalid proofs cannot revoke it. V1 compatibility recovers the same pair for its existing ten-minute window and cannot downgrade a V2 machine.

The previous AT remains valid only until the earlier of its original expiry and 60 seconds after rotation. Revocation rejects both generations immediately. Recovery payload is AES-GCM encrypted with the panel secret. Consumed tokens are represented only by hashes. Stage 3 must expire stored recovery ciphertext and prune exhausted history.

CI retains raw go-test JSON, readable output, build stderr, command context, exit statuses and test-evidence manifests for 30 days. A negative control disables recovery and must fail the intended runtime assertion; compilation failure or skipped tests are not acceptable evidence.
