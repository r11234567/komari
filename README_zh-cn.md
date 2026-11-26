> **Fork 声明**
>
> 本仓库是 [komari-monitor/komari](https://github.com/komari-monitor/komari) 的 Fork，主要用于个人使用、部署、测试和定制。
>
> 本仓库**并非上游项目的官方仓库**，此处所做的修改不应视为上游项目的一部分。
>
> 如需了解官方项目、文档、版本发布及相关支持，请以[komari-monitor/komari]为准。
>
> 上游项目的版权、许可证及贡献者署名均保持不变。
# Komari

![komari](https://socialify.git.ci/komari-monitor/komari/image?description=1&font=Inter&forks=1&issues=1&language=1&logo=https%3A%2F%2Fraw.githubusercontent.com%2Fkomari-monitor%2Fkomari-web%2Fd54ce1288df41ead08aa19f8700186e68028a889%2Fpublic%2Ffavicon.png&name=1&owner=1&pattern=Plus&pulls=1&stargazers=1&theme=Auto)

[English](./README.md) | [简体中文](./README_zh-cn.md)

Komari 是一款轻量级的自托管服务器监控工具，旨在提供简单、高效的服务器性能监控解决方案。它支持通过 Web 界面查看服务器状态，并通过轻量级 Agent 收集数据。
[面板发布](https://github.com/r11234567/komari/releases/latest) · [Agent 发布](https://github.com/r11234567/komari-agent/releases/latest) · [上游文档](https://www.komari.wiki/)

## 特性

- **实时监控**: 秒级实时数据展示。
- **轻量高效**：低资源占用，适合各种规模的服务器。
- **自托管**：完全掌控数据隐私，部署简单。
- **Web 界面**：直观的监控仪表盘，易于使用。
- **极强的可扩展性**: 支持自定义主题和插件。

## 部署与升级

1. 备份面板和数据，先升级面板，再升级 Agent。Docker 部署可使用 `ghcr.io/r11234567/komari:<版本号>`。详见[备份与恢复](docs/backup-restore.md)和 [Linux systemd 更新与回退](docs/self-update.md)。
2. 配置面板的 HTTPS 来源和可信代理。使用旧 Passkey 插件时，先阅读[原生 Passkey 迁移](docs/native-passkeys.md)，并确认备用登录方式可用。会话 Cookie 迁移后需重新登录。
3. 按 [Agent 安装与校验](https://github.com/r11234567/komari-agent/blob/main/docs/service-startup.md)操作。新节点通过浏览器审批注册，原地升级 Agent 时保留已有注册凭据。

## 功能配置

- [Cloudflare 橙云](docs/cloudflare-agent-grpc.md)配置 Agent WebSSH 与文件操作的 gRPC 双向流。
- [流量账本](docs/traffic-accounting.md)配置面板账期和流量校准，了解 Agent 样本补传。
- [临时开放 SSH](docs/temporary-ssh.zh-CN.md)适用于已安装救援辅助程序且配置 nftables 接入点的节点。
- [历史 CSV 导出](docs/background-csv-worker.md)通过独立 Linux Worker 导出留存的原始指标点。
- [公开主题与管理端隔离](docs/security/public-theme-origin.md)适用于部署第三方主题或插件的站点。

## 在反向代理 / IP 封禁系统后运行

### 声明可信代理

用 `KOMARI_TRUSTED_PROXIES` 声明哪些对端可以通过 `X-Forwarded-For` / `X-Real-Ip`
声明真实客户端地址：

| 取值 | 含义 |
| --- | --- |
| 不设置 | 仅信任回环代理（`127.0.0.1,::1`）。 |
| `127.0.0.1,::1` | 反向代理与 Komari 在同一台机器上。常见部署用这个。 |
| `none` | Komari 直接对外暴露。忽略转发头，直接使用连接对端地址。 |
| 逗号分隔列表 | 只信任列出的主机或 CIDR，例如 `10.0.0.0/8,192.168.1.5`。 |

代理从容器或独立主机连入时，显式填写其实际来源地址或 CIDR。限流、审计日志与登录会话
共用这套地址策略。无效或没有约束的取值会阻止服务启动。

### 外部封禁系统应如何判断 Komari 流量

所有响应都带 `X-Komari-Principal`，标明该请求的认证方式：`agent`、`user`、
`api-key` 或 `anonymous`。请记录并依据这个头部判断，而不要去匹配 URL：

- **不要计数** 头部为 `agent` / `user` / `api-key` 的请求。这些是已认证的 Komari
  客户端。它们的地址会变（家宽 agent 的前缀并不固定），所以按地址做白名单无法表达
  这一条；而按 API 路径做白名单又会连同「猜到这些路径的攻击者」一起放行。
- **应当计数** 头部为 `anonymous` 的请求，以及完全没有该头部的请求。这覆盖了探测
  `/actuator`、`/.env` 之类的扫描器、打在真实 API 路径上的未认证流量，以及所有并非
  由 Komari 提供的响应。
- `anonymous` 有意不区分「没带凭证」与「带了但被拒绝」，因为在代理日志里体现这个
  差别，等于告诉观察者哪些 token 和账号是存在的。

设置阈值前还有两点需要知道：

- **HTTP 429 是正常且会自行恢复的结果，不是攻击证据。** 它由可选的请求限流返回，
  也会由高成本历史读取在负载过高时独立返回——后者与限流开关无关。浏览器同时打开
  多张图表时出现一次 429 属于正常情况。每个 429 都带有准确的 `Retry-After`。
- **不要在指标读取接口上再加并发上限。** 这些读取本身已经会合并相同的在途查询、
  做短时缓存并限制自身并发；外部再加一层，等于对同一份工作设置了第二道更严的闸门，
  实际惩罚的多半只是正在加载图表的仪表盘。

另外，为兼容旧版本，面板仍接受通过查询参数传入的 agent token。如果你的 agent 可能
使用这种形式，请在访问日志格式中排除查询字符串，否则 token 会出现在封禁系统所读取
的日志里。

## 截图

| 页面         | 截图                                                                                                                                                         |
| ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 主页仪表盘   | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E4%B8%BB%E9%A1%B5%E4%BB%AA%E8%A1%A8%E7%9B%98.webp" width="800" alt="主页仪表盘">            |
| 后台仪表盘   | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E5%90%8E%E5%8F%B0%E4%BB%AA%E8%A1%A8%E7%9B%98.webp" width="800" alt="后台仪表盘">            |
| 历史图表     | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E5%8E%86%E5%8F%B2%E5%9B%BE%E8%A1%A8.webp" width="800" alt="历史图表">                       |
| 网页终端     | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E7%BD%91%E9%A1%B5%E7%BB%88%E7%AB%AF.webp" width="800" alt="网页终端">                       |
| 主题可自定义 | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E4%B8%BB%E9%A2%98%E5%8F%AF%E8%87%AA%E5%AE%9A%E4%B9%89.webp" width="800" alt="主题可自定义"> |
| 主题市场     | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E4%B8%BB%E9%A2%98%E5%B8%82%E5%9C%BA.webp" width="800" alt="主题市场">                       |

协议源码使用 Git 子模块，构建前请执行 `git submodule update --init --recursive`；详见 [Proto 子模块依赖](docs/proto-dependency.md)。
