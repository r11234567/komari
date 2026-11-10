# Komari 的 Nginx 与 CrowdSec 新配置

此目录包含可独立部署的 Nginx 与 CrowdSec 配置。根目录现场快照中的 `live/` 和 `reference/` 保留作迁移参考。

## 分工

- Komari 按已验证的探针、API Token、账号或来源地址限流，超额返回 HTTP 429 和 `Retry-After`。限流开关控制请求预算，既有查询合并和并发上限继续生效。
- CrowdSec 的 HTTP 场景处理未知路径扫描和 Web 攻击。已登记 RPC 的认证、授权及业务错误交给 Komari，避免把正常 400、401、403 和 429 累计成扫描封禁。
- 已有 IP 决策在 Nginx 认证前执行。因此精确列出的机器控制接口绕过此 bouncer 的访问阶段处理，后端继续验证凭据、DPoP、授权和请求预算。这个名单包含凭据刷新、公钥获取及 Agent 文件传输。其他虚拟主机、网页、未知 RPC 和新节点注册继续经过 bouncer。
- 名单从后端权限策略生成，覆盖当前声明的客户端 RPC。请求头、User-Agent、IP 前缀和任意 `/api/`、`/komari.*` 前缀均不能扩展名单。若另行使用 AppSec，机器接口的规则需要单独验证。
- 本机豁免只使用 Nginx 恢复真实来源后的环回地址。配置中没有任何公网 IP 或整个家庭 IPv6 前缀的永久白名单。

## 后端预算

| 请求类型 | 持续速率 | 突发额度 |
| --- | --- | --- |
| 密码登录入口，每来源地址 | 0.2 次/秒 | 5 |
| 探针上报，每节点 | 20 次/秒 | 40 |
| 探针网络探测，每节点 | 100 次/秒 | 400 |
| 探针控制请求，每节点 | 30 次/秒 | 100 |
| 探针新建流，每节点 | 20 次/秒 | 80 |
| 网页新建流，每主体或来源地址 | 5 次/秒 | 20 |
| 历史读取，每账号/API Token/匿名来源地址 | 2 次/秒 | 5 |
| 其他读取，已验证主体 | 40 次/秒 | 160 |
| 其他读取，匿名来源地址 | 10 次/秒 | 60 |

每条流仅在打开时扣额。旧主题 `/api/rpc2` 的五个历史方法另按消息计额，批量请求和 WebSocket 也逐条扣额。其业务过载沿用 JSON-RPC 错误响应并提供 `retry_after_seconds`，保留传输兼容性。上报、探测和控制额度分开，多个探针共享 NAT 时仍各自计额。API Token 使用 Token ID，轮换地址不会刷新同一 Token 的预算。登录入口始终按来源计额。

开启请求限流后，历史 HTTP 请求每主体最多同时 2 个，全局最多 `clamp(2 × GOMAXPROCS, 2, 8)` 个。额度耗尽立即返回 429，等待时间由 `Retry-After` 给出。

原生指标和管理仪表盘的物理读取另有独立保护，关闭请求限流后也继续生效。近期与长区间查询使用分开的槽位，`GOMAXPROCS=1` 时各 1 个，近期最多 4 个、长区间最多 2 个加权槽位。满载立即返回 429，不在服务器排队。原生指标读取期限为 15 秒，耗时达到 2 秒的读取结束后，在相同通道退避 2–15 秒。成功结果及 Ping 汇总最多缓存 5 秒并合并相同请求，缓存总量不超过 32 MiB。流量趋势保留自身 8 秒期限与读取保护。管理仪表盘内部模块仍使用原有的最多 30 秒共享加载期限，以保留已开始的合并工作。

前端每页面最多执行一个历史请求，启动间隔至少 1 秒，等待队列最多 32 个、最长 30 秒。429/503 按 `Retry-After` 暂停整个队列，最多重试一次。超时退避至少 10 秒并终止该请求。切换查询会取消旧请求，隐藏页面会取消图表读取，登录和终端输入独立进行。

11 月 7–8 日证据中历史请求的单秒峰值分别为 8 和 23，第二次包含 590 个 499 和 53 个 429。11 月 9 日第三次尖峰没有历史请求。本轮修改限制读取放大和重试压力，数据库压缩、保留期清理、备份和新存储引擎仍需各自验证。

## 部署

1. 使用包含本轮修改的配套后端与 Agent。在站点设置开启请求限流。认证本身的防滥用限制独立存在。
2. 将 `nginx/` 中的文件放入 `/etc/nginx/komari/`。修改 `http.conf` 的 `panel.example.com` 和 `komari_backend`，按实际域名和 Docker 后端地址填写。
3. 在 Nginx `http {}` 中引入 `/etc/nginx/komari/http.conf`。用本目录的 `crowdsec.conf` 替换旧全局 Lua hook，保留原私密 bouncer 配置及 API key，使用官方 `stream` 模式。
4. 在每一个承载 Komari 的 TLS server 或可信 Xray fallback 中引入 `/etc/nginx/komari/server.inc`。删除同一 server 的旧 `location /`、双向流 location 和指标限流 include，避免重复定义与旧的按 IP 限流继续继承。35 分钟代理超时配合后端授权期限，双向流使用 HTTP/2 上游。
5. Xray fallback 继续使用只绑定环回的 `proxy_protocol` 监听，只信任 Xray 的实际连接地址，通过 `real_ip_header proxy_protocol` 恢复来源。普通 TLS 直连使用 socket 来源。Nginx 向后端覆盖 `X-Forwarded-For`，不拼接外部提供的链。
6. Docker 面板设置 `KOMARI_TRUSTED_PROXIES` 为 Nginx 连接到面板时的实际来源 IP，例如固定 Docker 网关地址。只填写实际代理，直连填写 `none`。
7. 安装官方 `crowdsecurity/nginx` collection 和 `crowdsecurity/syslog-logs` parser，确保原始日志归一化阶段存在。将两份 parser YAML 放入 `/etc/crowdsec/parsers/s02-enrich/`。用新的采集文件替换旧的重复采集项，确保 Komari 访问日志只读一次。其他站点使用独立日志采集项，SSH journal 与 auth.log 二选一。
8. 从 Komari 的采集项移除 `/var/log/nginx/*.log` 和 error.log。旧 `local-whitelist.yaml` 对应的宽泛身份/IP 白名单可以移除。本方案通过专用访问日志判断限流，Nginx 的请求限流 error 日志不再成为该站点的封禁依据。
9. 执行 `nginx -t`、`crowdsec -t` 和 `cscli explain --file /var/log/nginx/komari-access.log --type nginx`。核对一个 Agent 请求、一个 429 和未知路径扫描的解析结果，再重载服务。
10. 检查旧决策并逐条删除已确认的误封。有效期内的旧封禁仍可能影响网页访问。先保留独立管理连接，验证后再结束切换。

配置中的 `X-Forwarded-Proto https` 用于 TLS server 或已由 Xray 终止 TLS 的可信本地入口。双向流可验证 `AttachSession`、`AttachFileOperations`、`UploadMetrics` 和 `StreamMetrics`。原有已登录连接的恢复窗口仍由后端和 Agent 决定。

## 简单验收

- 两个共享出口的 Agent 同时上报、恢复连接，互不消耗对方预算。
- 对测试账号/Token 发出超过预算的请求，检查 429 与 `Retry-After`，等待后可继续使用。
- 临时给测试来源设置 CrowdSec 决策，网页应被拦截。带有效凭据的已登记机器 RPC 仍可到达后端，错误凭据由后端拒绝，未知 RPC 仍被 bouncer 拦截。
- 用请求头伪造 `X-Komari-Principal` 或 `X-Forwarded-For`，确认不能改变来源身份或名单。
- 对专用日志执行解析，确认 RPC 和 429 获豁免，未知路径扫描仍进入标准 HTTP 场景。
- 检查 `cscli metrics` 的实际采集文件和解析计数，排除同一日志的重复采集。

官方说明见 [Nginx bouncer](https://docs.crowdsec.net/u/bouncers/nginx/)、[Nginx 请求限流](https://nginx.org/en/docs/http/ngx_http_limit_req_module.html) 和 [Nginx 日志解析器](https://github.com/crowdsecurity/hub/blob/master/parsers/s01-parse/crowdsecurity/nginx-logs.yaml)。

## 本地验证记录

2026-11-10 使用真实 Nginx 与测试专用模拟 bouncer 验证 7 项路由、来源覆盖和模拟封禁场景。使用官方 CrowdSec v1.8.2、本地测试 API 及官方 Nginx collection 验证 7 类日志。`verify/crowdsec.lua` 仅供测试容器使用，部署使用官方 bouncer。

```bash
cscli explain --file deploy/crowdsec/verify/parser-fixtures.log --type nginx --verbose > /tmp/komari-explain.txt
python3 deploy/crowdsec/verify/parser_checks.py /tmp/komari-explain.txt
python3 deploy/crowdsec/build-rpc-map.py
```

新后端增加或调整客户端 RPC 后重新生成名单，审查差异并执行 `nginx -t` 后重载。验证环境没有接入生产 LAPI、真实探针或故障节点数据库。真实 Xray、Nginx、Docker 的组合仍需部署验收。
