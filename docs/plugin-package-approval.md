# Plugin permission grants and package admission

Implemented 2026-11-03. This replaces the earlier content-approval proposal and
preserves official plugin runtime interfaces.

Direct uploads, chunked uploads and catalog installs stage first installs and
permission expansions without executing code. Ordinary updates of an already
approved plugin reuse its live grant when special permissions are unchanged or
reduced; a disabled plugin stays disabled after an update. Pending packages do
not replace the approved active installation. Catalog identity,
version and checksum are validated before retention; the backend independently
hashes the retained ZIP bytes with SHA-256.

The review shows source, version, package fingerprint, prior version, all
capabilities/runtime-limit differences, and the approving/updating user/time.
Initial approval, added special capabilities and reauthorization after revocation
require fresh scoped 2FA. Package byte or version changes alone do not require
reapproval. A zero-capability first installation still requires initial approval.
Runtime compatibility settings and limits are not special permission grants.

## Mandatory authorization

`PluginService/DecidePluginPackage` supports activation (including explicit
rollback), revocation and discard. Every decision requires a live human admin.
New or expanded grants require a fresh, single-use authorization from the existing trusted credential
ceremony: TOTP, native notification OTP, or the trusted Passkey adapter.
The resource is `ScopedResource("plugin-package:" + action, short, packageDigest,
version, fullPermissionsHash, expectedCurrentDigest)`. Proofs expire and bind
user/session/security state. The server rebuilds the authoritative activation
target from the actual archive before consuming a proof.

There is no raw-code, empty-proof or unconfigured-2FA path for new grants. A
proof-free activation must be covered by the installed plugin identity’s live,
non-revoked package/permission grant, checked after rebuilding the actual
archive under the serialized transaction lock. Historical grants and grants for
other identities cannot authorize an update. Explicitly supplied proofs are
still consumed for activation and must be valid even if the current grant could cover it. Legacy
`approved=true` / `permissions_approved=true` cannot grant execution. Tokens,
Agents and role-only plugin identities cannot approve or manage plugins.
Switching an already approved plugin off/on reuses its current grant without
2FA; caller-controlled approval flags cannot grant missing permissions.
Revocation clears the grant, so enabling afterward requires approval again.
Revocation and inactive-package discard reduce authority: an interactive human
administrator can perform them without 2FA, including when no second factor is
configured. They retain revision checks and auditing; optional old proof fields
do not block them. API-token revocation and legacy-key shutdown follow the same
rule, retaining owner/session checks and the existing Origin protection.

## Transactions and compatibility

Validation/extraction finishes before stopping the old runtime. A serialized
transition checks the expected current revision as well as the candidate.
Startup failure restores the old approved code/runtime. A durable journal
restores interrupted transitions before startup loads plugins. State writes
are atomic; Unix directory synchronization persists journal/state renames.
The reserved `.komari-package.json` installed receipt must match the persisted
approved digest on enable, restart and reload paths.

Rollback uses retained exact archive bytes and follows the same live permission
grant check; a revoked package or expanded capabilities need fresh 2FA. Revocation clears
the persisted grant and unloads the runtime, hooks, injected fragments, RPC and
scheduled runtime jobs. Missing/damaged retained ZIPs do not prevent revocation.
Discard removes inactive packages; uninstall removes retained artifacts too.
Authorization attempts/completed decisions record actor, action, digest and
previous revision. A consumed proof cannot be reused after failed activation.

Official HTML injection, HTTP hooks, routes/WebSockets, notification channels
and normal runtime APIs remain. Plugin-owned foreign Bearer authentication
stays separate from Komari token restrictions. A plugin cannot use system RPC
to administer its own/another plugin's lifecycle; these management mutations
require a human identity. Existing configuration access keeps its original
capability checks and does not become an unrelated package approval prompt.

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
wrong action/session, replay, first-install/expanded-grant denial, ordinary
update and switch reuse, disabled-state preservation, startup rollback,
interrupted recovery, state-write failure, missing-ZIP revocation, and staging
without execution. Final PR-head checks and embedded UI provenance are recorded
in the PR; these are separate from production validation.

The Web UI keeps approval/history in a compact on-demand dialog with one title
and description. Source, hash, actor and permission details are collapsed, and
statuses/source descriptions/permission labels follow the current locale.
Reductions use an ordinary confirmation without a verification ceremony.
Installs needing a grant open their review dialog;
ordinary updates complete without a permission prompt. API Tokens use a compact
multi-select node dropdown and describe only the available read scopes.
