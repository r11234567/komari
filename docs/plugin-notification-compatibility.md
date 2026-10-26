# Plugin notification compatibility and OTP delivery

`server.call("admin:sendNotification", {event: {event: "Login", ...}})` is
the official plugin contract. The handler also accepts this fork's flat event
payload. Both use the same notification routes; public RPC2 still rejects the
admin namespace and plugin calls still require approved `allowSystemRPC`.

The installed komari-otp-login 1.0.0 uses `Login` for both the code and the
successful-login message. This maps to the `login` notification route and all
its selected channels. Login messages are sent immediately, outside the alert
digest. The normal notification master switch and template still apply;
disabled notifications or empty routes currently return success without
delivery. Restoring payload compatibility does not establish trusted OTP
recipients or prove successful receipt.

A dedicated OTP credential channel should be a separate, explicitly configured
delivery boundary, with its own provider credentials and fixed trusted
recipient. It should deliver immediately with a bounded deadline, return an
explicit error when unconfigured or disabled, never fall back to alert routes,
and exclude OTP bodies from logs, audit records, digest queues and history.
Ordinary login-success alerts should remain on the existing login route.
Splitting by message-text matching would be unsafe: existing plugins label
both messages identically. A future dedicated OTP API should be opt-in and
retain the legacy API for unchanged third-party plugins.

This compatibility repair changes neither the plugin script nor its login or
session creation logic. It does not grant account/workspace 2FA authorization.
CI verifies accepted payloads, invalid-input redaction, admin-only access and
console error explanations without sending real notifications or login codes.
