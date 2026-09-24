# NovelAI API Proxy

独立的 Go 代理服务。服务端管理多个 NovelAI 账号，管理员签发独立的客户端 key，分别设置订阅点数、付费购入点数和 Opus 配额的使用权限与上限。新 key 默认在已启用账号池轮询，也可固定使用一个账号。所有访问 NovelAI 的请求按 FIFO 串行执行，生成流结束后才处理下一项。

其他项目的迁移步骤、请求示例和差异说明见 [INTEGRATION.md](INTEGRATION.md)；完整接口与配额结算规则见 [API.md](API.md)。

## 部署

可使用 Docker Compose 一起构建 Go 后端和管理面板。用 `openssl rand -hex 32` 生成管理员密钥；将 `.env.example` 复制为 `.env`，设置 `PROXY_ADMIN_KEY`。可选的 `PROXY_NAI_TOKEN` 仅在首次启动时导入为默认账号；其后在管理面板添加或更换 Token。若暂未配置任何账号，仍可登录管理面板；官方额度查询及生成操作会不可用。不要把 `.env` 或 Token 提交到仓库。

```bash
docker compose up -d --build
```

构建完成后，管理面板位于 `http://127.0.0.1:8787/`，API 使用同一地址。Compose 默认仅映射到主机 `127.0.0.1:8787`，以非 root 身份和只读根文件系统运行；`proxy-data` 卷保存 key 哈希、加密的客户端密钥与账号 Token、额度账本。需要局域网访问时，在 `.env` 中设置 `PROXY_BIND_ADDR` 为主机的局域网 IP，即可从其他设备访问同一个端口。

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

默认监听 `127.0.0.1:8787`，账本位于 `./data/keys.json`，账号保存在同目录的 `keys.json.accounts.json`。可通过 `PROXY_LISTEN_ADDR`、`PROXY_STATE_PATH`、`PROXY_QUEUE_SIZE` 和 `PROXY_QUOTA_TTL` 调整；每个账号的官方额度默认每 5 分钟最多自动刷新一次。只部署**一个实例**，因为队列和账本没有跨实例协调。服务账号也应专供本代理使用，避免外部消费干扰费用结算。公网访问应使用 HTTPS，并关闭反向代理的响应缓冲和敏感请求日志。

## 管理面板

前端源码位于 [`frontend/`](frontend/)，使用 React、Vite 和 Astryx Design System。默认部署时由 Go 服务直接提供构建产物；前端开发与独立部署步骤见 [frontend/README.md](frontend/README.md)。面板可管理账号，签发、查看、轮换、调整及撤销 key，核对待处理额度，并按 key 查看累计估算用量。各类上限可设为 `-1`（不设本地累计上限）；Opus 也可按满额百分比分配。配置页可启用单次多图的全局权限，再逐 key 授权；默认关闭。面板不自动轮询官方额度。

## 开发验证

```bash
go test ./...
go test -race ./...
go vet ./...
```

测试使用本地模拟上游，不使用真实 NovelAI Token。
