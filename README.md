# Plana NovelAI Proxy

一个独立部署的 NovelAI HTTP 转发服务。它只接受 Plana App 当前使用的固定接口，保持请求体、图片 ZIP、Vibe 向量和生成帧原样传输。服务不解析、存储或记录 Token、提示词、图片内容。

## 接口

| 本服务路径 | 方法 | NovelAI 上游 |
| --- | --- | --- |
| `/image/ai/generate-image-stream` | POST | `image.novelai.net/ai/generate-image-stream` |
| `/image/ai/generate-image` | POST | `image.novelai.net/ai/generate-image` |
| `/image/ai/upscale` | POST | `image.novelai.net/ai/upscale`，V5 扩散超分 |
| `/image/ai/encode-vibe` | POST | `image.novelai.net/ai/encode-vibe` |
| `/image/user/subscription` | GET | `image.novelai.net/user/subscription` |
| `/image/user/login` | POST | `image.novelai.net/user/login`，含令牌续期 |
| `/api/ai/upscale` | POST | `api.novelai.net/ai/upscale`，传统超分 |

`GET /healthz` 不要求密钥，仅返回 `ok`。其他路径不会转发，查询参数和非预期方法也会拒绝。

## 运行

需要 Go 1.22+。先生成独立的代理密钥：

```bash
openssl rand -hex 32
```

把结果作为 `PROXY_SHARED_KEY` 配置到运行环境。该密钥与 NovelAI Token 是两种凭证，不要提交到 Git 或打包进 APK。默认只监听 `127.0.0.1:8787`：

```bash
PROXY_SHARED_KEY='<生成的随机密钥>' go run ./cmd/plana-novelai-proxy
```

本机验证：

```bash
curl --fail http://127.0.0.1:8787/healthz
curl -i -H "X-Plana-Proxy-Key: <代理密钥>" \
  -H "Authorization: Bearer <NovelAI Token>" \
  http://127.0.0.1:8787/image/user/subscription
```

第二条请求会触达 NovelAI 并返回该账户订阅信息；不要在共享终端或日志中留下真实 Token。

### Docker Compose 与 HTTPS

将 `.env.example` 复制为 `.env` 并填入随机密钥，再运行：

```bash
docker compose up -d --build
```

Compose 仅把端口映射到主机 loopback，并以非 root、只读文件系统运行。若主机的 `8787` 已被占用，在 `.env` 中设置 `PROXY_HOST_PORT`，并把下方 Caddy 的目标端口改成相同值。可由主机上的 Caddy 提供公网 HTTPS：

```caddyfile
proxy.example.com {
    reverse_proxy 127.0.0.1:8787
}
```

为域名配置 DNS 后，Caddy 自动申请证书。不要开启记录请求头或请求体的访问日志。若使用其他反向代理，请关闭响应缓冲，确保逐帧预览立即到达客户端。健康检查仅表示服务进程可用，不会探测 NovelAI 账户或消费额度。

## 与 Plana App 接线

当前 App 的“代理访问 NovelAI”开关写死使用 `https://novelai.sora214.top/image`，也不会发送 `X-Plana-Proxy-Key`。因此本项目可以独立部署和用 HTTP 客户端测试，但不能只填一个地址就让当前发布版 App 使用私有代理。客户端需要添加：

1. 设置代理根地址，例如 `https://proxy.example.com`；由客户端分别拼接 `/image` 和 `/api`。
2. 在本机安全存储保存代理密钥。只在请求目标为所配置代理时发送 `X-Plana-Proxy-Key`，不得写进 URL、日志或构建产物。
3. 生成、Vibe、额度、登录和续期走 `/image`；传统超分走 `/api`。当前传统超分硬编码了 `api.novelai.net`，需要单独改接线。
4. 自定义第三方 NovelAI endpoint 继续保持原有地址和认证方式。

公网接入建议让客户端验证 HTTPS 地址，避免把代理密钥或 NovelAI Token 发往明文 HTTP。密钥泄露后在服务端轮换，并同步更新客户端设置。

## 行为与边界

- 代理鉴权使用 `X-Plana-Proxy-Key`；NovelAI 的 `Authorization: Bearer` 原样转发。登录的本地派生 access key 会经过代理。
- 每个请求体最多 64 MiB；默认最多 8 个同时进行的请求，可用 `PROXY_MAX_CONCURRENT` 调整。超额返回 `429` 和 `Retry-After: 2`。
- 上游响应头等待上限 6 分钟，生成流本身没有总时长上限；App 取消请求会取消上游请求。
- 不缓存响应，剥除上游 `Set-Cookie`。服务没有请求日志或数据持久化。
- 代理运营者在转发过程中能够看到 NovelAI Token、提示词、图片和登录派生 key。只有由可信主体控制的服务器才适合部署此服务。
- 本服务不修改 NovelAI 的生成协议、费用或服务规则；上游拒绝或限额状态会返回给客户端。

## 开发验证

```bash
go test ./...
go test -race ./...
go vet ./...
```

测试使用本机模拟上游，不使用真实 NovelAI Token，也不会向 NovelAI 发送请求。
