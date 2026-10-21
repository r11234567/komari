# 阶段 6 连续 CI 失败复盘

2026-10-21。阶段 6 的 Linux/systemd 完整功能验收现已通过，阶段 6 通过后已进行 CLI 清理，另有针对性回归。所有工作仍在草稿分支，没有合并、发布或修改 FRA 生产服务。

连续失败不能作为正常交付结果。此前的工作方式是看到首个失败就修补并推送，未先检查整个恢复状态机与测试服务契约；因此出现了“修到下一个失败”的过程，甚至把不存在的 proto 字段推到了 CI。这是推送前检查和实现审查不足。

## 从日志确认的原因

| 证据 | 直接失败 | 根因和影响 |
| --- | --- | --- |
| Agent run 39609854256，tests.log | `encoding/json imported and not used` | 测试源码未经过编译预检，纯编译错误进入 CI。 |
| Agent run 39611291049，systemd 日志 | service 无法启动 | `WorkingDirectory` 按 ExecStart 参数加了引号，真实 systemd 将其视为路径内容；原有安装器测试没有执行足够的真实启动验证。 |
| Agent run 39612921579，daemon 日志 | 已重启但不能 ACK | fixture 的 HTTP 传输与真实 daemon 不匹配，不能证明 daemon 的完整请求链路正常。 |
| Agent run 39615144821，helper 观察 | helper 运行但确认不前进 | 非 root daemon 不能直接 readlink root helper 的 `/proc/PID/exe`。应结合 root 拥有的 heartbeat、内核 UID、cgroup 与 PID start ticks 判断，而不能把权限拒绝当作未运行。 |
| Agent run 39616066308，host-journald.log | `host-bootstrap.json: permission denied` | 备份只保存 mode，恢复生成 `0640 root:root`，丢失原服务组，导致恢复后的普通账户无法读配置。属于真实恢复实现缺陷。 |
| Agent run 39616517883，host-journald.log | `404 Not Found`，reconciliation 被取消 | fixture 缺少 SubmitMetrics，daemon 主循环退出并取消 ACK worker；先前偶然 ACK 成功依赖竞态，不能作为稳定验收。 |
| Agent run 39618134583，build output | `SubmitMetricsResponse` 无 `Accepted` 字段 | 修补 fixture 时猜测了 proto 类型，未查生成代码和编译。实际字段是 `accepted_sequence` / `accepted_points`。 |

上述日志与 artifacts 保存在工作区 `staging/komari-privilege-audit-20261021/ci/`。GitHub artifacts 保留 30 天；本机下载没有设置自动删除。

## 整条链路复查发现的额外缺陷

1. **恢复状态没有先落盘。** 原来只在恢复完成后写 `restored`。如果恢复过程中再次中断，journal 可能仍是 `local_applied`，重试便可能走原目标的成功路径。现在开始恢复前先持久化 `restoring`，此后只能继续恢复。
2. **恢复回执不是稳定幂等的。** 显式 restore 会清空待发送回执，包括之前已发送但丢响应的 RESTORED 回执。现在恢复回执保留原 ID/原 payload；被恢复动作作废的旧成功回执单独保留为审计证据，不再重发。
3. **仅恢复旧文件清单不够。** 目标 daemon 可以新建或替换状态文件。现在停止服务后重新封闭并复制状态 inode，分别保留原始和恢复期间的权限记录；新文件继承原 state root 的账户，以限制性 mode 恢复访问。
4. **daemon 的旧成功回执可能在 OS 恢复后重发。** 现在发送 host observation 前检查当前身份、受保护 policy 和 root-owned applied record。确认失效的回执保留在 rejected history；临时读取错误只重试，不丢回执。
5. **服务端可能先收到 daemon ACK、后收到恢复回执。** 现在允许该 operation 记录 RESTORED，将当前目标标记失败，保留真实历史 applied 指针，阻止恢复前的新成功回执及新的远程调度。不会补造一个“已应用的旧版本”。
6. **数据库故障被当成任务不存在。** GetHostOperation 原来所有读取错误都返回 NotFound，不能用来判断未领取任务。现在只有真实无记录返回 NotFound，其他错误返回 Internal。未领取且未修改本机的 journal 才能按这个明确结果归档。
7. **主机工具会遗漏连接参数和工作目录。** Cloudflare Access 使用独立 root-owned JSON 文件，并传给 host RPC；helper 也读取该本机配置。服务 WorkingDirectory 只允许受保护目录或本次封闭的 state root，并保留原路径，避免丢失相对路径下的流量数据。

journal 独立升级到 schema 3：加入文件 UID/GID、原服务 UID、恢复权限记录等。它不改变 v2 wire protocol。之前开发分支产生的 schema 2 journal 缺少完整元数据，明确拒绝猜测恢复；没有生产迁移这个未发布格式的动作。

## 验证与交付门槛

- 编译先行：`scripts/ci/check-stage6-compile.sh` 编译 host、daemon、helper 相关生产包和测试，不执行主机变更。单并发、96 MiB Go 内存目标，适用于当前小机器。
- 本机只执行内存文件/临时目录回归。签名绑定、发布签名、恢复回执稳定性、新文件权限和失效 host observation 等 12 项测试通过；完整 Go 包测试和 PAM/systemd 场景交 CI。
- 真实 CI 必须依次通过：认证失败且 root 不绕过、身份提升/降权、daemon 独立确认、UUID/runtime r4 不丢、重放/过期拒绝、helper 安装/撤权/卸载、启动失败恢复、恢复文件 UID/GID 与工作目录、回执丢失、实际 kill 后恢复。
- 审计 workflow 对开发分支保留 PR 触发，移除同一 push 的重复触发。失败 logs、journald/kernel 补充日志、脱敏 journal 元数据始终上传，保留 30 天。
- 验收全部通过之前不称阶段 6 完成，不修改 CLI，不做 FRA 灰度。Windows/macOS/FreeBSD 仍明确 unsupported；跨编译不能代替平台特权事务验收。

四库继续固定同一 proto `e575defe19425b8d79f08457c5262f3dd51e32ee`，不能根据猜测添加生成代码不存在的字段。

## 最终验证结果

- Agent 源码 `9fe5463d6de43b451ebae25106b56ad4bc807b7c`：完整审计 https://github.com/r11234567/komari-agent/actions/runs/39625269490 全部通过。真实 PAM/systemd 事务用例运行 71.54 秒，通过身份提升/降权、helper 生命周期、启动失败恢复、丢回执、真实进程 kill，以及最终 daemon ACK 响应丢失后的本机恢复。Windows 状态测试、跨编译及全部原审计回归也通过。
- Backend 源码 `57295591c06dd60755e4a8bda8956396e6f0e801`：完整回归 https://github.com/r11234567/komari/actions/runs/39623198814 与 CodeQL 39623202093 通过。
- Web 两套入口构建/测试：39616080499 通过；已校验 lock hash 并嵌入 backend。Proto：e575defe19425b8d79f08457c5262f3dd51e32ee，四库一致。
- Agent CodeQL 39625269483 的 Go、Actions 两项 job 均成功；查询时 workflow 汇总仍显示 in_progress，未将其写成 workflow 已完成。

复审后的 run 39623176477 又发现并确认了 systemd `start-limit-hit`：故障注入耗尽启动计数，修复 unit 后仍不能重新启动。现由经过本机认证的事务对目标服务执行一次 reset-failed，再重启并核验；不移除正常运行期间的系统限速。此项修复已包含在上述最终通过源码和完整验收中。该失败 journald/services 日志也已下载保存。

完整通过日志位于 `ci/39625269490.log`，逐项本机预检与 CI 证据位于 `stage6-review-evidence.json`。阶段 6 文档已更新为验收结果；CLI 清理在上述完整验收之后单独进行，不混入根因修复提交。
