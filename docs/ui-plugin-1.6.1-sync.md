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
