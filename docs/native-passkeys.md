# Native passkeys

Set `KOMARI_PASSKEY_RP_ID` to the original relying party ID and `KOMARI_PASSKEY_ORIGIN` to the exact external HTTPS origin (comma-separated for multiple origins; include a non-default port, omit the trailing slash). Optional `KOMARI_PASSKEY_PANEL_ORIGIN` reserves the P3 Panel origin slot under the same RP ID. Set `KOMARI_PASSKEY_ALLOW_SYNCED=true` only when synchronized credentials are permitted. The reverse proxy must be a local/private trusted proxy that preserves the host and supplies the external HTTPS scheme. The legacy `komari-passkey` plugin is permanently retired and is rejected on startup, manual enablement, and backup restore regardless of whether native RP settings are configured. If native RP settings are incomplete or invalid, native Passkey login/management stays unavailable while non-Passkey backup factors remain usable.

## Rehearse on an isolated copy

Verify the installed package digest, the old RP and origin, and each credential's owner against an independent enrollment record. Confirm an alternative login from a fresh browser. Keep the original data, plugin configuration and counter database together under restricted access. A different RP requires re-enrollment.

Start the new binary once on the isolated database to apply the additive schema, then stop it. Configure the intended RP and run the importer against the stopped copy.

```sh
komari --database ./data/komari.db import-passkeys ./credentials-v1.json
komari --database ./data/komari.db import-passkeys ./credentials-v1.json --apply-offline
```

Preview opens SQLite read-only and reports eligible, conflicting and re-enrollment counts. Apply rolls back the entire transaction if any record requires reconciliation. Apply commits all records and the source digest together. An identical completed import reports that the source was previously applied. A changed input, revoked credential, per-user cap overflow, or conflicting credential stops the import. The importer preserves higher counters already recorded by Komari. The primary authentication database is SQLite. The metrics database is independent.

## Cut over

During a maintenance window, freeze all legacy credential writes, drain ceremonies and take a consistent backup. Repeat preview and apply on the upgraded primary database. Start with the same native RP settings. At the original origin, use an existing device to sign in without a username, complete a protected action, register a second key, list and delete it. Check password with 2FA and OAuth in fresh browser sessions. Keep the plugin stopped until the operator records the device checks, binary revision, migration counts and configuration digest.

If access fails, use an independently verified login and retain the current native database. Restoring a staged backup automatically preserves current `passkey_counters` high-water marks and `passkey_revocations` tombstones from the live primary database (including a custom `--database` SQLite path) into the restored SQLite file and publishes the restored database to the active `--database` location before opening it, preventing counter rollback or resurrection of explicitly deleted passkeys while clearing any legacy `komari-passkey` plugin enablement or approvals from the restored archive.

## Session-cookie upgrade

After upgrading to the host-prefixed session cookie, sign in again using the normal HTTPS panel origin. Login and logout clear the old cookie. On startup, the primary database discards legacy plaintext session rows and retains versioned session digests. Use a cookie's raw value only in the browser. Session-management APIs accept the stored identifier returned by the session list. OAuth callbacks use top-level GET navigation with SameSite=Lax, including when binding an external identity to a signed-in account.
