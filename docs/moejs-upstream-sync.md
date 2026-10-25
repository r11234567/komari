# MoeJS plugin synchronization

Source: komari-monitor/komari main at
`9e5f9a3a71daadc488e063193f383a3a464e8c48` (2026-10-25 reference).
Documentation: https://komari-document.pages.dev/dev/plugin/ . The documentation
still calls the engine Goja in its introduction; the current upstream code pins
`github.com/Calcium-Ion/moejs v0.1.0-alpha.9`.

Synchronized the runtime, native module adapters, private CommonJS loader and
event loop, plugin bindings, HTTP hooks and owned WebSocket route support.
Existing fork safeguards remain: os.Root manifest/asset confinement, permission
approval hashes, bounded system-RPC contexts, typed Connect management APIs,
and the explicit RPC2 compatibility allowlist. Plugin registration markers are
retained and removed on unload; plugins cannot register reserved system
namespaces to reopen management methods through RPC2.

Generic HTTP hooks skip workspace Connect calls, the file data plane and Agent
transfer endpoints, including attempts to redirect another hook into those
paths. Plugin system calls use a minimal internal principal with no browser
session or workspace grant. The `allowSystemRPC` permission remains necessary;
it does not become proof of fresh interactive 2FA. Custom WS routes retain their
route permission and lifecycle cleanup. Existing WS hook path matchers do not
expand the transport allowlist.

MoeJS is upstream's alpha engine. The focused Actions checkpoint covers native
module compatibility, timeouts, Promise/event-loop lifecycle, sandbox bounds,
plugin enable/unload, owned WebSocket echo/cleanup and RPC2/workspace isolation.
All dependency resolution and tests run on hosted runners; every failure keeps
complete logs. E2E and deployment remain excluded.
