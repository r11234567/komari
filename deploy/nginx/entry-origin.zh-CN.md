# 单域认证入口与可信代理

部署前在 Komari 容器/服务中设置唯一的外部认证 Origin，例如
`KOMARI_PUBLIC_ORIGIN=https://monitor.example.com`。只接受 scheme、host 和可选端口，
不带路径、账号、查询参数。入网批准链接固定为此 Origin 的 `/bind`。
已有部署升级时必须补此配置，否则新入网明确返回配置错误；既有凭据与数据库不迁移。

`KOMARI_TRUSTED_PROXIES` 默认为不信任代理。Nginx 与 Komari 同机且确实通过回环通信时
设为 `127.0.0.1,::1`；容器部署填写 Komari 实际看到的 Nginx peer 精确 IP，或受控的最小专用网段。
禁止 `0.0.0.0/0`、`::/0`，不要把整个私有网络当作可信代理，也不要让公网直连源站端口。
安装、升级、正常监听器使用相同配置；配置错误阻止启动，不能悄悄退回全信任。

Nginx 使用一个明确 `server_name`，默认 server 拒绝未知 Host。HTTP 入口只负责转到同一 hostname 的 HTTPS；
下列普通 HTTP 代理置于 HTTPS server 中，并与 `workbench-locations.conf` 的流式路由并列：

```nginx
location / {
    proxy_pass http://komari_backend;
    proxy_http_version 1.1;
    proxy_set_header Host $http_host;
    proxy_set_header X-Forwarded-Host $http_host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header Forwarded "";
    proxy_cache off;
    proxy_no_cache 1;
    proxy_cache_bypass 1;
}
```

上游头必须覆盖而非追加客户端提供的值。gRPC 路由同样用 `grpc_set_header` 覆盖；
不能只保护普通 HTTP 然后在 H2/流式路由信任原始转发头。保留 `$http_host` 中的外部端口，
Komari 再与固定 Origin 完整比较。不要用来源 IP 拼批准链接。

Cloudflare 橙云必须采用 Full (strict)，源站防火墙限制 CF/受控入口；若要还原访客 IP，
Nginx 仅对 Cloudflare 官方当前网段配置 `set_real_ip_from` 与 `real_ip_header CF-Connecting-IP`，
由 Nginx 覆盖转发给 Komari。不要让 Komari 直接信任任意请求中的 CF 或 XFF 头。
这些网段必须随运营配置维护，本模板不硬编码过期列表。

CF 与 Nginx 对 `/api/*`、`/komari.*`、`/bind*`、`/login*`、工作台/插件受保护路由以及含
Authorization/Cookie/Set-Cookie 的响应绕过缓存，关闭会改写认证协议正文的挑战/HTML 注入，
允许所需 POST/OPTIONS 与 H2。保持服务端 `Cache-Control: no-store`，不得缓存 401/403/429。
沿用同一外部 hostname，不增加子域名或端口。

CI 仅在隔离 fixture 验证真实 Nginx HTTPS、构建后的 Web 与已安装 Agent。
实际 CF 账户的规则、TLS 模式、来源网段和缓存命中仍需部署环境证据，不能以 CI 代替。

跨源调用仅允许与请求完整 Origin 相同或显式配置的完整 Origin。
`cors_allowed_origins` 与 `ws_allowed_origins` 中原有裸 hostname 或 `*` 必须改为
准确的 `https://host[:port]`；这些宽泛值不再授权跨源。单域同源访问无需新增 allowlist。
API Key 和预检的 Authorization 请求头均不能授予来源权限。允许列表应只包含可信控制台，
不能把主题 opaque Origin `null` 加入。预检先于认证，不会兑换凭据或消耗失败预算；
实际请求仍经过完整身份与权限校验。错误响应允许浏览器读取 Retry-After，禁止暴露 Set-Cookie。
