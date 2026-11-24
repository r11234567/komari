# Agent gRPC through Cloudflare DNS proxy

Use this mode for a panel hostname proxied by Cloudflare (orange-cloud DNS).

1. In Cloudflare, enable **Network → gRPC** for the zone. The proxied hostname
   must reach an HTTPS origin on port 443 that advertises HTTP/2 over TLS ALPN.
   Set SSL/TLS to **Full (strict)** with a valid origin certificate. See
   [Cloudflare's gRPC requirements](https://developers.cloudflare.com/network/grpc-connections/).
2. With Nginx at the origin, include
   [the workbench locations](../deploy/nginx/workbench-locations.conf) in the
   HTTPS virtual host. Enable HTTP/2 on that listener. The include forwards the
   duplex endpoints with `grpc_pass` to Komari's HTTP/2 cleartext (h2c) listener.
   Run `nginx -t` before reloading. See [Workbench through Nginx](workbench-nginx.md)
   for complete upstream configuration.
3. Update the panel, its bundled Web UI and the Agent. In the panel, open
   **Settings → General → Network access → Cloudflare proxy for Agents**
   (`/admin/settings/general`), enable it, and restart the updated Agent.
   Start with one Agent and confirm terminal input,
   terminal output and a directory listing before restarting the rest.
4. To restore Connect duplex, turn the setting off and restart the affected
   Agents. The same enrollment credentials and service configuration are used.

## Protocol selection and routing

The panel advertises the selected transport on the authenticated
`ConfigService/GetDesiredConfig` response. The Agent reads it at startup and
during its regular configuration sync, then uses it on the next attachment.
The default is Connect. With the setting enabled, updated Agents initiate native
gRPC (`application/grpc` or `application/grpc+proto`) for
`WebSSHService/AttachSession` and `FileService/AttachFileOperations`.
Keep `X-Komari-Agent-Duplex-Protocol` on the config response through all proxies.
The two wire protocols use the same procedure URLs and backend handlers,
including the same Agent authentication and workspace grants. Nginx routes by
procedure path, and the backend decodes the protocol by request content type.
Browser RPCs, Agent enrollment and renewal, lease streams and regular metrics
reporting continue to use Connect. Cloudflare HTTP/3 on the edge does not change
the origin's HTTP/2 requirement.

## Upload limits and timeouts

- Cloudflare [documents a maximum upload size per incoming HTTP request](https://developers.cloudflare.com/support/troubleshooting/http-status-codes/4xx-client-error/error-413/).
  Free and Pro zones have a 100 MB maximum, Business zones have 200 MB, and a
  zone can be configured with a lower cap. This is a request-body limit. Do not
  assume each gRPC message gets a new allowance. Cloudflare's gRPC documentation
  does not specify how it accounts for an indefinite stream. A 413 can also come
  from Nginx. The workbench attach locations set `client_max_body_size 4m`.
  File bytes on the Agent path use separate HTTP transfer requests and follow
  the body limit on their own locations.
- The workbench Nginx include already sets `client_body_timeout`,
  `grpc_read_timeout` and `grpc_send_timeout` to 35 minutes on these duplex
  procedures. These cover inactivity between operations, not the maximum
  workspace lifetime. Keep the timeouts scoped to the duplex routes. A larger
  Nginx timeout does not increase the Cloudflare edge limits. See
  [Nginx's gRPC module](https://nginx.org/en/docs/http/ngx_http_grpc_module.html)
  and [Cloudflare's connection limits](https://developers.cloudflare.com/fundamentals/reference/connection-limits/).

If a browser sees the session but the Agent never attaches, check the origin
access log for the two attach paths, HTTP/2 and the request content type. A
Cloudflare 403 on a native gRPC request may indicate that gRPC is disabled for
the zone.
