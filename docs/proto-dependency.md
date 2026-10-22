# Proto 子模块依赖

本库通过 `third_party/komari-proto` Git 子模块使用协议源码及已生成的 Go/TypeScript 代码。
**Git 记录的子模块提交是唯一协议版本依据**，构建不跟随远端分支，不需要发布 proto 包。

## 首次检出和日常更新

```bash
git clone --recurse-submodules <本仓库 URL>
# 已有工作目录在 pull、切换分支或 checkout 后执行：
git submodule update --init --recursive
node scripts/ci/check-protocol.cjs
```

Go 消费者通过本地 `replace` 使用子模块，`require ... v0.0.0` 只是固定占位符，不是协议版本。
Web 使用 `file:third_party/komari-proto`，`npm ci` 创建本地包链接；锁文件中的 package version 只是 npm 元数据。
Go/npm manifest、workflow 中均不另存协议 SHA。

## 升级协议

先在 komari-proto 提交并验证协议和生成代码，再在三个消费者分别执行：

```bash
git submodule update --init --recursive
node scripts/bump-proto.cjs <已审查的完整 40 位 proto 提交 SHA>
git diff --cached --submodule=log
git commit -m 'chore(proto): bump protocol submodule'
git push
```

脚本检出指定提交，Web 同时刷新本地 npm 锁元数据，只暂存子模块及必要锁文件，保留其他工作区改动。
提交和推送由操作者执行，CI 不自动追踪协议仓库的 main。
也可手动在子模块 `fetch`、`checkout --detach <SHA>` 后，在父仓库 `git add third_party/komari-proto` 并提交。

三个消费者应 bump 到同一提交。后端还应 bump `third_party/komari-web` 到已通过 CI 的 Web 提交，
前端构建会核对嵌套 Web 与后端的 proto 提交一致。显式覆盖 Web ref 后也会重新初始化嵌套子模块。

## 验证

```bash
node --test scripts/ci/protocol-submodule.test.cjs
node scripts/ci/check-protocol.cjs
# Go 消费者在安装 Go 后额外核对实际解析路径：
node scripts/ci/check-protocol.cjs --resolve-go
```

检查初始化状态、Git 指针一致性、源码未被直接修改，以及 Go/npm 的本地依赖路径。
CI 在 Linux/Windows、实际构建及安全扫描前递归检出子模块；回归和失败日志保留 30 天。
下载 GitHub 源码压缩包不会包含子模块源码，请使用递归 Git 检出进行构建。
