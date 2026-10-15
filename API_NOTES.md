# ChatGPT 图像生成 & 额度查询接口备忘录

> 本文记录我们目前复用的 `chatgpt.com` 后端接口，及其请求/响应关键字段。  
> 所有接口都走同一个 `Bearer {AUTH_TOKEN}`，host 固定 `https://chatgpt.com`。  
> 运行环境：`curl_cffi` + `impersonate="chrome124"`（chrome131 有时 TLS 握手失败，chrome124 稳定）。

---

## 0. 通用请求头

绝大多数接口共用下面这套头，区别只在于 `referer` / `x-openai-target-*`：

```
authorization: Bearer <AT>
accept: */*
accept-language: zh-CN,zh;q=0.9,en;q=0.8
content-type: application/json
origin: https://chatgpt.com
referer: https://chatgpt.com/                           # 或 /c/{conversation_id}
user-agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36
             (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36
oai-language: zh-CN
oai-device-id: <UUID>                                   # 首次 HEAD / 后从 cookie 取
oai-client-version: prod-be885abbfcfe7b1f511e88b3003d9ee44757fbad
oai-client-build-number: 5955942
```

> `AUTH_TOKEN` 是 Bearer JWT，有效期约 10 天。过期后要用户重新登录页面从 network 里抓。

---

## 1. 额度查询（重点！）

### `POST /backend-api/conversation/init`

用途：**新建会话前注册 + 返回当前账号各类功能剩余额度**。  
这就是网页左下角"今日还剩 XX 张图"数字的来源。

请求体：

```json
{
  "gizmo_id": null,
  "requested_default_model": null,
  "conversation_id": null,
  "timezone_offset_min": -480,
  "system_hints": ["picture_v2"]
}
```

响应体（200）：

```json
{
  "type": "conversation_detail_metadata",
  "banner_info": null,
  "blocked_features": [],
  "model_limits": [],
  "limits_progress": [
    { "feature_name": "deep_research",      "remaining": 25, "reset_after": "…" },
    { "feature_name": "odyssey",            "remaining": 40, "reset_after": "…" },
    { "feature_name": "file_upload",        "remaining": 80, "reset_after": "…" },
    { "feature_name": "paste_text_to_file", "remaining": 80, "reset_after": "…" },
    { "feature_name": "image_gen",          "remaining": 68, "reset_after": "2026-04-17T18:15:51Z" }
  ],
  "default_model_slug": "gpt-5-3",
  "atlas_mode_enabled": null
}
```

关键字段：

| 字段 | 含义 |
|---|---|
| `limits_progress[].feature_name == "image_gen"` | **生图额度**。Plus 每日满额约 100~120 次 |
| `limits_progress[].remaining` | 当前剩余次数（抓包时是 105/112，当前是 68） |
| `limits_progress[].reset_after` | 下次重置时间（UTC，通常每日一次） |
| `blocked_features` | 被风控限制的功能列表，正常为空 `[]` |
| `default_model_slug` | 账号默认模型（普通 Plus = `gpt-5-3`） |

用法：
- 每次 `run_once` 开头单独调一次即可做额度监控，**不消耗额度**（只在新建会话时必调，复用会话时可不调）。
- 独立脚本：`_check_image_gen_quota.py`。

---

## 2. 生图完整调用链

按顺序编号：

```
[1] GET  /                                            → 拿 oai-did cookie
[2] POST /backend-api/sentinel/chat-requirements      → 拿 chat_token (+可选 POW 挑战)
[3] POST /backend-api/conversation/init               → 注册 + 查余额（见 §1）
[4] POST /backend-api/f/conversation/prepare          → 拿 conduit_token（灰度分桶关键）
[5] POST /backend-api/f/conversation    (SSE)         → 正式下发 prompt，流式拿 file_id
[6] GET  /backend-api/conversation/{conv_id}          → 轮询补齐最终 file-service URL
[7] GET  /backend-api/files/{file_id}/download        → 拿短期签名 URL
[8] GET  <signed_url>                                 → 下载图片 bytes
```

> 复用现有会话（`FIXED_CONVERSATION_ID` 有值）时跳过 `[3]`，但 `[4][5]` 仍需每次调。

---

### [2] `POST /backend-api/sentinel/chat-requirements`

作用：拿 `chat_token`（写进 `openai-sentinel-chat-requirements-token`），以及判断是否要做 POW / Turnstile。

请求体：

```json
{ "p": "gAAAAAC...<get_requirements_token 生成>" }
```

响应关键字段：

```json
{
  "token": "...chat_token...",
  "proofofwork": {
    "required": true,
    "seed": "...",
    "difficulty": "0fffff"
  }
}
```

如果 `proofofwork.required=true`，需用本地 SHA3-512 暴力算 `openai-sentinel-proof-token`（见 `gen_image.py` 的 `generate_proof_token`）。

---

### [4] `POST /backend-api/f/conversation/prepare`

作用：**灰度桶分配**。服务器在这里决定本次请求走哪套生图后端（DALL-E 3 preview 或 IMG2 gray-bucket），返回一个 `conduit_token` 代表分桶决策。

请求头额外需要：

```
openai-sentinel-chat-requirements-token: <chat_token>
openai-sentinel-proof-token: <proof_token>     # 若 POW required
```

请求体：

```json
{
  "model": "auto",
  "system_hints": ["picture_v2"],
  "timezone_offset_min": -480,
  "conversation_id": null,              // 或已有会话 id
  "message_id": "<前端生成 UUID>",
  "supports_buffering": true
}
```

响应体：

```json
{ "conduit_token": "ct_...." }
```

`conduit_token` 要在 `[5]` 里通过请求头 `x-conduit-token` 传回去。

---

### [5] `POST /backend-api/f/conversation` (SSE)

作用：正式提交 prompt 并接收流式响应，里面会陆续下发 `image_gen_task_id` / 初始 `file_id`。

请求头额外需要：

```
openai-sentinel-chat-requirements-token: <chat_token>
openai-sentinel-proof-token: <proof_token>
x-conduit-token: <conduit_token>             # 关键！否则不进灰度桶
accept: text/event-stream
```

请求体骨架（精简）：

```json
{
  "action": "next",
  "messages": [{
      "id": "<msg_uuid>",
      "author": { "role": "user" },
      "content": { "content_type": "text", "parts": ["<prompt>"] },
      "metadata": {}
  }],
  "parent_message_id": "<head_or_new_uuid>",
  "model": "auto",
  "conversation_id": null,
  "system_hints": ["picture_v2"],            // ← 必须，开启图像工具
  "force_paragen": false,
  "force_rate_limit": false,
  "timezone_offset_min": -480,
  "reset_rate_limits": false,
  "supports_buffering": true
}
```

SSE 事件里要抓的字段：

| 字段 | 位置 | 作用 |
|---|---|---|
| `conversation_id` | `message.metadata` 或顶层 | 后续轮询用 |
| `image_gen_task_id` | `message.metadata.image_gen_async` | 确认任务已发起 |
| `author.name` | tool 消息 | **判灰度关键**：<br>· `dalle.text2im` / `t2uay3k.sj1i4kz` → DALL-E 3 preview<br>· IMG2 灰度时会是不同名字（需进一步抓包确认） |
| `content.parts[].asset_pointer` | assistant 消息 | `file-service://file-XXX` 或 `sediment://...` |

---

### [6] `GET /backend-api/conversation/{conversation_id}`

作用：SSE 结束后轮询补齐最终 file-service URL（尤其灰度会出第二张高清图）。

响应：完整会话 JSON，结构里 `mapping` 是消息树。

polling 策略（见 `poll_conversation_for_images`）：
- 使用 **baseline diff**：请求前先记录 "现有 tool 消息 id 集合"，轮询时只看新增的。
- `sediment://` 代表中间产物，`file-service://` 代表最终版。
- 如果出现 2 条以上新 tool 消息且最新 `sediment` 连续 4 轮不变 → IMG2 已稳定。
- 单 tool 消息 + 30s 内没出第二条 → `preview_only`（非灰度）。
- 最大等待 900s；连续 3 次 429 直接中止。

---

### [7] `GET /backend-api/files/{file_id}/download`

响应：

```json
{ "status": "success", "download_url": "https://files.oaiusercontent.com/…签名URL…" }
```

---

## 3. 其他已观察到的接口（非必用）

| 接口 | 方法 | 用途 | 响应 |
|---|---|---|---|
| `/backend-api/image-gen/image-paragen-display` | POST | **前端上报**：告诉后端"已展示 N 张图" | 204 空 |
| `/backend-api/conversation/{id}/async-status` | POST `{"status":null}` | 异步任务健康检查 | `{"status":"OK"}` |
| `/backend-api/accounts/check/v4-2023-04-27` | GET | 账号 features/entitlements | 用来查 `gpt_image_1` / `image_gen_better_text` 等灰度 flag |
| `/backend-api/files/library` | POST | 用户图像库列表 | 不用于本流程 |
| `/backend-api/models` | GET | 当前账号可用模型 | 诊断用 |
| `/backend-api/me` | GET | 用户基本信息 | 诊断用 |

---

## 4. 关键排查经验

1. **额度**：看 `/conversation/init` 响应 `limits_progress[image_gen].remaining`。  
   Plus 日配额约 100~120，每日 UTC 18:15 附近重置一次。
2. **灰度桶**：`conduit_token` 每次请求都可能不同，服务端随机分桶。  
   当前账号 `accounts/check` 的 features 里 **缺 `gpt_image_1` / `image_gen_better_text` / `image_gen_v2`**，属于"非白名单账号"，灰度命中率极低；HAR 抓包那次是偶发灰度。
3. **风控**：`blocked_features` 为空且 HTTP 未出现 403，就说明没被封。429 是瞬时限流，退避后即可恢复。
4. **TLS**：`curl_cffi` 用 `impersonate="chrome124"` 稳定；`chrome131` 偶发 `TLS connect error`。
5. **网络**：arxlabs.io 代理不稳时直接关掉 `PROXY_TEMPLATE`，走本机 Mihomo/Clash Verge TUN 直连更靠谱。

---

## 5. 相关脚本索引

| 脚本 | 用途 |
|---|---|
| `gen_image.py` | 主生图流程（含重试/轮询/下载） |
| `_check_image_gen_quota.py` | **仅查 `image_gen` 余额**，不消耗额度 |
| `_dump_acc.py` | 完整 dump `/accounts/check`，用于看 feature flag |
| `_check_quota.py` | 遍历多个诊断接口（me/models/accounts/check） |
| `_scan_har_gen.py` / `_scan_har_quota.py` | 扫 HAR 找接口/关键字段 |
| `_har_gen_endpoints.py` / `_dump_init.py` | Dump HAR 里特定接口的完整请求响应 |

---

_最后更新：2026-04-17_
