# Plugin package admission with mandatory 2FA

Implemented 2026-11-03. This replaces the earlier content-approval proposal and
preserves official plugin runtime interfaces.

Direct uploads, chunked uploads and catalog installs only stage packages. No
JavaScript runs, no hooks/routes are registered, no pending static files are
served, and the approved active installation stays untouched. Catalog identity,
version and checksum are validated before retention; the backend independently
hashes the retained ZIP bytes with SHA-256.

The review shows source, version, package fingerprint, prior version, all
capabilities/runtime-limit differences, and approving user/time. Matching
versions or permissions never transfer approval to different bytes. Even a
package with no special capabilities requires approval of its executable code.

## Mandatory authorization

`PluginService/DecidePluginPackage` supports activation (including explicit
rollback), revocation and discard. Every decision requires a live human admin
and a fresh, single-use authorization from the existing trusted credential
ceremony: TOTP, native notification OTP, or the trusted Passkey adapter.
The resource is `ScopedResource("plugin-package:" + action, short, packageDigest,
version, fullPermissionsHash, expectedCurrentDigest)`. Proofs expire and bind
user/session/security state. The server rebuilds the authoritative activation
target from the actual archive before consuming a proof.

There is no raw-code, empty-proof or unconfigured-2FA success path. Legacy
`approved=true` / `permissions_approved=true` cannot grant execution. Tokens,
Agents and role-only plugin identities cannot approve packages. Re-enabling an
approved package also requires fresh 2FA; disabling reduces authority and remains
available to a human administrator without a new grant.

## Transactions and compatibility

Validation/extraction finishes before stopping the old runtime. A serialized
transition checks the expected current revision as well as the candidate.
Startup failure restores the old approved code/runtime. A durable journal
restores interrupted transitions before startup loads plugins. State writes
are atomic; Unix directory synchronization persists journal/state renames.
The reserved `.komari-package.json` installed receipt must match the persisted
approved digest on enable, restart and reload paths.

Rollback uses retained exact archive bytes and a new 2FA proof. Revocation clears
the persisted grant and unloads the runtime, hooks, injected fragments, RPC and
scheduled runtime jobs. Missing/damaged retained ZIPs do not prevent revocation.
Discard removes inactive packages; uninstall removes retained artifacts too.
Authorization attempts/completed decisions record actor, action, digest and
previous revision. A consumed proof cannot be reused after failed activation.

Official HTML injection, HTTP hooks, routes/WebSockets, notification channels
and normal runtime APIs remain. Plugin-owned foreign Bearer authentication
stays separate from Komari token restrictions. A plugin cannot use system RPC
to administer its own/another plugin's lifecycle or configuration; these
management mutations require a human identity.

Saved configuration and `__storageDir__` survive updates. Runtime writes in the
plugin directory remain allowed and do not trigger continuous reapproval. As
before, updates replace the code directory; persistent data belongs in
`__storageDir__`. Admission is not continuous filesystem-integrity enforcement.
Approved same-origin scripts remain trusted code. This is not a sandbox for an
approved malicious plugin or protection from a fully compromised panel process.

## Migration and limits

Permission-only historical approvals do not silently become package approvals.
Old installations remain stopped until their existing-directory snapshot is
reviewed and approved with 2FA; the snapshot is labeled as an existing
installation, not attributed to a newly verified download source.

Before deployment, configure an independent native TOTP or verified native OTP
contact on the old release. An account relying solely on an unapproved Passkey
plugin cannot use that stopped plugin to bootstrap its own approval. Prepare
the independent factor instead of adding a 2FA bypass. No production settings
or credentials are changed by this implementation.

Retention is limited to 100 MiB per ZIP, 64 packages / 512 MiB total retained ZIP
storage. Existing extraction limits remain: 10,000 files, 128 MiB per file,
512 MiB extracted. Traversal, duplicate paths, symlinks and reserved receipts
are rejected. Limit errors preserve the active version; discard unused packages
explicitly.

## Verification

[Focused Linux validation](https://github.com/r11234567/komari/actions/runs/41581713320)
passed race-enabled plugin/gateway/archive suites and real native-TOTP package
and policy tests. Coverage includes missing/unconfigured 2FA, altered targets,
wrong action/session, replay, same-permission updates, startup rollback,
interrupted recovery, state-write failure, missing-ZIP revocation, and staging
without execution. Final PR-head checks and embedded UI provenance are recorded
in the PR; these are separate from production validation.
