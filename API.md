# API 使用方法

本代理的客户端请求使用 NovelAI 风格的 Bearer 认证。以下示例用 `BASE` 表示代理根地址，例如 `http://127.0.0.1:8787`；生产环境应使用 HTTPS。其他项目从官方图片接口迁移时，先看 [第三方项目接入指南](INTEGRATION.md)。

## 认证

```http
Authorization: Bearer <key>
```

`/admin/*` 使用 `PROXY_ADMIN_KEY`；其他需要认证的接口使用管理员签发的客户端 key。代理会在转发官方接口时，将客户端 key 替换为选定账号的 NovelAI Token。客户端不需要也不应持有上游 Token。

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
| `GET /admin/settings` | 查看全局功能配置，默认 `allow_multi_image: false` |
| `PUT /admin/settings` | 提交 `{"allow_multi_image":true}`，允许为 key 分配多图权限；关闭后立即阻断所有多图请求 |
| `POST /admin/keys` | 设置名称、账号策略与权限，签发 key；未指定的权限默认关闭，账号默认轮询池 |
| `PUT /admin/keys/{id}` | 只修改提交的字段，例如 `{"allow_purchased_anlas":true,"purchased_anlas_limit":20}` |
| `DELETE /admin/keys/{id}` | 撤销 key，释放未用分配额 |
| `POST /admin/keys/{id}/rotate` | 轮换客户端密钥，旧 key 立即失效；保留 ID、权限、额度与用量，响应返回新 key |
| `POST /admin/keys/{id}/reconcile` | `{"charged_anlas":N,"opus_charged_images":M}`，人工结算待核对预留额；Opus 字段可省略，默认保留原预留次数 |

`allow_fixed_anlas`、`allow_purchased_anlas` 和 `allow_opus` 是独立开关，分别对应订阅点数、付费购入点数和 Opus 配额。对应的 `fixed_anlas_limit`、`purchased_anlas_limit` 和 `opus_limit_images` 是这把 key 的**累计上限**；提高上限即可追加可用额。三种上限均可设为 `-1`，表示不设本地累计上限，实际请求仍受预计官方余额和权限约束。Opus 可将 `opus_limit_mode` 设为 `percent` 并使用 `opus_limit_percent`（0–100），以估算满额 1730 次折算累计上限：10% 为 173 次，向下取整；`images` 模式使用 `opus_limit_images`。`opus_effective_limit_images` 返回当前生效的折算次数。请求消耗选中账号的官方 Opus 配额。关闭权限会立刻使该项剩余可用额变为 0，但不清除已用记录。只有 Opus 权限、没有 Anlas 权限的 key 可以使用符合免费条件且无付费附加项的生成请求；当官方会用 Opus 免费生成时，没有 Opus 权限的 key 会被拒绝，即使它有 Anlas 预算，因为代理无法要求 NovelAI 改扣点数。

`allow_multi_image` 是逐 key 的单次多图权限。必须先打开全局同名配置才能为新 key 开启；旧账本及新 key 默认关闭。关闭全局配置时已有 key 的授权记录保留，但多图请求立即不可用。

`account_id` 选择上游账号。新 key 默认 `pool`，每次生成从已启用账号依次轮询，跳过额度不足或暂不可用的账号；也可指定 `GET /admin/accounts` 返回的账号 ID 进行固定绑定。停用账号后，池不会再选它，固定绑定的 key 在重新启用前不可生成。旧账本中没有 `account_id` 的 key 继续绑定 `default` 账号；已有用量的 key 不允许更换账号策略。所有账号共用同一条 FIFO 队列，不会跨账号并发转发。账号 Token 在服务端加密保存；更换管理员密钥后需重新录入无法解密的账号 Token。

签发或提高有限分配额时，代理使用各账号缓存的预计余额校验：固定 key 的剩余分配额不超过对应账号余额，池 key 的剩余分配额不超过已启用且可用账号扣除固定分配后的总余额。无限额度不预留固定点数，多把无限 key 可共享官方剩余额；管理统计中的 `unlimited_*_keys` 显示此类 key 数量，`unallocated_*` 只计算有限分配。旧版 `allocation_anlas` 仍可作为订阅点数上限的简写，旧账本也会映射为订阅点数策略。旧哈希 key 仍有效但无法显示明文，可通过轮换转换成新格式。NovelAI 决定实际先扣哪一类 Anlas，代理无法指定官方的扣费来源；这两个开关和上限控制的是**本地预算分类**，不是官方子账户。

## 客户端额度

```bash
curl -H "Authorization: Bearer $CLIENT_KEY" "$BASE/quota"
curl -H "Authorization: Bearer $CLIENT_KEY" "$BASE/user/subscription"
```

`GET /quota` 返回该 key 的账号策略、三组权限、累计上限、已用量、待核对量、剩余量和当前队列长度，不触发官方请求，也不返回明文 key。无限本地剩余额以 `-1` 表示。`GET /user/subscription` 使用缓存，保留官方风格的 `active`、`isGracePeriod` 和 `tier`；固定 key 使用所属账号的预计余额，池 key 使用可用账号预计余额的合计。`trainingStepsLeft` 按该 key 的本地剩余额裁剪。`usage` 显示账号 Opus 配额与该 key 剩余次数的较小值；池模式的百分比汇总上限为 100%，仅供兼容官方形状的展示，不代表单一账号的实际配额。这里显示的点数分类和 Opus 次数是代理预算，不代表官方账户的原始账目。

管理面板的逐 key 统计由 `GET /admin/keys` 的累计字段计算：已计入用量分别为 `fixed_anlas_spent - fixed_anlas_pending`、`purchased_anlas_spent - purchased_anlas_pending` 和 `opus_used_images - opus_pending_images`。待核对量单独显示；现有账本不提供按日历史曲线。

## 排队与结算

所有触达上游的调用按入队顺序逐个执行。等待中的客户端断开后会跳过；生成流在整个响应结束后才释放队列。`PROXY_QUEUE_SIZE` 默认容纳 64 个等待请求，满载时返回 429 和 `Retry-After: 2`。请求体上限 64 MiB；轮到请求时，服务在内存中暂存完整请求体并读取 JSON 请求描述用于费用预留，不写磁盘。

每个账号的官方订阅独立缓存 5 分钟，可用 `PROXY_QUOTA_TTL` 设为 1 分钟至 1 小时。缓存期内各请求只使用对应账号的本地预计余额；单账号刷新失败后 30 秒内不会反复请求官方，池模式会继续尝试其他可用账号。管理员可查看 `snapshot_age_seconds`，或调用刷新接口；因此 `GET /admin/quota` 和 `GET /user/subscription` 不保证实时反映官方变化。

生成前按像素、步数和附加项保守预留 Anlas；Vibe 编码预留 2 Anlas，超分预留 200 Anlas。Opus 免费尺寸请求按每张 1 次计入 key 上限；V5 的 Opus 配额低于 5% 且尚未耗尽时会拒绝这类请求，以免无法判断官方是否会改扣 Anlas。上游 2xx 会按预留值记账，明确的 4xx 会退回，5xx、连接中断和取消会保留为待核对额。代理**不再为每个任务查询前后官方余额**，因此本地扣费是保守估算，不会按单次官方实际扣费自动退款；周期刷新只校准各账号的预计余额，无法准确把差额追溯到某把 key。管理员可结合官方账目调整上限，或用 `/reconcile` 处理待核对请求；人工核对后下一次读取官方额度会重新获取快照。

导演增强按客户端采用的 28 步像素公式保守预留点数，背景移除按三倍基础费用加 5 计算。导演工具即使在官方 Opus 条件下可能免费，代理仍按点数预算保守记账；需要精确核账时请参考官方账户记录。

服务不记录明文 Token、key、提示词、图片或请求体；账号 Token 与可查看的客户端 key 以密文保存，进程在转发时仍能看到明文。
