# 管理面板

Vite/React 管理面板，使用 Astryx Design System 的 Neutral 主题。需要 Node.js 20.19+ 或 22.12+。默认由仓库根目录的 Dockerfile 构建，并随 Go API 在同一端口提供，无需单独部署前端。

## 本地开发

先启动仓库根目录的 Go API，再运行：

```bash
cd frontend
npm ci
npm run dev
```

访问 `http://127.0.0.1:5173/`。Vite 默认将 `/admin/*` 转发至 `http://127.0.0.1:8787`；其他地址可在 `frontend/.env.local` 中设置 `VITE_DEV_API_TARGET`。登录时输入 `PROXY_ADMIN_KEY`，密钥只保存在当前浏览器标签页的 `sessionStorage`，退出时清除。

## 与 Go 服务一起部署

`docker compose up -d --build` 会构建前端并放入单个服务镜像。面板位于 `/`，管理 API 位于同源 `/admin/*`。本机运行时执行 `npm run build`，再从仓库根目录启动 Go 服务；默认读取 `frontend/dist/`，也可设置 `PROXY_FRONTEND_DIR` 指向该目录。修改前端后，本机重新构建即可；Docker 部署需重建镜像并重启服务。

## 独立部署

在 `frontend/.env.local` 中配置浏览器可访问的 Go API 来源，例如：

```dotenv
VITE_API_BASE_URL=https://api.example.com
```

在 Go 服务中设置 `PROXY_ADMIN_ORIGIN=https://admin.example.com`，必须与前端实际来源完全一致。然后运行 `npm run build`，将 `frontend/dist/` 部署为静态站点。`VITE_API_BASE_URL` 在构建时写入前端资源，不能包含 Token 或管理员密钥。API 必须由 HTTPS 提供；使用上述同源部署时无需设置这两个配置。

面板只在进入或手动刷新时读取账号、密钥和额度接口。任务队列页在可见时每 5 秒读取一次本地队列状态，不请求官方额度；可查看正在执行的请求、等待顺序和每把 key 的占用。账号管理页可添加、替换 Token、启停账号和查看各账号额度；新密钥默认使用已启用账号池轮询，也可固定到指定账号。密钥编辑页可设置最多等待请求数，`-1` 不限、`0` 不排队。配置页可开启单次多图功能，随后才可在密钥管理中逐 key 授权；这两级权限默认都关闭。用量是每把 key 的累计估算值，待核对预留额单独显示；统计语义见仓库根目录的 [API.md](../API.md)。

配置页还可开启图片归档并设置保留天数和容量上限；保留天数 `-1` 表示不限日期。全局归档默认关闭，每把 key 默认参与归档，可在密钥编辑页单独关闭。“生成图库”按 key、IP 和时间筛选归档，提供缩略图、原图预览、下载、删除，以及待处理和失败统计。缩略图与原图均通过带管理员认证头的请求加载，不在图片 URL 中附加管理密钥。归档行为、存储和接口字段见 [API.md 的图片归档章节](../API.md#图片归档)。
