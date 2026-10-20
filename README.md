# DiskLens / 磁盘瞭望

DiskLens 是面向 fnOS 管理员的本地只读磁盘空间分析器。它把目录扫描、历史比较、空间账单、增长提醒和重复文件确认放在同一个白色左侧导航界面中，帮助定位“空间被什么占用”和“哪个目录持续增长”。

当前开源测试版为 `0.3.3`。ARM64 与 AMD64 FPK 已通过本地构建、包结构和浏览器回归；用户已于 2026-10-20 确认完成 fnOS 应用中心基础真机验收。公开仓库尚未附设备型号、CPU 架构和验收日志，目录撤权、NAS 重启恢复、长时间定时巡检及 AMD64 单独实机验证仍按未完成处理。

## 功能

- 多个授权目录统一扫描，自动合并重复及父子重叠范围。
- 概览、目录下钻、文件类型、大文件、完整索引筛选与 CSV 导出。
- 最近 20 次完整扫描、同范围历史比较，以及独立保留 365 天 / 2,000 条的空间账单。
- 只读重复文件检测：完整 SHA-256 后再逐字节确认，跳过链接、特殊文件和变化中的文件。
- 每日定时巡检、扫描速率和最长时间设置，以及增长、容量和覆盖提醒。
- fnOS 目录授权变化检查、管理员访问限制、同源写请求校验和无 CORS 默认策略。

## 安全与数据边界

普通扫描只读取文件元数据。只有用户主动启动重复检测时才读取候选文件内容。DiskLens 不删除、移动、重命名或自动清理用户文件，也不提供对应 API。

结果保存在 NAS 本机。文件大小为逻辑大小，不等于真实磁盘占用；快照、稀疏文件、压缩、reflink 和硬链接都会造成差异。重复结果只表示内容相同，不是自动删除建议。

## 安装测试包

从 [GitHub Releases](https://github.com/JoySec123/DiskLens/releases) 下载与设备 CPU 匹配的 FPK：

- Intel / AMD 设备：`DiskLens-0.3.3-amd64.fpk`
- ARM 设备：`DiskLens-0.3.3-arm64.fpk`

在 fnOS 应用中心手动安装。升级旧版时直接安装新包，不要先卸载；升级后关闭旧窗口并从桌面重新打开，确认左下角显示 `v0.3.3`。测试包和校验值见 [发布说明](doc/RELEASE-0.3.3.md)。

## 本地开发

需要 Go 1.26.9+、Node.js 和 npm。FPK 打包还需要官方 `fnpack`。

```sh
npm ci --ignore-scripts
go test -race ./...
go vet ./...
go build -o .build/disklens ./cmd/disklens
npm run test:ui
npm run test:gateway
bash scripts/build-fnos.sh arm64
bash scripts/build-fnos.sh amd64
node scripts/verify-packages.mjs
```

完整架构、授权、扫描语义和打包说明见 [开发文档](doc/DEVELOPMENT.md)。发布证据与未完成项见 [0.3.3 验证记录](doc/VERIFICATION-0.3.3.md) 和 [验收清单](doc/RELEASE-CHECKLIST-0.3.3.md)。论坛发布稿见 [doc/FORUM-POST.md](doc/FORUM-POST.md)。

## 参与贡献

提交问题或 PR 前请阅读 [CONTRIBUTING.md](CONTRIBUTING.md)。安全问题请按 [SECURITY.md](SECURITY.md) 私下报告，不要在公开 Issue 中附 NAS 地址、用户名、完整路径、连接日志或授权信息。

## 许可证

项目原创代码以 [MIT License](LICENSE) 开源。第三方组件仍遵循各自许可证，详情见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)；构建脚本会把实际依赖许可证一并放入 FPK。
