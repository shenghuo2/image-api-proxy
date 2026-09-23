# NovelAI API Proxy

独立的 Go 代理服务。服务端持有 NovelAI Token，管理员签发独立的客户端 key，分别设置固定 Anlas、已购 Anlas 和 Opus 免费生成权限与上限。所有访问 NovelAI 的请求按 FIFO 串行执行，生成流结束后才处理下一项。

接口认证、路由、密钥管理和配额结算的使用方法见 [API.md](API.md)。

## 部署

需要 Go 1.22+，也可使用 Docker Compose。用 `openssl rand -hex 32` 生成管理员密钥；将 `.env.example` 复制为 `.env`，设置 `PROXY_ADMIN_KEY` 和服务账户的 `PROXY_NAI_TOKEN`。不要把 `.env` 或 Token 提交到仓库。

```bash
docker compose up -d --build
```

Compose 仅把服务映射到主机 `127.0.0.1:8787`，以非 root 身份和只读根文件系统运行；`proxy-data` 卷保存 key 哈希与额度账本。本机直接运行：

```bash
PROXY_ADMIN_KEY='<管理员密钥>' PROXY_NAI_TOKEN='<NovelAI Token>' \
  go run ./cmd/novelai-api-proxy
```

默认监听 `127.0.0.1:8787`，账本位于 `./data/keys.json`。可通过 `PROXY_LISTEN_ADDR`、`PROXY_STATE_PATH`、`PROXY_QUEUE_SIZE` 和 `PROXY_QUOTA_TTL` 调整；官方额度默认每 5 分钟最多自动刷新一次。只部署**一个实例**，因为队列和账本没有跨实例协调。服务账户也应专供本代理使用，避免外部消费干扰费用结算。公网访问应使用 HTTPS，并关闭反向代理的响应缓冲和敏感请求日志。

## 开发验证

```bash
go test ./...
go test -race ./...
go vet ./...
```

测试使用本地模拟上游，不使用真实 NovelAI Token。
