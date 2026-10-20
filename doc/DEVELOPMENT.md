# 磁盘瞭望 DiskLens 开发文档

当前测试版：0.3.3。开发顺序和边界见 [ROADMAP-0.3.0.md](ROADMAP-0.3.0.md)，本次验证见 [VERIFICATION-0.3.3.md](VERIFICATION-0.3.3.md)，发布候选说明和未完成门槛见 [RELEASE-0.3.3.md](RELEASE-0.3.3.md) 与 [RELEASE-CHECKLIST-0.3.3.md](RELEASE-CHECKLIST-0.3.3.md)。

## 功能范围

- 白色工作区、浅灰左侧导航：概览、目录分析、文件类型、大文件、重复文件、扫描记录、空间账单、提醒、巡检设置。飞牛嵌入窗口隐藏重复应用名称，独立页面保留。
- 多个授权目录统一扫描；重复、父子重叠目录自动合并，最多 32 个选择项。
- 排除目录、相对路径和通配规则；默认界面预填 `@recycle`、`.snapshot`，用户可修改。`*.tmp` 匹配任意路径分量，`folder/*` 为标准单层通配；不支持特殊的递归 `**` 语义。绝对排除项按路径前缀处理，不作通配。
- 后台扫描进度、停止、刷新后恢复当前任务；成功结果写入 SQLite，取消或失败不会产生完整历史记录。
- 完整文件索引、目录逐层分析、面包屑；文件名/路径、类型、扩展名、大小和修改日期筛选、排序及分页。概览的 Top 20/50 仅为摘要，不限制查询范围。
- 复制绝对路径；HTTP 非安全上下文尝试兼容复制，失败显示可选择的路径。暂不依赖 fnOS 微应用 SDK。
- 文件系统总容量、可用容量，与扫描文件逻辑大小分开呈现；相同设备号合并显示。
- 保留最近 20 次成功扫描；重启恢复最近记录及扫描范围；查看历史文件列表，同范围记录比较新增/消失/变化文件、总量和一级目录变化。
- 导出完整筛选结果 CSV，包含文件名、绝对路径、类型、扩展名、大小、修改时间和扫描时间，防电子表格公式注入。
- 只读重复文件检测：同大小候选、全内容 SHA-256、逐字节确认；跳过硬链接别名、符号链接、特殊文件、失效或变化文件。限速、停止、分组分页及组内路径分页。
- 独立空间账单保留 365 天 / 最近 2,000 条；完整索引仍保留 20 次。一级目录与根目录散文件分别汇总，只比较相同扫描范围和排除规则。
- 可配置每日定时巡检、IANA 时区、扫描负载与超时，默认关闭定时执行。持久化每日执行标记，同一当地日期最多执行一次，只在开始时间后 1 小时内启动；任务繁忙时延后，错过窗口不补跑。
- 应用内增长/容量/覆盖提醒与已读状态。增长需同时满足大小和百分比阈值，基准或当前扫描读取不完整时不产生增长判断；不发送外部通知。
- 不删除、移动或修改原文件，不自动清理。文件内容只在用户启动重复检测后读取；普通扫描只读取元数据。

## 架构

Go 1.26.9+，标准库 HTTP 服务；`go.mod` 固定最低补丁版本，禁止用存在可达标准库漏洞的 Go 1.26.4 构建发布包。原生 HTML/CSS/JS 通过 embed 打包。SQLite 使用 `modernc.org/sqlite` 纯 Go 驱动，不要求 CGO、系统 SQLite 或 Node 运行环境。Node 仅用于图标生成与浏览器测试。

| 文件 | 职责 |
| --- | --- |
| cmd/disklens/main.go | HTTP、扫描任务、进程生命周期、单实例数据锁 |
| cmd/disklens/authorization.go | fnOS 授权来源与路径边界 |
| cmd/disklens/scanner.go | 元数据遍历、排除规则、流式索引回调与摘要 |
| cmd/disklens/storage.go | SQLite 初始化、扫描事务、目录汇总、历史保留 |
| cmd/disklens/query.go | 全量筛选、分页、历史、比较、CSV |
| cmd/disklens/duplicates.go | 限速内容读取、确认、后台任务和重复结果 |
| cmd/disklens/capacity.go | 文件系统容量，按设备号去重 |
| cmd/disklens/inspection.go | 定时巡检、负载设置、任务互斥、闲置维护 |
| cmd/disklens/ledger.go | 长期摘要、一级目录汇总、容量记录、提醒与过期清理 |
| cmd/disklens/request_security.go | 同源写请求与 fnOS Host 端口兼容 |
| cmd/disklens/web/ | 本地静态界面与 Lucide 图标 |
| fnos/ | FPK 元数据、权限、生命周期、桌面资源 |

### 数据存储

fnOS 启动脚本使用 `${TRIM_PKGVAR}/disklens`，通过 `DISKLENS_DATA_DIR` 传入；只对应用数据子目录设置 0700，不改变共享目录权限。程序规范化数据目录真实路径，扫描时自动排除它。数据目录锁防止第二个进程同时操作同一索引。

本地可通过 `DISKLENS_DATA_DIR` 配置目录，默认是系统用户缓存目录下的 DiskLens。启动时发现损坏或不可读数据库会报错停止，不覆盖原数据库。

SQLite 使用 WAL、外键、同步 NORMAL、最多 4 个连接。`scans` 保存成功扫描摘要，`entries` 保存每个文件和目录；扫描使用单事务，自底向上聚合目录，提交前删除超出 20 条的旧快照及其索引。失败或进程中断的事务回滚。历史清理不触碰原文件；数据库空闲页会复用，文件大小不一定立即缩小。多个完整快照会占用应用存储空间，实际容量取决于文件数和路径长度。

重复检测的哈希与成员保存在应用 SQLite 表中，避免把全部候选和分组放在内存。任务状态仅在当前进程保留，重启后需要重新检测，新任务替换上次重复结果。重复逻辑大小只计算每组除一份外的路径大小，不代表物理可回收容量。

schema v2 新增 `preferences`、`inspection_runs`、`summaries`、`rollups`、`alerts`。账单独立于完整索引，旧版索引不回填为长期账单。超期账单查询立即隐藏；启动、扫描提交及每小时闲置维护清理对应摘要、汇总、提醒和旧巡检记录。磁盘任务运行时暂缓维护，不因定时扫描开关关闭而停用维护。物理数据库空闲页不保证立即收缩。

CSV 先写入应用目录的临时文件；成功生成和再次验证授权后按已知长度下载，完成后移除临时文件。下载过程中也检查授权；连接中断时浏览器可检测长度不足。需要额外临时存储空间。

### 扫描与内容校验

- `os.OpenRoot` 约束读取边界，跳过链接；重复读取还检查路径分量、`O_NOFOLLOW`、非阻塞打开和普通文件类型。
- 普通扫描默认最长 360 分钟，可配置 1 至 1440 分钟；默认每秒 512 个目录项，可设 32 至 10000，0 为不限速。重复检测不设固定时间上限；内容读取上限默认 32 MiB/s，可选 8/32/64/128，API 允许 1 至 256。候选读取及确认读取共同计入限速。
- 同进程只运行一个扫描或重复检测任务，任务状态可轮询。限速等待可立即取消，底层存储系统调用阻塞则必须等待其返回。
- 重复检测对比当前文件大小、修改时间、身份，读取前后复核；同大小、同文件名、部分采样或仅哈希相等均不会直接作为确认依据。
- 文件系统不是冻结快照，扫描过程中内容可能变化；结果代表观察记录，不提供并发写入下的原子一致性承诺。比较不能把消失路径直接判为删除，也不推断搬迁。
- 文件大小是逻辑大小；硬链接按路径计入普通空间统计，稀疏文件、压缩、快照、reflink 与实际占用不同。修改时间不是访问时间，不能据此判断文件无用。
- 容量基于 `statfs`，并非物理磁盘管理。子卷、配额和保留空间可能影响其数值，不能直接相加当作 NAS 总容量。

## 授权与访问

fnOS 环境通过 `TRIM_APPNAME` 识别。优先用 `TRIM_API_TOKEN` 经 `/var/run/trim_open_gateway_apiscope.socket` 调用 `trim.file.getSharedAccessibleFolders`，声明 `trim.file.sharedAccess`。

- 成功返回空目录列表即撤权，不回退到旧授权。
- 授权 API 出错时拒绝查询，前端清空结果并显示错误。
- 缺少 Token 或系统 API Socket 时兼容读取 `TRIM_DATA_ACCESSIBLE_PATHS`，它是启动时快照，变更授权后须重启应用。
- 禁止扫描 `/`，不在 fnOS 默认扫描工作目录，也不通过本地开发变量覆盖 fnOS 授权。
- 查询、导出、历史、对比、重复结果均重新检查完整扫描范围；多根目录中任一根失去授权，就隐藏该快照全部结果。
- 扫描和重复检测运行期间周期性检查授权；撤权或授权服务故障停止继续处理。
- 只面向管理员，通过统一网关 `/app/DiskLens/`、`${TRIM_APPDEST}/app.sock` 访问，校验 `X-Trim-Userid` 和 `X-Trim-Isadmin: true`，`allUsers: false`。
- POST 要求 JSON，拒绝跨站修改请求。没有文件写入、迁移或删除 API。
- 0.3.2 修复 fnOS 网关将 `Host` 中端口移除导致的跨站误报。Unix Socket 网关模式的所有非 GET/HEAD/OPTIONS 请求必须携带 `X-DiskLens-Request: 1`，并拒绝 `Sec-Fetch-Site: cross-site/same-site`。仅在网关 Host 不含端口时允许 Origin 同主机匹配；直连仍比较完整 Host，不信任客户端 `X-Forwarded-*`。应用不启用 CORS，跨域页面无法通过预检发送该自定义标记。该标记用于 CSRF 防护，不是认证凭据；管理员校验仍独立执行。

开放授权 API 文档要求 fnOS 1.2.0401+。旧版环境变量兼容及网关能力仍需实际设备验证。

## API

fnOS 下加 `/app/DiskLens` 前缀，前端用相对 URL。

| 方法 | 路径 | 参数/用途 |
| --- | --- | --- |
| GET | /api/health | 版本和存活状态 |
| GET | /api/config | 当前授权、兼容提示、最近快照、上次扫描范围 |
| POST | /api/scan | `{roots:[],excludes:[]}`，兼容旧 `root`；返回 202 |
| GET / DELETE | /api/scan | 当前任务 / 请求停止 |
| GET | /api/files | `id,q,category,ext,min,max,since,until,sort,order,page,pageSize` |
| GET | /api/browse | `id,path,page,pageSize`；path 为空为扫描根列表 |
| GET | /api/history | 授权仍有效的成功扫描，按时间倒序 |
| GET | /api/compare | `before,after,page,pageSize`；必须同范围同排除规则 |
| GET | /api/export | files 同款筛选，忽略分页，CSV 下载 |
| GET | /api/capacity | 授权目录所在文件系统容量 |
| POST | /api/duplicates | `{id,minBytes,rateMiB}`；id 是历史快照 ID |
| GET | /api/duplicates | 任务与分组分页；附加 group 可查询组内路径分页 |
| DELETE | /api/duplicates | 请求停止，不发布未完成分组 |
| GET / POST | /api/inspection | 读取/保存巡检、扫描负载与提醒阈值；磁盘任务繁忙时不接受保存 |
| GET | /api/ledger | 可见账单列表；`before` / `after` 比较同范围账单，最多返回变化最大的 100 项并标记截断 |
| GET / POST | /api/alerts | 可见提醒分页，每页 25 条；`{id}` 确认为已读 |

大小单位为字节；`since` 包含起点，`until` 不包含终点，RFC3339 格式；界面按本地日期换算 UTC。扩展名逗号分隔，不区分 ASCII 大小写；排序字段 size/name/modified/path，默认 size desc。同值以路径稳定排序。分页默认 50，最多 200。

状态：running、cancelling、completed、cancelled、error。新扫描不会覆盖旧快照，只有成功提交才成为最近记录。

## 构建和验证

```bash
npm ci
npm run assets
go test -race ./...
go vet ./...
go build -o .build/disklens-preview ./cmd/disklens
npm run test:ui
npm run test:gateway
bash scripts/build-fnos.sh amd64
bash scripts/build-fnos.sh arm64
node scripts/verify-packages.mjs
```

`rsvg-convert` 生成桌面 PNG；Lucide 图标和许可证随包分发，不依赖 CDN。fnpack 使用 `.tools/fnpack`（可用 `FNPACK` 覆盖）。构建使用新 staging 目录，保留之前版本安装包；fnpack 返回成功后仍检查 FPK 是否存在。

本地启动：

```bash
DISKLENS_ROOTS="$PWD" DISKLENS_DATA_DIR="$PWD/.build/local-data" go run ./cmd/disklens
```

默认只监听 `127.0.0.1:8787`，通过 `DISKLENS_PORT` 更改。本地多个授权根用逗号分隔。浏览器测试使用本机 Chrome，测试扫描数据、应用数据全部隔离，结束后清理临时测试目录。

自动化覆盖：授权撤销、API 错误、网关管理员限制、跨站写请求、链接越界、多根去重、排除、5000 文件完整索引、全局筛选/排序/分页、目录汇总、取消回滚、历史保留、进程重启恢复、范围不匹配的对比、CSV 全量与公式转义、同内容确认、硬链接排除、文件变化、FIFO 拒绝、限速取消、任务互斥、时区/跨日调度、账单独立保留与闲置过期、增长/容量提醒。浏览器覆盖九页、真实扫描/复制/下载/对比/重复检测/设置/账单/提醒、375/620/800/1100 宽度，以及 Unix Socket 代理去端口和嵌入窗口单标题。

## 部署与验收

安装包：`dist/DiskLens-0.3.3-amd64.fpk` 和 `dist/DiskLens-0.3.3-arm64.fpk`。Intel/AMD 选 amd64，ARM64 选 arm64。

沿用 appname=DiskLens 升级，保留原授权。0.1.1 的结果只在内存，没有可迁移的持久历史；升级后首次扫描建立索引。无需删除应用或共享文件。

真机验收：管理员桌面入口、网关页面、授权新增/撤销、读权限、进度/停止、两个目录合并、排除规则、目录下钻、重启后的历史、同范围对比、重复检测限速、CSV 下载。先用小型测试目录验证；不要把重复大小当成删除建议。

本地测试和 Linux 构建不等于 fnOS 真机验证。用户于 2026-10-20 确认 0.3.3 已完成应用中心基础真机验收；公开仓库未附设备型号、CPU 架构和原始日志，权限撤销、重启恢复、长时间巡检及 AMD64 单独实机仍需独立证据。不承诺未测试的版本/架构兼容性。

官方参考：

- https://developer.fnnas.com/docs/core-concepts/environment-variables
- https://developer.fnnas.com/api/authorization/shared-access
- https://developer.fnnas.com/docs/core-concepts/gateway-registration
- https://developer.fnnas.com/api/page/routing
- https://developer.fnnas.com/docs/cli/fnpack
