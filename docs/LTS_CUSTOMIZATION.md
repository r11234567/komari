# Komari 1.2.5 LTS 魔改记录

本文记录 `r11234567/komari-lts` 相对原始 Komari 1.2.5 架构所做的长期维护修改，说明修改原因、具体实现、运行时行为和后续维护约束。

本文核对的最后一个功能提交是 `2fec215`（`Bound SQLite retention cleanup work`），对应正式版本 `1.2.5-LTS-1.5`。本文档提交晚于该正式镜像，因此镜像中的程序逻辑以 `2fec215` 为准。

## 1. 改造目标与非目标

这条 LTS 分支不是把 1.2.5 逐步改成 1.3.x，而是在保留 1.2.5 原始数据模型和低资源特性的前提下，解决长期历史查询、SQLite 写锁、数据丢失、维护任务抢占和新前端兼容问题。

核心原则：

1. 继续使用 `records`、`records_long_term`、`gpu_records`、`gpu_records_long_term` 和 `ping_records`。
2. 不要求 1.2.6/1.2.7/1.3.x 的独立监控指标数据库、DSN、表前缀或 TSDB 迁移。
3. 原始数据按配置保留；图表降采样在查询阶段完成，不靠删除原始数据解决浏览器 OOM。
4. 前端和后端都必须有时间范围、点数、超时和取消约束，不能只依赖主题自觉。
5. SQLite 前台写入优先；压缩和清理允许延期，不能让 Agent 上报周期性 `database is locked`。
6. 数据库短暂繁忙不能静默丢历史，资源和 Ping 都要有持久化重试 spool。
7. 新默认前端仅作为 1.2.5 后端适配层；删除没有后端语义的 1.3.x 设置。

## 2. 原始问题和根因

### 2.1 长时间 Ping 查询导致 OOM

旧接口把范围内原始行完整读入 Go slice，再编码成大 JSON。15 天以上、多节点或多 Ping 任务会同时放大 SQLite 扫描、Go 模型、JSON 编码和浏览器图表对象。浏览器超时只结束页面等待；若数据库扫描没有绑定请求 context，后端仍会继续跑，所以新窗口仍能看到 CPU、内存和换页持续上涨。

### 2.2 查看资源历史后内存迟迟不下降

一部分来自同样的全量查询；其余可能是 Go GC、SQLite page cache、Linux 文件页缓存和浏览器堆的正常延迟回收。RSS 不会在响应结束时等量立即下降。修复重点是限制峰值和并发，不是每次请求后强制 GC。

### 2.3 SQLite 写入、读取、压缩和清理竞态

Agent 上报、每分钟资源落库、Ping 落库、压缩、过期清理和任务结果清理原本可能各自直接写 SQLite。SQLite 同时只能有一个 writer，长扫描或长删除延长锁占用，最终表现为：

- Agent HTTP 已被接受，但历史写入失败；
- 每分钟反复 `database is locked`/`SQLITE_BUSY`；
- 图表时断时续或暂无数据；
- 清理与上报互相重试，CPU/I/O 长时间高；
- 大表启动 `AutoMigrate` 可能复制重建表并制造大量临时磁盘占用。

### 2.4 索引与移动端主题问题

节点历史需要 `(client, time)`，按任务查询 Ping 还需要 `(task_id, time)`。只有单列索引时仍可能扫描大量候选行再排序。

Mochi 1.1.8 又把 Ping 图从“一个节点的全部任务”改成“一个任务的全部节点”，并默认选择几乎恒定的“是否在线”，因此视觉上是一条直线。恢复多任务后，二十多个内置图例又会在移动端挤占图表高度。

## 3. 总体数据流

```text
Agent report / Ping result
          |
          v
  foreground write gate ---- busy/error ----> durable ingest spool
          |                                      |
          v                                      | periodic drain
 legacy SQLite tables <--------------------------+
          |
          +---- dedicated WAL query-only reader
          |              |
          |              v
          |      bounded history service
          |       - max 90 days
          |       - 20 s deadline
          |       - 1,500 default / 5,000 hard point budget
          |       - server-side time buckets
          |              |
          |              +--> legacy REST adapters
          |              +--> RPC2 metric adapters
          |              +--> default frontend / third-party themes
          |
          +---- low-priority maintenance gate
                         |
                         +--> one 15-minute compaction window/pass
                         +--> bounded retention deletion/pass
```

## 4. 有界历史查询服务

主要文件：

- `database/history/history.go`
- `web/rpc/jsonrpc/public.go`
- `web/rpc/jsonrpc/public.metrics_lts.go`
- `web/api/public/history_client.go`
- `web/router/router.go`

### 4.1 标准接口和硬限制

```text
POST /api/v1/history/query
GET  /api/v1/history/client.js
```

请求示例：

```json
{
  "type": "ping",
  "uuid": "client-uuid",
  "task_id": 10,
  "start": "2026-07-01T00:00:00Z",
  "end": "2026-07-16T00:00:00Z",
  "max_points": 1500
}
```

也可用 `hours` 代替 `start`/`end`；不传范围默认最近 4 小时。

| 项目 | 限制 |
| --- | --- |
| 最大在线查询范围 | 90 天 |
| 默认响应点预算 | 1,500 点 |
| 最大响应点预算 | 5,000 点 |
| 服务端 deadline | 20 秒 |
| 标准浏览器客户端 timeout | 25 秒 |
| 类型 | `load`、`ping`，load 可同时返回 GPU series |

`max_points` 是整个响应的预算，不是每个 series 各有一份。多任务或多 GPU 时，`limitTotalPoints` 在 series 间分配预算，再等距采样并保留首尾点。

### 4.2 服务端分桶和统计

后端按“请求时长 / 点预算”算所需最小分辨率，再从以下固定间隔选择：

```text
10s, 30s, 1m, 5m, 10m, 15m, 30m,
1h, 2h, 6h, 12h, 24h
```

Ping 桶返回 `avg`、`min`、`max`、`loss_count` 和 `total_count`。资源/GPU 桶返回 `metrics` 和 `total_count`。Ping summary 按有效样本数加权计算平均值、范围、latest、p50、p99 和标准差；分位数基于桶均值，因此 `approximate=true`。

### 4.3 内存、取消与错误

查询使用 `Rows()` 流式扫描，不再 `Find` 到完整模型 slice；GORM 绑定 `WithContext(ctx)`，扫描循环检查 `ctx.Err()`。HTTP 断开或 20 秒 deadline 到期后，取消会传播到数据库读取。

内存主要与“桶数 x series 数”相关，而不再与原始行数线性相关。后端仍需扫描匹配行，所以极端大库耗时仍取决于索引和磁盘；有界响应解决 OOM/渲染灾难，不意味着 90 天查询零成本。

`KomariHistoryClient` 支持稳定 key、同 key 新请求替换旧请求、外部 `AbortSignal`、本地 timeout 和显式 `cancel(key)`。

响应元数据：

- `resolution`：实际分桶间隔；
- `raw_count`：扫描原始行数；
- `returned_points`：最终点数；
- `sampled`：是否降采样；
- `series`：节点/任务/GPU 设备序列。

错误映射：400 参数错误、499 调用方取消、504 服务端超时、503 持久化或导出队列暂不可用。

## 5. 旧接口与 RPC2 兼容

保留主题常用路由：

```text
GET /api/records/load
GET /api/records/ping
GET /api/task/ping
```

load/Ping 历史已转接同一有界服务，并接受 `hours` 和 `max_points`。主题可以渐进迁移，不迁移也不会重新进入无限制查询路径。

新版默认前端通过以下 RPC2 adapter 读取旧表：

```text
public:listMetricDefinitions
public:queryMetrics
public:getPingMetricStats
admin:listMetricDefinitions
admin:updateMetricDefinition
```

支持 CPU/GPU、GPU 设备/显存/温度、RAM、Swap、Load、温度、磁盘、网络速率/累计量/区间流量、进程、TCP/UDP 连接和 Ping。adapter 未创建或启用新监控数据库，仍受 90 天、20 秒和 1,500/5,000 点限制。

## 6. SQLite 连接、索引和迁移

主要文件：`database/dbcore/dbcore.go`、`database/dbcore/write.go`。

### 6.1 连接布局

SQLite 主连接池：

```text
MaxOpenConns = 1
MaxIdleConns = 1
ConnMaxLifetime = 0
journal_mode = WAL
synchronous = NORMAL
busy_timeout = 1000 ms
cache_size = -65536     # 约 64 MiB
temp_store = FILE
```

历史查询单独打开 query-only reader：

```text
query_only = ON
MaxOpenConns = 1
MaxIdleConns = 1
cache_size = -16384     # 约 16 MiB
temp_store = FILE
```

独立 WAL reader 避免慢历史查询占住唯一写连接；读池仍限制为 1，防止多个大查询同时拖垮 1 GB 主机。

### 6.2 组合索引

启动时确保：

```sql
records(client, time)
records_long_term(client, time)
gpu_records(client, time)
gpu_records_long_term(client, time)
ping_records(client, time)
ping_records(task_id, time)
```

`idx_ping_record_task_time` 是 task-centric Ping 查询的关键新增索引。大库首次创建可能较慢，日志会打印开始和完成。索引只优化过滤/排序，不能替代响应点数上限。

### 6.3 冻结历史表迁移

1.2.5 历史 schema 在 LTS 中冻结。`records`、long-term、GPU 和 Ping 历史表只有不存在时才执行 `AutoMigrate`，避免每次启动为比较字段/约束而复制重建 GB 级 SQLite 表。普通小表仍按原逻辑迁移。

## 7. issue #573 类写入竞态修复

### 7.1 前台写门控

`dbcore.Write(ctx, fn)` 用容量 1 的 channel 串行化 SQLite writer，并记录排队前台 writer。busy 错误最多尝试 5 次，退避为：

```text
0 ms, 50 ms, 100 ms, 250 ms, 500 ms
```

只重试 `database is locked`、`database table is locked` 和 `sqlite_busy`；业务错误不误重试，已成功 callback 不会再执行。

### 7.2 维护任务让路

`dbcore.TryMaintenance` 用于压缩和清理。只要前台 writer 正在运行或排队，维护立即跳过本轮，下一次 cron 再试。

```text
Agent/监控持久化 > 管理写操作 > 压缩/清理
```

这从写入调度上解决“写入 + 读取 + 清理每分钟竞态”，而不是通过换数据库或丢原始数据规避。

## 8. 资源和 Ping 持久化 spool

### 8.1 Ping spool

目录：`data/ingest-spool/ping`。数据库写失败时以 JSON Lines 追加 `current.jsonl`，每条后 `fsync`。总上限 256 MiB，超限明确返回 `ErrPingSpoolFull`。

后台每 3 秒 drain：最多一个等待 segment；每批 500 条；事务 deadline 8 秒；`.offset` 原子更新，重启续传；完整提交后才删除 segment。

### 8.2 资源/GPU spool

文件：`data/ingest-spool/load-records.json`。每分钟资源平均记录和 GPU 记录在同一事务写入。失败时原子保存去重批次；下一轮合并旧 pending 和新数据后重试，成功才删除。

资源落库 deadline 10 秒。资源按 `client + second` 去重，GPU 额外包含 `device_index`。积压恢复时按节点复用流量累计值基线，不再为 backlog 每条记录重复执行相同的 `ORDER BY time DESC LIMIT 1`。

spool 处理短暂故障，不是无限消息队列；磁盘可写性、数据卷持久化和 256 MiB 上限仍需监控。

## 9. 压缩与过期清理

### 9.1 每次一个 15 分钟窗口

`CompactRecord` 启动时运行一次，此后每分钟尝试，但必须取得 maintenance gate。资源和 GPU 各自每轮最多处理一个完整 15 分钟窗口。

压缩 cutoff 为当前时间减 4 小时并按 15 分钟对齐，额外保留 1 小时 raw overlap：

- 最近约 4 小时仅原始数据；
- 4-5 小时可已有 long-term，同时保留 raw；
- 早于约 5 小时且聚合成功的窗口才删除 raw。

这修复分批压缩时 raw/long-term 边界缺口。聚合保留 1.2.5 语义：多数资源约 p70，网络瞬时速率约 p20，区间流量求和，累计网络量取窗口最新值，GPU 按设备聚合。

单窗口限制会让积压恢复较慢，但避免一分钟任务连续扫描约 49 万 long-term 行或长期占满单核。

### 9.2 清理批量

清理 cron 每 30 分钟运行。SQLite 每轮每表最多删除：

| 表 | 行数 |
| --- | ---: |
| `records_long_term` | 1,000 |
| `gpu_records_long_term` | 1,000 |
| `gpu_records` | 1,000 |
| `records` | 1,000 |
| `ping_records` | 1,000 |

SQL 使用 `rowid IN (SELECT rowid ... LIMIT ?)`，逐表走 maintenance gate；有前台写入时剩余工作延期。旧积压不会一次消失，这是低资源机器的主动取舍。若过期新增长期超过清理能力，应调整批量/频率或安排离线维护，不能恢复“循环删到空”。非 SQLite 保留条件删除。

## 10. 逐指标保留

配置键：`metric_retention_days_by_name`，允许 `0-36500` 天。更新通过 `admin:updateMetricDefinition` 并记录审计日志。

1.2.5 一行资源记录含多个字段，无法物理删除 CPU 而保留 RAM，所以分两层：

1. 查询层按每个 metric retention 截断；`0` 表示不返回该指标历史。
2. 物理表保留取同组最大值：资源/GPU 映射 `record_preserve_time`，Ping 映射 `ping_record_preserve_time`。

这样不同指标有不同可见期限，又不会误删同一行中仍需保留的字段。代价是物理库可能暂时保留已不可查询字段，直到同组最长保留期到期。UI 用天，底层兼容配置继续用小时。任务结果保留单独保存，未设置时回退资源保留时间。

## 11. 异步原始 CSV 导出

管理员接口：

```text
POST   /api/admin/history/export
GET    /api/admin/history/export/:id
GET    /api/admin/history/export/:id/download
DELETE /api/admin/history/export/:id
```

实现特性：

- 仅管理员可用，最大范围 90 天；
- 单 worker 后台生成，队列容量 8；
- 每次读取 15 分钟窗口并在窗口间释放 reader；
- Ping 输出每条原始 `ping_records`；
- 资源同时输出 raw/long-term，以 `source` 列区分；
- 状态为 queued/running/done/failed/cancelled；
- 文件在 `data/exports`，48 小时后清理；
- job 状态只在内存，重启不恢复进行中任务。

图表查询使用降采样，CSV 保持原始精度。导出仍产生磁盘和顺序读取负载，所以用单 worker 和分窗限制影响。

## 12. 默认前端适配

前端独立维护：

```text
git@github.com:r11234567/komari-web.git
branch: lts-1.2.5
tag:    1.2.5-LTS1-web.1
```

后端工作流固定该 tag，不跟随前端最新提交。

保留/新增：新版默认 UI；Load/Ping 有界 RPC2；`AbortController`、30 秒 timeout 和 `max_points`；`1h、6h、12h、1d、7d、15d` 后按 15 天递增，最高 90 天/保留上限；逐指标保留；数据库大小/备份/VACUUM；Cloudflared；管理员 CSV 导出。

删除：监控 DSN、低资源模式、数据库降采样配置、表名前缀、监控库迁移/恢复、无实际语义的连接池设置、官方新版升级提醒和 1.2.7 升级页，以及依赖新后端的安装流程。

`/admin` 布局做兼容，避免未认证状态直接暴露完整后台外壳；真正权限仍由后端 `RequireRole(RoleAdmin)` 保证，前端隐藏不是安全边界。

## 13. Cloudflared

沿用 1.2.5 的 `utils/cloudflared`，恢复管理 UI：

```text
GET  /api/admin/settings/cloudflared
POST /api/admin/settings/cloudflared/start
POST /api/admin/settings/cloudflared/stop
POST /api/admin/settings/cloudflared/remove-token
```

启动时仍可通过 `KOMARI_CLOUDFLARED_TOKEN` 自动启动。它与监控数据库无耦合，只是在设置页合并展示。

## 14. 主题市场与安装安全

新增市场源/catalog/安装接口。关键限制：catalog 2 MiB、主题包 100 MiB、HTTP timeout 45 秒、最多 10 次重定向且每跳重新校验、禁止 URL userinfo、DNS 指向 loopback/private/link-local 时拒绝、下载后校验 SHA-256、ZIP manifest short/version 必须匹配 catalog，并限制文件数、单文件、总解压量和 manifest 大小。

主题 short 严格校验并去重，防止路径穿越和同一主题显示两份。若公开域名被本地 DNS/代理解析成内部地址，会报 `requests to private or internal addresses are not allowed`；这是 SSRF 防护，不应删除，应修正 DNS/出口或市场源。

## 15. Mochi 1.1.8 运行时补丁

Mochi 未 fork，按部署要求直接位于数据卷 `data/theme/Mochi`，不包含在后端镜像或默认前端仓库。覆盖主题后需重新适配。

修改内容：

1. Ping 从“一个 task 对比所有节点”恢复为“选择节点，显示该节点全部 task”。
2. 复用 bundle 内 `PingChartV2`，走后端有界历史适配。
3. 停用旧 `/api/task/ping` + `task_id` 跨节点后台 effect，避免隐藏扫描。
4. 增加移动端节点选择器，保留 Load/Ping 切换。
5. 删除图表内部自动换行 legend，改为下方单行横向滚动任务按钮。
6. 更新 Service Worker precache revision。

运行时文件：

```text
data/theme/Mochi/dist/assets/chunk-Index-CA10GTqN.js
data/theme/Mochi/dist/assets/chunk-use-ping-summary-DAja585L.js
data/theme/Mochi/dist/sw.js
```

修改编译产物后至少运行 `node --check` 检查三个文件。已有页面不会热替换 JS，需关闭旧页面并重新打开/刷新。

## 16. CI、Snapshot 与正式发布

`.github/actions/build-frontend/action.yml` 使用 Node.js 24，从固定 tag `1.2.5-LTS1-web.1` 执行 `npm ci` 和 `npm run build`，产物作为 artifact 给所有 Go 架构复用。

Snapshot 和正式 release 都必须通过 Go 格式检查和 `go test ./...`。Snapshot push 到 main 自动触发，校验 SHA 仍为当前 main，构建 Windows/Linux amd64、arm64、386 和 Linux riscv64，使用 Zig 交叉编译，发布 prerelease 和 amd64/arm64 `snapshot` 镜像。concurrency 会取消旧构建，发布前再次校验 SHA，避免旧工作流覆盖新镜像。

GitHub Release published 后构建附件和多架构镜像，同时推送 release tag 与 `latest`。生产部署应先核对 OCI version/revision，再在 Compose 固定 digest，避免 `latest` 漂移。

## 17. 配置与目录

| 名称 | 用途 |
| --- | --- |
| `record_preserve_time` | 资源/GPU 物理保留小时数 |
| `ping_record_preserve_time` | Ping 物理保留小时数 |
| `task_result_preserve_time` | 任务结果保留小时数 |
| `metric_retention_days_by_name` | 每个逻辑指标可见天数 |
| `theme_market_sources` | 主题市场源 |
| `data/komari.db` | SQLite 主库 |
| `data/komari.db-wal` / `-shm` | WAL 文件 |
| `data/ingest-spool/ping` | Ping 重试队列 |
| `data/ingest-spool/load-records.json` | 资源/GPU 待写批次 |
| `data/exports` | CSV 临时文件 |
| `data/theme` | 外部主题与配置 |

## 18. 已知边界

1. 90 天是在线 API 上限，不是数据库保留上限；更长数据需分段导出或离线工具。
2. 降采样仍要扫描匹配原始行，索引和磁盘速度仍影响耗时。
3. Go RSS、SQLite cache 和 page cache 不会在请求结束时立即归还；应看长期趋势而非单次峰值。
4. 清理每 30 分钟只删有限行，旧错误数据会缓慢下降；不要恢复无限循环删除。
5. `VACUUM` 独占数据库且需额外空间，只应备份后在低流量窗口执行。
6. CSV job 状态不持久化；spool 也只保护短期故障并有容量上限。
7. Mochi 是数据卷补丁，不随升级重放，覆盖前必须备份。
8. `pkg/metric` 中存在通用指标存储代码不代表 LTS 启用了新监控库；当前兼容由 legacy adapter 提供。
9. 修改历史接口必须同时验证标准 REST、旧主题路由和 RPC2。

## 19. 验证清单

代码提交前：

```bash
git diff --check
gofmt -l <changed-go-files>
go test ./...
```

生产机资源不足时，以 GitHub Actions 作为完整构建 gate；编译主题仍应本地 `node --check`。

部署后检查容器健康、重启次数、日志和一次资源快照。功能上检查 1h/15d/90d 历史点数受控、切换范围会取消旧请求、Mochi 绘图区不被图例压缩、Agent 持续写入、无周期锁错误、spool 重启续传、CSV 仅管理员可用，以及 30 分钟清理周期不长时间满核。

## 20. 功能提交时间线

| Commit | 内容 |
| --- | --- |
| `2740544` | 第一版 LTS：有界历史、SQLite 仲裁、spool、导出、接口和工作流基础 |
| `c7283ee` | 修复 Go 格式检查 |
| `45ae85b` | 修复分批压缩边界，保留一小时 raw overlap |
| `c33e393` | release/snapshot 使用适配后的默认前端 |
| `dafd8ba` | 重新触发前端修复后的 snapshot |
| `84590ce` | 固定前端 tag `1.2.5-LTS1-web.1` |
| `d71ef0b` | 逐指标保留与主题市场 |
| `1045231` | 避免分钟持久化扫描 long-term 大表 |
| `2194009` | 避免启动重复迁移/重建历史大表 |
| `6c1ccfc` | backlog 恢复复用流量基线查询 |
| `2fec215` | SQLite 过期清理固定小批量，不再循环删空 |

## 21. 主要文件索引

| 文件 | 职责 |
| --- | --- |
| `database/history/history.go` | 历史分桶、点预算、Ping summary |
| `database/history/export.go` | 单 worker CSV 导出 |
| `database/dbcore/dbcore.go` | SQLite 连接、WAL reader、索引、迁移 |
| `database/dbcore/write.go` | 写门控、busy 退避、maintenance gate |
| `database/tasks/ping_spool.go` | Ping JSONL spool |
| `web/report/cache.go` | 资源聚合、待写 spool、流量 delta |
| `database/records/records.go` | 压缩、raw overlap、资源清理 |
| `database/tasks/ping.go` | Ping 写入与清理 |
| `web/rpc/jsonrpc/public.metrics_lts.go` | RPC2 legacy adapter |
| `web/rpc/jsonrpc/admin.metrics_lts.go` | 逐指标保留更新 |
| `web/api/admin/history_export.go` | CSV 管理接口 |
| `web/api/admin/theme_market.go` | 市场、SSRF 防护、校验安装 |
| `web/api/admin/theme.go` | 主题 ZIP 安全和生命周期 |
| `web/router/router.go` | REST/RPC2/Agent/admin 路由 |
| `.github/actions/build-frontend/action.yml` | 固定 LTS 前端构建 |
| `.github/workflows/snapshot.yml` | Snapshot 构建发布 |
| `.github/workflows/release.yml` | 正式构建与 latest 镜像 |

维护该分支时，应优先保持上述行为边界，而不是盲目同步上游新数据库。任何扩大查询点数、增加 SQLite writer、重新启用历史表 AutoMigrate，或让维护任务循环到清空的修改，都应视为高风险并单独压测。
