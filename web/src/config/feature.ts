// 前端 feature flag。
//
// 统一管理"功能是否对用户可见"的开关。以后要恢复文字模型的 UI,只需把
// ENABLE_CHAT_MODEL 改回 true,所有相关页面(在线体验 / 接口文档 / 用量统计 /
// 后台模型管理 …)会同时出现对应入口。
//
// 当前关闭原因:文字通路受 chatgpt.com 新 sentinel 协议影响,在 turnstile
// solver 接入前静默拒绝率很高,先把用户入口隐藏掉,避免误用扣费。后端路由
// (/v1/chat/completions / /api/me/playground/chat)保留,方便后续重新开启
// 和调试使用。
export const ENABLE_CHAT_MODEL = true
