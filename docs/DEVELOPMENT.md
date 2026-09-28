# Docker 版开发指南

部署和使用见 [使用指南](USAGE.md)，Cloudflare 版开发说明见 [Worker 开发指南](https://github.com/shenghuo2/image-api-proxy/blob/feat/cloudflare-workers/docs/DEVELOPMENT.md)。

## 本地运行

本机运行需要 Go 1.22+；先构建前端（Node.js 20.19+ 或 22.12+），再启动 Go 服务：

```bash
cd frontend
npm ci
npm run build
cd ..
```

```bash
PROXY_ADMIN_KEY='<管理员密钥>' PROXY_NAI_TOKEN='<NovelAI Token>' \
  go run ./cmd/novelai-api-proxy
```

本机也可跳过前端构建，仅启动 API。Go 服务会自动挂载工作目录下的 `frontend/dist/`；从其他目录启动时，设置 `PROXY_FRONTEND_DIR` 为构建产物目录。显式指定的目录必须含有 `index.html`，否则启动失败。

## 验证

```bash
go test ./...
go test -race ./...
go vet ./...
```

测试使用本地模拟上游，不消耗 NovelAI 点数。前端开发见 [前端文档](../frontend/README.md)。
