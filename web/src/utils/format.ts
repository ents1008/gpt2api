/**
 * 后端 credit 单位是"厘"(1 积分 = 10000 厘,方便做 token 等浮点成本)。
 * 展示时统一转成 2 位小数的"积分"。
 */
export const CREDITS_PER_UNIT = 10_000

export function formatCredit(v: number | null | undefined): string {
  if (v == null) return '0'
  const n = Number(v) / CREDITS_PER_UNIT
  if (!Number.isFinite(n)) return '0'
  return n.toFixed(2)
}

/** 单位:bytes → "xx.x MB" */
export function formatBytes(b: number | null | undefined): string {
  if (!b) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = b
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

export function formatDateTime(v: string | { Time: string; Valid: boolean } | null | undefined): string {
  if (!v) return '-'
  const s = typeof v === 'string' ? v : v.Valid ? v.Time : ''
  if (!s || s.startsWith('0001-')) return '-'
  // 后端通常返回 RFC3339,裁剪成 YYYY-MM-DD HH:mm:ss
  const d = new Date(s)
  if (isNaN(d.getTime())) return s
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

/**
 * 紧凑时间格式:MM-DD HH:mm (不显示年份和秒,适合列表类展示)。
 * 跨年的时间仍然按月-日展示,这个在后台审计 / 账号池等场景够用。
 */
export function formatDateShort(v: string | { Time: string; Valid: boolean } | null | undefined): string {
  if (!v) return '-'
  const s = typeof v === 'string' ? v : v.Valid ? v.Time : ''
  if (!s || s.startsWith('0001-')) return '-'
  const d = new Date(s)
  if (isNaN(d.getTime())) return s
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/** 后端 NullString/NullTime 的 Peel。兼容已经被序列化成裸字符串的情况。 */
export function nullVal(
  v: string | { Valid: boolean; String?: string; Time?: string } | null | undefined,
): string {
  if (v == null) return ''
  if (typeof v === 'string') return v
  if (!v.Valid) return ''
  return v.String ?? v.Time ?? ''
}

/**
 * 把网关 / 计费 / 图片服务返回的英文 error_code 翻译成中文提示。
 *
 * 英文 code 是稳定的机器标识(写进 DB / 返回给 OpenAI 客户端,不会改动),
 * 前端在展示时做一次映射,人类可读。
 */
const ERROR_CODE_LABEL: Record<string, string> = {
  // 计费/余额
  insufficient_balance: '积分不足',
  billing_error: '计费异常',

  // 限流
  rate_limit_rpm: '触发每分钟请求数限制 (RPM)',
  rate_limit_tpm: '触发每分钟 Token 限制 (TPM)',
  rate_limited: '请求过于频繁,稍后再试',
  rpm_limit: '触发每分钟请求数限制 (RPM)',
  tpm_limit: '触发每分钟 Token 限制 (TPM)',

  // 模型权限/类型
  model_not_allowed: '当前 Key 无权调用该模型',
  model_not_found: '模型不存在或已下架',
  model_disabled: '该模型已被禁用',
  model_type_mismatch: '模型类型不匹配(如对话模型用于生图)',

  // 请求校验
  invalid_request_error: '请求参数有误',
  invalid_request: '请求参数有误',
  unauthorized: '未授权或 API Key 无效',
  forbidden: '无权访问',
  key_disabled: 'API Key 已被停用',
  key_expired: 'API Key 已过期',
  quota_exceeded: 'API Key 配额已用完',

  // 功能未开启
  image_not_wired: '图片能力未接入,请联系管理员',
  feature_disabled: '该功能已被管理员关闭',

  // 图片服务
  service_error: '图片服务返回错误',
  service_timeout: '图片服务响应超时',
  service_unavailable: '图片服务暂不可用',
  service_init_error: '图片服务初始化失败',
  upstream_error: '图片服务返回错误',
  upstream_timeout: '图片服务响应超时',
  upstream_unavailable: '图片服务暂不可用',
  account_exhausted: '图片服务繁忙,请稍后重试',
  account_cooldown: '图片服务冷却中,请稍后重试',
  proxy_unhealthy: '代理不健康',

  // 其它
  internal_error: '服务器内部错误',
  canceled: '请求已取消',
}

/** 已翻译的错误码展示(未命中时原样返回,方便排障)。 */
export function formatErrorCode(code?: string | null): string {
  if (!code) return ''
  return ERROR_CODE_LABEL[code] || code
}

const IMAGE_ERROR_LABEL: Record<string, string> = {
  content_policy_rejected: '提示词可能违反内容安全策略,请修改后重试',
  image_assistant_reply: '图像助手回复',
  image_async_error: '图片生成任务返回错误',
  conversation_poll_error: '图片任务状态查询异常,请重试',
  image_task_stalled: '图片任务未返回结果,请重试',
  image_tool_without_asset: '图片生成未返回可用图片,请重试',
  turnstile_stalled: '图片服务验证卡住,请稍后重试',
  free_account_stalled: '图片任务未返回结果,请稍后重试',
  no_available_account: '图片服务繁忙,请稍后重试',
  rate_limited: '图片服务请求过于频繁,请稍后再试',
  auth_required: '图片服务授权状态异常,请稍后重试',
  network_transient: '图片服务网络波动,请重试',
  pow_timeout: '图片服务验证超时,请稍后重试',
  pow_failed: '图片服务验证失败,请稍后重试',
  turnstile_required: '图片服务验证未通过,请稍后重试',
  download_failed: '图片下载地址生成失败,请重试',
  upstream_error: '图片服务返回错误',
  service_error: '图片服务返回错误',
}

const IMAGE_ASSISTANT_REPLY_GENERIC = '图片生成未启动,请换一种描述重新生成'
const IMAGE_LABEL_ONLY_CODES = new Set([
  'auth_required',
  'no_available_account',
  'rate_limited',
  'network_transient',
  'pow_timeout',
  'pow_failed',
  'turnstile_required',
  'conversation_poll_error',
  'image_task_stalled',
  'image_tool_without_asset',
  'turnstile_stalled',
  'free_account_stalled',
  'download_failed',
])

function splitStoredImageError(raw: string): { code: string; detail: string } {
  const idx = raw.indexOf(':')
  if (idx <= 0) return { code: '', detail: raw.trim() }
  const code = raw.slice(0, idx).trim()
  if (!/^[a-z][a-z0-9_]*$/i.test(code)) {
    return { code: '', detail: raw.trim() }
  }
  return { code, detail: raw.slice(idx + 1).trim() }
}

function cleanImageErrorDetail(detail: string): string {
  return detail
    .replace(/chatgpt-freeaccount/gi, '图片服务')
    .replace(/chatgpt/gi, '图片服务')
    .replace(/upstream/gi, '服务')
    .replace(/上游/g, '服务')
    .replace(/账号池/g, '服务资源')
    .replace(/当前账号/g, '当前服务资源')
    .replace(/免费账号/g, '服务资源')
    .trim()
}

function compactText(s: string, max = 90): string {
  const oneLine = s.replace(/\s+/g, ' ').trim()
  if (oneLine.length <= max) return oneLine
  return `${oneLine.slice(0, max)}...`
}

function isJSONLikeText(s: string): boolean {
  const v = s.trim()
  return (
    (v.startsWith('{') && v.endsWith('}')) ||
    (v.startsWith('[') && v.endsWith(']')) ||
    /^\{?\s*"prompt"\s*:/i.test(v)
  )
}

/** 图片任务失败原因的用户侧短展示。原始 error 可放 title/tooltip 保留排障信息。 */
export function formatImageTaskError(error?: string | null): string {
  const raw = String(error || '').trim()
  if (!raw) return '图片生成失败,请重试'
  if (raw.startsWith('图像助手回复')) {
    return compactText(cleanImageErrorDetail(raw))
  }
  if (raw.startsWith('提示词可能违反') || raw.startsWith('提示词触发内容安全')) {
    return IMAGE_ERROR_LABEL.content_policy_rejected
  }
  if (raw.startsWith('图片生成未启动')) {
    const idx = raw.indexOf(':')
    const detail = idx >= 0 ? raw.slice(idx + 1).trim() : ''
    if (!detail || isJSONLikeText(detail)) return IMAGE_ASSISTANT_REPLY_GENERIC
    return `${IMAGE_ERROR_LABEL.image_assistant_reply}:${compactText(cleanImageErrorDetail(detail))}`
  }

  const { code, detail } = splitStoredImageError(raw)
  const label = code ? (IMAGE_ERROR_LABEL[code] || ERROR_CODE_LABEL[code] || code) : ''
  const publicDetail = cleanImageErrorDetail(detail)

  if (code === 'content_policy_rejected') {
    return IMAGE_ERROR_LABEL.content_policy_rejected
  }
  if (code === 'image_assistant_reply') {
    if (!publicDetail || isJSONLikeText(publicDetail)) {
      return IMAGE_ASSISTANT_REPLY_GENERIC
    }
    return `${IMAGE_ERROR_LABEL.image_assistant_reply}:${compactText(publicDetail)}`
  }
  if (code && IMAGE_LABEL_ONLY_CODES.has(code)) {
    return label
  }
  if (label) {
    if (!publicDetail || publicDetail === code || isJSONLikeText(publicDetail)) return label
    return `${label}:${compactText(publicDetail)}`
  }

  const cleaned = cleanImageErrorDetail(raw)
  return compactText(cleaned || '图片生成失败,请重试')
}
