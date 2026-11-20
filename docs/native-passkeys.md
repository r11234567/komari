# Native passkeys

Set `KOMARI_PASSKEY_RP_ID` to the original relying party ID and `KOMARI_PASSKEY_ORIGIN` to the exact external HTTPS origin (include a non-default port, omit the trailing slash). Set `KOMARI_PASSKEY_ALLOW_SYNCED=true` only when synchronized credentials are permitted. The reverse proxy must preserve the host and supply the external HTTPS scheme. Selecting native mode prevents the legacy komari-passkey package from loading, including after a plugin backup restore.

## Rehearse on an isolated copy

Verify the installed package digest, the old RP and origin, and each credential's owner against an independent enrollment record. Confirm an alternative login from a fresh browser. Keep the original data, plugin configuration and counter database together under restricted access. A different RP requires re-enrollment.

Start the new binary once on the isolated database to apply the additive schema, then stop it. Configure the intended RP and run the importer against the stopped copy.

```sh
komari --database ./data/komari.db import-passkeys ./credentials-v1.json
komari --database ./data/komari.db import-passkeys ./credentials-v1.json --apply-offline
```

Preview opens SQLite read-only and reports eligible, conflicting and re-enrollment counts. Apply rolls back the entire transaction if any record requires reconciliation. Apply commits all records and the source digest together. An identical completed import reports that the source was previously applied. A changed input or conflicting credential stops the import. The importer preserves higher counters already recorded by Komari. The primary authentication database is SQLite. The metrics database is independent.

## Cut over

During a maintenance window, freeze all legacy credential writes, drain ceremonies and take a consistent backup. Repeat preview and apply on the upgraded primary database. Start with the same native RP settings. At the original origin, use an existing device to sign in without a username, complete a protected action, register a second key, list and delete it. Check password with 2FA and OAuth in fresh browser sessions. Keep the plugin stopped until the operator records the device checks, binary revision, migration counts and configuration digest.

If access fails, use an independently verified login and retain the current native database. Preserve counter high-water marks and revocations before any restoration. Turning the native settings off selects legacy mode and can reactivate the old store. A pre-cutover snapshot alone cannot safely roll back post-cutover credential changes.
