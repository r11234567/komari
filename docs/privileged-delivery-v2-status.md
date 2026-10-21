# 阶段 0–5 与前端交付状态

2026-10-21。开发分支已经实施阶段 0–5，并提前修复两处前端。所有 PR 仍为草稿；尚未合并、发布或部署，FRA 生产服务与历史任务未修改。

| 仓库 | 草稿 PR | 本轮职责 |
| --- | --- | --- |
| komari-proto | https://github.com/r11234567/komari-proto/pull/2 | v2 完整签名/授权/观察协议；追加 InitializeNodeConfig 和 installation_baseline |
| komari | https://github.com/r11234567/komari/pull/12 | 原子新节点基线、旧节点隔离、身份默认值精确绑定、内嵌新版 system UI |
| komari-agent | https://github.com/r11234567/komari-agent/pull/36 | 可信安装/迁移/host 工具；基线及同身份批准应用；有效配置、分项入口、单写者、回执重试 |
| komari-web | https://github.com/r11234567/komari-web/pull/10 | 部署对话框与特权配置页复用权威状态组件；2FA 前重查；真实 applied 与初始化提示 |

三消费者固定同一新 proto：`cb1f5e8abec38b5983b22384e8e234e05de5c531`。Go 版本为 `v0.1.38-0.20261021121746-cb1f5e8abec3`，Web package/lock 均为 immutable git commit，无本地 replace。后端 web/public/systemUI/dist/source.json 记录 Web head、CI 实际 merge commit、proto、lock SHA 和构建运行链接。

阶段 3 已建立 Linux systemd 的 root-owned daemon/host binaries、manifest/policy、独立 service-owned state 和 root journal。host diagnose、migrate、update 已实现；config show 和 status privileged 正式可用，默认与 daemon 共用服务定位。迁移保留 endpoint、账户、自定义服务名/状态路径、凭据和已有 revisions，不重注册或自动重启。fresh install 暂用标准 state 目录，自定义 state overrides 明确拒绝；非 Linux systemd 的新安装、身份转换和 helper 管理明确 unsupported。部分写入后的修复仍需检查 journal/备份；完整可恢复 host transaction 属阶段 6。

阶段 4 首次 daemon 连接先协商服务与真实身份，只有服务端新建且无历史的 eligible 节点才创建 runtime r1 / privileged r1。本机 immutable proposal 固定重试；签名基线必须精确匹配 root-owned installation policy。各快照落盘加载后再 ACK；期间 applied 0 表示等待确认。旧节点、已有 r4、旧 pending r1 不被自动清零或补写成功。

阶段 5 同身份目标在新鲜 2FA 批准、完整 v2 签名及在线当前性检查后，由 daemon 应用。执行、WebSSH、文件操作、监控和 GPU 读取同一有效配置；普通账户只在现有 OS 身份内远控。bootstrap 不盖已应用快照，protected force-deny 优先；未知/未实现 runtime 字段拒绝。撤权阻止新任务，并在本机收到后约 500 ms 内取消受管理任务与会话；服务端 Watch 约 20 秒轮询，服务端先拦新调度。进程锁与 stale-disk 检查阻止旧快照覆盖；CLI、旧脚本、rescue 直接写 daemon state 的入口关闭。快照/回执原子持久化，重启重发原 ID/payload，拒绝证据保留。

保留阶段 0–2 的约束：Save CAS 包含 0；接收不批准；CompleteManualUpgrade/旧终态报告不能完成 v2；host LOCAL_APPLIED 也不等于 delivered；current applied 只由匹配的 daemon observation 推进。历史错误任务必须保留并 supersede，不能补造 confirmed_at 或成功。

最近已验证的源码 CI（后续纯文档更新不改变这些源码）：

| 验证 | GitHub run | 结果 |
| --- | --- | --- |
| Proto lint/breaking、Go/TS 生成与 hash | https://github.com/r11234567/komari-proto/actions/runs/39598492668 | 通过 |
| Web npm/test/lint、普通主题及 system UI 两套构建 | https://github.com/r11234567/komari-web/actions/runs/39599525133 | 通过 |
| 阶段 3–5 agent、真实 systemd 安装/迁移、受保护策略 + 签名批准、丢 ACK、所有原缺陷、跨平台 daemon/host 编译、Windows 实际持久化/锁 | https://github.com/r11234567/komari-agent/actions/runs/39604002086 | 通过 |
| Agent CodeQL | https://github.com/r11234567/komari-agent/actions/runs/39604002039 | 通过 |
| Backend 新 proto/基线/状态机/分项判权及前端嵌入 | https://github.com/r11234567/komari/actions/runs/39603688754 | 通过 |

本机只做格式、脚本语法和产物校验，没有运行完整构建/重测试。所有重测试放 GitHub CI，always 上传 JSONL、人类日志、编译 stderr、退出码和失败索引，保留 30 天。历史失败 artifacts 已下载到工作区 staging/komari-privilege-audit-20261021/ci/；阶段 5 原 6 项 agent 预期失败已全部提升为必须通过的正式测试，不再把失败算成功。

接下来的阶段 6 才实现本机系统认证、服务身份/权限档案转换、按需救援 helper、独立管理员信任根及完整主机事务/恢复。当前协商 supported host actions 为空，komari-host apply 明确报未实现；前端不能显示一个能完成这些动作的旧 sudo upgrade 命令。Windows/macOS/FreeBSD 跨编译不等于平台特权安装验收。阶段 7 还需随 host 动作补最终操作 UI/实时身份 IPC；阶段 8 才做 FRA 灰度与生产任务迁移。
