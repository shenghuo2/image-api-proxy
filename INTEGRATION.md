# 第三方项目接入指南

接入已有项目，通常只需修改两处：图片 API 基址换成代理地址，官方 Token 换成管理员签发的客户端 key。请求方法、路径和请求体保持官方格式。支持的接口见下表。

部署见 [README](README.md)，完整字段见 [API 参考](API.md)。`BASE` 表示代理根地址，例如 `https://nai.example.com`，不包含 `/console/`。

## 1. 获取客户端 key

由管理员在管理面板为接入项目签发一把客户端 key，开启所需的订阅点数、付费购入点数或 Opus 配额权限，并设置相应限制。建议每个项目使用独立的 key，便于撤销和查看用量。新 key 使用 `pst-` 加 24 位随机字符；管理员可在管理面板再次查看。旧版 key 需轮换后才可查看明文，轮换会使旧 key 立即失效。接入项目仍应将 key 保存在服务端密钥配置中。

新 key 默认在所有已启用的服务账号之间轮询；管理员也可将它固定到一个账号。账号选择是代理内部策略，不改变客户端的官方请求格式。停用账号后，池模式会跳过它；固定在该账号的 key 会暂时不可用。

本地累计上限可设为 `-1`，表示不再另设每把 key 的累计限制，实际使用仍受官方账户余额约束。Opus 的“按次数”模式也是累计上限；“按比例自动回充”模式为每把 key 保留独立余额，并按官方回充份额补入，最多存到所配置的满额比例。接入时官方若只剩 55%，配置 33% 的 key 起始约有满额的 18.15%，之后最多回充到满额的 33%。次数是按估算满额 1730 次换算，实际请求始终受官方余额约束。

接入项目只需知道代理根地址和这把客户端 key。代理在转发请求时将客户端 key 替换为服务端持有的 NovelAI Token；接入项目不需要持有官方 Token。

## 2. 替换图片 API 基址

| 原调用 | 接入代理后 | 说明 |
| --- | --- | --- |
| `https://image.novelai.net/ai/generate-image` | `$BASE/ai/generate-image` | 一次性生成，原始响应直接返回 |
| `https://image.novelai.net/ai/generate-image-stream` | `$BASE/ai/generate-image-stream` | 流式生成，保持流式读取 |
| `https://image.novelai.net/ai/upscale` | `$BASE/ai/upscale` | V5 扩散超分 |
| `https://image.novelai.net/ai/encode-vibe` | `$BASE/ai/encode-vibe` | Vibe 编码 |
| `https://image.novelai.net/ai/augment-image` | `$BASE/ai/augment-image` | 导演增强，按尺寸与类型预留点数 |
| `https://image.novelai.net/user/subscription` | `$BASE/user/subscription` | 返回这把客户端 key 可用的额度视图 |

如果现有客户端已经把 `/image` 放在图片接口路径前，也可以保持该前缀：`$BASE/image/ai/generate-image` 等价于 `$BASE/ai/generate-image`。配置 SDK 时，确认它最终生成的 URL 与表中一行完全匹配；有些 SDK 把主站 API 与图片 API 写在不同的基址中，只应替换图片 API 的基址。

```bash
export BASE='https://nai.example.com'
export CLIENT_KEY='<客户端 key>'
```

请求方法、官方 JSON 或 multipart 请求体、`Content-Type`、`Accept` 和响应解码方式保持原样。认证头改为：

```http
Authorization: Bearer <客户端 key>
```

例如现有项目发送 JSON 请求体，且已有官方格式的 `request.json` 时：

```bash
curl --fail-with-body -sS "$BASE/ai/generate-image" \
  -H "Authorization: Bearer $CLIENT_KEY" \
  -H 'Content-Type: application/json' \
  --data-binary @request.json \
  --output image.zip
```

如果现有项目发送 `multipart/form-data`，也可直接保留：代理从名为 `request` 的 JSON part 读取计费参数，把图片等二进制 part 和原始 Content-Type 原样转发。不要为了接入代理而把图片改为 base64 JSON。以下是 Vibe 编码的 multipart 示例：

```bash
curl --fail-with-body -sS "$BASE/ai/encode-vibe" \
  -H "Authorization: Bearer $CLIENT_KEY" \
  -F 'image=@image.png;type=image/png;filename=blob' \
  -F 'request={"image":"image","model":"nai-diffusion-4-5-full","information_extracted":1};type=application/json;filename=blob' \
  --output vibe.bin
```

流式接口只需把路径改为 `/ai/generate-image-stream`，并逐块消费响应；不要先把整个响应读完再交给原来的流解析器。ZIP、图片、Vibe 数据和流式帧不由代理重新编码。代理可能返回自己的纯文本错误，因此先检查 HTTP 状态码，再按官方格式解析成功响应。

请求体格式没有另起一套协议，但代理会在转发前检查费用和参数。目前生成请求只接受已知的 NAI 3/4/4.5/5 模型、`n_samples` 为 1–4、宽高各至少 64、总像素不超过 4,194,304、步数 1–50；Vibe 参考图最多 16 张，Director 参考图最多 10 张。普通生成、图生图及 Enhance（包括 Max）不再限制单边 2048，1024×3072、3072×1024、4096×768 等长图可正常转发，上游仍校验自身支持的尺寸。Opus 免费面积阈值为 1,048,576 像素，超过后使用点数预算。单次多图默认关闭，管理员须先在配置页开启全局开关，再为各 key 授权；多图全部按点数预留，不使用 Opus 免费配额。超出代理支持的范围会返回 `400` 和具体校验原因；上游自身的 `400` 状态与响应体原样返回。代理不转发查询参数或编码路径。

## 3. 代码中的最小改动

把基址和客户端 key 设为配置项，不要在业务代码中替换请求体字段。下面是 Node.js 服务端示例，假设 `request.json` 已由现有的官方请求构造逻辑生成：

```js
import { createReadStream, createWriteStream } from 'node:fs'
import { pipeline } from 'node:stream/promises'

const base = process.env.NAI_API_BASE_URL.replace(/\/$/, '')
const key = process.env.NAI_CLIENT_KEY

const response = await fetch(`${base}/ai/generate-image`, {
  method: 'POST',
  headers: {
    Authorization: `Bearer ${key}`,
    'Content-Type': 'application/json',
  },
  body: createReadStream('request.json'),
  duplex: 'half',
})

if (!response.ok) {
  throw new Error(`生成失败 (${response.status}): ${await response.text()}`)
}
if (!response.body) throw new Error('响应体为空')
await pipeline(response.body, createWriteStream('image.zip'))
```

如果项目使用 Python `requests`，同样只替换 URL 和 Bearer key；下载流式响应时持续读取，不要设置很短的总超时：

```python
import os
import requests

base = os.environ["NAI_API_BASE_URL"].rstrip("/")
key = os.environ["NAI_CLIENT_KEY"]

with open("request.json", "rb") as source:
    with requests.post(
        f"{base}/ai/generate-image",
        headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"},
        data=source,
        stream=True,
        timeout=(10, 600),
    ) as response:
        response.raise_for_status()
        with open("image.zip", "wb") as output:
            for chunk in response.iter_content(chunk_size=65536):
                if chunk:
                    output.write(chunk)
```

以上示例负责发送请求；`request.json` 仍须符合 NovelAI 对所选模型的要求。`generate-image-stream` 的成功响应应交给项目现有的官方流式帧解析器，不能当 ZIP 保存。

## 4. 读取当前 key 的额度

原项目若调用官方 `/user/subscription`，可继续调用 `$BASE/user/subscription`。响应保留 `active`、`isGracePeriod`、`tier` 和 `trainingStepsLeft` 这些官方风格字段，但余额已经按当前 key 的权限、累计上限和代理预计的官方余额裁剪：

| 字段 | 接入代理后的含义 |
| --- | --- |
| `trainingStepsLeft.fixedTrainingStepsLeft` | 该 key 仍可使用的订阅点数 |
| `trainingStepsLeft.purchasedTrainingSteps` | 该 key 仍可使用的付费购入点数 |
| `usage` | 所选账号或账号池的 Opus 配额与该 key 剩余次数共同限制后的视图；无权限或次数用尽时为 `null` |

代理另提供 `GET $BASE/quota`，同样使用客户端 Bearer key。它**只读取本地账本，不请求官方**，适合接入项目展示用量和剩余权限。主要字段如下：

| 字段 | 含义 |
| --- | --- |
| `name`、`id`、`revoked` | key 备注名、管理 ID、撤销状态 |
| `account_id` | `pool` 为已启用账号轮询，否则为固定账号 ID |
| `allow_fixed_anlas`、`fixed_anlas_limit`、`fixed_anlas_spent`、`fixed_anlas_pending`、`fixed_anlas_remaining` | 订阅点数权限、累计上限、估算已用、待核对和本地剩余 |
| `allow_purchased_anlas` 及对应的 `purchased_anlas_*` | 付费购入点数的同类数据 |
| `allow_opus`、`opus_limit_images`、`opus_limit_mode`、`opus_limit_percent`、`opus_effective_limit_images`、`opus_used_images`、`opus_pending_images`、`opus_remaining_images` | Opus 免费生成权限；`images` 为累计上限，`percent` 为自动回充额度条；已用量始终累计，剩余量随回充变化 |
| `allow_multi_image` | 此 key 的多图授权；还需全局配置开启才生效 |
| `allocated_anlas`、`spent_anlas`、`pending_anlas`、`remaining_anlas` | 两类点数的合计 |
| `queue_length` | 当前排队长度 |

`*_limit` 是**累计上限**，不是每月自动重置的余额；`*_spent` 包括 `*_pending`。已确认计入用量可按 `spent - pending` 计算。订阅点数与付费购入点数的划分是代理的**本地预算**，NovelAI 实际先扣哪类点数由官方决定，不能据此把两类本地统计当作官方账单。

导演增强按 28 步像素成本保守预留点数；即使官方 Opus 条件下可能免费，本地仍记入点数预算。

官方订阅默认缓存 5 分钟，缓存期内代理按已接收请求预计扣减。接入项目无需为每次生成前后反复查询 `/user/subscription`；如只需本地余额，读 `/quota` 即可。

```bash
curl --fail-with-body -sS "$BASE/quota" \
  -H "Authorization: Bearer $CLIENT_KEY"

curl --fail-with-body -sS "$BASE/user/subscription" \
  -H "Authorization: Bearer $CLIENT_KEY"
```

## 5. 排队、超时与重试

所有访问上游的请求在**单个代理实例内按 FIFO 串行执行**，生成流结束后才处理下一项。客户端可以提交并发请求，但等待时间会随队列增长；默认最多容纳 64 个等待请求。请给生成请求留出排队、上游处理和下载响应的时间，且始终完整读取或主动关闭响应体。只部署一个服务实例，因为多个实例之间不共享队列和账本协调。

需要在容器更新时保留排队任务，可选用 `POST /jobs/ai/generate-image` 等持久化任务路径，并为每次生成设置固定的 `Idempotency-Key`。收到 `202` 后保存 `id`；轮询 `GET /jobs/<id>`，状态为 `done` 时下载 `GET /jobs/<id>/result`。提交响应丢失时用相同标识和请求体重试。此模式会先保存请求体和结果，流式路由的结果也只能完成后领取；完整协议见 [API.md](API.md)。普通同步路由保持官方响应方式，更新时断开的连接无法恢复。

| 状态码 | 常见原因 | 接入方处理 |
| --- | --- | --- |
| `400` | 请求描述不是有效 JSON、multipart 缺少 `request` part，或路径/查询参数不受支持 | 修正请求；不要原样重试 |
| `401` | 未提供 Bearer key、key 无效或已撤销 | 检查客户端 key |
| `402` | 当前 key 权限、点数、Opus 次数或预计官方余额不足 | 展示额度不足；由管理员调整策略或等待额度恢复 |
| `404` / `405` | 路径不在支持列表，或方法不匹配 | 对照上面的路由表检查最终 URL |
| `413` | 请求体超过 64 MiB | 减小请求体 |
| `429` | 等待队列已满 | 读取 `Retry-After`（当前为 2 秒）并退避重试 |
| `502` | 官方额度暂不可用，或转发上游失败 | 稍后检查服务状态；不要密集重试 |
| `503` | 固定绑定的账号已停用，或账号池中没有启用账号 | 联系管理员启用或更换账号 |

生成请求转发到官方后，官方的状态码和响应体会直接返回。**不要对已经提交、随后超时或得到 5xx 的生成请求做无条件自动重试**：官方是否已生成以及是否扣费可能无法确认，代理会把预留额保留为“待核对”。明确的上游 4xx 会退回预留额，待核对记录可由管理员在管理面板处理。

## 6. 浏览器项目的接入方式

**客户端图片接口没有面向任意前端来源的 CORS 配置**。浏览器应用应由自己的后端或同源反向代理调用本服务，并把客户端 key 保存在服务端；不要将它放入公开的前端包、页面源码或浏览器持久存储。

现有客户端若依赖官方 Cookie、`/user/login`、未列出的路由、查询参数或任意自定义请求头，需要调整集成方式。代理不提供 `/user/login`、图像标注或标签建议，只转发少数必要请求头，并移除上游 `Set-Cookie`；受支持路径之外的官方功能应继续由原项目自行处理。

## 接入检查

1. `GET /healthz` 返回 `200`，确认代理地址可达。
2. 用客户端 key 请求 `GET /quota` 返回 `200`，确认权限与累计上限符合预期。
3. 用同一 key 请求 `GET /user/subscription`，确认官方额度可用且字段能被现有客户端读取。
4. 用项目现有的官方格式请求体发送一笔小尺寸生成，确认 ZIP 或流式帧按原解析逻辑处理。
5. 在管理面板确认该 key 的估算用量变化，并检查应用对 `402`、`429` 与不确定的超时/`5xx` 有明确处理。
