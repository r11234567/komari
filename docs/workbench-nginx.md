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

Back up the configuration, run `sudo nginx -t`, then `sudo systemctl reload nginx`.
Reconnect existing workspace tabs after reloading. The application also sends
`X-Accel-Buffering: no` for Connect streams, but response headers cannot fix
an upstream route which still uses HTTP/1.1 for a bidirectional request.

The workspace CI checkpoint runs a small file lease/attach/result exchange
through the checked-in Nginx locations on a disposable GitHub runner. It uses
synthetic grants and a simulated Agent response, without a browser E2E, and
retains the full Go and Nginx logs for 30 days.
