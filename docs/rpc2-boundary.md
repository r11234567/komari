# RPC2 兼容边界

`/api/rpc2` 只服务第三方大屏主题和插件前端。管理台不经过这条通道；所有管理功能走 Connect-RPC。

## 允许的方法

以下是完整清单，新增方法必须同时修改 `web/rpc/jsonrpc/transport.go` 与测试：

| 方法 | 用途 |
| --- | --- |
| `public:getMe` | 访客/登录态 |
| `public:getNodesInformation` | 节点列表 |
| `public:getPublicSettings` | 站点公开设置 |
| `public:getVersion` | 版本 |
| `public:getClientRecentRecords` | 最近记录 |
| `public:getRecordsByUUID` | 历史记录 |
| `public:getPingRecords` | 延迟记录 |
| `public:getPublicPingTasks` | 公开 ping 任务 |
| `public:listMetricDefinitions` | 指标目录 |
| `public:queryMetrics` | 指标查询 |
| `public:getPingMetricStats` | 延迟统计 |
| `public:recordVisitorEvent` | 访客事件 |
| `common:getNodes` | 主题兼容读取 |
| `common:getNodesLatestStatus` | 主题兼容实时状态 |

另外，插件运行时通过 `rpc.Register` 注册的方法会被标记为插件方法，允许从 RPC2 进入；插件卸载时随 `rpc.Unregister` 自动移除标记。

## 拒绝的命名空间

`admin:*`、`client:*`、`rpc.*` 以及未登记的 `common:*` 一律返回 `PermissionDenied`，错误信息为
`method is not available on the compatibility transport`。

这条规则作用于 POST 单条/批量请求和 WebSocket 帧，不作用于：

- `jsonRpc.Bind` 的进程内 REST 桥（`/api/admin/*` 等）；
- 服务器内部 `rpc.Invoke` / `CallFromGin`。

因此锁定 RPC2 不会影响现有管理页面，也不会影响主题的旧 REST 入口。

## 管理台走 Connect

终端、文件、编辑器、终端设置等新功能全部在 `web/connect` 注册。历史 admin REST 桥按阶段迁移，
迁移完成后删除对应 RPC2 handler；迁移期间 RPC2 传输层已经拒绝 admin 命名空间。

## 保留为 REST 的例外

- 登录 / OAuth / 2FA 绑定
- 主题与插件安装、备份/恢复、更新
- favicon / MMDB
- 文件下载、上传、预览 token（二进制数据面）

这些是重定向、流式或特殊鉴权形态，不属于 JSON-RPC 能表达的范围。

## 实现位置

- 允许清单与判定：`web/rpc/jsonrpc/transport.go`
- 插件方法标记：`pkg/rpc/registry.go`、`internal/plugin/rpc.go`
- 传输层测试：`web/rpc/jsonrpc/transport_allowlist_test.go`
- 标记生命周期测试：`pkg/rpc/plugin_methods_test.go`

## 回滚

允许清单是代码常量。紧急情况下可以临时把对应方法加入清单恢复访问，但不应把整个 `admin:*`
命名空间重新打开。
