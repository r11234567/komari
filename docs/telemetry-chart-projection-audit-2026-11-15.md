# Frontend chart projection audit — 2026-11-15

审计基线：`f0bab586`。保持小 SQLite + `.jsonl.zst`；本轮只调整读取规划、授权空集和响应投影，不添加持久化摘要、校验哈希、索引或 SQL 压缩块。

## 逐项核实

1. **Connect 空节点仍规划：成立；“空 scope 无过滤扫全库”：当前基线不成立。** `WithTelemetryEntities` 已保存空集，`seriesFilter` 已生成 `AND 1=0`。两个 Connect 入口此前仍无必要地获取 store 和规划。现在空授权集合在获取 store 前返回；Ping 保留原请求起止时间，并跳过读并发槽。底层空 scope 继续拒绝所有实体。
2. **投影循环覆盖响应窗口：成立；当前 helper 导致零时间：不成立。** 基线的 Telemetry helper 已全实体一次读取、使用共同窗口；legacy helper 也已补齐请求窗口。问题在于适配层仍把公共元数据寄托于最后一个 map 元素。本轮将返回值改成 `EntityPingSummaries`，批次显式携带唯一窗口、实际 interval 和 `ByEntity`。两个协议从批次取一次元数据，循环只分发节点统计。空节点、乱序、重复/空白 ID 不再参与决定窗口。兼容引擎和旧非标量实现也填齐直接查询的元数据。
3. **直接 Ping 查询只按理论年龄规划、单 task 未下推：成立。** `scalarPingSummary` 现在在同一只读快照里通过 `PlanChartInterval` 检查实际保留精度及可回答边界；`ChartInterval` 复用该快照，避免单连接池等待自己的事务。两个协议单个 task 的 Tags 同时用于规划和读取，未请求任务的粗桶及缺热边界不会误伤目标任务。

第 3 项的后果需要限定：并非“有 60s + 10s 混合桶就一定报 coverage”。现有 statistics-card 聚合及 `scalarAnswerableWindow` 已能回答部分此类窗口。使用 Go overlay 只换回旧规划的对照中，计数仍正确，但内部规划仍选择 10s，没有识别实际 60s 来源；新回归准确拦截这种规划不一致。单任务的反例则实际验证：目标 10s task 正确返回 3 次尝试，而包含另一个不可拆 60s task 时确实报 coverage；不能让后者误伤前者。

公共窗口必须在计算统计前确定。对已经按不同人口算出的统计事后简单取时间交集，无法修正分母，因此本轮沿用一次授权实体批次读取，并显式传回共同元数据。真实不可回答的覆盖仍返回错误；不把不完整统计伪装成成功。

双协议数值回归还发现一个实际投影缺陷：JSON-RPC 原先优先使用 latency 的成功数作为 `total`，而标量 loss 的 Count 才是全部尝试数。现在与 Connect 一致优先使用该 task 的 loss Count；是否有精确 loss 数据也按 task 判断，避免另一个 task 的 loss 让当前 task 被误标为精确统计。新增部分丢包、全部丢包、只有其他 task 有 loss 三种分母回归。

## 回归范围

- 直接 store 调用，无 HTTP 预规划：同节点 60s 迁移桶与 10s 新桶共存，验证实际 interval、向内窗口、成功数、尝试数、均值与丢包率。
- 单连接 reader：规划与读数共用事务，不死锁。
- 多节点、空节点、两种顺序、重复及空白 ID，分别验证 Telemetry 与 legacy 的批次窗口和直接 summary 元数据。
- 目标 task 有 10s 数据，未请求 task 有不可拆 60s 边界；过滤查询成功且数值正确，无过滤控制查询仍报 coverage。
- 空 scope 的 PlanChart 与 Chart 不受隐藏粗桶影响；Connect 隐藏/不匹配节点返回空结果和原始请求时间。

## 集中验证结果

- 最终源码 `go test -p 2 -count=1 -timeout=12m ./...` 全仓通过。
- `pkg/metric`、`database/metricstore`、`web/connect`、`web/rpc/jsonrpc` 四包 race 通过；`metric_legacy_fixture` 下 metric、migrations、metricstore、historyexport 四包通过；全仓 `go vet ./...` 通过。
- 负面对照：Go overlay 只换回旧年龄规划，新混合精度回归明确失败于内部仍选 10s、未识别实际 60s 来源；不修改工作区源码。
- 真实 HTTPS 面板 + 新安装 Agent 浏览器回归通过（2.1 分钟），覆盖真实 TCP Ping、store reload、并发图表/Ping、原始点与 summary 人口、流量与 dashboard、归档和备份。本轮新截图核对通过。
- 独立 `storage-upgrade.cjs` 总流程通过：启动迁移、保留层级及非对齐/滑动窗口原始人口 oracle、压缩后读取、多种历史图表截图、表结构审计和完整备份恢复。
- 新增双协议 Ping E2E：1h / 24h / 7d 单 task 返回相同有效窗口、实际 interval、总次数、成功次数、均值和丢包率，并逐个与合成原始观测核对；不存在的 task 返回空统计且保留原请求窗口。

总回归实际抓到并复现了 JSON-RPC 的分母错误（一次运行应为 511 次尝试、472 次成功，旧响应错把 total 返回为 472）；修正人口语义后原断言通过，没有降低断言。第一次完整恢复回归使用了 94 分钟前的本地备份，最近 60 分钟无点；确认备份及库内最新观测时间后，重跑真实 Agent 生成新备份，再完成完整恢复回归，没有改弱恢复断言。

仅使用本地一次性安装及明确生成的合成历史；没有提交或上传私有生产数据。本轮没有用静态紧凑文件或截图中的新库大小推断长期生产容量。新提交的 Actions 另按其 HEAD 跟踪，上一提交绿灯不计入本轮结果。
