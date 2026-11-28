# Linux 节点临时开放 SSH

[English](temporary-ssh.md)

临时 SSH 允许一个 IPv4 或 IPv6 来源地址连接节点上固定的 SSH 端口，有效期 15 分钟。节点需要新版 Agent、已授权的救援助手和可用的 nftables 接入点。网页 WebSSH 可独立使用。

## 配置节点

首次通过 `komari-host apply` 确认安装救援助手时，兼容的节点会用同一次本机 PAM 确认自动准备临时 SSH 接入点。之后在面板申请时，按页面提示完成二次验证即可。

已经安装救援助手但缺少接入点，或自动配置提示需要指定网卡、端口时，从你自己打开的 SSH 会话执行以下命令。已有配置且校验通过时，命令会直接保留。

```bash
sudo komari-host setup-rescue-ssh --manifest /etc/komari/services/komari-agent.json
```

命令会确认 SSH 监听端口、默认网卡和原生 nftables 入站链，并把空超时集合和规则写入持久配置。准备救援组件时，安装器可同时传入 `--install-rescue --install-setup-rescue-ssh`。若提示多个候选，可用 `--port`、`--interface`、`--table` 和 `--chain` 指定。若用自定义方式加载防火墙，可用 `--nft-config` 指向重启时实际加载的配置入口。UFW、firewalld 和 legacy iptables 会显示所检测到的防火墙，并提示使用原生 nftables 接入点或 WebSSH。

### 手动配置

在 **要开放 SSH 的节点** 上用 `sudo nft list tables` 和 `sudo nft -a list chain inet <表> <链>` 确认现用的入站过滤链。下面以 `public_edge_guard/input`、公网接口 `eth0`、TCP `8658` 为例。表名、链名、接口和端口以本机为准。

创建 `/etc/komari/services/komari-agent.temporary-ssh.json`，属主 `root:root`，权限 `0600`。如果 Agent 使用自定义服务名，将文件名中的 `komari-agent` 替换为实际服务名。

```json
{
  "table": "public_edge_guard",
  "chain": "input",
  "interface": "eth0",
  "port": 8658
}
```

在节点的**持久防火墙配置**中，把下面两个空集合放进现有的 `table inet public_edge_guard`。

```nft
set komari_rescue_ssh_v4 {
    type ipv4_addr
    flags timeout
}
set komari_rescue_ssh_v6 {
    type ipv6_addr
    flags timeout
}
```

在现有 `input` 过滤链中，将下面两条规则放在 `eth0` 的兜底丢弃规则之前。保留匹配顺序，两条都要配置，每个集合只由对应规则引用。提供 IPv6 时，保留公网接口所需的邻居发现规则。

```nft
iifname "eth0" ip saddr @komari_rescue_ssh_v4 tcp dport 8658 accept
iifname "eth0" ip6 saddr @komari_rescue_ssh_v6 tcp dport 8658 accept
```

按本机流程检查和加载防火墙配置，再查看生效的链和集合。持久配置只保存空集合，重新加载并重建集合会结束当前授权。后续 `ip`、`ip6` 的入站链、CrowdSec、云防火墙和 SSH 认证仍按各自策略处理。Komari 只管理这两个集合。目前支持单一 IP，UFW、firewalld 与 legacy iptables 没有专用接入方式。

## 申请并验证

在节点的救援面板选择「临时开放 SSH」。面板预填它所看到的管理员来源 IP。核对该 IP 与 SSH 客户端的实际出口一致，必要时修改，然后完成二次验证。

**在 NAT 后，填 SSH 客户端的公网出口 IP。** 在真正发起 SSH 的电脑上，用和 SSH 相同的网络或 VPN 执行：

```bash
curl -4 -fsS https://api.ipify.org
```

Windows PowerShell 使用 `curl.exe -4 -fsS https://api.ipify.org`。查 IPv4 后，用 `ssh -4` 连接节点公网 IPv4。如果经跳板或分流 VPN，目标节点看到的来源可能不同。已有同一路径的 SSH 会话时，在节点执行 `printf '%s\n' "$SSH_CONNECTION"`，第一列就是该会话的实际来源。出口 IP 改变后需要重新申请。

「救援辅助程序输出」显示本机监听、规则写入或撤销的回执。实际能否连接，从授权设备新建一次 SSH 连接确认。首次验收还应验证其他来源无法新建连接、主动撤销、完整 15 分钟到期与防火墙重载。已建立的连接可能按现有防火墙策略继续。短暂掉线会保留原到期时间。重新连接和重复提交同一请求均不会延长期限。可用 `sudo journalctl -u komari-agent-rescue.service --since '15 minutes ago'` 查看 Helper 日志。

## 按错误码排查

错误码和节点细节出现在「救援辅助程序输出」。修好后等 Helper 下一次上报，再重新打开救援面板。每次状态上报都会重新检查配置和规则。

### SSH_CONFIG_MISSING

节点没有找到临时 SSH 配置文件。先确认终端连的是面板里所选的目标节点，再按[配置节点](#配置节点)运行自动配置。

### SSH_CONFIG_INVALID

配置文件无法读取或校验。根据输出检查路径、`root:root`、`0600` 和 JSON 四个字段。文件及其上级目录要满足 Helper 的信任检查，不能用符号链接或由非 root 用户控制。

### SSH_NFT_READ_FAILED

Helper 无法读取或解析配置中的 nftables 表。在目标节点执行 `sudo nft list tables`，核对 JSON 的表名，再用 `sudo nft -j list table inet <表>` 确认能读取。不同节点可能用不同表名。手动命令成功但 Helper 仍报错时，查看 Helper 日志。

### SSH_NFT_RULES_INVALID

表存在，但链、两个超时集合或匹配规则不符合预期。对照[配置节点](#配置节点)，用 `sudo nft -a list chain inet <表> <链>` 检查实际顺序和接口，用 `sudo nft list set inet <表> <集合>` 检查集合类型及超时。两条规则要在兜底丢弃之前，接口、端口和表达式顺序必须匹配。修正后按本机持久配置流程加载。

### SSH_STATE_INVALID

Helper 无法读取授权状态，或状态与内核集合不一致。按输出检查本地状态路径、两个集合及 Helper 日志。持久配置中应只定义空集合，重载并重建集合会结束当前授权。如果输出指向损坏的 `*.temporary-ssh-state.json`，保留文件供排查，核对路径与属主后再申请新窗口。

### SSH_HELPER_UPDATE_REQUIRED

当前 Helper 没有上报新版临时 SSH 能力。在目标节点一起更新 Agent 和已安装的 Rescue Helper。通过 `sudo systemctl status komari-agent-rescue.service` 确认服务，等重新上报后再打开救援面板。

### SSH_STATUS_UNAVAILABLE

Helper 上报支持临时 SSH，但没有给出可用端口或诊断码。先升级 Helper 并等新状态。如果仍没显示端口，在目标节点核对 JSON 和已加载的防火墙接入点。

### 规则已写入但连接超时

先用 `sudo ss -ltn 'sport = :8658'` 核对实际端口的监听情况，再确认客户端出口 IP 和地址族。检查接入规则是否位于兜底丢弃之前，以及后续入站链、CrowdSec 和云防火墙是否允许。每次验证都用同一授权来源发起新连接。
