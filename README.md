# NovelAI API Proxy — Cloudflare Workers

独立 Serverless 版本，使用 TypeScript Worker、SQLite Durable Object 和 Workers Static Assets。无需服务器、R2 或 D1。按每天约 1000 次生成、平均结果约 2 MiB 的测试负载，Cloudflare Free 计划的请求、SQL 读写与存储额度够用；单 DO 全天活跃的时长预算也在免费额度内。真实请求 CPU、isolate 内存及上游吞吐仍需验证，详细测算见下表。NovelAI 订阅和生成费用另计。

支持管理面板、多账号、客户端 key、Anlas/Opus 配额、全局 FIFO、同步/流式生成及持久化任务。没有图库和归档，任务结果完成后保留 **30 分钟**。

## 选择版本

| 版本 | 分支 | 部署与功能 |
| --- | --- | --- |
| [Go / Docker](https://github.com/shenghuo2/image-api-proxy/tree/main) | `main`（默认） | Docker Compose，支持图库和长期归档 |
| [Cloudflare Workers](https://github.com/shenghuo2/image-api-proxy/tree/feat/cloudflare-workers) | `feat/cloudflare-workers` | 无需服务器，SQLite DO，任务结果保留 30 分钟，无图库 |

Cloudflare 一键部署（始终使用 `feat/cloudflare-workers` 分支）：

[![Deploy to Cloudflare](https://deploy.workers.cloudflare.com/button)](https://deploy.workers.cloudflare.com/?url=https%3A%2F%2Fgithub.com%2Fshenghuo2%2Fimage-api-proxy%2Ftree%2Ffeat%2Fcloudflare-workers)

两个版本独立维护，运行数据不自动互迁。Docker 主分支继续维护，分叉前代码保留在 Git 历史中（基线 `6e799b4`）。

## 文档

| 文档 | 面向读者 | 内容 |
| --- | --- | --- |
| [部署与使用](docs/USAGE.md) | 部署者、管理员、调用方 | 一键部署、首次配置、日常使用、限制和故障处理 |
| [开发与维护](docs/DEVELOPMENT.md) | 开发者 | 架构、本地环境、测试、构建和发布 |
| [API 参考](API.md) / [接入指南](INTEGRATION.md) | API 使用者 | 请求协议与第三方接入；Worker 差异以使用指南为准 |
| [验证记录](worker/VALIDATION.md) | 维护者 | 功能、负载和线上验证结果 |

## 一键部署

[![Deploy to Cloudflare](https://deploy.workers.cloudflare.com/button)](https://deploy.workers.cloudflare.com/?url=https%3A%2F%2Fgithub.com%2Fshenghuo2%2Fimage-api-proxy%2Ftree%2Ffeat%2Fcloudflare-workers)

按钮固定使用公开仓库的 `feat/cloudflare-workers` 分支和完整仓库根目录。Cloudflare 构建命令为 `npm run build`，部署命令为 `npm run deploy`，按提示填写随机管理员 Secret `PROXY_ADMIN_KEY`。部署后打开自己的 `/console/`，添加 NovelAI Token 并创建客户端 key。详见 [部署步骤](docs/USAGE.md#一键部署到-cloudflare)。

## 当前使用量与 Free 额度对比

线上快照时间：**2026-09-28 03:49 UTC**，部署为 `novelai-api-proxy-free.shenghuo2.workers.dev`。当前存储和行数来自管理员接口 `/admin/runtime`；每日负载数据来自本地真实 workerd + SQLite 的 **1000 次模拟生成**，不是线上账单。模拟包含 100 次同步生成、900 次持久化任务，结果约 2 MiB，按一天均匀到达，包含任务状态查询、下载和过期清理。

| 资源 | 当前线上可观测值 | 每天 1000 次生成的测算/测试值 | Free 额度 | 测算占额度 |
| --- | --- | --- | --- | --- |
| Worker 请求 | 全天统计未取得 | 基础 API 请求 2,803 次/天 | 100,000 次/天 | 2.80% |
| Durable Object 请求 | 全天统计未取得 | 基础 3,703 次/天（2,803 次 API + 900 次 alarm） | 100,000 次/天 | 3.70% |
| DO 活跃时长 | 未取得 | 单对象全天活跃的保守预算 11,059.2 GB-s/天 | 13,000 GB-s/天 | 85.07% |
| SQLite 读取行数 | 当前实例累计 8（SQL 7 + alarm 估计 1），非全天统计 | 479,287 行/天 | 5,000,000 行/天 | 9.59% |
| SQLite 写入行数 | 当前实例累计 0，非全天统计 | 35,834 行/天 | 100,000 行/天 | 35.83% |
| SQLite 存储 | 57,344 B（56 KiB） | 数据库抽样峰值 39,038,976 B（37.23 MiB） | 5 GB，账号总量 | 0.78%（按 5×10⁹ B 计算） |
| 临时数据及容量预留 | 0 B | 完成后占用抽样峰值 36.00 MiB | 项目自设 512 MiB 上限，非 Cloudflare 独立额度 | 项目上限的 7.03% |
| Worker 每请求 CPU | 未取得 | 本地测试不能验证线上 CPU | 10 ms/请求 | 待测 |
| 单 isolate 内存 | 未取得 | 无可靠的 isolate 峰值测量 | 128 MB | 待测 |
| Worker 启动时间 | 部署报告 1 ms | 不随每日生成次数直接累加 | 1,000 ms/次启动 | 当前值 0.10% |

**读表注意：**

- 当前 OAuth 凭据查询 Cloudflare Analytics 返回 401，查询订阅返回 403，因此没有取得线上全天计量，也没有确认该账号实际订阅是否为 Free。表中未知值不代表零；部署未升级套餐或绑定付款方式。
- `/admin/runtime` 的读写计数保存在 DO 实例内存中，实例重新加载后会重置；当前写入为 0 不代表今天没有写入。SQL 测试计数包含索引维护和删除，alarm 存储操作按每次一行保守计入。
- 请求测算是基础负载，不含用户额外轮询、管理页面和静态资源访问。当前 `run_worker_first: true`，静态资源访问也会调用入口 Worker，并通过 DO 查询管理路径，需要计入请求预算。常驻队列页面每 10 秒刷新，持续一天会额外产生约 8,640 次 API 请求。
- DO 时长按 `86,400 秒 × 0.128 GB` 估算，属于单对象全天活跃预算，不是当前实测。它只剩约 14.93% 余量；同账号其他 DO、部署期间实例切换等也可能消耗额度。每日额度按账号共享，UTC 零点重置。
- 存储峰值每完成 20 次生成抽样一次，不代表突发请求的绝对峰值；临时容量预留不等于 SQLite 物理大小。项目 512 MiB 容量上限不涵盖数据库全部索引和其他元数据。
- 本地 workerd 宿主进程 RSS 峰值约 329.91 MiB，包含运行时、SQLite 和测试框架，不能拿它直接对比线上 isolate 的 128 MB 限制。部署启动时间也不能代替每请求 CPU 测量。
- 目前已验证线上页面、鉴权和数据库读写，尚未执行真实 NovelAI 生成。单并发要持续完成 1000 次/天，平均每次占用队列需低于 86.4 秒；上游速度、长响应及真实 CPU/内存仍需验证。

额度来源（2026-09-28 核对）：[Workers 限制](https://developers.cloudflare.com/workers/platform/limits/)、[Durable Objects 定价与免费额度](https://developers.cloudflare.com/durable-objects/platform/pricing/)。完整测试口径见 [验证记录](worker/VALIDATION.md)。

## 原 Go 版本

Go 源码保留作行为参考，原部署说明见 [README.go.md](README.go.md)。此分支的管理面板针对 Worker 版，不含 Go 版图库。
