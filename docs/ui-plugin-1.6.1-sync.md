# Komari 1.6.1 control panel synchronization

Reference backend: `96cf0f79d8cfdf6cc8be54fd1976bbc026d58f7f` (tag 1.6.1).
The existing MoeJS alpha.9 engine, mutable per-plugin intrinsics and owned
WebSocket routes already match this release.

## Plugin management boundary

`internal/plugingateway` is shared by the existing internal `admin:*Plugin*`
RPC methods and the typed Connect PluginService. The RPC names, parameter and
result shapes, permission-approval hashes and configuration reload behavior
remain compatible with existing plugins. The gateway requires an admin principal
and honors request cancellation before performing side effects.

The public `/api/rpc2` compatibility allowlist is unchanged. Plugin-owned methods
retain their registered ACL; plugin markers cannot expose reserved namespaces.
The panel uses Connect rather than upstream's browser-side admin RPC2 calls.
Internal plugin system calls still require `allowSystemRPC` and cannot acquire
an interactive account/workspace grant. No workspace or account authorization
is implied by this gateway.

Validation runs on GitHub Actions using the plugin subsystem checkpoint, with
complete JSON/text/compiler logs retained for 30 days; no local build or E2E.

## Login settings step-up

The Account → Login route obtains its own 30-minute, session-bound grant after
consuming fresh 2FA. The HttpOnly/Secure/Strict cookie is restricted to settings
paths. Login settings (API key, password-login switch and OAuth settings) are
redacted from general settings reads until unlocked. Writes and OIDC reads are
checked inside the RPC handlers too, so an internal plugin/API key cannot use
an admin role to bypass interactive 2FA. Other settings keep their current APIs.
The grant expires on logout/session revocation or 2FA secret changes and cannot
unlock the workspace. Accounts without 2FA must enable it on their profile first.

## Archive uploads and market categories

The official 5 MiB chunk protocol supports plugin/theme/backup uploads, preserving
legacy upload endpoints. Every new upload is bound to a live interactive admin
session. Quotas are two uploads per session, four globally, 512 MiB total staged;
per-archive limits are 100 MiB for plugins, 128 MiB for themes and 256 MiB for
backups on this low-resource fork (legacy backup restore retains its limit).
Chunks stream to disk; exact sizes and indices are validated, duplicate chunks
replace atomically, and merge/finalize/cancel are serialized. Sessions expire
absolutely after 30 minutes, cleanup runs every minute and restart discards orphan
staging directories. Finalizers reuse existing archive validation and permission
approval. The old plugin upload now streams to disk with a 100 MiB limit too.
Market catalogs retain upstream's optional `category` field.
