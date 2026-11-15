# S01 入口边界：三轮独立交付

基准日期：2026-11-15，Asia/Singapore。执行依据：`AUDIT-AND-REBUILD-PLAN.zh-CN.md` §6.2 与 `CROSS-REPO-IMPLEMENTATION-PLAN.zh-CN.md` S01，以及其中允许按完整状态转换拆分语义包的规则。

本次三轮先闭合 S01 的三个独立语义：①私有站点的认证仪式与协议错误；②统一可信代理和外部认证来源；③跨源预检、认证失败与客户端错误恢复边界。三轮不冒充 S01–S03 已全部实施，也不声称主题/插件隔离、原生 Passkey 接管、刷新事务重建已完成。§6.2 的两套 gateway、14 项展示候选、Hidden/IP 与访客写事件边界继续作为 S04 的强制前提，不在入口修复中偷换成全量 RPC 转发。

## 轮次一：匿名仪式可达，授权与协议不降级

- 将私有站点放行范围固定为生成代码中的十条精确 procedure：两条浏览器 metadata、四条 PasswordLogin、四条 Enrollment bootstrap/refresh。无 service 通配、无 TrustedCredential/Admin/Revoke 放行。
- 认证、私有站点、DPoP 的前置错误使用 Connect 自带 ErrorWriter，统一保留 Connect unary/stream、gRPC、gRPC-Web 的错误编码；REST 保留现有 JSON 错误形状。显式坏 Bearer 仍先拒绝，不能借路径豁免退回 Cookie。
- 正向与拒绝测试通过完整 Gin 中间件及生成客户端，包含私有模式开/关、密码＋TOTP、取消与重放、真实会话、无 AT 入网/公钥发现/刷新、所有管理员/可信凭据方法拒绝。
- 新增 `Private-site entry acceptance` 组合门禁：先静态与 race 回归，再构建固定 Web/Server/Agent，经真实 HTTPS Nginx 执行专项浏览器登录和已安装 Agent 入网。只运行本轮 Komari E2E，不调用历史/导出/文件/终端等其他专项。

四仓组合：Server 基于 `e43cf9ba1271fb612428ad163bc4bc7627124896`；Agent `1a5fc8b1492f9a2e08a2031195e9e844bea342a1`；Web gitlink `a5a5a2ba638bfaddef1596ac72eaeb5a15b3766b`；三方 Proto gitlink `1de1d15f77b3f0aea1160bd6c06976d2a094aaeb`。本轮无 wire、生成代码或消费 API 变化，故不为凑数修改另外三仓。专项 E2E 记录实际四仓 SHA 并校验三个协议 pin 一致。

迁移：无数据库结构变更、无凭据重签发；已存在登录/因素仍由原 handler 验证。删除原仅两条 metadata 的硬编码私有入口判断及手写 RPC 鉴权 JSON。候选提交必须通过 CI/E2E 才能记验收通过。

## 轮次二：统一来源解析与固定批准链接

- 新建标准库 `pkg/requestmeta` 作为 HTTP 元数据边界：默认不信任代理，拒绝全网段和错误配置；Gin、API 预算、原始插件 hook、HTTPS 判断共用同一显式代理列表与右至左 IP 链规则。
- 删除私有/回环地址自动可信、任意第一 XFF 和桥接层直接 XFP 判安全的旧路径。外部 Origin 完整比较 scheme、host、port，多值/冲突转发头拒绝。正常、安装、两类升级监听器均校验配置。
- 批准链接仅来自 `KOMARI_PUBLIC_ORIGIN`；未配置时在创建申请前返回 FailedPrecondition，绝不从 Host、转发头或 Peer 拼接。无 wire、数据库、凭据格式变化，Agent/Web/Proto 固定第一轮组合。
- HTTPS fixture 的启动及所有既有重启路径显式传入 Origin/代理配置；Nginx HTTP/H2 覆盖转发头。部署模板见 `deploy/nginx/entry-origin.zh-CN.md`，保留单 hostname。
- 本地相关八包 `go vet -race` 与完整 `go test -race -count=1` 已通过。新增伪造头、冲突头、默认拒绝代理、固定链接与配置缺失测试；CI 增加这些包，专项 E2E 加入真实 Nginx 伪造头覆盖检查。远程门禁结果另行登记，未通过前不记验收完成。

## 轮次三：预检先于鉴权与可恢复错误分类

- 正常/遗留升级监听器先执行 CORS，真正预检不经过 Bearer 认证或失败预算；安装/存储升级入口的既有同源行为保持。删除 API Key 和 Authorization 预检的来源豁免。
- CORS/WS allowlist 使用完整 Origin，拒绝裸 hostname、通配符与 opaque Origin；补齐 Connect、workspace、credential action、bind 和当前 Agent 签名头。校验预检方法/请求头，追加 Vary 而不覆盖既有值，错误保留 no-store 和浏览器可读 Retry-After，不暴露 Set-Cookie。
- Agent `7e85a9eded915fee4db8f28257be9542a0f8cf24` 以 SDK `IsWireError` 区分原生凭据拒绝与代理 HTML 状态映射；代理失败保留 pending/凭据、受控重试，真实原生 Unauthenticated/PermissionDenied 仍停止主动续期。入网/刷新客户端不添加 Bearer 或 Cookie。Server 专项 pin 同轮更新，Web/Proto 无 wire/消费变化。
- 本地生产路由 race 场景和 security/api/connect 完整 race 回归已通过；Agent Windows enrollment/clientcore 单测、代理错误恢复和双类原生拒绝回归及 go vet 通过。组合专项继续只跑本项目 entry E2E，并增加真实浏览器跨源请求、401/429 可读性与策略撤销。
- 最终复查补齐通用请求限流器的原生协议拒绝：RPC 与认证拒绝共用 ErrorWriter，REST 保留已有三字段 JSON。新增真实 TLS/HTTP2 客户端的 Connect、gRPC、gRPC-Web unary/服务器流六种组合，验证有预算成功、耗尽时原生 ResourceExhausted、Retry-After/no-store；旧实现六种中五种错误映射为 Unavailable。此补充只追加第三轮修复提交，不改写前序历史。

## 提交与远程证据

- 第一轮 `6ac30a65179b7540cb7c2a17c523da6a387356bc`：38/38 PR checks 通过；专项 run `43716276569` 的 static-and-unit、nginx-browser-agent 均成功，归档 entry-acceptance.json 的六项语义全部通过。该轮按 GitHub PR 默认测试合并结果，实际构建 SHA 为 `94c85761370317527712f0ab8ebd2f44ca7d722b`；第三轮起专项显式 checkout 候选 SHA 并在 provenance 阶段校验，常规 CI 继续检验 PR 合并结果。
- 第二轮 `57550010bc9188aeb10f3a055ee0b0ad6500418f`：38/38 PR checks 通过；专项 run `43717521372` 两个 job 均成功，七项语义通过，实际构建 PR 合并 SHA `71eb8fd68c493e28a38be0014f253dd1d77547b7`。旧候选的唯一 CI 失败来自外部 ipinfo.io 429 后旧测试空指针，HTTP GeoIP 测试已改为确定性协议响应并新增限流/坏正文拒绝断言，未跳过测试或放宽生产校验。
- 用户明确要求保留历史后，不再 amend/force-push，后续只追加；此前候选 `9e2c4875`、`1866837d`、`bf28f611` 已分别保留在 `codex/audit-history/` 远程分支。纯外部限流/环境波动先在 GitHub 重跑，确认实现或测试根因才追加修复。

三轮均不改变协议生成结果和数据库 schema。固定 Proto 包 `0.1.37`；生成器配置仍为
protobuf-go `v1.36.5`、connect-go `v1.18.1`、protobuf-es `v2.16.0`。
后续候选的最终 CI/E2E 证据单独登记，不通过改写已验收候选回填结果。

## 仍需环境验收

真实 Cloudflare 账户、生产配置/包摘要、1C512M 和生产等量负载尚无本次实测。本地/CI 的 Nginx HTTPS 不等于 CF 橙云验收；协议和登录回归通过也不代表其他已知 S0/S1 关闭。中间开发提交不作为生产安全重构完成版本发布。
