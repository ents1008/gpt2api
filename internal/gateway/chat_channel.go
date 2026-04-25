package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/432539/gpt2api/internal/apikey"
	"github.com/432539/gpt2api/internal/billing"
	"github.com/432539/gpt2api/internal/channel"
	modelpkg "github.com/432539/gpt2api/internal/model"
	"github.com/432539/gpt2api/internal/upstream/adapter"
	"github.com/432539/gpt2api/internal/upstream/chatgpt"
	"github.com/432539/gpt2api/internal/usage"
	"github.com/432539/gpt2api/pkg/logger"
)

// dispatchChatToChannel 尝试把 chat 请求路由到外置渠道。
//
// 返回值:
//   - handled=true 表示本函数已响应客户端(成功 / 已写失败错误),调用方不应继续;
//   - handled=false 表示该本地模型没有配置渠道映射,调用方应回退到内置 ChatGPT 账号池。
func (h *Handler) dispatchChatToChannel(c *gin.Context,
	ak *apikey.APIKey, m *modelpkg.Model, req *ChatCompletionsRequest,
	rec *usage.Log, ratio float64, rpmCap int, tpmCap int64, startAt time.Time,
) bool {
	if h.Channels == nil {
		return false
	}
	routes, err := h.Channels.Resolve(c.Request.Context(), m.Slug, channel.ModalityText)
	if err != nil {
		if errors.Is(err, channel.ErrNoRoute) {
			return false
		}
		logger.L().Warn("channel resolve", zap.Error(err), zap.String("model", m.Slug))
		return false
	}
	if len(routes) == 0 {
		return false
	}

	refID := uuid.NewString()
	rec.RequestID = refID
	rec.ModelID = m.ID

	// RPM 限流
	if h.Limiter != nil {
		if ok, _, err := h.Limiter.AllowRPM(c.Request.Context(), ak.ID, rpmCap); err == nil && !ok {
			rec.Status = usage.StatusFailed
			rec.ErrorCode = "rate_limit_rpm"
			openAIError(c, http.StatusTooManyRequests, "rate_limit_rpm",
				"触发每分钟请求数限制 (RPM),请稍后再试")
			return true
		}
	}

	// 估算 & 预扣
	promptTokens := roughEstimateTokens(req.Messages)
	estTokens := req.MaxTokens
	if estTokens <= 0 {
		estTokens = 2048
	}
	estCost := billing.EstimateChat(m, promptTokens, req.MaxTokens, ratio)

	if h.Limiter != nil {
		if ok, _, err := h.Limiter.AllowTPM(c.Request.Context(), ak.ID, tpmCap,
			int64(promptTokens+estTokens)); err == nil && !ok {
			rec.Status = usage.StatusFailed
			rec.ErrorCode = "rate_limit_tpm"
			openAIError(c, http.StatusTooManyRequests, "rate_limit_tpm",
				"触发每分钟 Token 限制 (TPM),请稍后再试")
			return true
		}
	}

	if err := h.Billing.PreDeduct(c.Request.Context(), ak.UserID, ak.ID, estCost, refID, "chat prepay"); err != nil {
		rec.Status = usage.StatusFailed
		if errors.Is(err, billing.ErrInsufficient) {
			rec.ErrorCode = "insufficient_balance"
			openAIError(c, http.StatusPaymentRequired, "insufficient_balance",
				"积分不足,请前往「账单与充值」充值后再试")
			return true
		}
		rec.ErrorCode = "billing_error"
		openAIError(c, http.StatusInternalServerError, "billing_error", "计费异常:"+err.Error())
		return true
	}
	refunded := false
	refund := func(code string) {
		rec.Status = usage.StatusFailed
		rec.ErrorCode = code
		if refunded {
			return
		}
		refunded = true
		_ = h.Billing.Refund(context.Background(), ak.UserID, ak.ID, estCost, refID, "chat refund")
	}

	// 尝试候选渠道列表,直到某个成功。
	adReq := &adapter.ChatRequest{
		Model:       m.Slug,
		Messages:    req.Messages,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
	}

	var lastErr error
	var selected *channel.Route
	var stream adapter.ChatStream

	for _, rt := range routes {
		upstreamModel := rt.UpstreamModel
		s, err := rt.Adapter.Chat(c.Request.Context(), upstreamModel, adReq)
		if err != nil {
			lastErr = err
			_ = h.Channels.Svc().MarkHealth(context.Background(), rt.Channel, false, err.Error())
			logger.L().Warn("channel chat fail, try next",
				zap.Uint64("channel_id", rt.Channel.ID),
				zap.String("channel_name", rt.Channel.Name),
				zap.String("upstream_model", upstreamModel),
				zap.Error(err))
			continue
		}
		selected = rt
		stream = s
		break
	}

	if selected == nil {
		refund("upstream_error")
		msg := "所有上游渠道均不可用"
		if lastErr != nil {
			msg = msg + ":" + lastErr.Error()
		}
		openAIError(c, http.StatusBadGateway, "upstream_error", msg)
		return true
	}

	// 成功选到一个渠道,标记健康。
	_ = h.Channels.Svc().MarkHealth(context.Background(), selected.Channel, true, "")

	// 应用渠道级倍率(在 billing ratio 基础上叠乘)。
	channelRatio := selected.Channel.Ratio
	if channelRatio <= 0 {
		channelRatio = 1.0
	}

	id := "chatcmpl-" + uuid.NewString()
	if req.Stream {
		h.streamChannel(c, id, m.Slug, stream)
	} else {
		h.collectChannel(c, id, m.Slug, stream)
	}
	completionTokens := h.lastCompletionTokens(c)

	actual := billing.ComputeChatCost(m, promptTokens, completionTokens, ratio*channelRatio)
	if err := h.Billing.Settle(context.Background(), ak.UserID, ak.ID, estCost, actual, refID, "chat settle"); err != nil {
		logger.L().Error("billing settle", zap.Error(err), zap.String("ref", refID))
	}
	_ = h.Keys.DAO().TouchUsage(context.Background(), ak.ID, c.ClientIP(), actual)

	if h.Limiter != nil {
		real := int64(promptTokens + completionTokens)
		est := int64(promptTokens + estTokens)
		if diff := real - est; diff > 0 {
			h.Limiter.AdjustTPM(context.Background(), ak.ID, tpmCap, diff)
		}
	}
	rec.Status = usage.StatusSuccess
	rec.InputTokens = promptTokens
	rec.OutputTokens = completionTokens
	rec.CreditCost = actual
	_ = startAt
	return true
}

// streamChannel 将 adapter.ChatStream 转成 OpenAI SSE 写回给客户端。
func (h *Handler) streamChannel(c *gin.Context, id, model string, stream adapter.ChatStream) {
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	writeChunk(w, flusher, id, model, DeltaMsg{Role: "assistant"}, nil)

	var total strings.Builder
	finish := "stop"
	var completionTokens int
	for ch := range stream {
		if ch.Err != nil {
			logger.L().Warn("channel stream err", zap.Error(ch.Err))
			break
		}
		if ch.Usage != nil {
			completionTokens = ch.Usage.CompletionTokens
		}
		if ch.Delta != "" {
			total.WriteString(ch.Delta)
			writeChunk(w, flusher, id, model, DeltaMsg{Content: ch.Delta}, nil)
		}
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
	}
	writeChunk(w, flusher, id, model, DeltaMsg{}, &finish)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	if completionTokens <= 0 {
		completionTokens = (total.Len() + 3) / 4
	}
	c.Set("completion_tokens", completionTokens)
}

// collectChannel 非流式:收拢后一次性 JSON 返回。
func (h *Handler) collectChannel(c *gin.Context, id, model string, stream adapter.ChatStream) {
	var total strings.Builder
	finish := "stop"
	var completionTokens int
	for ch := range stream {
		if ch.Err != nil {
			logger.L().Warn("channel collect err", zap.Error(ch.Err))
			break
		}
		if ch.Usage != nil {
			completionTokens = ch.Usage.CompletionTokens
		}
		if ch.Delta != "" {
			total.WriteString(ch.Delta)
		}
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
	}
	if completionTokens <= 0 {
		completionTokens = (total.Len() + 3) / 4
	}
	c.Set("completion_tokens", completionTokens)
	resp := ChatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []ChatCompletionChoice{{
			Index:        0,
			Message:      chatgpt.ChatMessage{Role: "assistant", Content: total.String()},
			FinishReason: finish,
		}},
		Usage: ChatCompletionUsage{
			PromptTokens:     0,
			CompletionTokens: completionTokens,
			TotalTokens:      completionTokens,
		},
	}
	c.JSON(http.StatusOK, resp)
}
