# Workbench through Nginx

Both `WebSSHService/AttachSession` and `FileService/AttachFileOperations`
require bidirectional HTTP/2 between Agent, Nginx and Komari. Enabling HTTP/2
only on the public listener does not upgrade an HTTP/1.1 `proxy_pass` upstream.
Missing the FileService route can leave directory requests waiting until their
deadline, even while the terminal can connect.

Define this upstream in Nginx's `http` block, adjusting the address:

```nginx
upstream komari_backend {
    server 127.0.0.1:25774;
}
```

Include `deploy/nginx/workbench-locations.conf` inside the existing TLS server
block, before broader regex locations. Enable HTTP/2 on that public listener
using the syntax supported by the installed Nginx version. `grpc_pass` supplies
HTTP/2 to Komari's h2c listener for Connect framing as well as gRPC framing.
The two service families have explicit routes; RPC2/plugin gateway rules stay
in their existing locations. Keep existing certificate and security headers.

Apply the locations to **every virtual host/ingress** serving the panel (for
example TLS, fallback and QUIC listeners), following enabled symlinks to their
actual configuration files. Fixing only one ingress leaves Agents or browsers
using another ingress on the old HTTP/1.1 route.

A short global `client_body_timeout` also applies to the open Agent streaming
request. For example, a global 15s timeout disconnects a quiet terminal with
HTTP 408 every 15s even when `grpc_read_timeout` is 2h. The duplex location
therefore overrides the body timeout. The application's 30-minute absolute
and 3-minute idle workspace authorization remains authoritative and unchanged.

Back up the configuration, run `sudo nginx -t`, then `sudo systemctl reload nginx`.
Reconnect existing workspace tabs after reloading. The application also sends
`X-Accel-Buffering: no` for Connect streams, but response headers cannot fix
an upstream route which still uses HTTP/1.1 for a bidirectional request.

The workspace CI checkpoint runs a small file lease/attach/result exchange
through the checked-in Nginx locations on a disposable GitHub runner. It also
keeps a duplex request idle beyond a deliberately short inherited body timeout. It uses
synthetic grants and a simulated Agent response, without a browser E2E, and
retains the full Go and Nginx logs for 30 days.

## Signed rescue authorization stream

The same include now has an **exact** route for
`/komari.config.v1.PrivilegedDeliveryService/WatchPrivilegedDelivery`.
It disables request/response buffering and explicitly sets 35-minute upstream
read/write and downstream send timeouts. Apply this include to each panel
ingress; a broader `location /` with its own defaults is not sufficient when
another ingress still inherits a short timeout or buffers responses. Other
privileged unary methods and RPC2/plugin gateway routes are unaffected.

Timeouts have different meanings:

| Layer | Bound | Meaning |
| --- | --- | --- |
| Rescue helper HTTP client | No whole-response timeout | A healthy stream must not be killed after 30 seconds |
| Helper connection / TLS handshake | 5 seconds each | Unreachable peers cannot stall connection setup |
| Helper response headers | 10 seconds | Header establishment remains bounded |
| Helper watch context | 25 minutes | Rotate the connection and reread protected credentials |
| Backend stream policy | 30 minutes | Server-side maximum stream lifetime |
| Nginx stream location | 35 minutes | Inactivity limits between reads/writes, not total connection lifetime |
| Helper signed heartbeat | 1-second cadence, 2-second freshness | Lost/invalid/stale heartbeats close authorization independently of HTTP timeouts |

HTTP client `Timeout` covers the **entire response body**, whereas Nginx
`proxy_read_timeout` measures the gap between upstream reads. Increasing Nginx
timeouts cannot fix the helper's former 30-second total-response deadline.
Nginx `keepalive_timeout` applies to idle connections between requests, not an
active stream. Ordinary unary RPCs retain their existing deadlines.

The CI checkpoint keeps the real backend handler and policy interceptor open
through real Nginx for over 32 seconds, checks fresh signed frames on one
request, then revokes its isolated fixture credential and requires rejection.
It inherits a deliberately short 500ms read timeout; a negative control removes
the exact route and must fail with a real upstream timeout. The Agent checkpoint
also runs its production DPoP client for over 32 seconds and restores the old
30-second timeout as a separate expected-failure control. Full failure output
and Nginx logs are retained, without running browser E2E or tests on the VPS.
