# API 使用方法

请求使用 Bearer 认证。示例中的 `BASE` 是代理根地址，本地可用 `http://127.0.0.1:8787`，公网使用 HTTPS。已有项目接入见 [接入指南](INTEGRATION.md)。

## 认证

```http
Authorization: Bearer <key>
```

`/admin/*` 使用 `PROXY_ADMIN_KEY`；其他需要认证的接口使用管理员签发的客户端 key。代理会在转发官方接口时，将客户端 key 替换为选定账号的 NovelAI Token。客户端不需要也不应持有上游 Token。

内置管理页面默认位于 `/console/`，可用 `PUT /admin/settings` 的 `admin_ui_path` 字段修改。路径须以 `/` 开头，无结尾斜杠，长度不超过 128，只能包含英文字母、数字、`-`、`_` 和作为分隔符的 `/`；首段不能占用 `admin`、`ai`、`image`、`user`、`quota`、`healthz` 或 `jobs`。修改立即生效，旧页面地址不再提供静态文件；`/admin/*` 的 API 地址和认证方式不变。

独立管理前端跨域访问时，可设置 `PROXY_ADMIN_ORIGIN` 为前端的完整来源（例如 `https://admin.example.com`，不带路径或尾部斜杠）。只有该来源的 `/admin/*` 浏览器请求会获得 CORS 响应头；不使用 Cookie 凭证。

`GET /healthz` 无需认证，只检查服务进程状态。

## 官方接口

代理根地址对应 `https://image.novelai.net`。客户端替换基址后，保持原有方法、路径和请求体：

| 方法 | 代理路径 | 上游 |
| --- | --- | --- |
| POST | `/ai/generate-image-stream` | `image.novelai.net/ai/generate-image-stream` |
| POST | `/ai/generate-image` | `image.novelai.net/ai/generate-image` |
| POST | `/ai/upscale` | `image.novelai.net/ai/upscale` |
| POST | `/ai/encode-vibe` | `image.novelai.net/ai/encode-vibe` |
| POST | `/ai/augment-image` | `image.novelai.net/ai/augment-image` |
| GET | `/user/subscription` | 查询订阅并返回客户端可用额度 |

同样支持以 `$BASE/image` 为基址调用这些路径。`/ai/upscale` 对应仍可用的 V5 扩散超分；已停用的传统超分 `/api/ai/upscale` 不提供。`/user/login`、图像标注和标签建议不开放，服务端 Token 不会返回给客户端。查询参数、未列出的目标和非预期方法不会转发；生成模型仅接受 NAI 3/4/4.5/5 已知型号。

例如，使用官方格式的生成请求体 `request.json` 调用一次性生成接口：

```bash
curl -H "Authorization: Bearer $CLIENT_KEY" -H 'Content-Type: application/json' \
  --data-binary @request.json "$BASE/ai/generate-image" --output image.zip
```

生成、Vibe 编码、导演增强和扩散超分同时接受 JSON 或 `multipart/form-data` 请求体。multipart 中的 `request` part 必须是 JSON；图片等二进制 part 与原始 Content-Type 会原样转发给官方。代理只读取 `request` part 来估算费用。单次生成支持 `n_samples` 为 1–4；多图必须先在管理配置中全局开启、再逐 key 授权，默认均关闭。多图全部按点数预留，不使用 Opus 免费配额。

响应图片、ZIP、Vibe 向量和流式帧不重新编码。上游状态码与响应体直接返回给客户端；代理自身的鉴权、队列、参数和配额错误由代理返回。

生成尺寸的宽高各至少 64，总像素不超过 4,194,304；单边可超过 2048，例如 1024×3072、3072×1024 或 4096×768。普通文生图、图生图及 Enhance（包括 Max）共用此校验，普通、流式、`/image` 别名、multipart 和持久化任务使用同一规则，上游仍决定其实际支持的尺寸。超过 Opus 免费面积阈值 1,048,576 像素时使用点数预算。代理校验失败返回 `400`，响应体以 `unsupported request parameters:` 开头并包含原因；上游 `400` 原样返回且不重试、不增加成功次数。

管理员可选择开启生成图片归档。普通与流式生成、`/image` 别名及持久化任务的成功结果均可归档；流式请求只提取最终成图帧。归档在后台处理，不改变上述响应字节或流式传输；详见[图片归档](#图片归档)。

## 持久化任务接口

需要在服务更新后保留排队任务的客户端，可把受支持的图片 POST 路由加上 `/jobs` 前缀，发送相同的官方请求体和客户端 Bearer key。例如 `POST /jobs/ai/generate-image`、`POST /jobs/ai/generate-image-stream` 或 `POST /jobs/image/ai/upscale`。提交时建议提供稳定且每次生成唯一的 `Idempotency-Key`（最多 128 字符）；同一 key 用相同标识和请求体重新提交会得到同一任务 ID，请求体或路由不同则返回 409。未提供时服务会随机生成任务 ID，提交响应丢失后无法可靠地找到该任务。

```bash
curl -H "Authorization: Bearer $CLIENT_KEY" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: example-generation-001' \
  --data-binary @request.json "$BASE/jobs/ai/generate-image"
```

提交返回 `202`、`Location: /jobs/<id>` 和 JSON 状态。`GET /jobs/<id>` 返回 `waiting`、`running`、`done`、`interrupted` 或 `canceled`，并在 `done` 时提供 `result_url` 和 `upstream_status`。`GET /jobs/<id>/result` 返回保存的上游响应状态、Content-Type 和原始响应体；尚未完成时返回 409。流式生成任务的结果会在完成后一次性领取，不能实时观看进度。只能用提交任务所属的客户端 key 查询。`DELETE /jobs/<id>` 可取消等待任务，或删除已结束任务及结果；正在执行的任务不能安全取消，返回 409。

任务请求体先持久化，再进入与同步请求共用的单并发 FIFO 队列；额度仍在轮到执行时检查和预留。重启后等待中的任务按原顺序恢复。已开始但因进程退出而中断的任务会标记 `interrupted`，不会自动重试；上游是否生成、扣费可能无法确认，需要检查账号账目和待核对额度。结果及状态在完成 24 小时后于下一次启动或提交时清理。持久化任务在 `/data` 中保存明文提示词、上传图片和生成结果，文件权限为 0600，目录为 0700；请保护数据卷。普通同步路由不保存请求体，连接中断后仍无法恢复或领取结果。

## 管理员接口

下例中 `ADMIN_KEY` 是管理员密钥：

```bash
curl -H "Authorization: Bearer $ADMIN_KEY" "$BASE/admin/quota"
curl -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"alice","allow_fixed_anlas":true,"fixed_anlas_limit":100,"allow_purchased_anlas":false,"purchased_anlas_limit":0,"allow_opus":true,"opus_limit_images":50}' \
  "$BASE/admin/keys"
```

创建响应的顶层 `key` 是新签发的客户端 Bearer key，格式为 `pst-` 加 24 位大小写字母及数字。账本保存 SHA-256 哈希用于认证，并以管理员密钥派生的密钥加密保存明文，供管理员再次查看。更换 `PROXY_ADMIN_KEY` 后，旧密文无法解密，需逐把轮换。管理接口如下：

| 方法与路径 | 用途 |
| --- | --- |
| `GET /admin/quota` | 读取各账号缓存额度与汇总；`account_quotas` 按账号列出，`account_errors` 标记暂不可用账号 |
| `POST /admin/quota/refresh` | 请求刷新各已启用账号的官方快照；每账号距上次成功刷新不足 30 秒时返回缓存 |
| `GET /admin/accounts` | 列出账号名称、启停状态、Token 可解密状态和固定绑定的 key 数量；不返回 Token |
| `POST /admin/accounts` | 添加账号，提交 `{"name":"账号二","token":"<NovelAI Token>","enabled":true}` |
| `PUT /admin/accounts/{id}` | 修改名称、替换 Token 或设置 `enabled`；字段可部分提交 |
| `DELETE /admin/accounts/{id}` | 删除没有绑定 key 的账号；有固定绑定 key 时先处理 key |
| `GET /admin/accounts/{id}/quota` | 查询单账号缓存额度及有限固定分配额 |
| `POST /admin/accounts/{id}/quota/refresh` | 请求刷新单账号额度；受最短 30 秒刷新间隔限制 |
| `GET /admin/keys` | 列出每把 key 的权限、额度与状态；可解密的新 key 含 `key` 字段 |
| `GET /admin/queue` | 读取当前执行请求、等待池顺序、持久化任务 ID、请求来源 key 名称及全局等待容量；只读取本地队列，不请求官方额度 |
| `GET /admin/usage/hours` | 最近至多 8 天的成功生成时间桶，供“用量统计”热力图使用 |
| `GET /admin/settings` | 查看全局多图、归档及管理页面路径设置 |
| `PUT /admin/settings` | 部分更新设置，例如 `{"archive_enabled":true,"archive_retention_days":-1,"archive_max_bytes":21474836480}` 或 `{"admin_ui_path":"/private/console"}` |
| `GET /admin/images` | 按 `key_id`、重复的 `ip` 或 `exclude_ip`、`from`、`to`、`page` 筛选归档；固定每页 24 张 |
| `GET /admin/images/stats` | 返回图片数、占用字节、待处理数、失败次数和最近错误 |
| `GET /admin/images/ips` | 返回 IP 与对应图片数；可用 `q` 搜索，单次最多返回 500 个 |
| `GET /admin/images/overview` | 返回全量汇总、按 key 统计和指定窗口内逐小时生成数 |
| `GET /admin/images/{id}/thumbnail` | 返回管理员认证的 JPEG 缩略图，不超过 100 KiB |
| `GET /admin/images/{id}/original` | 下载原始 PNG |
| `DELETE /admin/images/{id}` | 删除单张图片及其缩略图 |
| `POST /admin/keys` | 设置名称、账号策略与权限，签发 key；未指定的权限默认关闭，账号默认轮询池 |
| `PUT /admin/keys/{id}` | 只修改提交的字段，例如 `{"allow_purchased_anlas":true,"purchased_anlas_limit":20}` |
| `DELETE /admin/keys/{id}` | 撤销 key，释放未用分配额 |
| `POST /admin/keys/{id}/rotate` | 轮换客户端密钥，旧 key 立即失效；保留 ID、权限、额度与用量，响应返回新 key |
| `POST /admin/keys/{id}/reconcile` | `{"charged_anlas":N,"opus_charged_images":M}`，人工结算待核对预留额；可加 `opus_charged_by_account` 指定各账号实际扣费次数，管理页提供逐账号输入 |

`allow_fixed_anlas`、`allow_purchased_anlas` 和 `allow_opus` 是独立开关。`fixed_anlas_limit`、`purchased_anlas_limit` 及 Opus 的 `images` 模式 `opus_limit_images` 是累计上限；`-1` 表示不设本地累计上限，实际请求仍受预计官方余额约束。Opus 的 `percent` 模式使用 `opus_limit_percent`（0–100）配置**独立、可回充的额度条**。`opus_effective_limit_images` 是满额容量，`opus_remaining_images` 是当前可用次数，`opus_used_images` 始终是累计用量。以满额约 1730 次估算，33% 的容量约为 570 次；若账号首次查询时只有 55% 可用，该 key 起始约为满额的 18.15%。每次生成只扣本 key 在选中账号下的余额；官方回充后按份额补入，最多到容量上限。关闭权限会立刻使剩余可用额变为 0，但不清除累计用量。

固定账号 key 优先取得对应账号的配置份额；账号池 key 按各账号扣除固定份额后的余量计算比例，并在账号间保留独立子余额。新配置中同一账号的固定比例合计、账号池比例合计均不得超过 100%。旧账本若已有超额比例，会保留原配置并按比例归一化实际份额；管理接口返回 `opus_share_warning: true`，后续增加该范围总比例的修改会被拒绝。按次数或不限次数的 key 只能使用比例 key 未分配的官方 Opus 余额。若发现代理外消耗，先扣未分配余额，再同比降低相关 key 的余额。只有 Opus 权限、没有 Anlas 权限的 key 可以使用符合免费条件且无付费附加项的生成请求；当官方会用 Opus 免费生成时，没有 Opus 权限的 key 会被拒绝，即使它有 Anlas 预算，因为代理无法要求 NovelAI 改扣点数。

官方 `usage.percent` 是已确认余额，`usage.timeUntilNextPercent` 是下一档 1% 的倒计时。代理至多预测下一档一次，并在下一次官方快照到达后确认或撤销预测；预测额可用于生成。字段缺失或无效时不预测。Tier 3 订阅无效且不在宽限期，或账号不是 Tier 3 时，管理接口及官方格式订阅响应中的可用 Opus 百分比显示为 0；官方可能仍返回非零的原始 `usage.percent`，账本会保留该快照供后续校正。仍使用现有官方额度缓存周期，不为预测另设轮询。管理及 `/quota` 响应增加 `opus_predicted`、`opus_confirmed_at` 和 `opus_pending_by_account`；旧百分比 key 升级后第一次成功查询官方额度才会初始化余额。管理员手动核对旧版遗留的、未记录账号归属的待核对量时，无法自动返还到某个账号的额度条。

`allow_multi_image` 是逐 key 的单次多图权限。必须先打开全局同名配置才能为新 key 开启；旧账本及新 key 默认关闭。关闭全局配置时已有 key 的授权记录保留，但多图请求立即不可用。

`queue_limit` 设置每把 key 同时占用的**等待位置数**，不计正在执行的请求。`-1` 表示不设逐 key 上限（旧 key 和新 key 默认值），`0` 表示该 key 只能在队列空闲时立即执行，正整数最多为 10000。修改上限只影响后续入队，不会踢出已经等待的请求。所有 key 仍受全局 `PROXY_QUEUE_SIZE` 限制。客户端 `/quota` 返回 `queue_limit`、全局 `queue_length` 和该 key 的 `key_queue_length`。

`account_id` 选择上游账号。新 key 默认 `pool`，每次生成从已启用账号依次轮询，跳过额度不足或暂不可用的账号；也可指定 `GET /admin/accounts` 返回的账号 ID 进行固定绑定。停用账号后，池不会再选它，固定绑定的 key 在重新启用前不可生成。旧账本中没有 `account_id` 的 key 继续绑定 `default` 账号；已有用量的 key 不允许更换账号策略。所有账号共用同一条 FIFO 队列，不会跨账号并发转发。账号 Token 在服务端加密保存；更换管理员密钥后需重新录入无法解密的账号 Token。

签发或提高有限分配额时，代理使用各账号缓存的预计余额校验：固定 key 的剩余分配额不超过对应账号余额，池 key 的剩余分配额不超过已启用且可用账号扣除固定分配后的总余额。无限额度不预留固定点数，多把无限 key 可共享官方剩余额；管理统计中的 `unlimited_*_keys` 显示此类 key 数量，`unallocated_*` 只计算有限分配。旧版 `allocation_anlas` 仍可作为订阅点数上限的简写，旧账本也会映射为订阅点数策略。旧哈希 key 仍有效但无法显示明文，可通过轮换转换成新格式。NovelAI 决定实际先扣哪一类 Anlas，代理无法指定官方的扣费来源；这两个开关和上限控制的是**本地预算分类**，不是官方子账户。

## 客户端额度

```bash
curl -H "Authorization: Bearer $CLIENT_KEY" "$BASE/quota"
curl -H "Authorization: Bearer $CLIENT_KEY" "$BASE/user/subscription"
```

`GET /quota` 返回该 key 的账号策略、三组权限、已用量、待核对量、剩余量、成功生成次数、图片张数和当前队列长度，不触发官方请求，也不返回明文 key。比例模式的剩余量是逐账号可用次数的合计；无限累计模式的本地剩余额以 `-1` 表示。`GET /user/subscription` 使用缓存，保留官方风格的 `active`、`isGracePeriod` 和 `tier`；固定 key 使用所属账号的预计余额，池 key 使用可用账号预计余额的合计。`trainingStepsLeft` 按该 key 的本地剩余额裁剪。`usage` 显示账号 Opus 配额与该 key 剩余次数的较小值；池模式的百分比汇总上限为 100%，仅供兼容官方形状的展示，不代表单一账号的实际配额。这里显示的点数分类和 Opus 次数是代理预算，不代表官方账户的原始账目。

管理面板的逐 key 统计由 `GET /admin/keys` 的累计字段计算：已计入用量分别为 `fixed_anlas_spent - fixed_anlas_pending`、`purchased_anlas_spent - purchased_anlas_pending` 和 `opus_used_images - opus_pending_images`。待核对量单独显示。`successful_generations` 为已确认成功的请求次数，`successful_images` 为这些请求的图片张数，均包含消耗 Anlas 与使用 Opus 的生成；一次多图只计 1 次请求。`formula_anlas` 为成功请求按官方公式计算、未扣除免费额度的参考点数。上述累计字段从支持它们的版本开始记录，旧版未记录的成功请求无法准确回填。

`PUT /admin/settings` 支持 `charge_pending_as_spent`（默认 `false`）。设为 `true` 会立即把所有 key 已有待核对预留（包括 Anlas 和 Opus 逐账号预留）提交为本地计费，并使之后无法确认结果的请求自动按预留值计费。预留已经包含在 spent/used 中，提交仅清除 pending，不重复扣减、不退款，也不增加成功生成次数。设置和已有预留在一个 SQLite 事务中提交，并等待当前 FIFO 请求结束；重启后仍会提交遗留预留。改回 `false` 只影响之后的未确认请求，不能恢复已提交的待核对状态。此选项是本地预算估算策略，不能证明上游实际扣费。

## 图片归档

全局归档默认关闭，可在管理面板“配置”页开启，也可通过管理员接口更新。`archive_retention_days` 默认 30，可设为 `-1` 表示不按日期清理，最大 36500；`archive_max_bytes` 默认 20 GiB，可设为 1 MiB 至 1 TiB。总容量计算原图与缩略图，超限时从最旧图片开始淘汰；修改策略后后台会执行清理。关闭全局或某把 key 的归档，只影响之后的生成，已有图片仍按保留策略管理。

```bash
curl -X PUT "$BASE/admin/settings" \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"archive_enabled":true,"archive_retention_days":30,"archive_max_bytes":21474836480}'

curl -X PUT "$BASE/admin/keys/$KEY_ID" \
  -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"archive_enabled":false}'
```

新旧 key 的 `archive_enabled` 默认均为 `true`，但只有全局归档开启时才会保存。每张成功生成的原始 PNG 单独保存，同时生成不超过 100 KiB 的 JPEG 缩略图；多图共用 `group_id`。记录包含 `created_at`（生成完成时间）、`ip`（提交时的调用者 IP）、`key_id`、当时的 `key_name` 和 `bytes`（原图加缩略图大小）。持久化任务取提交时的 IP，不取后台执行地址。流式请求只保存最终成图帧，解析或写盘失败不会改变原始生成响应。IP 转发配置见 [README.md](README.md#反向代理与调用者-ip)。

```bash
curl -H "Authorization: Bearer $ADMIN_KEY" "$BASE/admin/images/stats"

curl --get "$BASE/admin/images" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  --data-urlencode "key_id=$KEY_ID" \
  --data-urlencode 'exclude_ip=198.51.100.7' \
  --data-urlencode 'exclude_ip=203.0.113.24' \
  --data-urlencode 'from=2026-09-01T00:00:00Z' \
  --data-urlencode 'page=1'

curl -H "Authorization: Bearer $ADMIN_KEY" "$BASE/admin/images/ips"
curl --get "$BASE/admin/images/ips" \
  -H "Authorization: Bearer $ADMIN_KEY" --data-urlencode 'q=2001:db8'

curl --get "$BASE/admin/images/overview" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  --data-urlencode 'from=2026-09-18T16:00:00Z' \
  --data-urlencode 'to=2026-09-25T16:00:00Z' \
  --data-urlencode 'offset_minutes=-480'

curl -H "Authorization: Bearer $ADMIN_KEY" \
  "$BASE/admin/images/$IMAGE_ID/thumbnail" --output thumbnail.jpg
curl -H "Authorization: Bearer $ADMIN_KEY" \
  "$BASE/admin/images/$IMAGE_ID/original" --output original.png
curl -X DELETE -H "Authorization: Bearer $ADMIN_KEY" \
  "$BASE/admin/images/$IMAGE_ID"
```

列表还可用 `to` 筛选结束时间，`from` 和 `to` 都使用 RFC 3339 时间。重复传 `ip` 表示包含这些精确 IP，重复传 `exclude_ip` 表示排除这些 IP；两者不能同时使用，单次最多指定 100 个。原有的单个 `ip` 用法保持兼容。列表按完成时间倒序，固定每页 24 张，返回 `items`、`total`、`page` 和 `page_size`；每项的 `group_size` 表示同组图片总数。

`/admin/images/ips` 返回按图片数降序排列的 `items`（每项有 `ip`、`count`）和 `truncated`。不带 `q` 时，IP 总数不超过 500 会全部列出；超过时先返回前 500 个，再用 `q` 子串搜索其余地址。`/admin/images/overview` 要求 `from`、`to` 为 RFC 3339，窗口不超过 8 天，`offset_minutes` 为浏览器当前的 UTC 减本地时间分钟数（例如 UTC+8 为 `-480`）；`to` 不包含在窗口内。它返回全部归档的 `count`、`bytes`、`ip_count`、`key_count`、`group_count` 和按 key 汇总的 `keys`，以及窗口内的 `hours`（本地日期、0–23 点、归档图片数）。统计查询只访问本地 SQLite，不请求官方额度。

`/admin/images/stats` 返回 `count`、`bytes`、`pending`、`failures`、`last_error` 和 `ever_archived`，其中 `pending` 是等待后台归档处理的数量，不是生成队列长度。`ever_archived` 成功归档后永久为 `true`，删除或清理全部图片也不重置。升级时仅能从仍存在的旧图片回填。图片接口必须带管理员 Bearer 认证，不要把管理密钥拼入图片 URL；删除成功返回 204。

`GET /admin/usage/hours` 接受与 `/admin/images/overview` 相同的 `from`、`to`、`offset_minutes` 参数，返回 `hours` 数组；每项有本地 `date`、`hour`、`count`（成功响应的生成图片张数）和 `generations`（请求次数）。从 `v0.1.3` 起，生成响应为非空 `2xx`、额度结算成功，且普通响应带 PNG/ZIP 文件头或流式响应包含完整、有效的最终 PNG 帧时，写入本地 SQLite。普通、流式、`/image` 别名和持久化任务均参与；不依赖归档开关。收到完整最终 PNG 的流式请求，即使客户端随后关闭连接，也会计入成功次数和时间统计；只有预览帧、最终帧不完整或错误时不会计成功。旧请求缺少完成时间，无法回填。时间数据按 15 分钟聚合，只用于管理统计，不调用官方额度接口。

## 排队与结算

所有触达上游的调用按入队顺序逐个执行。等待中的普通同步客户端断开后会立即移出等待池；持久化任务提交后不依赖客户端连接。生成流在整个响应结束后才释放队列。`PROXY_QUEUE_SIZE` 默认容纳 64 个等待请求，达到全局或逐 key 上限时返回 429 和 `Retry-After: 2`，响应体分别为 `queue full` 或 `key queue full`。排队时不预扣额度，轮到请求时再检查权限与剩余额度；前面的请求耗尽额度后，后续请求可能被拒绝。请求体上限 64 MiB；普通同步请求轮到执行时在内存中暂存完整请求体，持久化任务入队前先将请求体写入磁盘。

`GET /admin/queue` 返回 `capacity`、`active` 和按位置排序的 `waiting`。每项只包含 key ID、备注名称、持久化任务 ID（若有）、请求路由与排队/开始时间，不包含 Token、提示词或图片。管理面板的“任务队列”页会在可见时每 5 秒读取一次这个本地快照；该请求不进入生成队列，也不会刷新官方额度。普通同步等待请求在服务进程重启后消失，持久化任务从磁盘恢复。

每个账号的官方订阅独立缓存 5 分钟，可用 `PROXY_QUOTA_TTL` 设为 1 分钟至 1 小时。缓存期内各请求只使用对应账号的本地预计余额；单账号刷新失败后 30 秒内不会反复请求官方，池模式会继续尝试其他可用账号。管理员可查看 `snapshot_age_seconds`，或调用刷新接口；因此 `GET /admin/quota` 和 `GET /user/subscription` 不保证实时反映官方变化。

生成前按像素、步数和附加项保守预留 Anlas；Vibe 编码预留 2 Anlas，超分预留 200 Anlas。Opus 免费尺寸请求按每张 1 次计入 key 上限；V5 的 Opus 配额低于 5% 且尚未耗尽时会拒绝这类请求，以免无法判断官方是否会改扣 Anlas。普通上游 2xx 会按预留值记账；流式请求收到完整有效终图即按成功结算，终图之后断连不会留下待核对。明确的 HTTP 4xx 或流内 4xx 错误帧会退回；5xx、终图前断连、取消及缺少有效终图的流式响应默认保留待核对预留。代理**不再为每个任务查询前后官方余额**，因此本地扣费是保守估算，不会按单次官方实际扣费自动退款；周期刷新只校准各账号的预计余额，无法准确把差额追溯到某把 key。管理员可结合官方账目调整上限，或用 `/reconcile` 处理待核对请求；人工核对后下一次读取官方额度会重新获取快照。

导演增强按客户端采用的 28 步像素公式保守预留点数，背景移除按三倍基础费用加 5 计算。导演工具即使在官方 Opus 条件下可能免费，代理仍按点数预算保守记账；需要精确核账时请参考官方账户记录。

服务不记录明文 Token 或 key；账号 Token 与可查看的客户端 key 以密文保存在 SQLite。普通同步请求不落盘提示词或请求体；若管理员开启归档，成功生成的原始图片与缩略图会单独保存。持久化任务为实现重启恢复，按上文所述将请求和结果写入数据卷。
