# Linux 一键更新与回退

Komari 会在版本接口中返回部署类型：`docker`、`linux`、`windows` 或 `unknown`。

Linux 一键更新面向使用官方安装脚本、由 systemd 管理的直接安装，兼容以下主流发行版及其后续版本：

- Ubuntu 18.04+
- Debian 9+
- CentOS / RHEL 7+
- Rocky Linux / AlmaLinux 8+

更新器不调用 `apt`、`dnf` 或 `yum`，而是使用与发行版无关的静态 Linux 二进制和 systemd 临时服务。实际是否可用以运行时能力检测为准。

## 更新事务

1. 从 `r11234567/komari` 的 GitHub Release 获取 `komari-update.json` 和独立签名 `komari-update.json.sig`。
2. 用本机固定的 Ed25519 发布公钥验证清单签名，再下载当前 CPU 架构的文件。核对版本号、七位构建标识、文件大小和 SHA-256 后，才执行候选文件的版本探测。
3. 由独立的 systemd 更新助手停止 Komari。
4. 冷备份当前程序和完整 `data` 目录。
5. 原子替换程序并重新启动服务。
6. 持续检查版本接口；新服务稳定运行后才确认成功。
7. 启动失败、版本不符或健康检查中断时，同时恢复旧程序和更新前数据。

更新助手会记录事务阶段。助手自身异常退出后，systemd 会再次启动它并继续回退。成功更新会保留最近两份回退快照。

## 发布公钥与签名

- 发布仓库的 Actions Secret `KOMARI_UPDATE_SIGNING_KEY` 应设置为 Base64 编码的 Ed25519 PKCS#8 DER 私钥。正式发布、从源码重建发布及快照会使用这把密钥生成独立签名。
- 用独立可信渠道向主机管理员发放 32 字节原始发布公钥的 Base64 编码。主机管理员在 systemd 的 `komari.service` 环境中设置 `KOMARI_UPDATE_SIGNING_PUBLIC_KEY` 并重启服务。
- 缺少本机公钥时，面板不提供一键更新。缺失签名、签名不匹配或二进制校验失败时，更新终止。旧发布可用已配置签名的发布流程重新构建。轮换签名密钥时，由主机管理员通过同一可信渠道更新公钥配置。

## 不启用一键更新的情况

- Docker：保留 GitHub 更新入口，应由宿主机更新镜像并重建容器。
- Windows：保留 GitHub 更新入口，不能复用 Linux 的 systemd 更新流程。
- 非 root、非 systemd 或当前进程不是 `komari.service` 主进程。
- 主数据库或 SQLite 指标库位于受管 `data` 目录之外。
- 指标数据使用外部 MySQL/PostgreSQL。Komari 无法替外部数据库制作可验证的回退快照，因此不会承诺一键回退。
- `data` 是独立挂载点。此布局无法通过目录交换完成原子数据回退。
