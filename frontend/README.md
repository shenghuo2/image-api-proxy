# 管理面板（Worker 版）

React/Vite 管理面板，使用 Astryx Design System，通过 Workers Static Assets 同源部署，默认入口 `/console/`。

账号、密钥、队列和配置操作见 [使用指南](../docs/USAGE.md)。没有图库、归档或缩略图。

本地热更新、构建及测试见 [开发指南](../docs/DEVELOPMENT.md)。在仓库根目录运行 `npm run build` 构建页面。

独立托管前端时，构建前设置 `VITE_API_BASE_URL` 为 API 根地址，并把 Worker 的 `PROXY_ADMIN_ORIGIN` 设为前端完整来源。同源部署无需此配置；构建变量不得包含管理员密钥或 Token。
