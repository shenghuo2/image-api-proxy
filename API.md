# API 使用方法

本代理的客户端请求使用 NovelAI 风格的 Bearer 认证。以下示例用 `BASE` 表示代理根地址，例如 `http://127.0.0.1:8787`；生产环境应使用 HTTPS。

## 认证

```http
Authorization: Bearer <key>
```

`/admin/*` 使用 `PROXY_ADMIN_KEY`；其他需要认证的接口使用管理员签发的客户端 key。代理会在转发官方接口时，将客户端 key 替换为服务端的 NovelAI Token。客户端不需要也不应持有服务端 Token。

`GET /healthz` 无需认证，只检查服务进程状态。

## 官方接口

代理根地址对应 `https://image.novelai.net`。客户端替换基址后，保持原有方法、路径和请求体：

| 方法 | 代理路径 | 上游 |
| --- | --- | --- |
| POST | `/ai/generate-image-stream` | `image.novelai.net/ai/generate-image-stream` |
| POST | `/ai/generate-image` | `image.novelai.net/ai/generate-image` |
| POST | `/ai/upscale` | `image.novelai.net/ai/upscale` |
| POST | `/ai/encode-vibe` | `image.novelai.net/ai/encode-vibe` |
| GET | `/user/subscription` | 查询订阅并返回客户端可用额度 |

同样支持以 `$BASE/image` 为基址调用这些路径。`/ai/upscale` 对应仍可用的 V5 扩散超分；已停用的传统超分 `/api/ai/upscale` 不提供。`/user/login` 不开放，服务端 Token 不会返回给客户端。查询参数、未列出的目标和非预期方法不会转发；生成模型仅接受 NAI 3/4/4.5/5 已知型号。

例如，使用官方格式的生成请求体 `request.json` 调用一次性生成接口：

```bash
curl -H "Authorization: Bearer $CLIENT_KEY" -H 'Content-Type: application/json' \
  --data-binary @request.json "$BASE/ai/generate-image" --output image.zip
```

响应图片、ZIP、Vibe 向量和流式帧不重新编码。上游状态码与响应体直接返回给客户端；代理自身的鉴权、队列、参数和配额错误由代理返回。

## 管理员接口

下例中 `ADMIN_KEY` 是管理员密钥：

```bash
curl -H "Authorization: Bearer $ADMIN_KEY" "$BASE/admin/quota"
curl -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"name":"alice","allow_fixed_anlas":true,"fixed_anlas_limit":100,"allow_purchased_anlas":false,"purchased_anlas_limit":0,"allow_opus":true,"opus_limit_images":50}' \
  "$BASE/admin/keys"
```

创建响应的顶层 `key` 是新签发的客户端 Bearer key，只返回一次；账本只保存其 SHA-256 哈希。管理接口如下：

| 方法与路径 | 用途 |
| --- | --- |
| `GET /admin/quota` | 读取缓存的官方快照、预计余额、固定/已购分配额、Opus 电池和队列长度 |
| `POST /admin/quota/refresh` | 请求刷新官方快照；距上次成功刷新不足 30 秒时返回缓存 |
| `GET /admin/keys` | 列出每把 key 的权限、各额度上限/已用/待核对/剩余和撤销状态 |
| `POST /admin/keys` | 设置名称与权限，签发 key；未指定的权限默认关闭 |
| `PUT /admin/keys/{id}` | 只修改提交的字段，例如 `{"allow_purchased_anlas":true,"purchased_anlas_limit":20}` |
| `DELETE /admin/keys/{id}` | 撤销 key，释放未用分配额 |
| `POST /admin/keys/{id}/reconcile` | `{"charged_anlas":N,"opus_charged_images":M}`，人工结算待核对预留额；Opus 字段可省略，默认保留原预留次数 |

`allow_fixed_anlas`、`allow_purchased_anlas` 和 `allow_opus` 是独立开关。对应的 `fixed_anlas_limit`、`purchased_anlas_limit` 和 `opus_limit_images` 是这把 key 的**累计上限**；提高上限即可追加可用额。Opus 单位是可使用的免费生成次数，所有 key 的请求仍消耗同一个官方 Opus 电池。关闭权限会立刻使该项剩余可用额变为 0，但不清除已用记录。只有 Opus 权限、没有 Anlas 权限的 key 可以使用符合免费条件且无付费附加项的生成请求；当官方会用 Opus 免费生成时，没有 Opus 权限的 key 会被拒绝，即使它有 Anlas 预算，因为代理无法要求 NovelAI 改扣点数。

签发或提高分配额时，代理使用缓存的官方固定/已购余额加上本地预计扣减分别校验；各类有效 key 的剩余分配额不能超过对应的预计余额。旧版 `allocation_anlas` 仍可作为固定 Anlas 上限的简写，旧账本也会映射为固定 Anlas 策略。NovelAI 决定实际先扣哪一类 Anlas，代理无法指定官方的扣费来源；这两个开关和上限控制的是**本地预算分类**，不是官方子账户。

## 客户端额度

```bash
curl -H "Authorization: Bearer $CLIENT_KEY" "$BASE/quota"
curl -H "Authorization: Bearer $CLIENT_KEY" "$BASE/user/subscription"
```

`GET /quota` 返回该 key 的三组权限、累计上限、已用量、待核对量、剩余量和当前队列长度，不触发官方请求。`GET /user/subscription` 使用缓存，保留官方的 `active`、`isGracePeriod` 和 `tier`；`trainingStepsLeft` 的 fixed/purchased 字段分别显示该 key 的本地剩余额（受预计官方余额约束）。`usage` 显示共享 Opus 电池与该 key 剩余次数的较小值；无 Opus 权限或次数用尽时为 `null`。这里显示的点数分类和 Opus 次数是代理预算，不代表官方账户的原始账目。

## 排队与结算

所有触达上游的调用按入队顺序逐个执行。等待中的客户端断开后会跳过；生成流在整个响应结束后才释放队列。`PROXY_QUEUE_SIZE` 默认容纳 64 个等待请求，满载时返回 429 和 `Retry-After: 2`。请求体上限 64 MiB；轮到请求时，服务在内存中暂存 JSON 用于费用预留，不写磁盘。

官方订阅默认缓存 5 分钟，可用 `PROXY_QUOTA_TTL` 设为 1 分钟至 1 小时。缓存期内各请求只使用本地预计余额；刷新失败后 30 秒内不会反复请求官方，过期时新任务会暂时失败，避免使用不可靠的额度。管理员可查看 `snapshot_age_seconds`，或调用刷新接口；因此 `GET /admin/quota` 和 `GET /user/subscription` 不保证实时反映官方变化。

生成前按像素、步数和附加项保守预留 Anlas；Vibe 编码预留 2 Anlas，超分预留 200 Anlas。Opus 免费尺寸请求按每张 1 次计入 key 上限；V5 电池低于 5% 且尚未耗尽时会拒绝这类请求，以免无法判断官方是否会改扣 Anlas。上游 2xx 会按预留值记账，明确的 4xx 会退回，5xx、连接中断和取消会保留为待核对额。代理**不再为每个任务查询前后官方余额**，因此本地扣费是保守估算，不会按单次官方实际扣费自动退款；周期刷新只校准共享账户的预计余额，无法准确把差额追溯到某把 key。管理员可结合官方账目调整上限，或用 `/reconcile` 处理待核对请求；人工核对后下一次读取官方额度会重新获取快照。

服务不记录 Token、key、提示词、图片或请求体；进程在转发时仍能看到这些内容。
