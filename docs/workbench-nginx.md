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
