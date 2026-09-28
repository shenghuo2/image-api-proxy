# NovelAI API Proxy

为 NovelAI 图片生成提供统一入口：管理多个账号，为不同调用方分配独立密钥和额度，按队列依次生成图片。

> 多账号 · 客户端密钥 · Anlas / Opus 配额 · 全局 FIFO · 同步 / 流式生成 · 持久化任务

## 功能

- **账号管理**：多个 NovelAI 账号轮询，也可将客户端固定到指定账号。
- **独立密钥**：按调用方分配权限、额度和排队上限，支持撤销及轮换。
- **配额管理**：订阅 Anlas、购入 Anlas、Opus 次数及按比例回充，支持异常扣费人工核对。
- **生成接口**：兼容官方图片请求格式，支持同步、流式和可查询的持久化任务。
- **管理面板**：查看账号、配额、任务队列和生成用量。

## 选择版本

**推荐用自己的 VPS 部署 Docker 版**，方便选择和管理出口 IP。IP 质量取决于服务商和使用历史；Cloudflare 版使用平台出口，无法指定独享 IP。

| 对比 | Docker 版（推荐） | Cloudflare Workers 版 |
| --- | --- | --- |
| 部署环境 | 自己的 VPS / 服务器 | Cloudflare，无需维护服务器 |
| 出口 IP | 由自己的服务器出口决定 | 平台出口，不提供固定独享 IP 选择 |
| 图片图库与归档 | 支持，可配置保留时间与容量 | 不提供 |
| 持久化任务结果 | 完成后保留 24 小时，归档另计 | 完成后保留 30 分钟 |
| 数据存储 | 本机持久化卷 | SQLite Durable Object |
| 成本 | VPS 费用 | Free 套餐可用，具体用量见部署教程 |
| 适合谁 | 长期使用、需要图库、希望自主控制出口 | 轻量使用、无需图库、希望快速部署 |
| 源码分支 | `main` | `feat/cloudflare-workers` |

两个版本独立维护，运行数据不自动互迁。NovelAI 订阅和生成费用均另计。

## 部署

### Docker / VPS（推荐）

按 [Docker 部署教程](docs/USAGE.md) 安装并启动。教程包含 Docker Compose、单容器、持久化存储、HTTPS 反向代理及升级说明。

### Cloudflare Workers

不想维护服务器时，可按 [Cloudflare 部署教程](https://github.com/shenghuo2/image-api-proxy/blob/feat/cloudflare-workers/docs/USAGE.md) 一键部署。

每天约 1000 次生成的免费额度测算见 [Cloudflare 教程](https://github.com/shenghuo2/image-api-proxy/blob/feat/cloudflare-workers/docs/USAGE.md#free-计划额度)。

## 文档

- [部署与使用](docs/USAGE.md)：安装、管理面板、图库、数据与反向代理。
- [API 参考](API.md) / [接入指南](INTEGRATION.md)：请求格式与第三方接入。
- [开发指南](docs/DEVELOPMENT.md)：本地运行与测试。
