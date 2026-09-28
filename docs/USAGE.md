# 部署与使用指南

面向部署者、管理员和 API 使用者。开发、测试与架构见 [开发指南](DEVELOPMENT.md)，免费额度对比见 [README](../README.md#当前使用量与-free-额度对比)。

## 一键部署到 Cloudflare

准备一个 Cloudflare 账号、GitHub/GitLab 账号和有效的 NovelAI 订阅。管理员登录密钥与 NovelAI Token 是两回事：**部署时只填管理员密钥，NovelAI Token 在部署完成后通过管理面板添加。**

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

**`PROXY_ADMIN_KEY` 是密码输入框，输入内容显示为圆点。圆点不代表系统已生成可用密码。** 模板现在留空，不再预填 `replace-…` 示例字符串。如果旧页面仍显示圆点，请全选并替换，再保存你填入的真实密钥；管理面板登录时要使用同一个值。

建议直接用密码管理器生成并保存至少 32 字符的随机密码，再粘贴到这个输入框。若已克隆源码，也可运行 `npm run secret:init` 自动生成 64 位十六进制密钥，打开根目录 `.dev.vars`，复制 `PROXY_ADMIN_KEY=` 右侧的值到表单。

Cloudflare 官方按钮没有文档化的随机 Secret 生成配置，无法由仓库控制该密码框的显示或增加生成按钮。这里采用空字段和明确提示；管理员密钥也用于账号 Token 加密，**不会写入构建日志或运行日志**。本地自动生成命令只输出文件位置，不输出密钥。

### 3. 部署并登录

点击部署，等待构建完成。复制 Cloudflare 给出的 `https://<worker>.<账号子域>.workers.dev` 地址，在后面加 `/console/` 打开管理面板，用第 2 步保存的管理员密钥登录。

部署过程无需绑定 R2、创建 D1 或配置自有域名。若失败，查看 Build 日志中的错误；日志不会提供管理员密钥。密钥遗失后的更换会影响已有账号密文，详见下文“配置、密钥与升级”。

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

## 首次配置与日常使用

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

## API 与 Go 版的差异

非图库业务沿用 [API.md](../API.md) 的认证、账号/密钥、配额字段和生成规则，包括 `/image` 别名、JSON 和 multipart 请求、Opus 按次数/按比例分配、多图双重授权、`/quota`、官方格式 `/user/subscription` 和 `/admin/usage/hours`。此处差异优先于 Go 版说明：

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

`GET /admin/runtime` 使用管理员认证，返回当前实例从启动以来的 `rows_read`、`rows_written`、`database_bytes`、`reserved_bytes` 和容量/结果限制。SQL 指标包含索引维护和删除，并保守计入 alarm 读写（每次按一行估计）；DO 重启会重置内存中的计数，不应将其当成 Cloudflare 账号日用量。账号总量应查看 Cloudflare Dashboard。

修改管理入口路径即时生效，旧入口及其资源返回 404。自定义路径不是认证替代。为确保旧路径立即失效，静态资源请求经过 Worker；这些请求计入 Worker 请求量，不能按“所有静态请求都不计费”估算。


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

`GET /healthz` 可检查部署是否响应，但不代表 NovelAI Token 有效或生成成功。`GET /admin/runtime` 是应用局部快照，账号实际额度与计量以 Cloudflare Dashboard 为准。

默认 Worker 名称为 `novelai-api-proxy`。早期线上验证实例名为 `novelai-api-proxy-free`，验证记录中的地址保留为历史事实；更新该已有实例时应沿用原名称。修改名称会创建另一个 Worker，不会迁移其 DO 数据。
