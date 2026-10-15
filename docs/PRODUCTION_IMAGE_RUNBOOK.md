# 生产图片链路排障手册

本文档是 `gpt2api` 图片链路的权威排障入口。目标不是记录某一次事故，而是固定回答以下问题：

- 生产服务、健康检查和日志在哪里；
- 一次请求如何从 HTTP 日志关联到图片任务、账号、上游会话和下载请求；
- 文生图、图生图、SSE、轮询、结果验证和图片代理分别由哪些代码负责；
- “繁忙”“验证卡住”“任务未返回结果”“下载失败”等提示真实对应什么内部状态；
- 账号为什么进入 `suspicious`、`throttled` 或 `dead`，何时会重新参与调度；
- 代理健康、账号绑定和图片字节直连之间是什么关系；
- 哪些数据库查询可以安全地用于只读诊断。

文档以源码符号为主、行号为辅。行号会随代码变化，符号名才是稳定入口。

## 1. 生产环境快速事实

第二台服务器当前部署约定：

| 项目 | 当前值 |
| --- | --- |
| SSH 别名 | `racknerd-cfa48eb` |
| systemd unit | `gpt2api.service` |
| unit 文件 | `/etc/systemd/system/gpt2api.service` |
| 可执行文件 | `/opt/gpt2api/bin/gpt2api` |
| 配置文件 | `/etc/gpt2api/config.yaml` |
| HTTP 监听 | `*:8080` |
| 健康检查 | `http://127.0.0.1:8080/healthz` |
| 就绪检查 | `http://127.0.0.1:8080/readyz` |
| 主日志 | `journalctl -u gpt2api.service` |
| 数据库 | MySQL，库名 `gpt2api` |

只读快速检查：

```bash
systemctl show gpt2api.service \
  --property=ActiveState,SubState,FragmentPath,ExecStart --no-pager

curl -fsS --max-time 5 http://127.0.0.1:8080/healthz
curl -fsS --max-time 5 http://127.0.0.1:8080/readyz

journalctl -u gpt2api.service --since "30 min ago" --no-pager -o short-iso
```

应用默认把结构化日志写到 `stdout`，生产由 systemd journal 收集。如果 `config.yaml` 的 `log.output` 被改为文件路径，journal 只保留 systemd 输出，应用日志应改查该文件。不要为了排障打印整个配置文件，里面包含数据库连接等敏感信息。

## 2. 先确认请求到底走哪条链路

不要看到“生图”就直接查账号池。`POST /v1/images/generations` 有两种执行所有者：

1. **外置渠道链路**
   - 仅适用于没有 `reference_images` 的纯文生图。
   - `internal/gateway/images.go` 在创建 `image_tasks` 之前调用 `dispatchImageToChannel()`。
   - 如果本地模型存在启用的 image 渠道映射，请求由 `internal/gateway/image_channel.go` 执行。
   - 该路径成功或失败都可能没有 `image_tasks` 行、没有 `task_id`、没有 Runner 阶段日志。
   - 所有已解析出的渠道都失败时，当前实现直接返回 502，不会再落回内置账号池。

2. **内置 ChatGPT 账号池链路**
   - `POST /v1/images/edits` 始终走这条链路。
   - `generations` 带 `reference_images` 时走这条链路。
   - 没有可用外置渠道映射的纯文生图也走这条链路。
   - 由 `internal/image/runner.go` 创建上游客户端并执行 requirements、上传、prepare、SSE、poll 和结果验证。

最先看 gpt2api access log 的真实 `path`。AstrBot 日志里的 `endpoint=edits` 等字段可能只是插件逻辑标签，不能代替服务器实际收到的 `/v1/images/generations` 或 `/v1/images/edits`。

渠道映射只读检查：

```sql
SELECT
    m.local_model,
    m.upstream_model,
    m.modality,
    m.enabled AS mapping_enabled,
    m.priority AS mapping_priority,
    c.id AS channel_id,
    c.name AS channel_name,
    c.type,
    c.base_url,
    c.enabled AS channel_enabled,
    c.status,
    c.last_test_ok,
    c.last_test_error
FROM channel_model_mappings AS m
JOIN upstream_channels AS c ON c.id = m.channel_id
WHERE m.modality = 'image'
  AND c.deleted_at IS NULL
ORDER BY m.local_model, c.priority, m.priority, m.id;
```

不要查询或输出 `api_key_enc`。

## 3. 五分钟故障定位流程

按以下顺序查，不要从账号总数或代理健康分开始猜：

1. 记录故障发生时间、调用路径、HTTP 状态和响应头 `X-Request-Id`。
2. 在 access log 中按 `request_id` 或精确时间定位 `http` 日志，确认真实 `path`、`status`、`cost`、`user_id`、`key_id`。
3. 判断是外置渠道还是内置账号池：
   - 有 `channel image fail, try next`：查渠道链路；
   - 有 `image runner ...` 或数据库存在 `img_...`：查 Runner；
   - 两者都没有：先检查请求是否在鉴权、模型白名单、RPM 或计费阶段提前返回。
4. 内置链路按时间或下游用户/Key 在 `image_tasks` 找到 `task_id` 和 `account_id`。
5. 用 `task_id` 搜完整阶段日志，不要只看最后一条 503 access log。
6. 用 `account_id` 查账号状态、`refresh_error`、冷却时间和代理绑定。
7. 如果 Runner 已有 `image runner result summary` 且 `verified_count >= 1`，gpt2api 已真实拿到图片字节；继续查 `/p/img/...` access log和 AstrBot/NapCat 发送链路。
8. 如果没有 `result summary`，根据最后一个阶段日志定位 requirements、上传、SSE、poll 或下载验证。

常用日志过滤：

```bash
# 精确请求 ID。测试请求应主动传入容易识别的 X-Request-Id。
journalctl -u gpt2api.service --since "30 min ago" --no-pager \
  | grep -F 'your-request-id'

# 图片任务 ID。
journalctl -u gpt2api.service --since "30 min ago" --no-pager \
  | grep -F 'img_xxx'

# 账号 ID。JSON 日志通常形如 "account_id":123。
journalctl -u gpt2api.service --since "30 min ago" --no-pager \
  | grep -F '"account_id":123'

# 只看图片主链关键事件。
journalctl -u gpt2api.service --since "30 min ago" --no-pager \
  | grep -E 'image runner|image proxy|channel image|scheduler image|"path":"/v1/images|"path":"/p/img'
```

## 4. 五套标识不要混用

当前实现存在多套 ID，它们有不同所有者：

| 标识 | 来源 | 出现位置 | 用途 |
| --- | --- | --- | --- |
| HTTP `request_id` | `middleware.RequestID()`；接收或生成 `X-Request-Id` | access log、响应头 | 定位一次 HTTP 请求 |
| usage `request_id` | 图片 Handler 内部 `uuid.NewString()` | `usage_logs.request_id`、计费流水引用 | 计费与 usage 对账 |
| `task_id` | `image.GenerateTaskID()` | `image_tasks.task_id`、Runner 日志、API 响应 | 图片任务主关联键 |
| `conversation_id` | 上游 SSE 返回 | `image_tasks.conversation_id`、SSE/poll 日志 | 上游会话关联 |
| `image_gen_task_id` | 上游 SSE 元数据 | `image runner SSE parsed` 日志 | 判断上游图片任务是否真实启动 |

关键限制：**HTTP `request_id`、usage `request_id` 和 `task_id` 不是同一个值，当前也没有直接持久化映射。**

因此正确关联方式是：

1. 用 HTTP `request_id` 找 access log，得到精确时间、路径、用户和 Key；
2. 用时间窗口加 `user_id` / `key_id` 查 `image_tasks`；
3. 取得 `task_id` 后查 Runner 日志；
4. 用 `task_id` 对应的 `account_id` 查账号和代理；
5. usage 记录只能按相同时间、用户、Key、状态和耗时辅助对账，不能把其 `request_id` 当作 HTTP `X-Request-Id`。

手工测试时建议主动传入不含隐私的 `X-Request-Id`，例如 `manual-image-20260809-1837-a1`。这能稳定定位 access log，但不会自动写入 `image_tasks`。

## 5. 内置图片任务完整阶段

### 5.1 Gateway

入口：

- `ImagesHandler.ImageGenerations()`：`POST /v1/images/generations`
- `ImagesHandler.ImageEdits()`：`POST /v1/images/edits`
- `ImagesHandler.ImageTask()`：`GET /v1/images/tasks/:id`
- `handleChatAsImage()`：图片模型经 `/v1/chat/completions` 转入图片链路

Gateway 依次完成 API Key、请求体、模型权限、RPM、渠道路由、预扣费、`image_tasks` 创建和 Runner 调用。内置同步请求总预算由 `imageRunTimeout()` 限制为 210 秒。

当前 Gateway 为单张请求传入 `MaxAttempts=8`，但这不是无条件跑八个账号：

- 整个 HTTP 图片运行仍受 210 秒总预算限制；
- 每个跨账号 attempt 默认最多 2 分钟，并在这个预算内持有同一个 scheduler lease；
- 如果首轮明确以 `image_task_stalled` 且 `tool_invoked=false` 结束，Runner 会在同一账号、同一代理和同一超时预算内再执行一次完整 turn；
- 已经出现图片任务启动证据的请求不会做同账号复试，避免重复出图；
- 只有 `shouldRetryRunAttempt()` 明确认定为账号级或瞬态错误才换账号；
- 内容安全拒绝、普通助手文本等不会盲目跨账号重试。

### 5.2 Scheduler

`Scheduler.Dispatch()`：

- 从 `Account.DAO.ListDispatchable()` 取最多 30 个候选；
- 按状态、近期成功/失败、错误原因、已知额度和 `last_used_at` 排序；
- 检查 `min_interval_sec`；
- 用 Redis `acct:lock:{id}` 保证一个账号同时只有一个图片任务；
- 所有候选忙或未满足最小间隔时，默认最多排队 120 秒，200ms 起步、最大 2s 的指数退避；
- 图片账号没有真实 `account_proxy_bindings` 时直接跳过，并标记 `throttled/missing_proxy_binding` 24 小时。

因此 `no_available_account` 不只表示“数据库没有账号”，还可能表示：

- 所有可调度账号仍在冷却；
- AT 已过期；
- 候选都被 Redis 锁占用；
- 都未满足最小调用间隔；
- 候选都缺少代理绑定；
- 120 秒排队预算结束前仍未获得 lease。

### 5.3 Chat requirements、POW 和 Turnstile

`Runner.runOnce()` 获得 lease 后，由 `runLeaseAttempt()` 用账号的 AT、device ID、session ID、cookies 和绑定代理创建 `chatgpt.Client`，再执行 `ChatRequirementsV2()`。同账号复试不会重新 Dispatch，也不会在首轮 no-tool 时提前迁移账号状态。

关键日志：

- `image runner chat requirements`
  - `turnstile_required`
  - `pow_required`
  - `persona`
- `image turnstile solved`
  - `has_turnstile_token`
  - `turnstile_token_len`

401 在上游分类为 `auth_required`，账号进入 `dead/auth_required_401`。403 分类为 `upstream_forbidden`，账号进入可恢复的 `suspicious`，不会被当作 401 永久失效。

### 5.4 参考图上传

`UploadFile()` 是固定三步：

1. `POST /backend-api/files`：通过账号控制客户端和绑定代理创建文件；
2. `PUT upload_url`：通过无代理 asset client 直传原始图片字节；
3. `POST /backend-api/files/{file_id}/uploaded`：通过账号控制客户端和绑定代理登记完成。

关键日志：

- `image runner upload reference failed`
- `image runner references uploaded`

错误文本会明确带阶段：`create file`、`upload PUT` 或 `register uploaded`。不要把三者统称为“下载失败”。

### 5.5 Prepare

`PrepareFConversation()` 获取 `conduit_token`：

- 成功日志：`image runner prepare ok`
- 失败日志：`image runner prepare failed`

除明确 429 外，当前 prepare 失败会记录告警后继续尝试 SSE。因此单独看到 `prepare failed` 不能判定最终请求失败，必须继续看后续 `SSE parsed` 和 `result summary`。

### 5.6 SSE

`StreamFConversation()` 发起 `/backend-api/f/conversation`，`ParseImageSSE()` 聚合 conversation、图片任务 ID、tool 状态和图片引用。Runner 默认只等待 45 秒出现图片任务启动信号。

核心日志 `image runner SSE parsed` 应重点看：

- `conv_id`
- `image_gen_task_id`
- `tool_invoked`
- `turn_use_case`
- `pollable`
- `image_task_started`
- `sse_timed_out`
- `sse_fids` / `sse_sids`

解释：

- 已获得足够 refs：记录 `image runner enough refs from SSE, skip polling`，直接进入下载验证；
- 没有 refs，但有真实图片任务启动证据：进入 conversation poll；
- 没有 refs，且 `pollable=false`：记录 `image runner SSE did not start image task`，立即分类，不再空轮询；
- 常见 `image_task_stalled` 细节为 `sse_no_image_task; tool_invoked=false`，表示上游没有真正启动图片工具，不表示服务进程宕机；首轮遇到这一精确终态时会在当前 lease 内复试一次，第二次仍失败才进入账号状态迁移和跨账号逻辑；
- 不要只看日志里的 `tool_invoked=false` 判定失败。若同时已有 `image_task_started=true` 或图片 ref，说明图片任务已经产生，Runner 会继续取图且不会重复执行 turn。

### 5.7 Conversation poll

`PollConversationForImages()` 最长默认等待 2 分钟，日志 `image runner poll done` 包含：

- `poll_status`
- `poll_attempts`
- `poll_fids` / `poll_sids`
- `poll_conversation_errors`
- `poll_429`
- `poll_tool_messages` / `poll_new_tool_messages`
- `poll_async_error_type` / `poll_async_error_text`
- `poll_assistant_reply_text`
- `poll_last_error`

最终分类由 `classifyPollFailureCode()` 决定。只有真实图片任务证据才能压过普通 assistant 文本；没有任务 ID 或图片引用时，上游明确的错误文本应尽早终止。

### 5.8 结果取址和字节验证

拿到 refs 后，Runner 不会只相信 `download_url`，而会：

1. `ImageDownloadURL()` 获取当前有效签名地址；
2. `FetchImage()` 立即下载实际图片字节；
3. 至少一张验证成功后记录 `image runner result summary`；
4. 把字节放入进程内 verified-image cache；
5. 成功后写 `image_tasks`，扣减本地额度快照。

`image runner result summary` 的决定性字段：

- `task_id`
- `account_id`
- `conv_id`
- `proxy_bound`
- `refs`
- `signed_count`
- `verified_count`

`verified_count >= 1` 表示 gpt2api 已经真实下载并验证过图片字节。此后 QQ 未收到图，应查 `/p/img/...` 或 AstrBot/NapCat，而不是继续把问题归因于 SSE。

## 6. 公开提示与内部错误的对应关系

公开响应会隐藏账号、供应商和内部传输细节。排障必须看 `image_tasks.error` 与 Runner 日志。

| 内部错误码 | HTTP | 典型公开提示 | 真实含义与处理 |
| --- | ---: | --- | --- |
| `no_available_account` | 503 | 图片服务繁忙 | 调度窗口内未拿到 lease；不等同于进程宕机 |
| `image_quota_depleted` | 503 | 图片服务繁忙 | 上游明确返回图片额度耗尽；账号进入限流冷却 |
| `auth_required` | 503 | 图片服务授权状态异常 | 上游 401；账号标记 `dead/auth_required_401` |
| `upstream_forbidden` | 503 | 图片服务状态异常 | 上游或代理 403；账号标记 `suspicious` 后重试 |
| `rate_limited` | 503 | 图片服务请求过于频繁 | 429 或明确 reset；账号 `throttled` |
| `network_transient` | 503 | 图片服务网络波动 | EOF、reset、连接失败、TLS timeout 等可恢复网络错误 |
| `turnstile_required` / `pow_*` | 503 | 图片服务验证未通过 | requirements/challenge 未完成 |
| `image_task_stalled` | 503 | 图片任务未返回结果 | SSE 没有真实启动图片任务，或任务启动后没有结果 |
| `turnstile_stalled` | 503 | 图片服务验证卡住 | Turnstile required 且后续没有图片结果 |
| `free_account_stalled` | 503 | 图片任务未返回结果 | free 账号无图片结果且没有更具体终态 |
| `conversation_poll_error` | 503 | 图片任务状态查询异常 | conversation 查询发生错误 |
| `poll_timeout` | 503 | 图片生成失败或内部通用提示 | 已进入 poll，但预算结束仍无结果 |
| `image_tool_without_asset` | 503 | 图片生成未返回可用图片 | tool 消息存在，但没有可下载的 ref |
| `download_failed` | 503 | 图片下载地址生成失败 | 结果取址或字节验证失败 |
| `content_policy_rejected` | 400 | 提示词可能违反内容安全策略 | 内容拒绝，不是技术故障，不应据此冷却账号 |
| `image_assistant_reply` | 400 | 图像助手回复或未启动 | 上游返回文本而非图片任务 |
| `image_async_error` | 400 | 图片生成任务返回错误 | 上游图片异步任务明确失败；部分技术文本可先跨账号重试 |

图片接口公开 JSON 只保留 `error.message` 和 `error.type`，不会返回内部 code。内部 code 在 `image_tasks.error`、`usage_logs.error_code` 和日志中查。

## 7. 账号状态、冷却和重试

调度优先级：

1. `healthy`
2. `warned`
3. 冷却到期的 `suspicious`
4. 带明确 `cooldown_until` 且已到期的 `throttled`

`dead` 永不进入正常调度。`throttled` 如果 `cooldown_until` 为空，也被视为人工或长期停用，不会自动恢复。

主要状态迁移：

| 原因 | 新状态 | 默认暂停 |
| --- | --- | ---: |
| `auth_required_401` | `dead` | 永久，需重新授权或人工处理 |
| `upstream_403`、`proxy_connect_403`、`turnstile_stalled` | `suspicious` | 2 小时 |
| `upstream_network_transient`、`download_failed`、`poll_timeout`、`conversation_poll_error`、`image_task_stalled`、`image_tool_without_asset`、`free_account_stalled`、`pow_*`、`image_async_transient` | `suspicious` | 30 分钟 |
| 其他可恢复 suspicious 原因，如 `prepare_403` | `suspicious` | 使用动态 429 冷却，代码默认 300 秒 |
| 明确 429 或额度 reset | `throttled` | 上游 reset；无明确值时使用动态 429 冷却 |
| `missing_proxy_binding` | `throttled` | 24 小时 |

一个 `suspicious` 账号冷却后可以低优先级复试；成功出图后 `RestoreHealthy()` 只恢复 `suspicious`。成功不能自动证明 `dead`、长期 `throttled` 或 RT 失效已经恢复。

## 8. 代理、账号绑定和流量边界

### 8.1 绑定是图片调度的硬条件

账号到代理的权威关系是：

- `account_proxy_bindings.account_id -> proxy_id`
- `proxies.id -> scheme/host/port/credentials/enabled/health`

图片 Scheduler 在 Runner 之前就会拒绝没有绑定的账号，因此 `GPT2API_FORCE_PROXY_URL` 不能替代正常的账号绑定来参与图片调度。环境变量回退只存在于后续客户端构造和图片代理路径，不应被当作生产绑定策略。

### 8.2 控制请求走代理，资源字节直连

`chatgpt.Client` 同时持有：

- `hc`：带账号控制代理的 uTLS client；
- `assetHC`：当配置了代理时，额外创建的无代理 uTLS client。

实际边界：

| 操作 | 客户端 | 是否走账号代理 |
| --- | --- | --- |
| chat requirements / prepare / SSE / conversation poll | `hc` | 是 |
| 创建参考文件 | `hc` | 是 |
| 上传参考图 Azure PUT | `assetHC` | 否 |
| 登记参考文件 uploaded | `hc` | 是 |
| 获取生成图 download URL | `hc` | 是 |
| 下载生成图真实字节 | `assetHC` | 否 |

外部预签名 CDN URL 下载时不带 Bearer；`chatgpt.com` 回源 URL 会附带账号鉴权头，但网络仍由 `assetHC` 直连。该设计用于避免参考图和生成图的大流量消耗付费代理。

### 8.3 代理 health score 不是 ChatGPT 可用性证明

代理 Prober 默认只要以下任一轻量目标返回 2xx/3xx 就算成功：

- `api.ipify.org`
- Cloudflare trace
- `httpbin.org/ip`

成功 `health_score +10`，失败 `-20`。因此 `health_score=100` 只证明通用 HTTP(S) 代理探测通过，不能证明：

- ChatGPT requirements 一定不会 403；
- 当前账号 AT 有效；
- Azure 文件 PUT 可达；
- 图片 CDN 直连可达；
- device/session/cookies 与账号匹配。

## 9. 图片代理 `/p/img` 与“生成成功但群里没图”

成功响应返回的是自家签名路径：

```text
/p/img/<task_id>/<idx>?exp=<unix_ms>&sig=<hmac>
```

签名默认有效 24 小时。`ImageProxy()` 的执行顺序：

1. 校验签名和过期时间；
2. 从 `image_tasks` 读取 refs、`account_id` 和 `conversation_id`；
3. 优先从进程内 verified-image cache 读取 Runner 已验证的字节；
4. cache miss 时重新解出账号和代理、获取上游 download URL、直连下载字节；
5. 可选执行本地 2K/4K Catmull-Rom 放大并放入进程内 LRU。

服务重启会清空 verified-image cache 和 upscale cache，但不会清空 `image_tasks`。重启后的旧 `/p/img` 首次访问必须重新依赖原账号、代理绑定和上游 ref。如果账号已失效或绑定丢失，可能出现“任务当时成功，后来图片代理 502”。

相关日志：

- `image proxy resolve account`
- `image proxy missing account proxy`
- `image proxy build client`
- `image proxy download_url`
- `image proxy fetch`
- `image proxy upscale`

判断边界：

- 同步请求未出现 `result summary`：生成阶段问题；
- 出现 `result summary verified_count>=1`，但 `/p/img` 502：图片代理/cache miss 问题；
- `/p/img` access log 为 200，AstrBot 仍未发群：gpt2api 之后的机器人发送链路问题。

## 10. 数据库只读查询

以下 SQL 不包含写操作。连接数据库时使用服务器已有的安全凭据入口，不要把 DSN、密码、AT、RT、cookies 或代理密码复制到命令行历史、聊天或工单。

### 10.1 最近图片任务

```sql
SELECT
    id,
    task_id,
    created_at,
    started_at,
    finished_at,
    TIMESTAMPDIFF(
        SECOND,
        created_at,
        COALESCE(finished_at, NOW())
    ) AS elapsed_s,
    status,
    account_id,
    key_id,
    size,
    upscale,
    LEFT(prompt, 120) AS prompt_preview,
    error
FROM image_tasks
WHERE created_at >= NOW() - INTERVAL 30 MINUTE
ORDER BY id DESC
LIMIT 100;
```

分享结果前应删除或打码 `prompt_preview`。

### 10.2 精确任务

```sql
SELECT
    task_id,
    status,
    account_id,
    user_id,
    key_id,
    model_id,
    n,
    size,
    upscale,
    conversation_id,
    JSON_LENGTH(file_ids) AS file_count,
    JSON_LENGTH(result_urls) AS result_count,
    error,
    estimated_credit,
    credit_cost,
    created_at,
    started_at,
    finished_at
FROM image_tasks
WHERE task_id = 'img_xxx';
```

### 10.3 长时间未终结任务

```sql
SELECT
    task_id,
    status,
    account_id,
    TIMESTAMPDIFF(SECOND, created_at, NOW()) AS age_s,
    created_at,
    started_at,
    error
FROM image_tasks
WHERE status IN ('dispatched', 'running')
  AND created_at < NOW() - INTERVAL 4 MINUTE
ORDER BY created_at ASC;
```

该查询只用于识别脏状态。不要仅凭任务年龄直接批量更新或删除。

### 10.4 最近错误分布

```sql
SELECT
    SUBSTRING_INDEX(error, ':', 1) AS error_code,
    COUNT(*) AS failures,
    MIN(created_at) AS first_seen,
    MAX(created_at) AS last_seen
FROM image_tasks
WHERE status = 'failed'
  AND created_at >= NOW() - INTERVAL 24 HOUR
GROUP BY SUBSTRING_INDEX(error, ':', 1)
ORDER BY failures DESC, last_seen DESC;
```

### 10.5 账号池状态

```sql
SELECT
    status,
    COUNT(*) AS accounts,
    SUM(cooldown_until IS NULL) AS no_cooldown,
    SUM(cooldown_until > NOW()) AS cooling,
    SUM(token_expires_at IS NOT NULL AND token_expires_at <= NOW()) AS token_expired
FROM oai_accounts
WHERE deleted_at IS NULL
GROUP BY status
ORDER BY status;
```

### 10.6 调度候选基础集合

```sql
SELECT
    id,
    status,
    cooldown_until,
    token_expires_at,
    last_used_at,
    refresh_error,
    image_quota_remaining,
    image_quota_reset_at
FROM oai_accounts
WHERE deleted_at IS NULL
  AND (
      status IN ('healthy', 'warned', 'suspicious')
      OR (status = 'throttled' AND cooldown_until IS NOT NULL)
  )
  AND (cooldown_until IS NULL OR cooldown_until <= NOW())
  AND (token_expires_at IS NULL OR token_expires_at > NOW())
ORDER BY
    FIELD(status, 'healthy', 'warned', 'suspicious', 'throttled'),
    last_used_at ASC;
```

这只是 SQL 基础候选。真实 Scheduler 还会检查候选上限 30、`min_interval_sec`、Redis 锁和代理绑定。

### 10.7 账号与代理绑定

```sql
SELECT
    a.id AS account_id,
    a.status,
    a.cooldown_until,
    a.refresh_error,
    b.proxy_id,
    p.scheme,
    p.host,
    p.port,
    p.enabled,
    p.health_score,
    p.last_probe_at,
    p.last_error
FROM oai_accounts AS a
LEFT JOIN account_proxy_bindings AS b ON b.account_id = a.id
LEFT JOIN proxies AS p ON p.id = b.proxy_id
WHERE a.deleted_at IS NULL
ORDER BY a.id DESC;
```

不要查询 `password_enc`、`auth_token_enc`、`refresh_token_enc`、`session_token_enc` 或 cookie 密文。

只看缺失或失效绑定：

```sql
SELECT
    a.id AS account_id,
    a.status,
    a.refresh_error,
    b.proxy_id,
    p.enabled,
    p.deleted_at
FROM oai_accounts AS a
LEFT JOIN account_proxy_bindings AS b ON b.account_id = a.id
LEFT JOIN proxies AS p ON p.id = b.proxy_id
WHERE a.deleted_at IS NULL
  AND (
      b.account_id IS NULL
      OR p.id IS NULL
      OR p.enabled = 0
      OR p.deleted_at IS NOT NULL
  )
ORDER BY a.id;
```

### 10.8 每个账号最近图片结果

```sql
SELECT
    a.id AS account_id,
    a.status,
    a.refresh_error,
    COUNT(t.id) AS attempts_24h,
    SUM(t.status = 'success') AS successes_24h,
    SUM(t.status = 'failed') AS failures_24h,
    MAX(CASE WHEN t.status = 'success' THEN t.finished_at END) AS last_success_at,
    MAX(CASE WHEN t.status = 'failed' THEN t.finished_at END) AS last_failure_at
FROM oai_accounts AS a
LEFT JOIN image_tasks AS t
  ON t.account_id = a.id
 AND t.created_at >= NOW() - INTERVAL 24 HOUR
WHERE a.deleted_at IS NULL
GROUP BY a.id, a.status, a.refresh_error
ORDER BY last_success_at DESC, last_failure_at DESC, a.id DESC;
```

### 10.9 Usage 对账

```sql
SELECT
    id,
    request_id,
    user_id,
    key_id,
    model_id,
    account_id,
    image_count,
    credit_cost,
    duration_ms,
    status,
    error_code,
    created_at
FROM usage_logs
WHERE type = 'image'
  AND created_at >= NOW() - INTERVAL 30 MINUTE
ORDER BY id DESC
LIMIT 100;
```

再次强调：这里的 `request_id` 是 usage/计费引用，不是 HTTP `X-Request-Id`。

## 11. AstrBot 与 gpt2api 对照

第二台服务器当前相关容器名：

- `astrbot`
- `napcat`
- `sub2api`
- `sub2api-edge`

只读日志对照：

```bash
docker logs --since 30m --timestamps astrbot 2>&1
docker logs --since 30m --timestamps napcat 2>&1
```

对照原则：

1. 以 gpt2api access log 的 `path/status/cost` 为 HTTP 真相；
2. 以 `image_tasks` 和 Runner 的 `task_id/account_id` 为图片执行真相；
3. 以 `/p/img/...` access log 为图片字节是否已从 gpt2api 发出的真相；
4. 以 AstrBot 日志为下载、消息构建和 QQ 发送结果；
5. 以 NapCat 日志为 OneBot/QQ 连接和实际发送结果。

典型边界：

- AstrBot 没有对应 gpt2api access log：请求未到 gpt2api，查插件路由或网络；
- gpt2api 503 且无 Runner 日志：可能在鉴权、模型、计费、RPM 或外置渠道阶段失败；
- Runner 成功但无 `/p/img` 请求：AstrBot 没有下载返回 URL；
- `/p/img` 200 但无 QQ 图片：AstrBot 到 NapCat 的发送阶段；
- NapCat 已返回成功消息 ID：gpt2api/AstrBot 主链已完成，继续查 QQ 平台展示或撤回逻辑。

## 12. 关键源码索引

优先按符号名搜索，括号内为当前大致行号。

| 文件 / 符号 | 职责 |
| --- | --- |
| `internal/server/router.go` `NewRouter`（约 70） | `/healthz`、`/readyz`、管理 API、`/v1/images/*` 路由装配 |
| `internal/middleware/request_id.go` `RequestID` | `X-Request-Id` 生成/透传 |
| `internal/middleware/logger.go` `AccessLog` | HTTP `method/path/status/cost/request_id/user_id/key_id` 日志 |
| `internal/gateway/images.go` `ImageGenerations`（约 151） | generations 鉴权、模型、计费、渠道分支、任务创建、Runner 调用 |
| `internal/gateway/images.go` `ImageEdits`（约 947） | multipart 图生图/编辑入口 |
| `internal/gateway/images.go` `ImageTask`（约 364） | 图片任务查询 |
| `internal/gateway/images.go` `localizeImageErr`（约 586） | 内部错误到公开中文提示 |
| `internal/gateway/images.go` `publicImageErrorDetail`（约 667） | 过滤敏感/内部错误详情 |
| `internal/gateway/images.go` `imageErrorHTTPStatus`（约 887） | 内部错误到 HTTP 状态 |
| `internal/gateway/image_channel.go` `dispatchImageToChannel`（约 29） | 纯文生图的外置渠道路由与渠道重试 |
| `internal/gateway/images_proxy.go` `ImageProxy`（约 72） | `/p/img` 签名校验、cache、账号解析、回源和本地放大 |
| `internal/image/model.go` | `image_tasks` 模型、状态和稳定错误码 |
| `internal/image/dao.go` | 图片任务创建、running/account、成功/失败持久化 |
| `internal/image/runner.go` `normalizeRunOptions`（约 156） | attempt/SSE/upload/poll 默认预算 |
| `internal/image/runner.go` `collectVerifiedImageDownloads`（约 201） | 结果取址和真实字节验证 |
| `internal/image/runner.go` `classifySSEStartFailure`（约 394） | SSE 未启动图片任务时的 fail-fast 分类 |
| `internal/image/runner.go` `markAccountFailureState`（约 502） | 401 dead 与可恢复错误 suspicious 分流 |
| `internal/image/runner.go` `shouldRetryRunAttempt`（约 536） | 是否允许跨账号重试 |
| `internal/image/runner.go` `Run`（约 668） | 串行/并行尝试、任务终态和本地额度扣减 |
| `internal/image/runner.go` `runOnce`（约 853） | 一次完整的账号图片尝试 |
| `internal/account/dao.go` `ListDispatchable`（约 118） | 可调度账号 SQL、状态优先级和近期结果排序 |
| `internal/account/dao.go` `SetStatusReason`（约 374） | 状态、冷却和机器可读原因持久化 |
| `internal/account/dao.go` `RestoreHealthy`（约 411） | suspicious 成功后的恢复 |
| `internal/account/dao.go` `SetBinding/GetBinding`（约 551） | 账号代理绑定 |
| `internal/scheduler/scheduler.go` `Dispatch`（约 167） | 最多 120 秒排队、退避和 lease 获取 |
| `internal/scheduler/scheduler.go` `tryDispatchOnce`（约 221） | 候选扫描、min interval、Redis 锁、缺代理处理 |
| `internal/scheduler/scheduler.go` `MarkSuspicious`（约 385） | 可恢复失败的原因化冷却 |
| `internal/scheduler/scheduler.go` `MarkDeadWithReason`（约 418） | 永久不可调度状态 |
| `internal/upstream/chatgpt/client.go` `New`（约 60） | uTLS 控制 client 与无代理 asset client |
| `internal/upstream/chatgpt/client.go` `UpstreamError` 分类 | 401、403、429 和上游状态分类 |
| `internal/upstream/chatgpt/files.go` `UploadFile`（约 61） | 参考图 create / PUT / register 三步上传 |
| `internal/upstream/chatgpt/image.go` `PrepareFConversation`（约 97） | 获取 conduit token |
| `internal/upstream/chatgpt/image.go` `StreamFConversation`（约 169） | 图片 SSE 请求 |
| `internal/upstream/chatgpt/image.go` `ParseImageSSE`（约 302） | SSE tool、task、ref 解析 |
| `internal/upstream/chatgpt/image.go` `PollConversationForImages`（约 983） | conversation 轮询和 `PollDebug` |
| `internal/upstream/chatgpt/image.go` `ImageDownloadURL`（约 1237） | file-service / sediment 取址 |
| `internal/upstream/chatgpt/image.go` `FetchImage`（约 1289） | 鉴权规则、直连下载和 16MB 限制 |
| `internal/proxy/model.go` / `service.go` / `dao.go` | 代理记录、解密构建 URL、持久化 |
| `internal/proxy/prober.go` `probe`（约 211） | 通用代理连通性探测和 health score |
| `internal/channel/dao.go` `ResolveByLocalModel`（约 154） | 外置渠道映射解析顺序 |

## 13. 当前私有部署快照

以下是 2026-08-09 最后核验的部署事实，不应替代实时查询：

- `gpt2api.service` 为 `active/running`，监听 8080，`/healthz` 返回 200；
- 账号控制请求使用共享 711Proxy Rotating 记录，最后核验代理 ID 为 `938`；
- 已废弃的 Sticky 记录 `937` 停用；
- 本地注册侧通过仅 WireGuard 可达的 `wg-711proxy-relay.service` 连接同一动态代理供应商；
- 中继监听 `10.66.67.1:19110`，仅供 `10.66.67.0/24` 的 `wg0` 流量使用；
- 中继 unit 不保存代理用户名或密码；
- 参考图 PUT 和生成图真实字节继续直连，不走付费代理流量。

代理 ID、账号数量和绑定会变化。每次事故都应先执行本章 SQL 读取当前状态，不要把上述数字硬编码进脚本或修复逻辑。

## 14. 生产变更边界

日常排障默认只读。除非已经用日志、任务和账号记录确认根因，否则不要：

- 批量恢复 `dead` 或清空 `cooldown_until`；
- 仅因为 403 就刷新或替换 AT/RT；
- 仅因为代理 health score 低就批量换绑；
- 删除 `image_tasks` 来掩盖 running/failed 状态；
- 在生产服务器编译；
- 为了测试发送高成本真实图片请求；
- 输出 `/api/admin/accounts/:id/secrets`、配置 DSN 或任何密文列。

代码发布约定：本地测试和本地交叉编译，上传明确的二进制产物，仅重启 `gpt2api.service`，随后检查 unit、健康入口、监听端口和新日志。不要顺带重启 AstrBot、NapCat、sub2api 或整台服务器。

## 15. 一页式判断表

| 看到的最后证据 | 结论 | 下一步 |
| --- | --- | --- |
| 无 gpt2api access log | 请求未到 gpt2api | 查 AstrBot 路由、URL、网络 |
| access log 401/403，且无 Runner | 下游 API Key/模型权限阶段 | 查 Key 和模型白名单，不查上游账号 |
| `channel image fail, try next` | 外置渠道链路 | 查渠道映射、base URL、上游返回 |
| `no_available_account` | Scheduler 未取得 lease | 查候选状态、冷却、锁、min interval、绑定 |
| `auth_required_401` | 账号 AT 被上游拒绝 | 账号 dead；核对授权链路 |
| `upstream_403` / `prepare_403` | 可恢复的上游/环境 403 | 看代理、指纹、cookies；账号冷却复试 |
| `upload reference failed` | 参考图三步上传失败 | 从错误文本区分 create、PUT、register |
| `prepare failed` 后仍有 SSE | prepare 降级继续 | 不要提前判死，继续看 SSE |
| `SSE did not start image task` | 上游未启动图片工具 | 看 `detail`、`tool_invoked`、assistant 文本 |
| `poll done` timeout/rejected | 已启动后未拿到结果或明确失败 | 看 `PollDebug` 字段和最终分类 |
| `download url failed` | 上游取址失败 | 查账号授权、conversation/ref 类型 |
| `fetch image failed` | 真实图片字节直连失败 | 查 CDN/estuary、直连网络和鉴权头 |
| `result summary verified_count>=1` | gpt2api 生图与下载验证成功 | 查 `/p/img` 和机器人发送 |
| `/p/img` 502 | 图片代理回源失败 | 查 cache miss、账号、绑定、download/fetch 日志 |
| `/p/img` 200，QQ 无图 | gpt2api 已完成 | 查 AstrBot 和 NapCat |

## 16. 文档维护规则

当图片主链发生结构变化时，同一个改动必须更新本手册：

- 新增或删除阶段、错误码、状态迁移或冷却规则；
- 修改默认 timeout、attempt、queue wait 或候选上限；
- 改变控制代理与资源字节的网络边界；
- 改变 HTTP request ID、usage ID 与 task ID 的关联方式；
- 改变生产 unit、路径、端口、健康入口或日志所有者；
- 改变外置渠道与内置账号池的路由优先级。

事故的临时时间线不要不断追加到本文件。稳定因果、最终行为和通用查询写在这里；单次事故证据留在对应任务记录中。
