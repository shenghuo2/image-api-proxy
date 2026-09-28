# 验证记录

验证日期：2026-09-28。分支 `feat/cloudflare-workers`，基线 `6e799b4`。

## 已通过的检查

- `npm run types`、`npm run check`、`npm run format:check`。
- 在没有本地 `.dev.vars` 的环境执行 `wrangler types --check`，确认 CI 不依赖开发者密钥文件。
- **41 项功能测试**：账号池与固定绑定、Opus 回充/预测/退款、付费配额、多图权限、鉴权和 key 轮换；FIFO 与响应完成边界；multipart 原始字节；完整/破损流式帧；幂等、取消、过期、容量保护、结果超限、DO 重启恢复；管理页面路径与静态资源。
- `npm run build`：前端构建及 Worker 部署 dry-run 通过。Worker 上传体积 79.65 KiB，gzip 20.50 KiB。
- 原 Go 实现 `go test ./...` 通过。
- Worker 完整依赖树 `npm audit`：0 个已知漏洞。
- 真实本地 `wrangler dev --local` 进程：健康检查、管理页面、未授权拒绝，以及账号 Token 的加密创建/读取/删除通过。测试后已关闭本地服务器。

前端保留一个 Vite 提示：主 JS chunk 大于 500 kB（gzip 约 170 kB）；不影响构建或 Worker CPU，因为前端资源在浏览器执行。

## 1000 次模拟生成

通过 `npm run test:load` 复现，最新机器可读输出位于 `artifacts/load.json`（忽略进版本控制）。CI 也执行该测试，并上传 `worker-load-metrics` 报告。使用真实 workerd、DO SQLite 和 alarm；上游是内存模拟，不调用 NovelAI，也不消耗点数。

负载包括 100 次同步生成、900 次持久化任务；普通结果和流式结果混合，每个结果约 2 MiB。模拟一天均匀请求，每次推进业务时钟 86.4 秒，执行 30 分钟结果过期及清理。每个任务查询状态并下载结果。该测试验证预算和账本一致性，不代表真实网络下的吞吐基准。

| 指标 | 本次结果 |
| --- | ---: |
| 完成生成 | 1,000 |
| API 请求 | 2,803 |
| Alarm 执行 | 900 |
| 读取行数（含 alarm 保守估计） | 479,287 |
| 写入行数（含 alarm 保守估计） | 35,834 |
| 完成后结果占用抽样最大值 | 36.00 MiB |
| SQLite 大小抽样最大值 | 37.23 MiB |
| workerd 宿主进程峰值 RSS | 329.91 MiB |
| 单 isolate 堆峰值 | 本环境无法可靠测量 |

负载测试约 163 秒完成。宿主进程 RSS 包含测试框架和数据库，不能据此认定线上 isolate 的内存占用。测试每次生成后清理 mock 调用历史，避免测试框架人为保留全天的图片响应。


## 测量边界

- SQL 行数由 SQLite cursor 累计，包含索引写入和删除；alarm 存储操作额外按每次一行保守计入。计数不含初始化账本；本地测试辅助调用不计入生产 API 请求数。
- 存储指标在每完成 20 次生成后抽样，表示均匀负载下的完成后占用，不包含正在执行任务的最大容量预留，也不代表突发负载的绝对峰值。
- 2803 次 API 请求不是所有真实访问的总数。生产环境还要加上更多轮询、管理页面与静态资源请求，并考虑同账号其他项目的额度。
- workerd 的 `process.memoryUsage().heapUsed` 在本环境返回 0，代表不支持此测量，**不是内存占用为零**。报告另记录 Linux workerd 进程的峰值 RSS，包含 SQLite、运行时及测试框架；不能将其直接与线上单 isolate 的 128 MB 限制比较。
- 已完成真实 Cloudflare 部署和基础线上验证，见下文；账号订阅计划未确认。实际 Worker/DO CPU、isolate 内存、NovelAI 连通性及长时间传输仍需验证。此次没有调用真实生成 API，没有产生上游费用。

## 线上部署（2026-09-28）

- 地址：<https://novelai-api-proxy-free.shenghuo2.workers.dev/console/>。
- Worker：`novelai-api-proxy-free`，版本：`e72549d8-cd08-4b35-90e1-e429b7bf8108`。
- 首次部署创建 SQLite Durable Object binding `PROXY`，使用 `v1` migration 和固定对象名 `global-v1`；未创建 D1 或 R2。
- 使用 `wrangler deploy --secrets-file .dev.vars` 上传管理员 Secret。密钥文件未纳入版本控制，权限为 `0600`；需妥善备份。
- Cloudflare 报告 Worker 启动时间 1 ms；这不是每次请求的 CPU 测量。
- curl 线上验证通过：`/healthz`、`/console/` 及 JS/CSS 返回 200；无凭据访问管理接口返回 401；管理员读取账号和配置返回 200；图库接口返回 404。
- 创建禁用的合成测试账号返回 201，后续独立请求确认记录存在，再删除返回 204；测试账号已清理，未发起上游请求。
- 环境连接存在波动：Wrangler 曾出现连接超时，重试后部署成功；Python urllib 请求健康接口返回 Cloudflare 1010，而 curl 验证通过。不能据此保证所有客户端和网络均可访问。
- 当前 OAuth 凭据读取账号订阅接口返回 403，无法确认当前账号是否为 Free。部署过程未升级套餐或绑定付款方式；Free 额度兼容性仍以配置、模拟负载测算及后续真实 Free 环境验证为准。
- 尚未配置真实 NovelAI Token；用户可在管理面板添加账号后创建客户端 key，继续验证真实生成。
