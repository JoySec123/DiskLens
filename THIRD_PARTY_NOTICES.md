# 第三方组件说明

DiskLens 使用 Go 标准库、`modernc.org/sqlite`、`golang.org/x/sys`、Lucide 图标及其传递依赖。依赖版本以 `go.mod`、`go.sum` 和 `package-lock.json` 为准。

各组件仍遵循其原许可证。`scripts/build-licenses.mjs` 会从当前固定依赖生成 FPK 内的许可证集合和 `NOTICES.txt`；仓库内的 Lucide 资源同时保留 [Lucide 许可证](cmd/disklens/web/assets/LUCIDE-LICENSE.txt)。本项目根目录的 MIT License 不替代第三方许可证。
