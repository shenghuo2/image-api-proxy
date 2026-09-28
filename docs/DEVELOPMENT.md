# 开发与维护指南

面向贡献者。部署、管理员操作和客户端接入见 [使用指南](USAGE.md)，容量测算见 [README](../README.md#当前使用量与-free-额度对比)，已执行的验证见 [验证记录](../worker/VALIDATION.md)。

## 工程与架构

| 路径 | 职责 |
| --- | --- |
| `worker/src/index.ts` | 薄入口、管理路径校验、Static Assets 与 DO 转发 |
| `worker/src/proxy.ts` | 单个 ProxyCoordinator DO、认证、账号、全局 FIFO、生成及 alarm |
| `worker/src/store.ts` | SQLite、事务、任务分块和过期清理 |
| `worker/src/ledger.ts` | 配额预留、退款、Opus 回充及人工核对 |
| `worker/src/outcome.ts` | 增量流式结果识别，跳过图片内容 |
| `worker/test/` | 功能、重启、任务、流式与负载测试 |
| `frontend/` | React/Vite 管理面板 |
| `wrangler.jsonc`、根 package.json | 整仓一键部署及根目录 CLI 构建入口 |
| `worker/wrangler.jsonc` | 独立 Worker 开发、测试及原 CLI 入口 |
| Go 源码、README.go.md | 原版行为对照，不参与 Worker 部署 |

单部署使用固定 DO 名称 `global-v1`，SQLite 保存账本、队列、幂等记录和临时结果。SQLite DO 不是 D1。请求状态与额度预留必须先原子持久化，再向上游发送；响应结束并结算后才释放执行位置。重启中断不自动重发。结果按最大 1 MiB BLOB 分块，保留 30 分钟，身份与幂等摘要保留 24 小时。

## 环境与本地运行

推荐 Node.js 24，npm 锁文件必须提交。仓库根目录执行：

```bash
npm ci
npm run build
cp worker/.dev.vars.example worker/.dev.vars
# 编辑 worker/.dev.vars，设置随机 PROXY_ADMIN_KEY
npm --prefix worker run dev
```

访问 `http://localhost:8787/console/`。本地 SQLite 位于 `worker/.wrangler/`，不连接线上数据。前端热更新另开终端运行 `npm --prefix frontend run dev`；默认 `/admin/*` 代理到 8787。可在 `frontend/.env.local` 设置 `VITE_DEV_API_TARGET`。

根目录 `npm run build` 会分别 `npm ci` 安装两个子工程并构建前端，适用于干净的 Cloudflare Builds 环境；日常前端迭代可只运行 `npm --prefix frontend run build`。

## 配置维护

根目录部署配置与 `worker/wrangler.jsonc` 保持相同业务配置，区别仅为 schema、入口和静态资源的相对路径。修改绑定、兼容日期、migration 或变量时同步两份配置；一键部署允许用户为自己的实例修改 Worker 名称。根目录与 worker 目录的 `.dev.vars.example` 均仅放占位提示。

调整 Worker 配置后运行 `npm --prefix worker run types` 更新绑定类型。测试池的 Miniflare/Wrangler 通过 overrides 与锁文件固定兼容版本，升级时一起检查运行时兼容性。不要删除 migration 或更换固定对象名来升级已有实例。

## 检查与测试

```bash
npm --prefix worker run types -- --check
npm --prefix worker run check
npm --prefix worker run format:check
npm --prefix worker test
npm --prefix worker run test:load
npm run deploy -- --dry-run
```

最后一条仅编译并验证根目录发布入口，不上传。`npm --prefix worker run build` 则验证原 Worker 子目录入口。功能测试会先构建前端；模拟上游不消耗 NovelAI 点数。1000 次混合负载约需 3 分钟，输出 `worker/artifacts/load.json`，不能当作真实 Free 运行时 CPU/内存或上游吞吐证明。

GitHub Actions 执行类型、格式、功能测试、负载测试和两个部署入口的 dry-run。Go 行为对照修改可运行 `go test ./...`。

## 发布与线上验证

完整构建后按 [使用指南](USAGE.md) 发布。首次上线用独立测试 key 验证普通/流式生成、任务领取、过期和重启恢复；真实生成会消耗上游额度。关注 Cloudflare 的请求、CPU、内存错误、SQL 行数与 DO Duration，保留不明确的扣费项供人工核对。

README 按钮固定指向 `https://github.com/shenghuo2/image-api-proxy/tree/feat/cloudflare-workers`。`main` 保留 Go/Docker 实现，`feat/cloudflare-workers` 维护完整 Worker 工程；更新 Worker 时推送 `feat/cloudflare-workers`，不要覆盖默认 `main` 分支。仅存在本地 worktree 或仅部署到 workers.dev 不足以更新按钮源码。按钮必须指向已发布的公开 GitHub/GitLab 仓库根目录（可含分支），不能指向 `worker/`。发布分支或仓库迁移后更新 README 按钮 URL。官方行为见 [Deploy to Cloudflare 文档](https://developers.cloudflare.com/workers/platform/deploy-buttons/)。

## 数据和日志

不要提交真实 `.dev.vars`、管理员密钥或账号 Token。服务不记录完整请求体、图片及 Token；SQLite 保存累计用量和 8 天的生成时间桶。输入和结果仅按持久化任务生命周期保留。Secret 同时用于 Token/key 加密，轮换影响已有密文，遵循使用指南。

## 双版本维护约定

- Docker 使用 `main`，Worker 使用 `feat/cloudflare-workers`；不进行整分支互相合并。
- 通用协议与配额修复在两个实现分别移植，并运行各自测试。
- 发布标签分别使用 `docker-v*`、`workers-v*`。分叉基线为 `6e799b4`，可通过 Git 历史查看。
- Worker CI 只针对 `feat/cloudflare-workers` 分支的 push 和目标为 `feat/cloudflare-workers` 的 PR；未来 Docker 镜像发布需限定 `main` 或 `docker-v*`，避免 Worker 改动触发镜像发布。
- Worker 分支暂留 Go 源码作为行为对照，不参与 Cloudflare 构建。
