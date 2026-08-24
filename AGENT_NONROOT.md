# Agent非Root运行指南

## 概述

komari-agent支持以非root用户运行，在Linux系统上会自动检测并适配权限级别。本文档说明非root运行的能力和限制。

## 权限检测机制

### 自动检测

Agent启动时会自动检测运行权限：

```go
// core/capability/privilege_linux.go
func privilegeState() (reportv1.PrivilegeMode, bool, string) {
    if os.Geteuid() == 0 {
        return reportv1.PrivilegeMode_PRIVILEGE_MODE_LINUX_ROOT, true, ""
    }
    return reportv1.PrivilegeMode_PRIVILEGE_MODE_LINUX_NON_ROOT, false, 
           "running as a non-root Linux user; privileged service control is unavailable"
}
```

检测结果会在每次心跳上报中包含：
- `PRIVILEGE_MODE_LINUX_ROOT`: root用户
- `PRIVILEGE_MODE_LINUX_NON_ROOT`: 非root用户
- `PRIVILEGE_MODE_WINDOWS_ADMINISTRATOR`: Windows管理员
- `PRIVILEGE_MODE_WINDOWS_STANDARD_USER`: Windows标准用户

## 非Root功能支持

### ✅ 完全支持（无需特殊权限）

| 功能 | 说明 |
|------|------|
| **基础监控** | CPU、内存、磁盘使用率、网络流量统计 |
| **系统信息** | 主机名、OS版本、内核版本、架构 |
| **进程统计** | 进程数、TCP/UDP连接数 |
| **GPU监控** | GPU使用率、显存（需相应驱动支持） |
| **Connect-RPC通信** | 与服务端的所有通信 |
| **配置同步** | 接收并应用服务端配置 |
| **Ping探测** | ICMP ping任务（使用用户态实现） |
| **任务执行** | 执行shell命令（权限受用户限制） |
| **WebSSH** | 远程终端（以当前用户身份） |
| **Rescue模式** | 紧急救援功能（独立helper进程） |

### ⚠️ 受限功能（需要CAP_NET_RAW）

#### Return Route探测（ICMP traceroute）

**功能**: 内置的ICMP路径追踪，用于网络诊断

**限制**: 需要`CAP_NET_RAW`权限才能发送原始ICMP包

**检测逻辑**:
```go
// core/capability/privilege_linux.go
func returnRouteProbeState() *reportv1.CapabilityState {
    if os.Geteuid() == 0 {
        return available()
    }
    // 检查/proc/self/status中的CapEff字段
    status, err := os.ReadFile("/proc/self/status")
    if err == nil {
        for _, line := range strings.Split(string(status), "\n") {
            if !strings.HasPrefix(line, "CapEff:") {
                continue
            }
            value, parseErr := strconv.ParseUint(
                strings.TrimSpace(strings.TrimPrefix(line, "CapEff:")), 16, 64)
            if parseErr == nil && value&linuxCapabilityNetRaw != 0 {
                return available()
            }
            break
        }
    }
    return limited("built-in ICMP return-route probes require root or CAP_NET_RAW")
}
```

**降级行为**: 如果没有权限，return route探测功能会标记为`limited`，但不影响其他监控功能

### ❌ 完全不支持（需要root）

| 功能 | 说明 | 替代方案 |
|------|------|----------|
| **特权服务控制** | systemctl等系统服务管理 | 使用sudo或以root运行 |
| **低级网络操作** | 修改防火墙、路由表 | 配置sudo权限 |
| **挂载文件系统** | mount/umount操作 | 使用root权限 |

## 授予CAP_NET_RAW权限

如果需要return route探测功能，可以为Agent二进制授予`CAP_NET_RAW`权限：

### 方法1: 使用setcap（推荐）

```bash
# 授予komari-agent CAP_NET_RAW能力
sudo setcap cap_net_raw+ep /usr/local/bin/komari-agent

# 验证权限
getcap /usr/local/bin/komari-agent
# 输出: /usr/local/bin/komari-agent cap_net_raw=ep
```

**优点**:
- 仅授予必要的网络权限，安全性高
- 不需要以root运行整个进程
- 符合最小权限原则

**注意事项**:
- 每次更新Agent二进制后需要重新设置
- 可以在systemd service中自动化处理（见下文）

### 方法2: 使用sudo包装

```bash
# 创建sudo规则允许特定用户运行route探测
echo "komari-agent ALL=(ALL) NOPASSWD: /usr/local/bin/komari-agent-route-probe" | sudo tee /etc/sudoers.d/komari-agent

# 不推荐：允许整个agent以root运行
# sudo komari-agent --endpoint=https://... --token=...
```

**不推荐**：这会给予过多权限，违反最小权限原则

## Systemd Service配置

### 非Root运行（推荐）

```ini
[Unit]
Description=Komari Agent
After=network.target

[Service]
Type=simple
User=komari-agent
Group=komari-agent
ExecStartPre=/usr/bin/setcap cap_net_raw+ep /usr/local/bin/komari-agent
ExecStart=/usr/local/bin/komari-agent \
    --endpoint=https://your-dashboard.example.com \
    --token=YOUR_TOKEN_HERE
Restart=always
RestartSec=10

# 安全强化
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/komari-agent

[Install]
WantedBy=multi-user.target
```

**说明**:
- `User=komari-agent`: 以专用用户运行
- `ExecStartPre`: 启动前自动设置CAP_NET_RAW
- 安全强化选项限制了Agent的系统访问

### 创建专用用户

```bash
# 创建系统用户（无登录权限）
sudo useradd -r -s /bin/false komari-agent

# 创建数据目录
sudo mkdir -p /var/lib/komari-agent
sudo chown komari-agent:komari-agent /var/lib/komari-agent
```

## 能力检测和上报

Agent会将自身权限状态上报到Dashboard：

### 上报内容

```json
{
  "metadata": {
    "capabilities": {
      "privilege_mode": "PRIVILEGE_MODE_LINUX_NON_ROOT",
      "return_route_probe": {
        "available": false,
        "limitation": "built-in ICMP return-route probes require root or CAP_NET_RAW"
      }
    }
  }
}
```

### Dashboard显示

Dashboard会根据上报的能力状态显示：
- ✅ 功能可用
- ⚠️ 功能受限（显示原因）
- ❌ 功能不可用

## 验证非Root运行

### 检查当前运行状态

```bash
# 查看Agent进程的用户
ps aux | grep komari-agent

# 查看有效用户ID
sudo cat /proc/$(pgrep komari-agent)/status | grep -E "Uid|Gid"

# 查看有效权限
sudo cat /proc/$(pgrep komari-agent)/status | grep Cap
```

### 查看Agent日志

```bash
# Systemd日志
sudo journalctl -u komari-agent -f

# 查找权限相关信息
sudo journalctl -u komari-agent | grep -i "privilege\|capability\|permission"
```

### Dashboard验证

1. 登录Dashboard
2. 查看Agent详情页
3. 检查"Capabilities"部分
4. 确认`privilege_mode`显示为`PRIVILEGE_MODE_LINUX_NON_ROOT`

## 故障排查

### Return Route探测不可用

**症状**: Dashboard显示return route功能受限

**检查步骤**:

1. 验证CAP_NET_RAW权限:
```bash
getcap /usr/local/bin/komari-agent
```

2. 如果没有权限，设置它:
```bash
sudo setcap cap_net_raw+ep /usr/local/bin/komari-agent
```

3. 重启Agent:
```bash
sudo systemctl restart komari-agent
```

4. 检查Agent日志确认能力已启用

### Agent无法启动

**症状**: Systemd service启动失败

**可能原因**:
1. 配置文件路径错误
2. Token无效
3. 网络连接问题
4. 文件权限问题

**检查步骤**:

```bash
# 查看详细错误
sudo journalctl -u komari-agent -n 50

# 手动运行测试
sudo -u komari-agent /usr/local/bin/komari-agent \
    --endpoint=https://your-dashboard.example.com \
    --token=YOUR_TOKEN_HERE

# 检查配置文件权限
ls -la /etc/komari-agent/
```

### 任务执行权限不足

**症状**: 某些任务执行失败，提示权限错误

**说明**: 这是预期行为。非root用户只能执行自己权限范围内的操作。

**解决方案**:
1. 为特定任务配置sudo规则
2. 或者为需要高权限的任务使用root运行的Agent实例

## 安全建议

### 1. 使用最小权限原则

✅ **推荐**:
- 以专用非root用户运行
- 仅在需要时授予CAP_NET_RAW
- 使用systemd安全强化选项

❌ **不推荐**:
- 直接以root运行（除非确实需要）
- 授予过多的sudo权限
- 共享系统用户

### 2. 定期审计

```bash
# 检查Agent的有效权限
sudo cat /proc/$(pgrep komari-agent)/status | grep Cap

# 查看最近的任务执行
sudo journalctl -u komari-agent | grep "execution"

# 审计sudo使用（如果配置了）
sudo grep komari /var/log/auth.log
```

### 3. 网络隔离

考虑使用防火墙规则限制Agent的网络访问：

```bash
# 仅允许访问Dashboard
sudo iptables -A OUTPUT -m owner --uid-owner komari-agent \
    -d dashboard.example.com -j ACCEPT
sudo iptables -A OUTPUT -m owner --uid-owner komari-agent -j REJECT
```

## Windows支持

Windows平台的权限管理不同：

- **标准用户**: 大部分功能可用，但某些系统信息受限
- **管理员**: 完全功能（需要UAC提升）

Windows Agent会自动检测并上报权限级别：
- `PRIVILEGE_MODE_WINDOWS_ADMINISTRATOR`
- `PRIVILEGE_MODE_WINDOWS_STANDARD_USER`

## 总结

komari-agent在非root环境下运行良好，仅在以下情况需要额外权限：

1. **Return Route探测**: 需要CAP_NET_RAW（可选）
2. **特权系统操作**: 需要root或sudo配置（根据具体任务）

对于大多数监控场景，非root运行已足够，并且更安全。

## 相关资源

- Agent权限检测代码: `komari-agent/core/capability/privilege_linux.go`
- Proto定义: `komari-proto/komari/report/v1/report.proto`
- Systemd示例: `komari-agent/scripts/komari-agent.service`
- 安全强化指南: `docs/SECURITY.md`（待创建）