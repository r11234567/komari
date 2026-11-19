# 遥测存储四项复审与修复（2026-11-15）

## 结论和边界

四项缺陷均存在。先前把预设图的精度下降接受为正常结果、只合并磁盘文件却没有收敛 SQL 所有权目录，判断不完整，本次修正。

本次仍使用小 SQL 加 `.jsonl.zst`：SQL 保留图表标量、最近 15 分钟原始显示窗口和交接暂态；冷原始记录供导出。没有恢复 SQL 压缩统计块、分位包、在线 SHA 账本或全盘同步。

第三方给出的 10–14 MB 目录体积是估算，不能当作实测。容量分别用真实生产私有副本、持续在线测试和专门的目录周转测试测量，不把低采样密度的目录测试冒充完整生产容量测试。

## 1. 交接段与永久目录混在一起

### 根因

`SegmentDuration=24h` 不是“每天只有一个 segment”：字节上限和热队列压力仍可提前封段。这些上限承担背压职责，单纯调大到一天的数据量会重新扩大 SQL 原始积压。

物理 append 只消除了碎文件。同机同日每次交接仍保留独立 segments 和 members，永久元数据随交接次数增长。

### 修改

- 保持有界交接，不为减少目录行而增大热队列。
- 完整交接、无热行/收据/旧 summary/gap 引用且无有效租约后，同机、同观测日、同文件的相邻 extent 归并 SQL owner 和成员范围/计数，删除被吸收的 owner 和 members。
- 只改小目录事务；不解压、不重写压缩字节，不新建永久 extent 明细表。
- 读取时回溯既有 skippable index，还原组合 extent。冻结导出继续持有自己的稳定输入。
- 帧数、子 extent 数和索引字节各用一个小整数限定合并预算。新发布直接记录；旧 v3 extent 首次维护时读取一次索引初始化，之后不为每次合并重扫整日索引。
- 不跨文件、机器、日期或有效租约合并；旧 v2/SHA 文件保持兼容。达到读取预算或 64 MiB 物理轮转上限时可有多个 owner，不能承诺所有负载下严格“一机一天一行”。
- schema 升到 11，转换 fence 为 -11，防止旧二进制误读组合目录。

主要实现：`telemetry_catalog_merge.go`、`telemetry_catalog_gc.go`、`telemetry_archive_frames.go`。

## 2. 档位期限没有覆盖滚动查询的边界

### 根因

预设跨度恰好等于细层保留期，网络延迟、时钟推进和晋级都会使 start 略微跨过期限。1h/7d/15d 因此被升档。测试允许 60s/3600s/7200s 只会固化错误。

1m 层保留到 24h 确实浪费；但“4h 后没有用途”不成立，实际还有 6h 和 12h 预设。

### 修改

- 导入、在线晋级、裁剪、查询年龄策略共用一张 tier 表。
- 每个非末级加 5 分钟余量；1m 从 24h 缩到 12h 加余量。保留 6h=1m、12h=2m，24h=5m。
- 源层存在性检查同时检查真实 first/last 观测交集，粗桶只有几何边缘伸入窗口时不应使整图降级。
- 预设 E2E 恢复严格精度：1h=10s、6h=60s、12h=120s、24h=300s、7d=1800s、15d=3600s、30d=7200s。
- 实际新注册 Agent 只有一个点可以显示 dot；完整历史合成输入必须多个点并渲染 line，不能用单点通过历史曲线验收。

余量只能保护尚未到期的细数据，不能凭空恢复旧版本已删除的精度。升级库若只剩粗人口，接口继续公开真实源层。

主要实现：`telemetry_tier_policy.go`、`telemetry_queries.go`、`real.spec.cjs`。

## 3. 首桶和两个公共协议使用不同查询规划

### 根因

旧 start 落在不可拆的标量桶内部；只细化热窗口无法回答这个历史边界。另有先用未裁剪 start 选层、JSON-RPC 继续用旧步长的问题。客户端第二次对齐请求隐藏了第一次响应的缺口。

### 修改

新增共用 `PlanChart`，Connect/JSON-RPC 的资源图和 Ping 统计按相同顺序执行：

1. 用同一次请求的 now，先按指标 retention 裁剪。
2. 按裁剪后的跨度、真实源层选兼容步长。
3. 旧 start 向内对齐完整输出格；旧 end 向内取完整格末端。最近窗口仍可用完整热人口精确细化。
4. 响应公开实际 coverage 起止和 boundary_adjusted；fill_empty 只填有效窗口。

这是公开有效查询边界，不伪造被裁掉的半桶，也不把未回答的边界标成完整。最左边返回的实际桶不应每次固定 unavailable。其余真实缺失仍须如实报告。

`storage-curves.cjs` 删除二次请求 workaround：第一次故意不对齐，直接以响应实际窗口对照独立原始 oracle 的逐点时间、计数和值；并通过真实 JSON-RPC 请求与 Connect 对比。

主要实现：`telemetry_queries.go`、`web/connect/metrics_query.go`、`web/rpc/jsonrpc/public.metric.go`。

## 4. 原始过期和目录 GC 用了不同期限

### 根因

原始按 ArchiveRetentionDays 删除文件，却没有及时推进 raw_history_floor；目录等图表 MAX(retention_days) 才清理。导出再过滤 expired，丢失“这里本来有原始数据”的证据，返回成功的空 CSV。

### 修改

- 原始过期事务同时单调推进 raw_history_floor 和发布 expiring，提交后才删文件。没有待删文件也推进策略期限。
- 两条原始导出路径明确拒绝 expired/expiring，返回 coverage 错误；不把这些 owner 过滤成空输入。
- 旧数据库尚未补 floor 的 expired 行也明确拒绝。GC 删除最后成员证据前补记单调水位。
- expired owner 解除引用和租约后即可有界回收，不等 90/365 天图表期限。legacy 缺失标记沿用兼容保留规则。
- 字典独立回收：原始 owner 提前消失后，图表到期仍能回收无用 series/key/due；擦除身份屏障继续保留。

主要实现：`telemetry_expire.go`、`telemetry_catalog_gc.go`、`telemetry_export.go`、`telemetry_export_snapshot.go`。

## 验证方式和结果

大框完成后集中验证。失败保留原断言并分析原因，没有放宽迁移 180 秒门槛或图表精度。

- 最终核心 race：metric、metricstore、trafficledger、historyexport、Connect、JSON-RPC 六包通过。
- 新回归覆盖租约阻止合并、旧计数初始化和合并事务回滚、合并前后冻结导出、重启读取、继续追加、过期前后/旧 expired 无水位拒绝、图表保留，以及延迟和非对齐窗口。
- 私有生产副本：6,605,430 个保留观测、57,060 个标量格；18 个非空历史查询在退休前后完全一致。退休后主 SQL 17,874,944 bytes，继续写入 360 批后 18,071,552 bytes。这两项是主库尺寸，未冒充运行 SQL+WAL 峰值。节点身份和数据均不公开。
- 30 天目录周转通过（602.52 秒）：12,960 次交接后为 180 owners、5,220 members；独立 dbstat 目录实际占 1,208,320 bytes（约 1.15 MiB）。未做 VACUUM，主 SQL 7,245,824 bytes，测试逐次记录的 SQL+WAL+SHM 峰值 8,518,984 bytes（约 8.12 MiB）。该低密度场景只用于归因目录增长。
- 36 小时持续在线通过：3 台 ×8 指标、每 30 秒上报，12,960 次接收事务、103,680 个观测；SQL+WAL+SHM 峰值 11,352,120 bytes（约 10.83 MiB），6 个物理归档/11 个暂态逻辑单元。每分钟同时检查汇总、晋级、归档、删除和完整实时窗口，没有实时锯齿。
- 实际安装的新 Agent 场景通过：原始/统计人口一致、短保留期先裁剪再选层、32 路并发、流量趋势、dashboard、归档后图表和完整备份。CPU 默认只保留 1 天，因此该场景的 7d/15d/30d 请求严格要求裁到 1 天并返回 5m；完整历史 fixture 另行严格要求长期预设精度。
- 合成旧库迁移 fixture race 通过（58.77 秒）；全包 `go test -count=1 ./...` 通过。
- 完整迁移恢复浏览器通过：真实 SQLite 锁导致的失败/重启/重试、180 秒门槛内自动迁移、离线退休后重开、两次完整实时窗口验证、全部预设和旧滑块窗口的单请求逐点 oracle、Connect/JSON-RPC 对照、实例图和 dashboard 新截图，以及实际 ZIP 备份恢复后的 HTTP 图表与账本检查。
- 边界对照按 UTC 纳秒比较，避免 RFC3339Nano 字符串省略尾零或 JavaScript 毫秒截断造成误判；仍严格要求边界收缩小于一个输出格，不放宽到整桶以上。
- 远程四库服务和浏览器运行结果以 PR #76 当前头提交关联的 Telemetry storage contracts / Feature audit 为准，不能引用此前提交的绿色检查。

公开长期场景只使用公式生成的 6 台机器 ×29 序列 ×72 次交接/天 ×30 天，共 12,960 次真实交接。它专门隔离目录增长，断言最终 180 owners、5,220 members，且测量未 VACUUM 的 SQL/WAL/SHM 峰值；结束后由独立 Python SQLite/dbstat 归因 segments/members 的真实页占用。

生产快照只存在仓库外私有测试目录；CI 输入仅为公式合成数据。
