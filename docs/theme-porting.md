# 第三方主题移植指南

本文对应本仓库当前代码，每条都标注了判定点位置，便于核对。适用对象是把上游
Komari 主题移到本分支、或为本分支新写主题的人。

## 一句话结论

**大部分上游主题不需要改代码。** 面向公开大屏的接口（`public:*`、`common:*`、
`/api/clients`、`/api/records/*`）全部原样保留；只有直接调用**管理端 REST 桥接**
的主题需要改，因为其中一部分已随管理台迁移到 Connect 而移除。

## 契约来源

契约由服务端自己声明，不必猜：

```
GET/POST /komari.browser.v1.BrowserService/GetThemeContract
```

返回（`web/connect/browser.go:128`）：

| 字段 | 值 | 含义 |
| --- | --- | --- |
| `schemaVersion` | `1` | 清单结构版本 |
| `manifestName` | `komari-theme.json` | 清单文件名 |
| `connectBasePath` | `/komari.browser.v1.BrowserService/` | 类型化接口基路径 |
| `legacyJsonRpcAvailable` | `true` | **RPC2 仍然可用**，主题不必迁移 |

`legacyJsonRpcAvailable` 是本分支对主题的长期承诺：`/api/rpc2` 不会为了"架构统一"
被摘掉。

## 安装包布局

主题装的是**能直接安装的 ZIP**，不是源码压缩包。校验点在
`web/public/public.go:239`，它要求这两个路径存在：

```
komari-theme.json     ← 清单，必须在 ZIP 根
dist/index.html       ← 静态资源根，服务端把 dist/ 挂到站点 /
```

配套建议一起打进去：`preview.*`（清单里 `preview` 字段声明的那个文件名，主题市场
列表要用）、`LICENSE`、`README.md`。

两个容易踩的点：

- **不要出现以 `_` 开头的文件。** 服务端用 Go 的 `embed`，它会忽略下划线开头的
  文件，装进去就是 404。Vite 一般不产出这类文件名，但自定义 `assetFileNames` 时要注意。
- **`dist/` 必须是站点根下可用的。** 服务端把 `dist/` 挂在 `/`。有客户端子路由
  （如 `/server/xxx`）的主题必须用 `base: "/"`；只有单页、没有子路由时
  `base: "./"` 也能用。

清单没有 `schemaVersion` 字段是允许的，会被归一化为 v1（前端
`normalizeThemeManifest`，`komari-web/tests/connectMigration.test.ts` 有覆盖）。

## index.html 的四个锚点

服务端在返回 HTML 前做注入。少一个锚点不会报错，而是**对应功能静默失效**——这是
移植时最容易漏的一类问题。判定点在 `web/public/public.go`：

| 锚点 | 用途 | 缺失后果 | 判定点 |
| --- | --- | --- | --- |
| `A simple server monitor tool.` | 站点描述 | 后台设置的描述不生效 | `public.go:429` 字面 `ReplaceAll` |
| `<html lang="en">` / `<html lang='en'>` / 根元素不带 `lang` | 站点语言 | 语言设置被忽略 | `public.go:169` 三种模式按序匹配 |
| `</head>` | 自定义 head 注入 | 自定义 head 被前置到文档最前 | `public.go:82` |
| `</body>` | 自定义 body、标题同步、主题热重载 | 脚本被追加到文档末尾 | `public.go:71`、`public.go:109` |

两点说明：

- **描述是字面替换，语言是按序匹配。** 描述那句原文必须逐字保留（可以放在
  `meta[name=description]`、`og:description`、`twitter:description` 里，`ReplaceAll`
  会全部替换，社交预览也就跟着对了）。语言只认上表那三种写法，写死
  `lang="zh-CN"` 或 `lang="en-US"` 都会让设置失效。
- **标题不用管。** `documentTitlePattern` 是 `<title(?:\s[^>]*)?>.*?</title\s*>`
  （`public.go:61`），任何 `<title>` 都会被替换成站点名，不需要写成
  `Komari Monitor`。

## 可直接用的 RPC2 方法

以下方法在本分支上仍然注册，主题可以照旧调 `/api/rpc2`（`GET` 升级为 WebSocket，
`POST` 走单次 HTTP）：

**`public:` 组**（`web/rpc/jsonrpc/public.go`）

```
getVersion              getMe                   getPublicSettings
getNodesInformation     getClientRecentRecords  getRecordsByUUID
getPingRecords          getPublicPingTasks      getPingMetricStats
queryMetrics            listMetricDefinitions
```

**`common:` 组**（`web/rpc/jsonrpc/common.go`）

```
getNodes        getNodesLatestStatus    getNodeRecentStatus
getRecords      getPublicInfo           getMe                   getVersion
```

还有这些非 RPC2 的公开入口也未变动：`/api/clients`（WebSocket 在线列表）、
`/api/nodes`、`/api/public`、`/api/me`、`/api/version`、`/api/records/load`、
`/api/records/ping`、`/api/task/ping`。

## 需要改的：已移除的管理端 REST 桥接

本分支把管理台从 RPC2 迁到了 Connect，随之删掉了下列 **REST 桥接路由**。注意删的
只是 REST 路由，**对应的 `admin:*` 方法仍然注册在 `/api/rpc2` 上**，就是为了不打死
未改造的主题。

| 已移除的 REST | 主题应改为 | 说明 |
| --- | --- | --- |
| `/api/admin/dashboard`、`/dashboard/charts`、`/dashboard/alerts` | `admin:getDashboard` 等，经 `/api/rpc2` | 或用 `komari.admin.v1.DashboardService` |
| `/api/admin/ping`（及 `/add`、`/edit`、`/delete`、`/order`） | `admin:getAllPingTasks` 等 | 或用 `komari.admin.v1.PingTaskService` |
| `/api/admin/session/*`、`/api/admin/logs`、`/api/admin/clipboard*`、`/api/admin/record/clear*`、`/api/admin/database/size`、`/api/admin/database/vacuum` | 同名 `admin:*` 方法 | 或用 `komari.admin.v1.MaintenanceService` |
| `/api/admin/task/*` | `admin:getTasks`、`admin:exec` 等 | 或用 `komari.exec.v1.ExecutionService` |

改法很短。以本次移植的 junimo 为例，原来是：

```ts
return await apiGet("/api/admin/ping", z.array(PingTaskSchema), options);
```

改成走 RPC2，并保留 REST 兜底，让同一份主题在本分支和上游都能用：

```ts
try {
  return await rpcCall("admin:getAllPingTasks", {}, z.array(PingTaskSchema), options);
} catch (error) {
  if (options?.signal?.aborted) throw error;
  // 上游仍提供该 REST 路由
  return await apiGet("/api/admin/ping", z.array(PingTaskSchema), options);
}
```

`admin:*` 方法要求管理员会话；主题在公开大屏上不应该依赖它们，只有主题自带的管理
面板才需要。

**仍然保留的管理端 REST**（主题管理与插件按兼容策略不动）：
`/api/admin/theme/*`（含 `settings`、`market/*`）、`/api/admin/plugin/*`、
`/api/admin/client/list`。

## 可选：类型化的 Connect 接口

不想再拼 JSON-RPC 的话，下列过程**访客即可调用**（角色表见
`web/connect/interceptor.go:52`）：

```
/komari.browser.v1.BrowserService/GetPublicInfo
/komari.browser.v1.BrowserService/ListAgents
/komari.browser.v1.BrowserService/GetAgent
/komari.browser.v1.BrowserService/GetThemeContract
/komari.browser.v1.BrowserService/WatchAgentStatus     ← 服务端流，30 分钟预算
/komari.metrics.v1.MetricsService/QueryMetrics
/komari.metrics.v1.MetricsService/ListMetricDefinitions
/komari.metrics.v1.MetricsService/ListPingTasks
/komari.metrics.v1.MetricsService/GetPingStats
```

调用方式就是普通 POST，`Content-Type: application/json`，body 是请求消息的 JSON：

```ts
const response = await fetch("/komari.browser.v1.BrowserService/ListAgents", {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  credentials: "same-origin",
  body: "{}",
});
```

需要完整类型时用 proto 生成客户端，仓库在
`https://github.com/r11234567/komari-proto`（TS 产物已入库，可直接
`npm i github:r11234567/komari-proto#<tag>`）。

`GetTrafficTrend` 是管理员权限，不能在公开大屏上用。

## 打包与发布

构建、打包、发版全部交给 GitHub Actions，本地只需要 `npm run dev`。本次移植的三个
主题都是同一套流程，可以直接照抄：

- `komari-theme-adhesive-note/.github/workflows/release.yml`
- `komari-animal-island/.github/workflows/release.yml`
- `junimo/.github/workflows/build-package.yml`

流程是「打 `v*` 标签 → 构建 → 校验契约 → 打包 → 附 SHA-256 上传 Release」，其中
两件事值得照做：

- **校验清单版本与 tag 一致。** 不一致时主题市场的自动更新会拒绝，而这在发版当时
  没有任何报错，只有装的人才发现更新不了。
- **打包用固定时间戳。** 主题市场按 SHA-256 校验分发包，ZIP 条目时间戳一变整包
  hash 就变，同一份源码打两次得到两个 hash，谁都无法自行验证 Release 里的包确实来自
  这份源码。`scripts/package-zip.mjs` 里用 1980-01-01 固定，需要真实时间时用
  `SOURCE_DATE_EPOCH` 覆盖。

## 移植自查清单

1. 搜一遍 `/api/admin/`，命中上表「已移除」的改成 `/api/rpc2`（保留 REST 兜底）
2. `dist/index.html` 里四个锚点齐全（描述原文、`lang="en"`、`</head>`、`</body>`）
3. 有客户端子路由则 `base: "/"`
4. 产物没有 `_` 开头的文件
5. `komari-theme.json` 的 `version` 与要打的 tag 一致，`short` 只含 `[A-Za-z0-9_-]`
6. `preview` 字段声明的文件真的在 ZIP 里

前两条是实际会出问题的，后面几条是发版当时不报错、装的时候才暴露的。
