# Backup restore boundaries

Restore imports Komari database files, cold telemetry, the export queue, themes,
plugins and plugin data, TLS files, notification data, and supported site assets.
Files outside the application layout remain available in the original ZIP.
Restored regular files are non-executable.

After a restore, review and approve plugins before enabling them. The archive
does not restore plugin approvals, installation receipts, or pending plugin
installation transactions. Plugin data is preserved, so upload the original
plugin package again if the package manager requests it.

Install cloudflared outside Komari's data directory. Put it on the service's
PATH or set KOMARI_CLOUDFLARED_BIN to the installed binary.
