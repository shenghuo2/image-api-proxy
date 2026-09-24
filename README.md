# NovelAI API Proxy

独立的 Go 代理服务。服务端持有 NovelAI Token，管理员签发独立的客户端 key，分别设置订阅点数、付费购入点数和 Opus 配额的使用权限与上限。所有访问 NovelAI 的请求按 FIFO 串行执行，生成流结束后才处理下一项。

其他项目的迁移步骤、请求示例和差异说明见 [INTEGRATION.md](INTEGRATION.md)；完整接口与配额结算规则见 [API.md](API.md)。

## 部署

需要 Go 1.22+，也可使用 Docker Compose。用 `openssl rand -hex 32` 生成管理员密钥；将 `.env.example` 复制为 `.env`，设置 `PROXY_ADMIN_KEY` 和服务账户的 `PROXY_NAI_TOKEN`。若暂未配置 NovelAI Token，仍可登录管理面板查看已存储的密钥；官方额度查询及依赖额度的操作会不可用。不要把 `.env` 或 Token 提交到仓库。

```bash
docker compose up -d --build
```

Compose 默认仅把服务映射到主机 `127.0.0.1:8787`，以非 root 身份和只读根文件系统运行；`proxy-data` 卷保存 key 哈希、加密的客户端密钥与额度账本。需要局域网访问时，在 `.env` 中设置 `PROXY_BIND_ADDR` 为主机的局域网 IP，前端开发服务器使用 `--host 0.0.0.0` 启动。本机直接运行：

```bash
PROXY_ADMIN_KEY='<管理员密钥>' PROXY_NAI_TOKEN='<NovelAI Token>' \
  go run ./cmd/novelai-api-proxy
```

默认监听 `127.0.0.1:8787`，账本位于 `./data/keys.json`。可通过 `PROXY_LISTEN_ADDR`、`PROXY_STATE_PATH`、`PROXY_QUEUE_SIZE` 和 `PROXY_QUOTA_TTL` 调整；官方额度默认每 5 分钟最多自动刷新一次。只部署**一个实例**，因为队列和账本没有跨实例协调。服务账户也应专供本代理使用，避免外部消费干扰费用结算。公网访问应使用 HTTPS，并关闭反向代理的响应缓冲和敏感请求日志。

## 管理面板

独立前端位于 [`frontend/`](frontend/)，使用 React、Vite 和 Astryx Design System。开发与独立部署步骤见 [frontend/README.md](frontend/README.md)。面板可签发、查看、轮换、调整及撤销 key，核对待处理额度，并按 key 查看累计估算用量。各类上限可设为 `-1`（不设本地累计上限）；Opus 也可按满额百分比分配。配置页可启用单次多图的全局权限，再逐 key 授权；默认关闭。面板不自动轮询官方额度。

## 开发验证

```bash
go test ./...
go test -race ./...
go vet ./...
```

测试使用本地模拟上游，不使用真实 NovelAI Token。
