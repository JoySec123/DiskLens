# DiskLens 0.3.2 修复与验证

日期：2026-10-19。

## 修复内容

- 飞牛嵌入窗口只显示系统标题；侧栏内不再重复显示“磁盘瞭望”。独立浏览器页面保留应用名称。
- 修复网关移除 Host 端口后，正常扫描被误报为“不允许跨站操作”的问题。
- 前端所有写请求统一附加 `X-DiskLens-Request: 1`，涵盖开始扫描、停止扫描、重复检测、设置保存和提醒确认。
- 跨站拦截单独返回 `cross_site_request`，不再误显示为目录撤权，也不清空已显示的扫描结果。

## 原因与边界

在用户授权的 ARM64 fnOS 测试机上，0.3.0 已复现：通过同源浏览器对 `/app/DiskLens/api/health` 发送无副作用的 POST 返回 403。保留浏览器 Origin 中的 `:5666` 会被拒绝；仅去掉 Origin 端口则返回 200，确认旧代码中完整 Origin Host 与网关 Host 比较不兼容。

0.3.2 只在启用管理员校验的 Unix Socket 网关模式下兼容缺失端口的 Host，同时强制非 GET/HEAD/OPTIONS 请求携带自定义标记。应用不提供 CORS 授权，其他来源的浏览器请求无法通过预检携带标记；现代浏览器的 cross-site、same-site 请求也被拒绝。Origin 的主机仍必须一致，不接受路径、用户信息、空来源、无效协议或来源列表。原有无 Origin 的非浏览器请求在网关模式下也必须携带标记。

直接访问开发服务仍比较完整 Host，且不信任客户端提供的 `X-Forwarded-Host`。自定义标记不是登录凭据，飞牛管理员认证独立执行。不修改飞牛 nginx、安全策略或目录授权配置。

## 已通过的本地验证

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `node scripts/test-ui.mjs`：原有扫描、筛选、导出、重复检测、授权、停止、设置、账单、提醒及九页响应式测试。
- `node scripts/test-gateway-ui.mjs`：Unix Socket、Host 去端口、不带 Fetch Metadata 的代理场景；扫描完成、保存设置、DELETE、自定义标记缺失拒绝、同主机不同端口的跨域预检被拒绝、错误状态不丢失结果、嵌入标题唯一。
- `node scripts/verify-packages.mjs`：ARM64/AMD64 版本、ELF 架构、图标、网关入口、最小权限、启停脚本与许可证。

截图 `.build/screenshots/gateway-embedded-desktop.png` 来自本地代理模拟，不是飞牛真机升级后的截图。

## 安装包

| 架构 | 文件 | SHA256 |
| --- | --- | --- |
| ARM64 | `dist/DiskLens-0.3.2-arm64.fpk` | `cb71bdcae3387ab2c06db17db04af8b5d371c0fac854f9a44160d74a3afa031d` |
| AMD64 | `dist/DiskLens-0.3.2-amd64.fpk` | `1f0c4d8dd993adc7c35317faf156a3afa9db83d7fcb5eae2c1c88593f2c641a8` |

通过飞牛应用中心手动安装对应架构的包进行升级，无需卸载。升级后关闭旧应用窗口，再从桌面打开，以加载更新后的前端请求标记。

## 尚待真机验证

测试机本轮仍为 0.3.0；0.3.2 未远程升级。fnOS skill 的 OAuth 尚待用户自行确认，不代替用户登录或读取授权码。

升级后检查版本为 0.3.2、只显示一个标题、已授权目录扫描完成、设置保存和停止操作无跨站错误。旧版数据升级、撤权、长时间定时巡检等发布验收仍需另外记录，不以本次本地测试代替。
