// Package adapter 定义"上游供应商"的统一抽象。
//
// 之前 gateway 直接 hardcode 了 chatgpt.com 的调用栈(Bootstrap / chat-
// requirements / conversation/prepare / conversation/SSE),无法复用到
// OpenAI、Gemini、Anthropic 这些第三方 API。
//
// 这里把"给定请求 → 返回 OpenAI 兼容响应"这个核心动作抽成 Adapter 接口,
// 由 channel.Router 按本地模型挑选适配器并调用。每个适配器内部自行处理:
//   - 请求体转换(例如 OpenAI → Gemini generateContent)
//   - 鉴权(Bearer / x-goog-api-key / ...)
//   - 流式协议转换(Gemini 走非流式 → OpenAI SSE chunk)
//
// 目前仅实现 openai / gemini 的 text + image;video 预留模态位,
// 后续单独接入。
package adapter

import (
	"context"
	"io"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

// ChatRequest 统一的 chat 请求体,尽量向 OpenAI 格式靠拢。
type ChatRequest struct {
	Model       string                `json:"model"`
	Messages    []chatgpt.ChatMessage `json:"messages"`
	Stream      bool                  `json:"stream"`
	Temperature float64               `json:"temperature,omitempty"`
	TopP        float64               `json:"top_p,omitempty"`
	MaxTokens   int                   `json:"max_tokens,omitempty"`
}

// ChatChunk 流式输出的单个片段。
// FinishReason 非空时表示流结束,Usage 会在最后一个 chunk 带上(如上游未提供则估算)。
type ChatChunk struct {
	Delta        string
	FinishReason string
	Usage        *ChatUsage
	Err          error
}

type ChatUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ChatStream 适配器返回的统一 chunk 通道。调用方需 Drain 直到通道关闭。
type ChatStream = <-chan ChatChunk

// ImageRequest 统一的图片生成请求。
type ImageRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	N      int    `json:"n,omitempty"`
	Size   string `json:"size,omitempty"`   // 1024x1024 / 512x512 / auto
	Format string `json:"format,omitempty"` // url / b64_json
}

type ImageResult struct {
	// URLs 与 B64s 至少填一个;URL 由适配器直接给出(OpenAI 返回的签名 URL / Gemini 的
	// File API URI)。如果供应商只给出 base64,可以填 B64s,gateway 会按需落盘并生成签名 URL。
	URLs []string
	B64s []string
}

// Adapter 代表一个具体的上游供应商适配器实例(openai / gemini / ...)。
//
// 一个渠道(Channel)对应一个 Adapter 实例。上层根据本地模型 + 模态从渠道路由器
// 取 Adapter,再调用对应方法。
type Adapter interface {
	// Type 返回适配器类型(openai / gemini),用于日志。
	Type() string
	// UpstreamFor 把本地模型 slug 转成上游模型(由渠道绑定的 Mapping 决定,
	// 通常由路由层填好传下来)。
	Chat(ctx context.Context, upstreamModel string, req *ChatRequest) (ChatStream, error)
	ImageGenerate(ctx context.Context, upstreamModel string, req *ImageRequest) (*ImageResult, error)
	// Ping 发一次轻量校验请求,用于"测试连接"。
	Ping(ctx context.Context) error
}

// VideoCapable 可选接口:后续 Gemini Veo / OpenAI Sora 接入时实现它。
type VideoCapable interface {
	VideoGenerate(ctx context.Context, upstreamModel string, prompt string) (*VideoResult, error)
}

type VideoResult struct {
	URL string
}

// drainCloser 用于适配器内部快速把 stream 关掉。
type drainCloser interface {
	io.Closer
}

// Params 构造适配器时需要的共用参数。
type Params struct {
	BaseURL  string
	APIKey   string
	TimeoutS int
	// Extra 预留透传 JSON(例如 gemini 可能需要 project / location)。
	Extra string
}
