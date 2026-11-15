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

## 验收边界

真实 Cloudflare 账户、生产配置/包摘要、1C512M 和生产等量负载尚无本次实测。本地/CI 的 Nginx HTTPS 不等于 CF 橙云验收；协议和登录回归通过也不代表其他已知 S0/S1 关闭。中间开发提交不作为生产安全重构完成版本发布。
