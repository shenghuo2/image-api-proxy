# NovelAI API Proxy

独立的 Go 代理服务。服务端管理多个 NovelAI 账号，管理员签发独立的客户端 key，分别设置订阅点数、付费购入点数和 Opus 配额的使用权限与上限。新 key 默认在已启用账号池轮询，也可固定使用一个账号。所有访问 NovelAI 的请求按 FIFO 串行执行，生成流结束后才处理下一项。

其他项目的迁移步骤、请求示例和差异说明见 [INTEGRATION.md](INTEGRATION.md)；完整接口与配额结算规则见 [API.md](API.md)。

## 部署

可使用 Docker Compose 一起构建 Go 后端和管理面板。用 `openssl rand -hex 32` 生成管理员密钥；将 `.env.example` 复制为 `.env`，设置 `PROXY_ADMIN_KEY`。可选的 `PROXY_NAI_TOKEN` 仅在首次启动时导入为默认账号；其后在管理面板添加或更换 Token。若暂未配置任何账号，仍可登录管理面板；官方额度查询及生成操作会不可用。不要把 `.env` 或 Token 提交到仓库。

```bash
docker compose up -d --build
```

也可直接拉取已发布的单镜像运行（包含前端和后端）：

```bash
docker volume create novelai-proxy-data
docker run -d --name novelai-api-proxy --restart unless-stopped \
  --env-file .env \
  -e PROXY_LISTEN_ADDR=0.0.0.0:8787 \
  -e PROXY_BIND_ADDR=127.0.0.1 \
  -e PROXY_STATE_PATH=/data/keys.json \
  -v novelai-proxy-data:/data \
  -p 127.0.0.1:8787:8787 \
  shenghuo2/novelai-api-proxy:v0.1.2
```

镜像在 Docker Hub 使用标签 `shenghuo2/novelai-api-proxy:v0.1.2`，支持 `linux/amd64` 和 `linux/arm64`；此版本包含图片归档及 SQLite 数据结构兼容升级。需要局域网访问时，将 `-p` 中的 `127.0.0.1` 换成主机局域网 IP，并相应修改 `PROXY_BIND_ADDR`；公网访问应通过 HTTPS 反向代理。升级时继续挂载同一个 `/data` 卷；Compose 部署执行 `docker compose up -d --build`。`docker run` 部署请将镜像标签改为新版并重新创建容器，保留原数据卷。

构建完成后，管理面板位于 `http://127.0.0.1:8787/`，API 使用同一地址。Compose 默认仅映射到主机 `127.0.0.1:8787`，以非 root 身份和只读根文件系统运行；`proxy-data` 卷保存 SQLite 账本、持久化任务的请求体和结果，以及开启归档后生成的原图和缩略图。需要局域网访问时，在 `.env` 中设置 `PROXY_BIND_ADDR` 为主机的局域网 IP，即可从其他设备访问同一个端口。

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

默认监听 `127.0.0.1:8787`，SQLite 数据库位于 `./data/keys.json.sqlite`，任务请求体和结果位于 `keys.json.jobs/`，归档位于 `keys.json.archive/`。`PROXY_STATE_PATH` 仍指定旧账本路径的基名；首次启动时校验并一次性导入旧 `keys.json`、账号和设置 JSON 及任务元数据。导入失败会拒绝启动，不会创建空账本；若留下未完成的 `.sqlite` 文件，先核对并恢复旧 JSON 备份，再移走该未完成数据库后重试。成功后旧 JSON 文件原样保留为迁移前备份、不再更新。回退旧镜像前须从升级前备份恢复这些文件。

数据库使用 `PRAGMA user_version` 管理结构版本。已有的无版本 SQLite 会在启动时校验完整性和必需表，再在事务内升级设置及索引；旧设置缺少归档字段时补入 30 天、20 GiB 默认值。缺表、无效设置或高于当前程序支持的版本会拒绝启动，不会重建空账本。升级前请备份整个 `/data` 卷。数据库使用 WAL；备份运行中的实例时应使用 SQLite 在线备份或先停止容器，不能只复制主 `.sqlite` 文件。旧镜像不理解新数据库结构，降级前须恢复升级前备份。

可通过 `PROXY_LISTEN_ADDR`、`PROXY_STATE_PATH`、`PROXY_QUEUE_SIZE` 和 `PROXY_QUOTA_TTL` 调整；每个账号的官方额度默认每 5 分钟最多自动刷新一次。只部署**一个实例**，因为队列没有跨实例协调；更新容器时应先停止旧实例，再启动新实例。服务账号也应专供本代理使用，避免外部消费干扰费用结算。公网访问应使用 HTTPS，并关闭反向代理的响应缓冲和敏感请求日志。

频繁更新且有用户在排队时，接入方应使用 [API.md](API.md) 的持久化任务接口，并为每次请求提供 `Idempotency-Key`。普通官方同步接口仍兼容，但连接断开后的请求无法重新接收结果。收到停止信号后，服务停止接纳新任务，让正在执行的任务最多继续 7 分钟；等待中的持久化任务由新进程恢复。Compose 预留 8 分钟停止宽限。请求体和结果在卷中以权限 0600 的文件保存，完成 24 小时后在服务重启或下次提交时清理；请按实际请求大小规划卷空间和备份策略。

## 管理面板

前端源码位于 [`frontend/`](frontend/)，使用 React、Vite 和 Astryx Design System。默认部署时由 Go 服务直接提供构建产物；前端开发与独立部署步骤见 [frontend/README.md](frontend/README.md)。面板可管理账号，签发、查看、轮换、调整及撤销 key，核对待处理额度，并按 key 查看累计估算用量。各类上限可设为 `-1`（不设本地累计上限）；Opus 也可按满额百分比分配。每把 key 可设置最多等待请求数，任务队列页展示当前执行项和等待池。配置页可启用单次多图的全局权限，再逐 key 授权；默认关闭。面板不自动轮询官方额度。

图片归档默认关闭。在面板“配置”页开启后，普通及流式图像生成、`/image` 别名和持久化任务的成功结果都会进入后台归档；流式请求只保存最终成图帧。归档保留每张原始 PNG，并生成不超过 100 KiB 的 JPEG 缩略图；单次多图按一次生成分组。每把 key 默认参与归档，可在密钥编辑页单独关闭，关闭只影响后续生成。“生成图库”支持按 key、IP、时间筛选、预览、下载和删除，显示待处理数、失败次数与最近错误。默认保留 30 天、总容量 20 GiB；保留天数为 `-1` 时不限日期，但仍受容量上限约束。原图和缩略图仅通过管理员认证接口访问；目录与文件权限分别为 0700 和 0600。归档解析或写盘失败不会改变生成响应。配置字段和接口示例见 [API.md](API.md#图片归档)。

归档为防止异常上游响应耗尽内存，对单张 PNG 设 32 MiB、完整响应设 128 MiB 的解析上限；超限时只跳过归档，不影响客户端收到的原始响应。

## 反向代理与调用者 IP

图库记录提交生成请求时的调用者 IP；持久化任务也使用提交时的地址，不使用后台执行地址。默认以连接地址为准，忽略客户端自行提交的 `X-Forwarded-For`。当 Nginx 与本服务在同一台宿主机、服务仅绑定宿主机 `127.0.0.1` 时，设置 `PROXY_BIND_ADDR=127.0.0.1`；代理会自动信任本机回环和 Docker 默认网桥网关。局域网绑定时不会自动信任转发头。

若 Nginx 在另一台主机，且代理通过宿主机的局域网地址提供服务，可按下面的示例配置。把地址换成代理宿主机实际可达的地址；Nginx 应独占代理的外部入口。如果 Nginx 前面还有其他代理，先在 Nginx 上配置其可信来源，确保 `$remote_addr` 是最终调用者地址。

```nginx
server {
    listen 443 ssl;
    server_name nai.example.com;
    ssl_certificate /path/to/fullchain.pem;
    ssl_certificate_key /path/to/privkey.pem;
    client_max_body_size 64m;

    location / {
        proxy_pass http://192.168.123.154:8787;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_request_buffering off;
        proxy_buffering off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
```

代理宿主机的 Compose `.env` 示例：

```dotenv
PROXY_BIND_ADDR=192.168.123.154
PROXY_TRUSTED_PROXY_CIDRS=172.17.0.1/32
```

`172.17.0.1` 仅是示例：Docker 端口映射可能让容器把所有外部连接都看成网桥网关。应以容器实际看到的连接地址为准，使用精确的 `/32`，并重建容器使环境变量生效。**只允许远端 Nginx 主机访问代理的 8787 端口**，否则直连者也能伪造转发头。Docker 发布端口的流量可能绕过普通宿主机 `INPUT` 规则，需要在网络边界限制来源；使用 Docker iptables 后端时也可在 `DOCKER-USER` 链限制。Nginx 用 `$remote_addr` 覆盖传入的 `X-Forwarded-For`，不要将客户端原有的该请求头原样透传。其他部署拓扑可通过 `PROXY_TRUSTED_PROXY_CIDRS` 配置实际受控代理的精确 CIDR；多个 CIDR 以逗号分隔。

## 开发验证

```bash
go test ./...
go test -race ./...
go vet ./...
```

测试使用本地模拟上游，不使用真实 NovelAI Token。
