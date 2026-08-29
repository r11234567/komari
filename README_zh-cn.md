# Komari

![komari](https://socialify.git.ci/komari-monitor/komari/image?description=1&font=Inter&forks=1&issues=1&language=1&logo=https%3A%2F%2Fraw.githubusercontent.com%2Fkomari-monitor%2Fkomari-web%2Fd54ce1288df41ead08aa19f8700186e68028a889%2Fpublic%2Ffavicon.png&name=1&owner=1&pattern=Plus&pulls=1&stargazers=1&theme=Auto)

[English](./README.md) | [简体中文](./README_zh-cn.md)

Komari 是一款轻量级的自托管服务器监控工具，旨在提供简单、高效的服务器性能监控解决方案。它支持通过 Web 界面查看服务器状态，并通过轻量级 Agent 收集数据。
[文档](https://www.komari.wiki/) 

## 特性

- **实时监控**: 秒级实时数据展示。
- **轻量高效**：低资源占用，适合各种规模的服务器。
- **自托管**：完全掌控数据隐私，部署简单。
- **Web 界面**：直观的监控仪表盘，易于使用。
- **极强的可扩展性**: 支持自定义主题和插件。

## 本版本改进

- **非 Root Agent**：Agent 可在非 Root 权限下运行，降低部署复杂度和安全风险。
- **Agent 救援模式**：提供恢复通道，用于诊断和恢复失联 Agent。
- **原始数据 CSV 导出**：导出保留的原始指标点，便于审计和离线分析。
- **可选不降采样**：可选择是否启用降采样，原始数据留存与 Rollup 分开管理。
- **Connect-RPC 连接**：使用 Connect-RPC 提供统一、高效的 Agent 与 API 通信。
- **数据库优化**：增加更细粒度 Rollup、增量清理、自适应维护和更高效的查询读写路径。


## 截图

| 页面         | 截图                                                                                                                                                         |
| ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 主页仪表盘   | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E4%B8%BB%E9%A1%B5%E4%BB%AA%E8%A1%A8%E7%9B%98.webp" width="800" alt="主页仪表盘">            |
| 后台仪表盘   | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E5%90%8E%E5%8F%B0%E4%BB%AA%E8%A1%A8%E7%9B%98.webp" width="800" alt="后台仪表盘">            |
| 历史图表     | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E5%8E%86%E5%8F%B2%E5%9B%BE%E8%A1%A8.webp" width="800" alt="历史图表">                       |
| 网页终端     | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E7%BD%91%E9%A1%B5%E7%BB%88%E7%AB%AF.webp" width="800" alt="网页终端">                       |
| 主题可自定义 | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E4%B8%BB%E9%A2%98%E5%8F%AF%E8%87%AA%E5%AE%9A%E4%B9%89.webp" width="800" alt="主题可自定义"> |
| 主题市场     | <img src="https://b2.akz.moe/awesome-pictures/komari-screenshot/%E4%B8%BB%E9%A2%98%E5%B8%82%E5%9C%BA.webp" width="800" alt="主题市场">                       |
