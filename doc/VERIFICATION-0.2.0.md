# DiskLens 0.2.0 本机验证记录

验证日期：2026-10-19。开发环境：macOS arm64，Go 1.26.4、Node 20、Chrome、fnpack 1.2.3。

## 验收证据

| 项目 | 证据 |
| --- | --- |
| 多根合并、父子去重与排除 | TestMultipleRootsAndExcludeValidation；浏览器真实双目录扫描，69 个文件 |
| 全量索引、目录汇总、下钻 | TestIndexFullQueriesAndDrilldown、TestLargeIndex；5000 文件最后一页；浏览器三级目录进入与返回 |
| 完整筛选、排序、分页、时间 | TestIndexFullQueriesAndDrilldown、TestQueryValidation；浏览器搜索原 Top 50 外的文件、类型/最小大小、下一页 |
| 复制与导出 | 浏览器读取剪贴板、下载并解析 CSV；后端导出 75 行数据，验证不受 50 行分页限制及公式转义 |
| 历史、取消回滚、保留策略 | TestPersistenceCancellationAndRetention；取消不提交；22 次扫描保留 20 份，源文件不变 |
| 进程重启恢复 | 浏览器脚本停止服务、同数据目录重启，最近快照 ID、69 个文件、3 条历史保持一致 |
| 同范围比较 | TestComparisonExportAndRevocation；浏览器验证新增 1、消失 1、变化 1；不同排除规则拒绝比较 |
| 重复内容确认 | TestDuplicateVerificationAndHardlinks；同大小不同内容不分组，硬链接别名跳过；浏览器真实文件逐字节确认并查看组内路径 |
| 内容变化、链接、FIFO | TestChangedFilesLinksAndCancellation，拒绝失效或非普通文件，不阻塞打开 FIFO |
| 任务互斥、重复检测停止 | TestDuplicateLifecycleCancelAndConflict；重复检测期间新扫描返回 409；停止后可再次扫描 |
| 授权与访问 | 既有授权 API/管理员边界测试；新查询、历史、对比、导出、重复结果撤权后不可访问；跨站写请求和 text/plain 拒绝 |
| 自身数据目录 | TestCrossSiteMutationAndDataPath；实际路径规范化、带特殊字符的数据目录、自动排除自身索引 |
| 界面与资源 | 六页实际操作，375/620/800/1100 宽度无页面横向溢出；桌面与手机截图人工查看 |
| Linux 包 | amd64/arm64 均编译并通过 fnpack，ELF 架构、版本、网关、图标、Scope、数据目录与依赖许可证检查通过 |

通过的命令：

```bash
go test -race -count=1 ./...
go vet ./...
go build -o .build/disklens-preview-v020 ./cmd/disklens
DISKLENS_TEST_BIN=.build/disklens-preview-v020 npm run test:ui
bash scripts/build-fnos.sh amd64
bash scripts/build-fnos.sh arm64
node scripts/verify-packages.mjs
```

前端 JS、测试/构建脚本语法检查及生命周期 Bash 语法检查均通过。截图保存在 `.build/screenshots/`，包括概览、大文件、历史对比、重复文件的桌面图和手机图。

## 安装包

| 架构 | 文件 | SHA-256 |
| --- | --- | --- |
| Intel/AMD | dist/DiskLens-0.2.0-amd64.fpk | 2a2300b7407315cbbd3eee35b76e594c8c7d487438fb469a71b58324ec37588d |
| ARM64 | dist/DiskLens-0.2.0-arm64.fpk | d5935ac831899a8933f83a053e569c0406c8e3a417813f714d7ace257ab52373 |

本地预览：http://127.0.0.1:8788。健康检查返回 `{"ok":true,"version":"0.2.0"}`。这是本机服务，不是 NAS 地址。

## 未验证边界

- 未在用户 fnOS 设备安装运行，网关和实际共享目录 ACL 必须按发布说明验收。
- 未将 5000 文件的集成测试等同于百万文件性能验证，也未声称支持所有 NAS 文件系统、配额和快照配置。
- 重复检测不是冻结文件系统快照；检测后文件仍可能变化，不应直接驱动删除。
- 本轮没有文件删除、迁移、Windows 隐私清理或自动清理功能。
