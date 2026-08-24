# Komari RPC迁移指南

## 概述

Komari正在从传统的JSON-RPC 2.0 over WebSocket (RPC2)迁移到现代的Connect-RPC (gRPC-web兼容)。本文档描述迁移策略、当前状态和兼容性策略。

## 迁移策略

### 核心原则
- **主力生产使用Connect-RPC**：所有新功能和主要模块使用Connect-RPC
- **RPC2作为备用降级**：保留RPC2用于主题、插件和协议降级
- **渐进式迁移**：不破坏现有部署，平滑过渡

### 架构决策

**为何保留RPC2？**

1. **主题兼容性**：现有主题生态使用RPC2 API，需要时间改造
2. **插件生态**：第三方插件依赖RPC2，完全迁移需要下游配合
3. **降级容错**：网络不稳定环境下，WebSocket可作为Connect-RPC的备用方案
4. **渐进式迁移**：避免一次性破坏所有现有集成

**Connect-RPC优势**

- 类型安全的proto定义
- 更好的流式支持
- HTTP/2多路复用
- 标准化的错误处理
- 更容易的负载均衡和代理支持
- 更好的浏览器兼容性（gRPC-web）

## 当前状态

### ✅ 已完全迁移到Connect-RPC

| 模块 | Proto定义 | 服务端 | Agent客户端 | Web前端 | 状态 |
|------|-----------|--------|-------------|---------|------|
| **Rescue（救援）** | `komari/rescue/v1/rescue.proto` | `web/connect/rescue.go` | `rescue/helper.go` | 已集成 | ✅ 生产就绪 |
| **AgentReport（上报）** | `komari/report/v1/report.proto` | `web/connect/report.go` | 需添加 | N/A | ⚠️ 服务端完成 |
| **Metrics（指标）** | `komari/metrics/v1/metrics.proto` | `web/connect/metrics.go` | N/A | 已集成 | ✅ 完成 |
| **Execution（执行）** | `komari/exec/v1/exec.proto` | `web/connect/execution.go` | 需添加 | 已集成 | ⚠️ 服务端完成 |
| **NetworkProbe（探测）** | `komari/network/v1/network.proto` | `web/connect/network.go` | 部分完成 | N/A | ⚠️ 需完善 |
| **Config（配置）** | `komari/config/v1/config.proto` | `web/connect/config.go` | 需添加 | N/A | ⚠️ 服务端完成 |
| **Deployment（部署）** | `komari/deployment/v1/deployment.proto` | `web/connect/deployment.go` | 需添加 | N/A | ⚠️ 服务端完成 |
| **Browser（浏览器）** | `komari/browser/v1/browser.proto` | `web/connect/browser.go` | N/A | 已集成 | ✅ 完成 |
| **Plugin（插件）** | `komari/plugin/v1/plugin.proto` | `web/connect/plugin.go` | N/A | 已集成 | ✅ 完成 |
| **WebSSH** | `komari/webssh/v1/webssh.proto` | `web/connect/webssh.go` | N/A | 需迁移 | ⚠️ 服务端完成 |
| **AgentEvents** | 内嵌在各proto | `web/connect/agent_events.go` | 需添加 | N/A | ⚠️ 服务端完成 |

### ⚠️ 保留RPC2兼容（备用降级）

| 模块 | 原因 | 废弃计划 |
|------|------|----------|
| **主题（Theme）** | 现有主题生态依赖RPC2 API | 长期保留，鼓励新主题使用Connect |
| **插件（Plugin）** | 第三方插件生态，无改造能力 | 长期保留 |
| **Agent降级** | V2协议失败3次后降级到V1 | 保留作为容错机制 |
| **Legacy客户端** | 旧版Agent可能仅支持RPC2 | 保留直到旧版完全退役 |

### ❌ 需要迁移（生产不应再主力使用RPC2）

| 模块 | 当前状态 | 迁移优先级 | 预计工作量 |
|------|----------|------------|------------|
| **Agent主服务器** | 仍使用WebSocket V2 | 🔴 最高 | 3-5天 |
| **Admin Dashboard** | RPC2 (`admin:getDashboard`等) | 🔴 高 | 2-3天 |
| **Terminal（终端）** | WebSocket (`/api/clients/terminal`) | 🟡 中 | 2-3天 |
| **Remote（远程桌面）** | WebSocket (`/api/clients/remote`) | 🟡 中 | 2-3天 |

## 迁移路线图

### Phase 1: Agent核心通信切换（当前阶段）✅

**目标**：Agent主服务器使用Connect-RPC，RPC2仅作降级

**任务清单**：
- [x] 创建本迁移文档
- [ ] 在`komari-agent/server/`中添加connect-RPC客户端
- [ ] 重构`ReportClient`接口支持connect协议
- [ ] 实现connect→RPC2降级逻辑
- [ ] 更新GitHub Actions构建配置
- [ ] 验证非Root运行正常工作

**验收标准**：
- Agent默认使用connect-RPC上报
- Connect失败后能自动降级到RPC2
- 所有平台（Linux/Windows/macOS）构建成功
- 非Root用户运行正常（Linux CAP_NET_RAW检查正确）

### Phase 2: Admin Dashboard迁移

**目标**：管理员Dashboard完全使用Connect-RPC

**任务清单**：
- [ ] 创建`komari/admin/v1/admin.proto`定义
- [ ] 实现`web/connect/admin.go`服务端
- [ ] 更新`komari-web`前端调用connect-RPC
- [ ] 移除`admin:getDashboard`等RPC2绑定
- [ ] 添加兼容性测试

**涉及RPC2方法**（需替换）：
```
admin:getDashboard
admin:getDashboardCharts
admin:getDashboardAlertItems
admin:listClients
admin:getClient
admin:addClient
admin:editClient
admin:removeClient
...（约60个admin:*方法）
```

### Phase 3: Terminal & Remote迁移

**目标**：WebSSH和远程桌面使用Connect-RPC流式传输

**任务清单**：
- [ ] 在`komari/webssh/v1/webssh.proto`添加流式方法
- [ ] 在`komari/remote/v1/remote.proto`定义远程桌面协议
- [ ] 实现connect服务端流式处理
- [ ] 更新前端WebSocket连接为connect流
- [ ] 性能测试和优化

**技术难点**：
- 双向流式传输的连接管理
- 终端会话的低延迟要求
- 远程桌面的大数据量传输优化

### Phase 4: 文档和工具

**任务清单**：
- [ ] 编写主题开发者Connect-RPC迁移指南
- [ ] 提供RPC2→Connect转换工具/脚本
- [ ] 更新API文档
- [ ] 提供迁移示例代码

## Agent实现细节

### Agent协议选择逻辑

```go
// komari-agent/server/protocol.go (伪代码示意)

func (s *Server) UploadReport() error {
    // 1. 优先尝试Connect-RPC
    if err := s.connectClient.SubmitReport(ctx, report); err == nil {
        return nil
    }
    
    // 2. Connect失败，尝试WebSocket V2
    if err := s.wsV2Client.SubmitReport(ctx, report); err == nil {
        return nil
    }
    
    // 3. V2失败3次，降级到V1
    if s.v2FailureCount >= 3 {
        return s.wsV1Client.SubmitReport(ctx, report)
    }
    
    return err
}
```

### 非Root运行支持

**Linux CAP_NET_RAW检查**：

komari-agent在非root运行时会检查`CAP_NET_RAW`权限，用于：
- ICMP return-route探测
- 某些网络诊断功能

如果没有此权限，相关功能会优雅降级，但不影响基础监控。

**授予权限（可选）**：
```bash
# 授予komari-agent CAP_NET_RAW能力
sudo setcap cap_net_raw+ep /usr/local/bin/komari-agent
```

**权限检查代码**：参见`komari-agent/core/capability/privilege_linux.go`

## 主题开发者指南

### 如何判断是否需要迁移

**无需迁移**的情况：
- 仅使用公开API（`/api/public/*`）
- 仅读取静态配置
- 不涉及实时数据订阅

**需要迁移**的情况：
- 使用`/api/admin/*`管理接口
- 使用WebSocket实时数据
- 调用任务执行API

### 迁移步骤

#### 1. 检测服务端支持

```javascript
// 检测服务端是否支持Connect-RPC
async function detectRPCSupport() {
    try {
        // 尝试调用Connect-RPC端点
        const response = await fetch('/komari.metrics.v1.MetricsService/ListMetrics', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
        });
        return response.ok;
    } catch {
        return false;
    }
}
```

#### 2. 改造API调用

**旧方式（RPC2）**：
```javascript
// 使用JSON-RPC 2.0
const response = await fetch('/api/rpc2', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
        jsonrpc: '2.0',
        id: 1,
        method: 'admin:getDashboard',
        params: { sections: 'all' }
    })
});
```

**新方式（Connect-RPC）**：
```javascript
// 使用Connect-RPC
import { createPromiseClient } from '@connectrpc/connect';
import { createConnectTransport } from '@connectrpc/connect-web';
import { MetricsService } from './gen/komari/metrics/v1/metrics_connect';

const transport = createConnectTransport({
    baseUrl: window.location.origin,
});

const client = createPromiseClient(MetricsService, transport);
const metrics = await client.listMetrics({});
```

#### 3. 兼容性处理

```javascript
// 同时支持新旧两种方式
class KomariAPIClient {
    async getDashboard() {
        if (await this.supportsConnect()) {
            return this.getDashboardViaConnect();
        }
        return this.getDashboardViaRPC2();
    }
}
```

### Connect-RPC完整示例

参见：`docs/examples/theme-connect-migration/`（待创建）

## 插件开发者指南

**当前策略**：插件保持使用RPC2，**无需迁移**

原因：
1. 插件生态由下游项目和第三方维护
2. 我们无改造能力
3. RPC2将长期作为插件API保留

如果你是新插件开发者，建议：
- 优先使用Connect-RPC（未来趋势）
- 但可以继续使用RPC2（长期支持）

## 测试和验证

### 功能测试清单

- [ ] Agent使用Connect-RPC正常上报
- [ ] Connect失败能降级到RPC2
- [ ] RPC2失败能降级到V1
- [ ] 所有Dashboard数据正确显示
- [ ] Terminal连接稳定，无延迟
- [ ] Remote桌面流畅运行
- [ ] 旧主题仍能正常工作（RPC2兼容）
- [ ] 旧插件仍能正常工作（RPC2兼容）
- [ ] 非Root用户运行正常

### 性能测试

- [ ] Connect-RPC vs RPC2延迟对比
- [ ] 大量Agent并发上报测试
- [ ] 流式传输稳定性测试
- [ ] 内存和CPU使用率对比

## 废弃时间表

**明确说明**：由于komari是自用项目+下游项目，我们：
- **不具备议价能力**：无法强制下游/第三方迁移
- **不具备完全改造能力**：无法改造所有第三方主题/插件

因此：

### 长期保留（无废弃计划）
- 主题RPC2 API
- 插件RPC2 API
- Agent降级机制中的RPC2备用路径

### 主力生产废弃（但保留代码）
- Admin Dashboard RPC2（迁移到Connect后废弃）
- Terminal/Remote WebSocket（迁移到Connect后废弃）
- Agent主通信路径的RPC2（仅作降级保留）

**RPC2代码不会删除**，但会标记为`@deprecated`和`Legacy compatibility only`。

## FAQ

### Q: 为什么不完全废弃RPC2？
A: 因为主题和插件生态依赖RPC2，且我们无法强制下游迁移。RPC2将作为长期兼容层保留。

### Q: 新功能还会添加RPC2接口吗？
A: 不会。新功能仅提供Connect-RPC接口。特殊情况（如主题强烈需求）可以酌情考虑。

### Q: Connect-RPC是否兼容gRPC客户端？
A: 是的。Connect-RPC使用gRPC-web协议，兼容标准gRPC客户端（需HTTP/2支持）。

### Q: Agent非Root运行有哪些限制？
A: 主要是ICMP探测（return-route）需要`CAP_NET_RAW`权限，其他功能不受影响。

### Q: 如何查看当前Agent使用的协议？
A: 查看Agent日志，会输出`Using protocol: connect-rpc/v2/v1`。

## 相关资源

- Proto定义：`komari-proto/komari/*/v1/*.proto`
- Connect服务端：`komari-1.2.5-fix2/web/connect/`
- Agent客户端：`komari-agent/server/` 和 `komari-agent/rescue/`
- 前端示例：`komari-web/src/api/connect/`
- Connect-RPC官方文档：https://connectrpc.com/

## 更新历史

- 2026-08-24: 创建本文档，明确迁移策略和长期兼容性承诺
