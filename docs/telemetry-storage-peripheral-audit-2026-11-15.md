# Telemetry 外围入口、CSV、调度与查询审计（2026-11-15）

本轮起点：`0bb83dff73a0c3951cf659aee3de08c87c3b7cdb`。逐项检查实际调用链，不将第三方报告中的推测当作事实。目标仍是小 SQL 保存图表标量和少量目录、`.jsonl.zst` 保存原始观测；不恢复 SQL 压缩块，也不增加逐点内容哈希、分位数包或原始点来源索引。

## 一、迁移、回滚与 CLI 的四项核实

| 项目 | 核实结论 | 根因与处理 |
|---|---|---|
| 回滚漏删 v11 标量表 | 存在 | 回滚的旧清单遗漏 `rollup_keys/rollup/rollup_due/raw_gaps`。补齐所有实际 Telemetry 表，包含本轮可选导入游标表；验证回滚产物没有任何 `telemetry_*` 对象，再升级后的多节点、多标签总数、丢包与总和不翻倍。 |
| 生产构建打不开 upstream 1.3.1 | 存在 | 生产旧格式入口拒绝 rollup-only 布局，旧 fixture 构建未覆盖此入口。使用已存在的 schema 检查与原子 1.3.1 adapter，转换成离线 reader 可读的普通 staging 结构；不启动旧压缩 writer。 |
| `import-legacy-history` 写入退役旧表 | 存在 | 目标工厂没有激活 Telemetry。工厂现在先验证切换状态，再启用 Telemetry；导入直接生成年龄匹配的标量与原始档案。已存在的非空旧 metrics 目标仍须先完成明确的离线切换及业务账本桥接，不能假装完成。 |
| 1.2.7 向导与启动 writer 顺序错误 | 存在 | 向导原来先写旧表，再进入第二次转换；启动也在激活前执行 writer。现在统一写活动引擎，先检查不支持的跨后端搬运，再导入与清理来源。旧 raw-only 跨库 copier 不能复制新引擎的标量和冷档，明确拒绝这种不完整迁移。 |

直接导入不能简单改成 `WriteBatch` 后就宣布完成：旧表的 upsert 幂等性与原有流量账本桥接也必须保留。因此：

- 使用每个源文件路径、逻辑表一个小游标。游标与接受的点同事务提交；归档和收据回收后仍可重试，避免重复计数。不建立逐点导入目录或内容 SHA。
- 每个输入子批按机器、UTC 日及最多 256 点划分，及时汇总、归档、退休热行；完成时做年龄晋升与目录归并。
- `records` 与显式选择的 `records_long_term` 使用不同游标；重叠时间戳优先精确记录，切换 include-long-term 不移动原始流的游标。
- 1.2.7 的流量桥接直接读取尚未删除的 monitoring 源，复用已有日结、分钟、周期首日与 counter 恢复算法，并设置下一次在线报告的 counter baseline。CLI 历史恢复不重写独立业务账本。
- 游标的重试契约是**源路径和有序输入保持不变**。它不是任意替换/编辑源文件后的内容去重系统。私有生产来源与数据库不进入版本库、CI 或上传包。

真实向导 E2E 还发现 1.2.7 受限监听器遗漏了新版 `PasswordLoginService`，导致界面提交登录时返回 404。现在复用已有受限登录注册，与 storage-v4 向导保持一致，包含原有二次验证流程；不开放监控、Agent 或凭据管理等普通 RPC。补充路由回归，并通过真实浏览器登录、启动转换和监听器交接验证。

## 二、CSV 的三项核实

### 1. “严格 Aggregate 导致失败并重试五次”：报告描述已过时，但规划缺陷存在

当前 worker 已使用 `ChartBatchBounded`，并非报告中的严格 `Aggregate`；错误处理也不是所有 coverage 错误重试五次，只有短暂数据库 busy 才有限重试。因此不以该错误路径为依据修改重试策略。

实际问题是创建任务的 3m 等请求步长没有结合已经保留的 5m 来源，且窗口边界未统一规划。新增共用 `PlanChartInterval`，与图表共用来源精度、retention 和实际可回答边界。worker 在冻结 summary 时规划每个指标，manifest 保存各自实际 interval 和窗口；CSV 的 `bucket_end`、`output_interval_seconds` 与实际查询一致。旧 manifest 缺少新可选字段时仍使用原任务 interval。

反例：24h、任意请求时间、请求 180s、实际来源 300s，必须成功导出正确均值与样本数，输出明确标注 300s；不是仅让任务显示 done。

### 2. v3 归档没有使用硬链接：存在

`ArchiveFile != ""` 既表示共享物理文件，又被原分支当作必须复制的理由，导致 v3 一律复制。现在优先链接物理文件，并冻结原 extent 的 offset/length 和 ingest 水位；跨文件系统等链接失败时只复制对应 extent，并将其重新基于 0 定位。

帧索引读取以 extent 为局部文件，manifest 帧 offset 转为底层物理文件的绝对 offset。旧 v2 整段校验也使用 extent reader。验证要求包含 `os.SameFile`，同文件非零 offset、目录归并、后续追加、原始路径过期删除和跨设备 copy fallback，不能只验证几条解码值。

### 3. End 的开闭区间冲突：存在

热表快照和直接原始导出均为 `<= End`，visitor 又额外执行 `< End`。删除后者，统一复用点匹配规则。测试包含热/冷两种输入、End 前后 1ns，以及 raw 和 sample30s 真实 worker 任务恰好等于 End 的点。

## 三、后台任务的两项核实

### 1. 排空仍 200ms 重跑且强行叫醒 tiers：存在

排空后 summary 保持默认休眠，只有时间预算用完而仍有工作时才快速重试。移除 summary 排空时的即时 tiers 唤醒。

不能把原来的每次空跑改成“每次上报再扫描所有 rollup_due”。现在汇总已经持有写事务，并且知道刚写入哪些标量，直接从这批标量计算最早晋升时刻，同事务条件性提前 tiers 的 due；没有额外全目录扫描或单独写事务。租约条件比较当前时间，不能用未来 due 误判当前租约已经过期；失败 backoff 也不能被覆盖。

### 2. 标量 erased 任务清理恒空旧 summary：存在

标量删除已经同步处理实际标量表，后台旧 summary sweep 在标量模式直接返回；保留旧非标量行为。测试持有 writer gate，确认标量空 sweep 不竞争写锁。

## 四、图表与两个协议的三项核实

### 1. 近期时间不等于热数据完整：存在

单靠 `now - RawWindow` 不能证明导入后、压力回收后或 legacy 60s 来源可拆分。新增实际数据边界规划：只索引探测左右边界交叉的标量桶，复用完整原始人口校验；能完整细化的热桶保持精确窗口，缺原始人口的边界向内收缩至完整输出桶。多节点使用共同可回答窗口。

统计卡片的最终边界复核与聚合放在同一只读快照，响应返回实际窗口，避免用“忽略 Gaps”掩盖丢失的统计分母。内部确实不可回答的 gap 仍报 coverage 错误。规划不解压冷档，不为这件事新建持久索引。

### 2. Connect PingStats 缺授权 scope，且按节点查询：存在

Connect 和 JSON-RPC 在规划前都放入授权实体 scope。共用 `PingSummariesForEntities`，Telemetry 一次读取全部授权节点，再线性分发各个统计视图；保留每个点的 EntityID 和标签。旧引擎兼容路径仍采用原有逐实体查询，不能将没有 scope 机制的旧查询误当成安全的全节点查询。

### 3. JSON-RPC 丢弃 coverage 元数据：存在

Telemetry 分支直接使用 `ChartBatch`。按节点分发 points、sources 和 gaps 后，再按标签分组；只有 gap 的标签系列也保留。公开 DTO 明确使用小写字段，并返回实际窗口、boundary_adjusted、来源精度、accepted/summarized 水位及逐点 coverage。

填空不覆盖 unavailable 桶，也不将它变成普通 null；真实 0 值仍然是有效观测。保留旧引擎 `SeriesBatch` 的兼容行为。

## 五、集中验证记录

验证以运行时数据、人口计数、窗口、输出内容和 UI 为准，不放宽原有图表断言。

- 完整 production Go 测试：通过。
- 核心八个包的 race：通过。
- `metric_legacy_fixture` 兼容回归：通过。
- `go vet ./...`：通过。
- SQLite、PostgreSQL 17、MySQL 8.4、MariaDB 11.4 真实数据库：production 与 fixture 两个 race 二进制分别乱序运行两遍，全部通过，无数据库跳过。覆盖缺热人口边界、授权实体 scope、迁移、归档、导出与保留期交接。
- 真实 Agent 浏览器：通过；旧 metrics 自动升级、写锁失败后重试、重启、compact、滑动窗口、备份恢复及多预设曲线 oracle：通过。
- 新增 1.2.7 向导：真实浏览器登录和确认转换通过，没有第二次 storage-v4 转换；原 monitoring 表已删除，四张旧 staging/block 表均为 0 行。24h 图表返回 288 个 CPU 点，均值 42，coverage complete；Ping 统计和 `/admin/dashboard` 图表通过，读取的是本轮新截图。
- 独立真实 CSV worker：在正式 4% CPU、128 MiB cgroup 下，raw、sample30s、summary 全部通过；每种输出均含 289 个 CPU 点，包含精确 End，数值均为 42。summary 请求 180s，实际输出明确为 300s 且 coverage complete。
- 30 天目录周转：6 节点 × 29 指标 × 每日 72 次 handoff，12,960 次交接后目录归并为 180 个 owner、5,220 个 member；运行中 SQL+WAL+SHM 峰值 8,764,744 B（约 8.36 MiB），目录页 1,167,360 B，未 VACUUM。
- 36 小时持续读写：3 节点 × 8 指标、30s 上报，12,960 次 ingest 事务、103,680 个观测；SQL+WAL+SHM 峰值 10,713,624 B（约 10.22 MiB），6 个物理归档文件、10 个逻辑单元，未 VACUUM。

两组容量回归各自证明指定负载下的运行中增长与目录周转，不能拿来替代 6 节点、29 指标、3s 全频率生产容量结论；本轮未将静态 VACUUM 后文件大小当作运行容量。浏览器和 CSV 使用明示的合成历史及本地一次性安装；没有将私有生产数据库、凭据或密钥放入提交、CI 或上传产物。

新增回归覆盖 rollback→re-upgrade、生产 1.3.1 打开/迁移/重开、fresh/migrated/retired/online 四类导入目标、提交前后故障恢复、收据回收后重复导入、source 缩短、账本重复桥接与首条在线增量、真实 CLI 两次进程、summary 导出 3m/5m、硬链接与跨设备 fallback、闭 End、缺热人口统计、节点 scope、gap-only 标签、排空休眠与活动租约。
