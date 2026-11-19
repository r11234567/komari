# Control-plane compromise and independent authorization

Status: architecture boundary and conditional design, not an implemented claim.

Current scoped tokens, fresh human approval, fixed workspace lifetimes, Agent
identity proofs, local user/root ceilings and isolated OS workers constrain
ordinary callers. They do not make a fully compromised panel process trustworthy:
that process controls its own authorization decisions and any signing material
it can use. An unconditional external signing service has the same problem.

If protection from that threat becomes a requirement, the Agent needs a separate
trust anchor provisioned or changed locally/out of band. An independent human
device or policy authority must verify the actual approval and issue a
node-verifiable grant bound to at least:

- Agent identity and its pinned trust anchor, OS user and locally allowed ceiling.
- Operation and scope; canonical file paths and content digests for mutations,
  or the exact privileged configuration revision/content.
- Agent-generated challenge, nonce, short expiry and revocation generation.
- For interactive terminals, an end-to-end authenticated channel binding to the
  approving client, so a compromised relay cannot inject commands after approval.

The panel can relay the request and encrypted/authenticated data, but cannot
mint grants, alter approved content, replace the enrolling trust anchor, choose a
new account, or silently authorize future nodes. The independent authority must
evaluate intent and policy itself, rather than accepting the panel's statement
that 2FA already succeeded. Approval of an interactive shell inherently grants
the selected user's command authority during that session; a content hash of
the initial start request alone does not authenticate later keystrokes.

Revocation needs an independently verifiable path and bounded offline leases.
During an outage, the node expires the grant rather than extending it based on
panel heartbeats. Clock rollback, restart persistence, replay across nodes,
re-enrollment, key rotation, user recovery and authority outage behavior need
explicit fail-closed rules and real-node tests before this model can be claimed.

Compatibility implications should be decided separately: official plugins may
retain their panel APIs, but those APIs cannot bypass independent node approval
for remote actions. Node-local roots and OS identity rules remain ceilings even
after independent approval. This requires a coordinated protocol/client/Agent
project and an enrollment/recovery migration; it is not achieved by another
panel-held signing key or by the package approval UI in this release.
