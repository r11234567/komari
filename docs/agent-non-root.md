# Agent 非 root / 非管理员运行

本文所述行为均对应 `komari-agent` 当前代码，标注了判定点文件位置，便于核对。

## 权限判定

| 平台 | 判定点 | 判定方式 | 非特权时的 `privilege_mode` |
| --- | --- | --- | --- |
| Linux | `core/capability/privilege_linux.go` | `os.Geteuid() == 0` | `PRIVILEGE_MODE_LINUX_NON_ROOT` |
| Windows | `core/capability/privilege_windows.go` | `windows.Token(0).IsElevated()` | `PRIVILEGE_MODE_WINDOWS_STANDARD_USER` |
| 其他 | `core/capability/privilege_other.go` | 恒为非特权 | `PRIVILEGE_MODE_OTHER` |

判定结果经 `capability.Detect()` 汇总，随心跳上报在 `AgentMetadata.capabilities`（见 `komari/report/v1/report.proto`），面板据此显示各能力可用性与受限原因。

## 非特权下被禁用的能力

以下能力在非 root / 非管理员时**一律不可用**，这是有意的安全设计：远程控制类操作以 Agent 进程自身身份执行命令，非特权进程若暴露这些入口，等于把一个无人值守的命令执行面交给控制端。

- `RemoteControl`（远程控制）
- `Execution`（远程命令执行）
- `Webssh`（Web 终端）
- `ServiceControl`（系统服务控制）

判定集中在 `core/capability/capability.go` 的 `remoteControlAllowed()`，并且在各调用点独立复核一次，构成纵深防御：

- `server/execution.go:27`
- `server/task.go:37`
- `terminal/terminal.go:36`、`terminal/terminal.go:63`
- `cmd/root.go:76`（Windows 运行提示）

即便能力上报被篡改，执行入口仍会拒绝。

## 非特权下仍然可用的能力

- CPU / 内存 / 磁盘 / 网络 / GPU / 挂载点采集
- 通过 Connect-RPC 上报与配置同步
- Ping 探测（用户态实现，不需要原始套接字）
- 救援模式的诊断类动作（见下）

## 回程路由探测（唯一需要额外授权的常规能力）

内置 ICMP traceroute 需要发送原始 ICMP 包：

- Linux：root，或具备 `CAP_NET_RAW`。判定点 `core/capability/privilege_linux.go` 的 `returnRouteProbeState()`，读 `/proc/self/status` 的 `CapEff` 并检查 bit 13。
- Windows：需要已提升的服务令牌。
- 其他平台：不可用。

不满足时该能力上报为受限，附带原因字符串，**不影响其他监控**。

### 授予方式：优先用 systemd Ambient Capabilities，而不是 setcap

**重要**：如果没有关闭自动更新（`--disable-auto-update`），Agent 会在运行期替换自身二进制（`update.CheckAndUpdate()`）。`setcap` 是附着在**文件**上的，二进制一被替换，file capabilities 就丢了，探测会在某次自动更新后静默变成受限。

因此推荐由 systemd 在每次启动时授予，与二进制是否被替换无关：

```ini
[Unit]
Description=Komari Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=komari-agent
Group=komari-agent
ExecStart=/usr/local/bin/komari-agent --endpoint=https://dashboard.example.com --token=YOUR_TOKEN

# 仅为回程路由探测授予原始套接字权限；不需要该功能时整段删除
AmbientCapabilities=CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_RAW

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/komari-agent
StateDirectory=komari-agent

Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

注意 `NoNewPrivileges=true` 与 `AmbientCapabilities` 可以共存：ambient 能力在 exec 时已在进程上，不属于"提权"。

如果确实要用 `setcap`（例如不用 systemd 管理），则必须同时关闭自动更新，否则权限会在更新后丢失：

```bash
sudo setcap cap_net_raw+ep /usr/local/bin/komari-agent
getcap /usr/local/bin/komari-agent
```

## 救援模式与权限

救援能力由**独立的特权 helper 二进制** `cmd/komari-agent-rescue` 承担，与主 Agent 进程分离。

- 所有变更类动作（关机、重启、网络隔离各模式、恢复网络、回滚在线配置）在 `rescue/actions_linux.go` 中逐个由 `isPrivileged()` 门禁把守，非特权时返回明确错误而不是部分执行。
- 诊断类动作不需要特权。
- 隔离状态文件默认 `/etc/komari-agent/rescue-isolation.json`，可由 `ActionConfig.IsolationStatePath` 覆盖；目录 `0700`、文件 `0600`。非 root 环境若要运行 helper，必须把该路径指到进程可写的位置。
- 动作不接受来自控制端的参数（`rescue/actions.go` 中 `ExecuteAction` 显式拒绝 `arguments`），控制端无法借此拼出任意命令。

## 创建专用用户

```bash
sudo useradd -r -s /usr/sbin/nologin komari-agent
sudo install -d -o komari-agent -g komari-agent -m 0750 /var/lib/komari-agent
```

## 核对运行状态

```bash
# 进程身份
ps -o user,group,cmd -p "$(pgrep -f komari-agent)"

# 实际生效的能力位（授予 CAP_NET_RAW 后 CapEff 应包含 bit 13 / 0x2000）
grep -E '^(Uid|Gid|CapEff|CapAmb)' "/proc/$(pgrep -f komari-agent)/status"
```

面板侧在节点详情的能力区块可以直接看到 `privilege_mode` 与各能力的受限原因，这是判断"是不是权限问题"最快的入口。

## 测试覆盖

`core/capability/capability_test.go` 覆盖了非 root / Windows 标准用户 / Windows 管理员三组 fixture，断言非特权时不得声明远程控制能力；`komari-agent` 的 `build.yml` 在 ubuntu 与 windows 两个 runner 上运行 `TestPrivilegeFixturesDeclareLimitedCapabilities`。
