# Cloudflare 部署与使用

无需服务器，使用 Cloudflare Workers、SQLite Durable Object 和静态管理页面。开发说明见 [开发指南](DEVELOPMENT.md)。

## 一键部署到 Cloudflare

需要 Cloudflare 账号、GitHub/GitLab 账号和 NovelAI 订阅。部署时填写管理员密钥；NovelAI Token 在部署完成后添加。

### 1. 创建自己的实例

[![Deploy to Cloudflare](https://deploy.workers.cloudflare.com/button)](https://deploy.workers.cloudflare.com/?url=https%3A%2F%2Fgithub.com%2Fshenghuo2%2Fimage-api-proxy%2Ftree%2Ffeat%2Fcloudflare-workers)

点击按钮后登录并授权复制仓库。源码固定来自 `feat/cloudflare-workers` 分支的完整工程，无需自己切换分支，也无需创建数据库。

### 2. 填写部署表单

| 页面项目 | 如何填写 |
| --- | --- |
| 仓库名 / Worker 名称 | 默认即可，也可以改成自己喜欢的名称 |
| 构建命令 | 保持 `npm run build` |
| 部署命令 | 保持 `npm run deploy` |
| `PROXY_ADMIN_KEY` | **必填：粘贴你自己生成并保存的随机管理员密钥，至少 32 字符** |
| 其他变量和绑定 | 保持默认，SQLite DO 和静态资源自动创建 |

`PROXY_ADMIN_KEY` 默认留空，填写后显示为圆点。请保存自己填入的密钥，稍后用它登录管理面板；如果页面已有内容，先清空再填写。

建议直接用密码管理器生成并保存至少 32 字符的随机密码，再粘贴到这个输入框。若已克隆源码，也可运行 `npm run secret:init` 自动生成 64 位十六进制密钥，打开根目录 `.dev.vars`，复制 `PROXY_ADMIN_KEY=` 右侧的值到表单。

部署表单需要手动填写密钥。密钥不会出现在部署日志中，请自行保存。

### 3. 部署并登录

点击部署，等待构建完成。复制 Cloudflare 给出的 `https://<worker>.<账号子域>.workers.dev` 地址，在后面加 `/console/` 打开管理面板，用第 2 步保存的管理员密钥登录。

无需 R2、D1 或自有域名。部署失败时查看 Build 日志；密钥管理见下文。

## 命令行部署（自动生成密钥）

需要 Node.js 22.12+，推荐 Node.js 24。从源码部署：

```bash
git clone --branch feat/cloudflare-workers https://github.com/shenghuo2/image-api-proxy.git
cd image-api-proxy
npm ci
npm run secret:init
npm run build
npx --prefix worker wrangler login
npm run deploy -- --secrets-file .dev.vars
```

`secret:init` 自动生成随机密钥并写入根目录 `.dev.vars`，文件权限为 `0600`。**打开该文件查看登录密钥并保存到密码管理器**。文件已被 Git 忽略；存在时命令拒绝覆盖，避免更新时更换密钥。不要提交或分享该文件。

更新同一个实例只需 `npm run build` 和 `npm run deploy`，无需再次生成或上传密钥。已有部署仍应保持相同 Worker 名称和 DO namespace。

## Free 计划额度

按每天 1000 次生成、每次结果约 2 MiB 的模拟负载，请求、SQL 读写和存储均在免费额度内。

| 项目 | 1000 次生成的测算用量 | Free 额度 | 占比 |
| --- | --- | --- | --- |
| Worker 请求 | 2,803 次/天 | 100,000 次/天 | 2.80% |
| DO 请求（含 alarm） | 3,703 次/天 | 100,000 次/天 | 3.70% |
| DO 时长（按全天活跃计算） | 11,059.2 GB-s/天 | 13,000 GB-s/天 | 85.07% |
| SQLite 读取 | 479,287 行/天 | 5,000,000 行/天 | 9.59% |
| SQLite 写入 | 35,834 行/天 | 100,000 行/天 | 35.83% |
| SQLite 存储 | 抽样峰值 37.23 MiB | 账号合计 5 GB | 约 0.78% |
| Worker CPU | 待线上验证 | 10 ms/请求 | — |
| 单 isolate 内存 | 待线上验证 | 128 MB | — |

免费额度由同账号下的项目共享。额外轮询、页面访问和重复下载会增加用量；队列页连续打开一天，每 10 秒刷新约增加 8,640 次请求。DO 时长的余量最小，约为 15%。

单并发要完成 1000 次/天，平均每次生成及传输需在 86.4 秒内完成。实际速度取决于上游。测试方法见 [测试记录](../worker/VALIDATION.md)，额度以 [Workers 限制](https://developers.cloudflare.com/workers/platform/limits/)和 [DO 定价](https://developers.cloudflare.com/durable-objects/platform/pricing/)为准。

## 开始使用

1. 在“账号”页面添加 NovelAI Token，并启用账号。多个启用账号可组成轮询池。
2. 在“密钥”页面为每位调用者或每个项目创建独立客户端 key，配置账号池/固定账号、订阅 Anlas、购入 Anlas 和 Opus 权限及额度。多图需要全局和 key 双重授权。
3. 调用方将图片 API 基址改成 Worker 根地址（不含 `/console/`），使用客户端 key 作为 `Authorization: Bearer <key>`。不要把管理员密钥或 NovelAI Token 交给调用方。
4. 同步或流式生成沿用官方请求格式；长任务可使用持久化任务接口，按返回提示轮询并在完成后 30 分钟内下载。
5. 管理面板查看队列、用量及配额。执行中断或扣费不明确时先核对上游及本地预留，确认后人工结算，避免直接重试导致重复扣费。

持久化任务流程：`POST /jobs/ai/generate-image`（带客户端 key、请求 JSON 和可选 `Idempotency-Key`）→ `GET /jobs/<id>` → `GET /jobs/<id>/result`。详细字段与请求示例见 [API 参考](../API.md)，已有项目接入见 [接入指南](../INTEGRATION.md)。Worker 特有约束以下文为准。

## 配置、密钥与升级

| 配置 | 默认值 | 用途 |
| --- | --- | --- |
| `PROXY_ADMIN_KEY` | 无，必须设置 Secret | 至少 32 字符的管理员 Bearer 密钥，也是 AES-GCM 密钥派生材料 |
| `PROXY_QUEUE_SIZE` | `64` | 全局等待位置数，不含执行中任务 |
| `PROXY_QUOTA_TTL_SECONDS` | `300` | 每账号官方额度缓存时间 |
| `PROXY_ADMIN_ORIGIN` | 空 | 独立前端的完整允许来源；同域部署留空 |

不要提交 `.dev.vars`，也不要把上游 Token 写入 `wrangler.jsonc`。上游地址固定为 `https://image.novelai.net`，路由采用白名单并禁止跟随重定向。替换管理员密钥会导致原 Token 和可显示 key 的密文无法解密；需重新录入账号 Token、轮换客户端 key。账号 Token 不返回给管理 API。

全新部署不导入 Go 数据目录。升级沿用 namespace 和固定 DO 名称；不要删除 DO namespace 来更新代码。队列、预留和任务状态在发送上游前落盘。部署/运行时重启后，等待任务恢复，执行中任务成为 `interrupted`，不自动重试可能已经扣费的生成；管理员使用原有 reconcile 接口核对余额。

使用一键部署创建的 Git 仓库更新源码后，由 Workers Builds 使用根目录命令重新构建部署；命令行用户运行 `npm run build` 和 `npm run deploy`。保持原 Worker 名称、DO namespace 和 `global-v1` 不变。升级不需要重新创建 Secret。

管理员密钥应备份到密码管理器。浏览器只在当前标签页的 sessionStorage 保存登录密钥，退出登录时清除。变更管理路径后记下新地址；路径本身不代替鉴权。

## 接口限制

认证、生成和配额字段见 [API 参考](../API.md)。Worker 版有以下限制：

- `/admin/images*` 返回 `404`，归档相关设置与 key 字段不再支持；`PUT /admin/settings` 只接受 `allow_multi_image`、`admin_ui_path`。
- 请求体上限为 **16 MiB**。同步/流式响应直接转发，不缓存、不重新编码；流式成功计数仅检查完整最终 PNG 帧。
- `/jobs` 请求体持久化后才返回 `202`。任务共享同步请求的单并发 FIFO；用 DO alarm 执行，不依赖普通 Worker 的响应后存活时间。
- 任务结果最多 **32 MiB**，完成后保留 **30 分钟**。请求体在任务完成或中断后删除。状态新增 `expires_at`、`result_expired`、`error`、`poll_after_seconds`。
- 查询示例：`POST /jobs/ai/generate-image` → `GET /jobs/<id>` → `GET /jobs/<id>/result`。结果过期返回 `410`；未完成/中断/取消返回 `409`。流式任务保存原始流式字节，完成后一次领取。
- 同一 key 下的 `Idempotency-Key` 最多 128 字符；相同标识与路由、Content-Type、请求字节返回同一 ID，不同内容返回 `409`。结果过期或手动删除后仍保留状态和幂等摘要 24 小时，期间不重新生成。24 小时后可重新使用该标识。
- `DELETE /jobs/<id>` 取消等待任务或删除结束任务的结果，保留去重记录；执行中返回 `409`。
- 容量控制统计请求体、已保存结果和等待/执行任务的结果预留，总上限 **512 MiB**。上传阶段最多暂存一个任务，超额返回 `429` 和 `Retry-After: 15`。队列可能在未达到 64 项前因容量保护而拒绝任务。未到期结果不会被提前淘汰。
- 接收结果超过 32 MiB 或写入失败时，标记 `interrupted`、删除部分结果，保留未核对的额度，不自动重试。明确上游 4xx 响应退款，5xx/连接中断保留预留。
- 结果从到期时刻起不可访问；物理分块由 alarm/后续请求批量清理。清理若与生成重叠，会等待当前执行完成，不保证数据库中的字节在第 30 分钟瞬间删除。
- 管理队列页可见时每 10 秒刷新；客户端按响应的 `Retry-After` / `poll_after_seconds` 查询任务，等待较久时从 5 秒退避到 15 秒。

`GET /admin/runtime` 可查看数据库大小、容量预留和当前实例读写次数。计数在 DO 重启后重置；账号每日用量请看 Cloudflare Dashboard。

修改管理路径后旧入口立即返回 404，新入口仍需密钥登录。页面和静态资源都经过 Worker，会计入请求量。

## 常见问题

| 现象 | 处理方式 |
| --- | --- |
| 管理接口返回 401 | 检查管理员密钥；生成接口应使用客户端 key |
| 任务提交返回 429 | 队列或临时容量已满，遵循 Retry-After，避免立即循环重试 |
| 结果返回 410 | 已过 30 分钟领取期限，无法恢复；幂等标识在 24 小时内仍不会重新生成 |
| 任务 interrupted / 待核对额度 | 查看错误并核对扣费，人工结算；服务不会自动重发 |
| 修改管理路径后旧页面 404 | 使用新路径登录，API 基址保持不变 |
| 一键部署找不到前端或静态目录 | 确认复制完整仓库，构建命令为 npm run build |
| Free 超额或 CPU/内存错误 | 查看 Cloudflare Dashboard 的 Workers/DO 统计；减少轮询和负载，并检查同账号其他项目 |

`GET /healthz` 用于检查服务是否在线。添加 NovelAI 账号后，另行测试生成接口。

默认 Worker 名称为 `novelai-api-proxy`。更新已有实例时沿用原名称；改名会创建新 Worker，原数据不会自动迁移。
