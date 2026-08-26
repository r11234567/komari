# 第三方主题移植指南

把上游 Komari 主题移到本分支，或为本分支新写主题。本文对应仓库当前代码，标注了
判定点位置便于核对；示例来自实际移植过的三个主题
（adhesive-note、animal-island、junimo）。

## 目标形态

**公开大屏的数据读取全部走 Connect。** 主题不应再使用 `/api/rpc2`、
`/api/clients` WebSocket，或 `/api/nodes`、`/api/records/*` 这类 JSON 端点——
它们仍然存在（`legacy_json_rpc_available` 为 true，未改造的主题不会被打死），
但新主题不必再迁就它们。

只有两类接口按其性质留在 REST：

- **登录与 OAuth**（`/api/login`、`/api/oauth`）：前者要由服务端写下会话 Cookie，
  后者是浏览器跳转，都不是 RPC 能表达的形态。
- **主题与插件管理**（`/api/admin/theme/*`、`/api/admin/plugin/*`）：按兼容策略
  保持不动。

## 契约来源

契约由服务端自己声明，不必猜：

```
POST /komari.browser.v1.BrowserService/GetThemeContract
```

返回（`web/connect/browser.go`）`schema_version`、`manifest_name`、
`connect_base_path`、`legacy_json_rpc_available`。

## 依赖与客户端

```bash
npm i @connectrpc/connect @connectrpc/connect-web @bufbuild/protobuf \
      github:r11234567/komari-proto#v0.1.28
```

```ts
import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { BrowserService } from "@komari/proto/komari/browser/v1/browser_pb";
import { MetricsService } from "@komari/proto/komari/metrics/v1/metrics_pb";

const transport = createConnectTransport({
  baseUrl: window.location.origin,
  useBinaryFormat: true,
  // 站点可能开启私有访问，凭据必须随请求发出，否则会被判为访客。
  fetch: (input, init) => fetch(input, { ...init, credentials: "same-origin" }),
});

export const browser = createClient(BrowserService, transport);
export const metrics = createClient(MetricsService, transport);
```

三处容易踩的构建配置：

- **`moduleResolution` 必须是 `bundler`（或 `node16`/`nodenext`）**，proto 包用
  子路径 exports 分发。
- **不能开 `erasableSyntaxOnly`**：proto 包以未编译的 `.ts` 分发，生成代码用
  `enum` 表达 protobuf 枚举，而 `enum` 不是可擦除语法；这些是被直接导入的源码而非
  `.d.ts`，`skipLibCheck` 也管不到。
- **`uint64` 到了 TS 是 `bigint`**，参与算术前先 `Number(...)`。

## 接口对照

| 旧调用 | Connect |
| --- | --- |
| `/api/public`、`public:getPublicSettings` | `BrowserService/GetPublicInfo` |
| `/api/nodes`、`public:getNodesInformation`、`common:getNodes` | `BrowserService/ListAgents` |
| `/api/me`、`public:getMe` | `BrowserService/GetSession` |
| `/api/clients` WebSocket、`common:getNodesLatestStatus` 轮询 | `BrowserService/WatchAgentStatus`（服务端流） |
| `/api/records/load`、`public:getRecordsByUUID`、`common:getRecords` | `MetricsService/QueryMetrics` |
| `/api/records/ping`、`public:getPingRecords` | `MetricsService/QueryMetrics`（`ping.latency_ms`，按 `task_id` 标签分组） |
| `public:getPingMetricStats` | `MetricsService/GetPingStats` |
| `/api/task/ping`、`public:getPublicPingTasks` | `MetricsService/ListPingTasks` |
| `/api/admin/ping` | `admin.v1.PingTaskService/ListPingTasks`（需管理员会话） |

访客可直接调用的过程见 `web/connect/interceptor.go` 的角色表：BrowserService 的
`GetPublicInfo`/`ListAgents`/`GetAgent`/`GetSession`/`GetThemeContract`/
`WatchAgentStatus`，以及 MetricsService 的 `QueryMetrics`/`ListMetricDefinitions`/
`ListPingTasks`/`GetPingStats`。`GetTrafficTrend` 是管理员权限，不能用在公开大屏。

## 从轮询改成推送

`WatchAgentStatus` 是服务端流，按事件推送**单个**节点的变化，不是整表快照。两点
经验：

- **自己累积快照。** 事件只带一个 agent，界面要的是全量表。
- **推送粒度 ≠ 渲染粒度。** 节点多时逐条事件都提交一次会让整面卡片墙持续重排。
  三个主题都把原来的「轮询间隔」改为约束渲染节奏：事件先累积，按间隔 flush 一次。

```ts
let afterEventId = "";
for await (const event of browser.watchAgentStatus(
  { agentIds: [], afterEventId },
  { signal, timeoutMs: 0 },   // 流不能套用一元请求的超时
)) {
  const agent = event.agent;
  if (!agent) continue;
  afterEventId = agent.eventId || afterEventId;   // 位点，重连时不必从头重放
  snapshot.set(agent.agentId, toRealtime(agent, event.latestReport));
  scheduleFlush();
}
```

断线要自己重连（`catch` 后延迟重试），页面不可见时可以断开流、可见时再接上。

## 从记录接口改成指标查询

历史曲线由 `QueryMetrics` 重建：服务端按 metric 名 + 标签分组返回序列，主题按
时间戳归并成自己的行结构。指标名见
`database/metricstore/metrics.go`：

```
cpu.usage  memory.used  swap.used  load.average  disk.used
net.in.rate  net.out.rate  net.total.up  net.total.down
traffic.up  traffic.down  process.count  connections.tcp  connections.udp
gpu.usage  ping.latency_ms  ping.loss
```

两个要点：

- **空桶与 0 要区分。** `QueryPoint.value` 缺失表示该桶没有采样，不是数值 0。
- **聚合方式与点数上限是整次请求级的。** 如果主题要按指标区分（例如流量用 `sum`、
  速率用 `max`，点数上限也不同），把指标按 (聚合方式, 点数上限) 分组、每组发一次
  请求即可，HTTP/2 多路复用下并发几次比放弃逐指标精度划算。junimo 的
  `services/connect.ts` 就是这么做的。

延迟统计不要在浏览器里重算：`GetPingStats` 直接给 min/max/avg/p50/p99/丢包，与
后台口径一致。

## 安装包布局

装的是**能直接安装的 ZIP**，不是源码压缩包。校验点在
`web/public/public.go:239`，要求这两个路径存在：

```
komari-theme.json     ← 清单，必须在 ZIP 根
dist/index.html       ← 静态资源根，服务端把 dist/ 挂到站点 /
```

- **不要出现以 `_` 开头的文件**：服务端用 Go 的 `embed`，它会忽略这类文件名，
  装进去就是 404。
- **有客户端子路由的主题必须用 `base: "/"`**；单页无子路由时 `base: "./"` 也可以。
- 清单没有 `schemaVersion` 是允许的，会被归一化为 v1。

## index.html 的四个锚点

服务端在返回 HTML 前做注入。少一个锚点不会报错，而是**对应功能静默失效**——这是
移植时最容易漏的一类问题。判定点在 `web/public/public.go`：

| 锚点 | 用途 | 缺失后果 | 判定点 |
| --- | --- | --- | --- |
| `A simple server monitor tool.` | 站点描述 | 后台设置的描述不生效 | `public.go:429` 字面 `ReplaceAll` |
| `<html lang="en">` / `<html lang='en'>` / 根元素不带 `lang` | 站点语言 | 语言设置被忽略 | `public.go:169` 三种模式按序匹配 |
| `</head>` | 自定义 head 注入 | 自定义 head 被前置到文档最前 | `public.go:82` |
| `</body>` | 自定义 body、标题同步、主题热重载 | 脚本被追加到文档末尾 | `public.go:71`、`public.go:109` |

描述是**字面替换**（可以同时放在 `meta[name=description]`、`og:description`、
`twitter:description`，`ReplaceAll` 会全部替换）；语言只认上表那三种写法，写死
`lang="zh-CN"` 会让设置失效。标题不用管：正则会替换任何 `<title>`。

## 数据模型里的两个坑

- **一次性付费是负周期。** 数据模型用 `-1` 表示，而 `billing_cycle_days` 是
  无符号的，会被钳成 0。改用 `billing_one_time` 判断，需要时再还原成 `-1`。
- **隐藏节点由服务端过滤。** 访客拿到的一律是可见节点，主题不必自己判 `hidden`。

## 打包与发布

构建、打包、发版全部交给 GitHub Actions。三个主题用的是同一套流程，可以直接照抄：

- `komari-theme-adhesive-note/.github/workflows/release.yml`
- `komari-animal-island/.github/workflows/release.yml`
- `junimo/.github/workflows/build-package.yml`

流程是「打 `v*` 标签 → 构建 → 校验契约 → 打包 → 附 SHA-256 上传 Release」，其中
两件事值得照做：

- **校验清单版本与 tag 一致**（`komari-theme.json`、`package.json`、
  `package-lock.json` 三处都要同步）。不一致时主题市场的自动更新会拒绝，而发版
  当时没有任何报错，只有装的人才发现更新不了。
- **打包用固定时间戳。** 主题市场按 SHA-256 校验分发包，ZIP 条目时间戳一变整包
  hash 就变，同一份源码打两次得到两个 hash，谁都无法自证。需要真实时间时用
  `SOURCE_DATE_EPOCH` 覆盖。

## 移植自查清单

1. 搜 `/api/`：除登录/OAuth、主题与插件管理外都应改为 Connect
2. 搜 `new WebSocket`：实时状态改用 `WatchAgentStatus`
3. 流的 `timeoutMs: 0`，并自己实现重连与事件位点
4. `bigint` 转 `number` 后再参与计算
5. `dist/index.html` 四个锚点齐全
6. 有子路由则 `base: "/"`；产物没有 `_` 开头的文件
7. 清单 `version` 与 tag 一致，`short` 只含 `[A-Za-z0-9_-]`
8. `preview` 字段声明的文件真的在 ZIP 里

前四条决定能不能跑，后四条是发版当时不报错、装的时候才暴露的。
