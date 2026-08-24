# 主题开发者Connect-RPC迁移指南

## 概述

本指南帮助主题开发者将基于RPC2的主题迁移到Connect-RPC。**注意**：RPC2将长期保留，迁移不是强制性的，但推荐新主题使用Connect-RPC。

## 快速检查

### 你的主题需要迁移吗？

**✅ 无需迁移**的主题：
- 仅使用静态数据（settings.json配置）
- 仅调用公开API（`/api/public/*`）
- 无实时数据需求

**⚠️ 建议迁移**的主题：
- 使用管理员API（`/api/admin/*`）
- 使用WebSocket实时数据
- 调用任务执行API
- 需要流式数据（dashboard实时更新）

## 迁移收益

### Connect-RPC优势

1. **类型安全**：Proto定义提供完整的类型检查
2. **更好的工具支持**：代码生成、类型提示、IDE补全
3. **流式支持**：原生支持Server-Sent Events和双向流
4. **标准化**：基于gRPC-web，生态系统更成熟
5. **更好的错误处理**：结构化错误信息

### 兼容性保证

- RPC2端点长期保留
- 旧主题无需强制迁移
- 可以混合使用（部分功能用Connect，部分用RPC2）

## 迁移步骤

### Step 1: 安装Connect客户端库

```bash
npm install @connectrpc/connect @connectrpc/connect-web
```

或使用yarn：

```bash
yarn add @connectrpc/connect @connectrpc/connect-web
```

### Step 2: 生成TypeScript客户端（可选）

如果想要完整的类型支持，可以从proto生成TypeScript代码：

```bash
# 安装protoc和connect插件
npm install -D @bufbuild/protoc-gen-es @connectrpc/protoc-gen-connect-es

# 生成代码
npx buf generate https://github.com/r11234567/komari-proto.git
```

生成的代码会在`src/gen/`目录下。

### Step 3: 创建Connect客户端

**方法A：使用生成的TypeScript代码（推荐）**

```typescript
// src/api/connect.ts
import { createPromiseClient } from '@connectrpc/connect';
import { createConnectTransport } from '@connectrpc/connect-web';
import { MetricsService } from '../gen/komari/metrics/v1/metrics_connect';
import { AgentReportService } from '../gen/komari/report/v1/report_connect';

const transport = createConnectTransport({
  baseUrl: window.location.origin,
  // 自动从cookie中获取认证信息
});

export const metricsClient = createPromiseClient(MetricsService, transport);
export const reportClient = createPromiseClient(AgentReportService, transport);
```

**方法B：不生成代码，直接调用（简单场景）**

```typescript
// src/api/connect-raw.ts
async function callConnectRPC(service: string, method: string, request: any) {
  const response = await fetch(`/${service}/${method}`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
    },
    body: JSON.stringify(request),
  });
  
  if (!response.ok) {
    throw new Error(`RPC failed: ${response.statusText}`);
  }
  
  return response.json();
}

// 使用示例
export async function listMetrics() {
  return callConnectRPC(
    'komari.metrics.v1.MetricsService',
    'ListMetrics',
    {}
  );
}
```

### Step 4: 迁移API调用

#### 示例1：获取Dashboard数据

**旧方式（RPC2）**：

```javascript
async function getDashboard() {
  const response = await fetch('/api/rpc2', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      jsonrpc: '2.0',
      id: Date.now(),
      method: 'admin:getDashboard',
      params: { sections: 'all', limit: 10 }
    })
  });
  
  const data = await response.json();
  if (data.error) {
    throw new Error(data.error.message);
  }
  
  return data.result;
}
```

**新方式（Connect-RPC）**：

```typescript
import { metricsClient } from './api/connect';

async function getDashboard() {
  const response = await metricsClient.listMetrics({
    // Connect-RPC使用结构化请求
  });
  
  return response;
}
```

#### 示例2：执行任务

**旧方式（RPC2）**：

```javascript
async function executeTask(uuid, command) {
  const response = await fetch('/api/rpc2', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      jsonrpc: '2.0',
      id: Date.now(),
      method: 'admin:exec',
      params: {
        uuid: uuid,
        command: command
      }
    })
  });
  
  const data = await response.json();
  return data.result;
}
```

**新方式（Connect-RPC）**：

```typescript
import { executionClient } from './api/connect';

async function executeTask(agentId: string, command: string) {
  const response = await executionClient.createExecution({
    agentId: agentId,
    spec: {
      shell: 'bash',
      script: command,
    }
  });
  
  return response.execution;
}
```

#### 示例3：实时流式数据

**旧方式（WebSocket + RPC2）**：

```javascript
const ws = new WebSocket('ws://localhost/api/clients');
ws.onmessage = (event) => {
  const data = JSON.parse(event.data);
  updateDashboard(data);
};
```

**新方式（Connect流式）**：

```typescript
import { agentEventsClient } from './api/connect';

async function watchAgentEvents(agentId: string) {
  for await (const event of agentEventsClient.subscribeEvents({
    agentId: agentId,
  })) {
    console.log('New event:', event.event);
    updateDashboard(event.event);
  }
}
```

### Step 5: 错误处理

**Connect-RPC统一错误处理**：

```typescript
import { ConnectError } from '@connectrpc/connect';

try {
  const response = await metricsClient.listMetrics({});
} catch (err) {
  if (err instanceof ConnectError) {
    // Connect标准错误
    console.error('Code:', err.code);
    console.error('Message:', err.message);
    
    // 检查特定错误码
    if (err.code === 'PERMISSION_DENIED') {
      showLoginPrompt();
    }
  }
}
```

### Step 6: 兼容性处理（渐进式迁移）

如果想同时支持新旧两种方式：

```typescript
class KomariAPIClient {
  private useConnect: boolean = false;
  
  async detectSupport() {
    try {
      // 尝试调用Connect端点
      await fetch('/komari.metrics.v1.MetricsService/ListMetrics', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: '{}',
      });
      this.useConnect = true;
    } catch {
      this.useConnect = false;
    }
  }
  
  async getDashboard() {
    if (this.useConnect) {
      return this.getDashboardViaConnect();
    }
    return this.getDashboardViaRPC2();
  }
  
  private async getDashboardViaConnect() {
    // Connect实现
  }
  
  private async getDashboardViaRPC2() {
    // RPC2实现
  }
}

// 使用
const api = new KomariAPIClient();
await api.detectSupport();
const dashboard = await api.getDashboard();
```

## API映射表

### 常用RPC2方法 → Connect服务

| RPC2方法 | Connect服务 | Connect方法 |
|---------|-------------|------------|
| `public:getNodesInformation` | `MetricsService` | `ListMetrics` |
| `public:getClientRecentRecords` | `MetricsService` | `QueryMetrics` |
| `admin:getDashboard` | `MetricsService` | `ListMetrics` + 聚合 |
| `admin:listClients` | `AgentReportService` | 订阅Agent上报 |
| `admin:exec` | `ExecutionService` | `CreateExecution` |
| `admin:getTasks` | `ExecutionService` | `ListExecutions` |
| `client:getPingTasks` | `NetworkProbeService` | `ListProbes` |

## Proto定义位置

所有proto定义在：`https://github.com/r11234567/komari-proto`

主要服务：

```
komari/
├── metrics/v1/metrics.proto       # 指标查询
├── report/v1/report.proto          # Agent上报
├── exec/v1/exec.proto              # 任务执行
├── config/v1/config.proto          # 配置管理
├── network/v1/network.proto        # 网络探测
├── rescue/v1/rescue.proto          # 救援模式
├── webssh/v1/webssh.proto          # 终端
└── browser/v1/browser.proto        # 浏览器控制
```

## 完整示例

### React + Connect-RPC完整示例

```typescript
// src/hooks/useMetrics.ts
import { useEffect, useState } from 'react';
import { metricsClient } from '../api/connect';
import type { Metric } from '../gen/komari/metrics/v1/metrics_pb';

export function useMetrics() {
  const [metrics, setMetrics] = useState<Metric[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<Error | null>(null);
  
  useEffect(() => {
    async function fetchMetrics() {
      try {
        const response = await metricsClient.listMetrics({});
        setMetrics(response.metrics);
      } catch (err) {
        setError(err as Error);
      } finally {
        setLoading(false);
      }
    }
    
    fetchMetrics();
    
    // 每5秒刷新一次
    const interval = setInterval(fetchMetrics, 5000);
    return () => clearInterval(interval);
  }, []);
  
  return { metrics, loading, error };
}

// src/components/Dashboard.tsx
import { useMetrics } from '../hooks/useMetrics';

export function Dashboard() {
  const { metrics, loading, error } = useMetrics();
  
  if (loading) return <div>Loading...</div>;
  if (error) return <div>Error: {error.message}</div>;
  
  return (
    <div>
      {metrics.map(metric => (
        <div key={metric.name}>
          {metric.name}: {metric.value}
        </div>
      ))}
    </div>
  );
}
```

### Vue + Connect-RPC完整示例

```typescript
// src/composables/useMetrics.ts
import { ref, onMounted, onUnmounted } from 'vue';
import { metricsClient } from '../api/connect';

export function useMetrics() {
  const metrics = ref([]);
  const loading = ref(true);
  const error = ref<Error | null>(null);
  
  let interval: number;
  
  async function fetchMetrics() {
    try {
      const response = await metricsClient.listMetrics({});
      metrics.value = response.metrics;
      error.value = null;
    } catch (err) {
      error.value = err as Error;
    } finally {
      loading.value = false;
    }
  }
  
  onMounted(() => {
    fetchMetrics();
    interval = setInterval(fetchMetrics, 5000);
  });
  
  onUnmounted(() => {
    clearInterval(interval);
  });
  
  return { metrics, loading, error };
}

// src/components/Dashboard.vue
<script setup lang="ts">
import { useMetrics } from '../composables/useMetrics';

const { metrics, loading, error } = useMetrics();
</script>

<template>
  <div>
    <div v-if="loading">Loading...</div>
    <div v-else-if="error">Error: {{ error.message }}</div>
    <div v-else>
      <div v-for="metric in metrics" :key="metric.name">
        {{ metric.name }}: {{ metric.value }}
      </div>
    </div>
  </div>
</template>
```

## 测试

### 单元测试示例

```typescript
// src/api/__tests__/connect.test.ts
import { describe, it, expect, vi } from 'vitest';
import { metricsClient } from '../connect';

describe('Connect API', () => {
  it('should fetch metrics', async () => {
    const metrics = await metricsClient.listMetrics({});
    expect(metrics.metrics).toBeDefined();
    expect(Array.isArray(metrics.metrics)).toBe(true);
  });
  
  it('should handle errors', async () => {
    // Mock网络错误
    vi.spyOn(global, 'fetch').mockRejectedValueOnce(
      new Error('Network error')
    );
    
    await expect(
      metricsClient.listMetrics({})
    ).rejects.toThrow();
  });
});
```

## 常见问题

### Q1: Connect-RPC比RPC2慢吗？

A: 不会。Connect-RPC基于HTTP/2，支持多路复用，通常比WebSocket更快。初始连接可能稍慢，但后续请求更快。

### Q2: 可以混合使用RPC2和Connect吗？

A: 可以。你可以渐进式迁移，部分功能使用Connect，部分保留RPC2。

### Q3: Connect需要额外的服务器配置吗？

A: 不需要。komari后端已经同时支持两种协议，无需额外配置。

### Q4: 如何调试Connect-RPC请求？

A: 在浏览器开发者工具的Network标签中，Connect请求显示为普通的POST请求，可以查看请求/响应的JSON payload。

### Q5: Connect支持认证吗？

A: 支持。Connect-RPC自动继承HTTP cookie认证，与RPC2相同。

## 资源

- **Connect-RPC官方文档**: https://connectrpc.com/docs/web/getting-started
- **Proto定义仓库**: https://github.com/r11234567/komari-proto
- **komari-web示例**: https://github.com/komari-monitor/komari-web (查看`src/api/connect/`目录)
- **迁移讨论**: GitHub Issues

## 获取帮助

如果在迁移过程中遇到问题：

1. 查看本指南的常见问题部分
2. 查看`MIGRATION_RPC.md`了解整体迁移策略
3. 在GitHub Issues提问
4. RPC2长期支持，不必担心迁移时间压力

## 更新历史

- 2026-08-24: 创建本指南
