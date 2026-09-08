# Komari

![komari](https://socialify.git.ci/komari-monitor/komari/image?description=1&font=Inter&forks=1&issues=1&language=1&logo=https%3A%2F%2Fraw.githubusercontent.com%2Fkomari-monitor%2Fkomari-web%2Fd54ce1288df41ead08aa19f8700186e68028a889%2Fpublic%2Ffavicon.png&name=1&owner=1&pattern=Plus&pulls=1&stargazers=1&theme=Auto)

[English](./README.md) | [简体中文](./README_zh-cn.md)

Komari 是一款轻量级的自托管服务器监控工具，旨在提供简单、高效的服务器性能监控解决方案。它支持通过 Web 界面查看服务器状态，并通过轻量级 Agent 收集数据。
[文档](https://www.komari.wiki/) 

## 特性

- **实时监控**: 秒级实时数据展示。
- **轻量高效**：低资源占用，适合各种规模的服务器。
- **自托管**：完全掌控数据隐私，部署简单。
- **Web 界面**：直观的监控仪表盘，易于使用。
- **极强的可扩展性**: 支持自定义主题和插件。

## 本版本改进

- **非 Root Agent**：Agent 可在非 Root 权限下运行，降低部署复杂度和安全风险。
- **Agent 救援模式**：提供恢复通道，用于诊断和恢复失联 Agent。
- **原始数据 CSV 导出**：导出保留的原始指标点，便于审计和离线分析。
- **可选不降采样**：可选择是否启用降采样，原始数据留存与 Rollup 分开管理。
- **Connect-RPC 连接**：使用 Connect-RPC 提供统一、高效的 Agent 与 API 通信。
- **数据库优化**：增加更细粒度 Rollup、增量清理、自适应维护和更高效的查询读写路径。
- **可信客户端地址**：通过 `KOMARI_TRUSTED_PROXIES` 声明哪些对端可以设置转发头，使限流与审计日志记录的地址不再由调用方自行决定。
- **可见的拒绝提示**：HTTP 429/401/403/5xx 会在页面上弹出提示，且不依赖当前主题，不再只留下一张空白图表。
- **便于反代识别**：所有响应都带 `X-Komari-Principal`，反向代理无需靠猜测 URL 就能区分已认证流量与探测流量。


## 在反向代理 / IP 封禁系统后运行

### 声明可信代理

用 `KOMARI_TRUSTED_PROXIES` 声明哪些对端可以通过 `X-Forwarded-For` / `X-Real-Ip`
声明真实客户端地址：

| 取值 | 含义 |
| --- | --- |
| 不设置 | 信任所有对端的转发头。保持旧行为，**且客户端地址完全由调用方决定**。 |
| `127.0.0.1,::1` | 反向代理与 Komari 在同一台机器上。常见部署用这个。 |
| `none` | Komari 直接对外暴露。忽略转发头，直接使用连接对端地址。 |
| 逗号分隔列表 | 只信任列出的主机或 CIDR，例如 `10.0.0.0/8,192.168.1.5`。 |

不设置不只是「不够精确」。客户端地址既是限流的分桶依据，也是审计日志与登录会话
记录的来源地址；在信任所有对端的状态下，调用方可以每个请求换一个头部值来获得全新
的限流额度、把自己的流量记到别人的地址上，或伪造审计日志里的来源地址。取值格式
错误会在启动时报错，而不会静默退回「信任所有代理」。

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
