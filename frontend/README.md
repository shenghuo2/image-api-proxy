# 管理面板

独立的 Vite/React 前端，使用 Astryx Design System 的 Neutral 主题。需要 Node.js 20.19+ 或 22.12+。

## 本地开发

先启动仓库根目录的 Go API，再运行：

```bash
cd frontend
npm ci
npm run dev
```

访问 `http://127.0.0.1:5173/`。Vite 默认将 `/admin/*` 转发至 `http://127.0.0.1:8787`；其他地址可在 `frontend/.env.local` 中设置 `VITE_DEV_API_TARGET`。登录时输入 `PROXY_ADMIN_KEY`，密钥只保存在当前浏览器标签页的 `sessionStorage`，退出时清除。

## 独立部署

在 `frontend/.env.local` 中配置浏览器可访问的 Go API 来源，例如：

```dotenv
VITE_API_BASE_URL=https://api.example.com
```

在 Go 服务中设置 `PROXY_ADMIN_ORIGIN=https://admin.example.com`，必须与前端实际来源完全一致。然后运行 `npm run build`，将 `frontend/dist/` 部署为静态站点。`VITE_API_BASE_URL` 在构建时写入前端资源，不能包含 Token 或管理员密钥。API 必须由 HTTPS 提供；前端和 API 同源时可省略这两个配置并在反向代理中转发 `/admin/*`。

面板只在进入或手动刷新时读取管理接口。账号管理页可添加、替换 Token、启停账号和查看各账号额度；新密钥默认使用已启用账号池轮询，也可固定到指定账号。配置页可开启单次多图功能，随后才可在密钥管理中逐 key 授权；这两级权限默认都关闭。用量是每把 key 的累计估算值，待核对预留额单独显示；统计语义见仓库根目录的 [API.md](../API.md)。
