# Komari 特权配置修复实施清单

日期：2026-10-21。本文承接 README.md 的审计结果和 REMEDIATION.md 的设计，确定开发阶段、代码边界、验收门槛和发布顺序。阶段 0–2 已在开发分支实施并由 GitHub CI 验证；阶段 3–8 仍为后续工作，其中 komari-host 命令尚未实现。生产配置未修改。

## 交付约定

- 四库分工固定：komari-proto 管协议；komari 管审批、授权和服务端状态；komari-agent 管 daemon、本机管理工具、安装器；komari-web 管展示和操作。
- 在 komari-agent 仓库新增独立二进制 komari-host，不新增第五个仓库。它由管理员安装、按需运行，不常驻，不向普通 agent 提供免认证提权入口。
- 首次安装普通 agent 和 komari-host，救援 helper 按需安装。不能把首次安装的管理工具与后续安装的救援服务混为一谈。
- 密码仅进入操作者本机的系统认证通道，不进入 daemon、WebSSH、配置下发、RPC、argv 或日志。面板 2FA 与主机系统认证各管自己的授权边界。
- Linux systemd 为首个开放平台；Windows/macOS 分平台验收后开放。其余 init 未实现时明确返回不支持，不输出成功。
- 全链路以协议能力 privileged_delivery_v2 协商放行。仅升级前端或二进制版本号，不算支持。
- 本次保留救援与普通远控互斥规则；不要顺带修改产品策略。

## 阶段 0：固定回归用例和迁移边界

**修改范围：komari、komari-agent 的测试和设计约定。**

1. 将本目录审计用 overlay 测试整理为两个仓库正式回归测试，保留行为断言；测试名称标明需要满足的规则，不保留“故意失败”为发布结果。
2. 固定四个首要回归：agent 接收不改变审批；sudo CLI 不等于服务已提权；默认 CLI 与 daemon 定位到同一服务；关闭执行但开启 WebSSH 时各入口分项判权。
3. 固定升级不变量：保留 node ID、凭据、endpoint、自定义服务名/目录/状态文件名和已有 runtime revision；不得重新注册。
4. 给现有命令及 runtime proto 字段做清单：有读者、bootstrap 专用、已被面板接管、无实际读者、平台不支持。后续按清单清理，避免一次删掉旧服务启动所需参数。

**验收：** 现有问题在回归测试中可复现，已有正常监控/配置测试继续通过；确定每项缺陷归属后进入阶段 1，不要求此阶段把新测试全部修绿。

## 阶段 1：先定协议，拆开审批、主机动作、实际生效

**修改范围：komari-proto。**

现有 config.proto 追加兼容字段；新增 komari/host/v1/host.proto，专门描述结构化主机任务，禁止携带任意 shell 或任意下载 URL。

| 数据 | 需要的内容 | 写入者 |
| --- | --- | --- |
| 配置目标 | revision、canonical config hash、desired settings | 面板保存，由服务端落库 |
| 审批 | approval status/generation、approver、confirmed_at、有效期、superseded | 服务端 |
| 传输 | received_revision、received_at、update_sequence | agent 接收回执；服务端分配序列 |
| 主机任务 | operation ID、node/service ID、revision/hash、允许动作、目标身份/权限档案、nonce、签发/过期时间、签名 | 服务端批准后生成 |
| 主机结果 | operation ID、journal phase、平台错误、实际服务观察、可重试回执 ID | komari-host |
| daemon 观察 | applied revision/hash、真实 daemon 身份、effective capabilities、观察时间、启动实例 ID | 正在运行的 daemon |
| 节点摘要 | 当前 desired 对象、当前 applied 对象、审批/任务/观察状态、update_sequence | 服务端直接返回 |

具体接口约定：

1. Watch 增加 after_sequence；配置内容不变但批准变化也必须推送。保留旧 after_revision 读取兼容，不允许旧接口写新版任务。
2. 增加节点配置摘要读取接口 GetNodeConfigState；不要让客户端分页扫描历史猜 applied。
3. 主机任务接口包含 PrepareHostOperation、ReportHostOperation、GetHostOperation。Prepare 在线检查当前授权并领取有界执行租约，绑定 operation ID；相同 ID 重试幂等。它不能授予超出面板批准的动作。
4. nonce 在面板确认成功后生成。签名覆盖整个批准包，不能只签 settings/plan 而漏掉批准代次、目标服务、动作和有效期。
5. 主机动作拆为 SET_SERVICE_IDENTITY、INSTALL_RESCUE_HELPER、REMOVE_RESCUE_HELPER；helper 的启停/租约另行表达，不能用 agent 提权动作代替 helper 安装。
6. 能力协商列明 v2、host tool、平台适配、helper 管理与支持的 runtime 字段。未知或未实现的字段明确拒绝，不允许忽略后 ACK。
7. 规范化哈希使用明确定义的规范表示，并在 Go/TS 用固定测试向量验算；不要依赖普通 JSON map 顺序或不同语言默认序列化。

**验收：** buf lint、breaking 检查、生成 Go/TS 代码；旧 enum 数字与字段语义不变；旧消息可解码；签名测试覆盖任一受保护字段被篡改。发布一个新 proto 版本，三个消费者锁定同一版本。

## 阶段 2：先修服务端状态机，封住旧链路错误写入

**修改范围：komari。**

主要落点：database/models/models.go、database/privilegeddelivery/privilegeddelivery.go、database/clients/deployment_profile.go、web/connect/privileged.go、web/connect/register.go；新增 host operation 的模型、存储与 handler。

1. 数据迁移增加审批字段、事件序列、当前 applied 指针、主机 operation 和 receipt 幂等记录；历史原数据保留。旧 confirmed_at=NULL 的任务不得推导成“已批准”。
2. Save 严格校验 expected revision，包括 expected=0；事务内建立新目标并 supersede 旧 pending/nonce。并发保存用 CAS/事务约束，不能覆盖别人的修改。
3. Confirm 完成新鲜 2FA 后原子批准当前目标并签发任务。同一批准的重试返回原结果，不重复延长有效期；过期后的重新批准增加 generation。
4. Report 接收事件只更新 received。daemon 不得写审批结果，未批准、过期、已取代、错误 hash 的回执不能更新 applied。
5. 主机任务结果只记录主机动作；需要重启的变更必须等新 daemon 报告目标 revision/hash、身份及必需能力，再推进 current_applied_revision。不要把 CLI 返回码 0 当 delivered。
6. operation 领取时检查当前目标和审批代次；相同领取重试不产生第二次动作。新目标取代正在执行的目标时保留操作日志，旧 daemon 回执不得把旧目标改成最新已完成。
7. “已应用配置”和“硬件/功能可用性”分开：GPU 无设备可记录配置已应用、GPU unavailable；服务身份不符合目标则不得完成身份变更。
8. WebSSH、execution 的服务端调度从已批准/已应用能力和本机限制判权；撤权目标生效前先阻止新增相关任务，防止迁移窗口继续扩大访问。
9. 旧 agent 的监控、已有普通配置继续兼容；禁止其旧 Report/Complete 把新版任务改成成功。UI 明确提示特权流程需要升级。

**验收：** 审计中的 10 个后端失败场景修绿，并覆盖先接收后 2FA、重复确认、0 版本冲突、过期/重放、旧报告覆盖终态、分页之外的当前 applied。此阶段可独立发布，但新版特权操作入口保持关闭。

## 阶段 3：建立可信安装布局和 komari-host，迁移旧安装

**修改范围：komari-agent 安装器、发布构建、路径解析和新管理工具。**

主要落点：cmd/service_config.go、cmd/root.go、install.sh、install.ps1、update/update.go；新增 cmd/komari-host/main.go、internal/hostcmd/、core/hostops/ 和平台适配层。发布流水线同时构建 daemon 与 host tool；现有脚本退出特权执行路径。

Linux 新安装采用明确布局：

| 路径示例 | 所有者/用途 |
| --- | --- |
| /usr/lib/komari/komari-agent、komari-host | root 拥有，父目录也不可被 komari 写；程序和管理入口 |
| /etc/komari/service.json、trusted-keys、host-policy | root 拥有；服务定位、信任根、本机授权策略 |
| /var/lib/komari/agent/ | komari 拥有；凭据、普通配置、有效配置快照、候选任务、outbox |
| /var/lib/komari-host/ | root 拥有；事务日志、服务备份、host profile、管理回执 |
| /run/komari/ | 受控 IPC；只读诊断与配置通知，不能请求无认证提权 |

路径可定制，但信任边界不可变：普通 agent 无法替换随后会被管理员执行的程序或信任根。root 工具读取 agent 候选时按不可信输入处理，验证签名、限制大小、拒绝路径穿越/符号链接；复制进 root 所有事务目录后再使用。

旧安装迁移流程：

1. 从真实服务管理器读取 unit/服务名、ExecStart、账号、工作目录，解析真实二进制和显式状态路径；不要从 sudo 后的 HOME 猜。
2. 用受限备份保留原服务和数据，生成带版本的 service manifest；同时修正程序及父目录所有权。自定义路径保留，不强行重命名状态文件。
3. daemon 与所有 CLI 共用同一解析器：显式服务选择/路径优先于已安装清单；独立运行模式最后才使用用户默认路径。多服务不能自动猜一个。
4. 已安装模式找不到清单时诊断“需迁移”，不能静默读空文件然后宣称无任务。
5. 安装器自动附加的 disable 参数与管理员 force-deny 分开。能证明来自旧安装器时迁移到 bootstrap 元数据；来源不明则在本机迁移预览中由管理员明确保留/解除。
6. 普通自更新不能再直接写 root 程序目录。面板下发更新意图可下载暂存；实际替换由本机管理员运行 komari-host 的 update 子命令，并校验发布签名/版本哈希。不添加常驻免密更新后门。
7. 清退可伪造 --verified、Perl 改 JSON、密码 argv、按字符串替换 unit 的旧路径。兼容入口只定位并调用可信 host tool，不执行 agent 可写脚本。

**验收：** 空机安装、自定义服务/路径、符号链接、旧安装无清单、多个服务、权限错误均有覆盖；普通 daemon 修改管理程序/信任根失败；迁移前后 node ID 和 runtime revision 不变。可安装工具，但暂不开放身份转换。

## 阶段 4：首次上线自动形成 runtime r1 与特权基线 r1

**修改范围：komari-proto enrollment/report、komari enrollment/clients、komari-agent cmd/login.go、core/enrollment、clientcore 和配置存储。**

首次上线的固定时序：

1. 管理员安装时写入受保护的本机策略与服务清单，说明当前身份、已安装组件、允许范围和本地强制禁止项。
2. 登录协商时 daemon 上报安装基线及实际观察。服务端对新节点幂等创建 runtime r1 和 privileged baseline r1；重复轮询不产生 r2。
3. 基线只能覆盖当前安装已经授权的范围，默认从严。未安装 helper 就是未安装，不能因面板默认值写成已启用；单纯建立基线不自动批准新的 root 转换。
4. daemon 原子持久化并加载基线，报告 revision/hash/身份/有效能力。服务端收到合格 ACK 后将两条 applied 置为 1。
5. ACK 前显示“基线初始化中”；若本机实际不满足，显示差异并保持未确认，不能伪造 applied=1。
6. 已有节点走迁移，保留各自版本，不拿“首次 r1”逻辑重置 FRA runtime r4 或覆盖其旧特权 r1 历史。

**验收：** 新节点正常完成登录后为 runtime desired/applied=1/1、privileged desired/applied=1/1；中断重试无重复版本；ACK 丢失可补发；不能通过伪造基线扩大本机授权。

## 阶段 5：统一有效配置和持久化回执，修掉开关无效

**修改范围：komari-agent；配合 komari 分项判权。**

主要落点：core/runtimeconfig/store.go、core/privileged/store.go、core/capability/、clientcore/client.go、server/execution.go、terminal/terminal.go、monitoring/monitoring.go、monitoring/unit/ip.go。

1. 新增 EffectiveConfig，合并普通配置、已签名批准、root 管理的 host policy、实际 OS 限制；worker、监控上报、终端、执行入口读同一份对象。
2. WebSSH=master && webssh && local-policy && actual-permissions；execution 使用自己的子开关。同权限级的已批准变更可由 daemon 应用，无需重复 sudo；未批准候选只保存。
3. GPU 采集真正读取有效开关；get_ip_addr_from_nic 去掉 flags OR snapshot 的“只能开不能关”。协议未实现字段返回明确错误，不推进 applied。
4. 启动参数只作 bootstrap；面板接管字段以受支持的已应用快照为准；本机 force-deny 单独优先。有效配置给出每个关键字段的来源和阻止原因。
5. 同一 daemon 状态文件只有 daemon 写。host tool 只写自己的 root journal/profile，并通过受控通知触发 daemon 重读；CLI 不再与 daemon 同时修改 privileged.json。daemon 使用进程间锁禁止同服务多实例写状态。
6. 回执 outbox 持久化后再发送，按 receipt ID 幂等重试。重启、断网、CLI 退出不能丢失确认；旧回执不得覆盖新快照。
7. 撤权立即阻止新任务，按既定会话策略处理旧连接；回滚若扩大能力，要求新批准。消除反复 rollback 可无认证重新启用的漏洞。

**验收：** agent 审计回归修绿；WebSSH 开/执行关和反向组合在服务端、领取、最终执行三个位置一致；远端关闭网卡选项有效；重启和断网后快照与回执不丢。

## 阶段 6：完成本机身份转换与按需安装救援

**修改范围：komari-agent 的 komari-host、平台适配、rescue；komari host operation/回执处理。**

待实现的用户入口：`sudo komari-host apply --revision N`。nonce 不放在命令行。工具按受保护的清单定位节点与服务，读取已签名任务并向服务端校验当前性。root 自己的 euid 只表示有操作 OS 的权限，不代表目标服务已是 root。

### 6A：Linux systemd 身份转换

1. 查询真实 daemon PID/UID、unit、capability bounding、NoNewPrivileges 和设备/组权限；生成结构化 diff。
2. 使用独立 Linux PAM service 做新鲜本机认证，包含 account 检查；口令通过本机终端 conversation 交给 PAM。sudo 缓存和 euid=0 不替代这一步；无需把密码送给 agent。
3. 在线领取 operation 执行租约；验证批准代次、签名、服务、动作、hash、有效期和未被取代状态，禁止重放。
4. 获取主机操作锁，写 durable journal 和旧 unit/profile 备份；按明确权限档案生成 unit/drop-in，不做字符串删参数。
5. 迁移必要状态目录 owner/ACL，原子写 host profile，重启目标服务。root 档案也必须检查 capability/NNP，不能只改 User=root。
6. 验证新 PID/UID、服务档案及 daemon 加载结果；新 daemon ACK 后服务端完成。网络断开时本地明确显示“本机已应用，待主控确认”。
7. 启动失败恢复旧 unit/profile/数据访问权限并验证；崩溃/断电从 journal 继续或恢复。回执持久化重试。

### 6B：救援 helper 按需安装

1. 首次安装不装完整 helper。面板批准 INSTALL_RESCUE_HELPER 后，仍运行同一个 host apply 入口。
2. 版本固定到受信发布清单。可由 agent 预下载，也可 host tool 下载；host tool 必须独立检查发布签名、平台、版本、哈希并安全解包，不能执行缓存内的任意脚本。
3. 安装独立 root helper/服务，写专用配置、权限、受控通信与撤销规则。普通 agent 保持 komari，不为装 helper 而改成 root。
4. 分别观察 installed、running、authorized、lease-ready；helper 自己验证租约/动作权限，普通 agent 不能凭本地 IPC 命令获得任意 root 执行。
5. 关闭授权立即拒绝新救援动作；REMOVE_RESCUE_HELPER 通过已批准的本机操作停止并移除服务。没有网络时可用已验证缓存，开始扩大授权前仍要在线检查任务当前性。
6. journal 是恢复和故障诊断依据，systemd/journald/kernel 日志作补充。kernel 日志不参与判定“管理员已经授权”，日志缺失也不替代显式状态检查。

### 6C：Windows/macOS

- Windows：受保护目录和 ACL、SCM 服务身份转换、实际凭据验证与授权 API、原子替换/重启、失败恢复；弹出凭据框或 UAC 提权本身不能冒充额外要求的新鲜凭据验证。
- macOS：系统授权 API、launchd 转换、真实 UID/权限检测、Keychain/受保护目录的职责划分、重启与恢复。
- 两平台执行与 Linux 相同的签名任务、journal、回执协议。平台适配未验收前，上报 unsupported，不展示可完成按钮。

**验收：** 在真实服务环境测试 sudo 缓存、认证失败、root CLI/普通 daemon、CAP_NET_RAW 限制、启动失败、操作中断、过期/重放/取代、helper 未安装到可用及卸载。不得只用 mock systemctl 当作平台验收。

## 阶段 7：CLI 与两处前端一起收口

**修改范围：komari-agent cmd；komari-web；komari 内嵌 system UI。**

CLI 固定收口：

| 命令/参数 | 修改 |
| --- | --- |
| komari-agent config show [--json] | 正式实现；显示脱敏路径、来源、desired/approved/applied、真实 daemon 身份及差异；无 IPC 时标注磁盘快照 |
| komari-agent status privileged | 复用同一服务定位与状态读取，不能报告 CLI euid 冒充 daemon |
| komari-host apply --revision N | 唯一本机特权操作实现；按签名任务执行身份转换或 helper 安装 |
| komari-agent upgrade | 保留弃用兼容入口，给出/转交可信 host tool；不再含另一套提权实现 |
| update-conf | 从复制 Cobra Command 改为明确的兼容 alias，并给迁移提示 |
| runtime 启动 flags | 仅保留明确 bootstrap/standalone 语义，help 标注面板接管后的优先级 |
| force-disable 等本机禁止 | 独立保留，不能在迁移时无声删除 |
| 无读者参数 | 先显式弃用/兼容解析一个迁移周期，再删除；未知参数默认报错 |

config show 不输出 credentials、私钥、nonce 或完整授权包。环境布尔值 true/false 均可设置，非法值报错。

前端修改落点：src/components/admin/AgentDeploymentDialog.tsx、src/pages/admin/privileged-config.tsx，以及共用 connect/hooks。两处页面复用同一状态组件，避免再复制一套流程。

1. 从 GetNodeConfigState 直接取 desired 和 applied，明确分栏；旧 applied r0 显示“未确认初始基线”，不能把 desired r1 放到“当前生效”。
2. 以 update_sequence 订阅；断线重连先拉摘要。仅 awaiting-confirmation 显示 2FA。
3. 提交确认冲突后拉权威状态：已批准显示原批准结果；被取代/过期显示对应操作；不把状态冲突解释成验证码错误。
4. 仅需本机动作时展示对应平台 host 命令；同身份能力变更显示“等待 agent 应用”，不要求每次 sudo。
5. 展示主机动作进度与 daemon 最终确认，区分本地完成待回执、失败已恢复、设备不可用。倒计时按时/分或日期显示。
6. 不支持的平台/旧协议节点提示升级条件；未安装 helper 不显示已运行。命令文本与 CLI 实际 help/可执行行为对照验收。
7. 执行 build:system，并将产物嵌入 komari/web/public/systemUI/dist 后重建服务端发布物。浏览器实际静态资源版本要与 RPC 匹配。

**验收：** 两入口完整走通接收→2FA→本机动作→daemon 确认；刷新页面不丢状态；历史超过 5 条仍显示真实 applied；复制的命令确实存在且定位正确。前端构建和相关测试通过。

## 阶段 8：FRA 灰度，再扩大发布

开发依赖为 0→1→2→3→4→5→6→7→8；可以在同一协议定义下开发，但生产入口必须等依赖全部验收后打开。

发布顺序：proto 依赖 → 后端兼容迁移/旧写入限制 → agent 与 host tool/安装迁移 → 新 system UI 所在后端发布物 → 仅 FRA 开启 v2 → 其他 Linux 节点 → 已验收的 Windows/macOS。

FRA 执行单：

1. 受限备份服务定义、凭据和状态；安装已验证 daemon/host tool，补 service manifest 与目录权限。保留 UUID 247679f3-f4f0-4d3c-b98c-8dfa8c82edda、runtime r4 和原 endpoint。
2. config show 默认定位正确，显示真实服务账户 komari、runtime r4 和旧 privileged applied r0。此时不强行补“已应用 r1”。
3. 将旧任务 yzpvcPe1ktG8gIiiuwh0GBvJqKGwWxOO/r1 标为旧协议不一致并 supersede，失效旧 nonce；保留历史，不补造 confirmed_at。
4. 新建特权 r2：远控开、WebSSH 开、执行关、救援关、GPU 关、目标 root。新鲜 2FA 后进入等待本机操作，agent 提前接收不得改变审批。
5. 本机运行新命令 sudo komari-host apply --revision 2；认证、转换服务、重启并验证实际权限档案。
6. daemon 报告 privileged r2、runtime r4、真实身份及有效能力，主控收到后显示已应用。WebSSH 可用；执行在服务端与 agent 均被拒绝。
7. 重启后复验；测试断网补报、撤权与失败恢复。无新状态覆盖、无身份漂移、无配置回退后扩大灰度。

**最终交付条件：** 新安装自动基线 1/1；旧安装身份与配置不丢；正确 2FA 不再因接收竞态失败；默认查看命令正确；sudo 操作真正转换目标服务；helper 可按需独立安装；关闭的能力确实不可用；故障有可恢复记录，成功有 daemon 实际确认。

## 每阶段的审查材料

每个阶段提交应包含：涉及文件/接口、兼容和数据迁移行为、回归结果、平台支持矩阵、发布开关与恢复路径。当前交付包括阶段 0–2 的协议、后端修复、回归脚本和 CI；生产服务尚未修改。
