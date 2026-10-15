package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/432539/gpt2api/internal/scheduler"
	"github.com/432539/gpt2api/internal/upstream/chatgpt"
	"github.com/432539/gpt2api/pkg/logger"
)

// QuotaDecrementor 允许 Runner 在生图成功后立即扣减账号剩余额度,
// 无需等待下一次后台探测即可在前端看到正确数字。
type QuotaDecrementor interface {
	DecrQuota(ctx context.Context, accountID uint64, n int) error
}

// QuotaLimitRecorder records definitive upstream quota exhaustion signals.
type QuotaLimitRecorder interface {
	MarkQuotaDepleted(ctx context.Context, accountID uint64, resetAt *time.Time) error
}

type AccountScheduler interface {
	Dispatch(ctx context.Context, modelType string) (*scheduler.Lease, error)
	MarkRateLimited(ctx context.Context, accountID uint64)
	MarkRateLimitedFor(ctx context.Context, accountID uint64, cooldownFor time.Duration)
	MarkWarned(ctx context.Context, accountID uint64)
	MarkDead(ctx context.Context, accountID uint64)
	MarkDeadWithReason(ctx context.Context, accountID uint64, reason string)
	MarkSuspicious(ctx context.Context, accountID uint64, reason string)
	RestoreHealthy(ctx context.Context, accountID uint64)
}

type AccountRecovery interface {
	RecoverImageCredential(
		context.Context,
		uint64,
		string,
		string,
	) (authToken string, cookies string, recovered bool, err error)
}

type imageDownloadClient interface {
	ImageDownloadURL(ctx context.Context, convID, ref string) (string, error)
	FetchImage(ctx context.Context, url string, maxBytes int64) ([]byte, string, error)
}

type verifiedImageDownload struct {
	Ref         string
	URL         string
	ContentType string
	Data        []byte
}

// Runner 单次/多次生图的执行器。封装完整的 chatgpt.com 协议链路:
//
//	ChatRequirements → PrepareFConversation → StreamFConversation (SSE) →
//	ParseImageSSE → (需要时) PollConversationForImages → ImageDownloadURL
//
// IMG2 已正式上线,不再做"灰度命中判定 / preview_only 换账号重试"这些节流操作,
// 拿到任意 file-service / sediment 引用即算成功,以速度和效率优先。
type Runner struct {
	sched           AccountScheduler
	dao             *DAO
	quotaDecr       QuotaDecrementor // 生图成功后立即扣减账号额度(可空,空时跳过)
	accountRecovery AccountRecovery
}

const quotaDepletedCooldown = 24 * time.Hour
const maxVerifiedImageBytes = 16 * 1024 * 1024

var errNoVerifiedImageDownloads = errors.New("no verified image downloads")

type verifiedImageDownloadsError struct {
	cause error
}

func (e *verifiedImageDownloadsError) Error() string {
	if e == nil || e.cause == nil {
		return errNoVerifiedImageDownloads.Error()
	}
	return errNoVerifiedImageDownloads.Error() + ": " + e.cause.Error()
}

func (e *verifiedImageDownloadsError) Unwrap() error {
	if e == nil || e.cause == nil {
		return errNoVerifiedImageDownloads
	}
	return errors.Join(errNoVerifiedImageDownloads, e.cause)
}

var (
	imageQuotaRetryAfterPattern = regexp.MustCompile(
		`(?i)limit resets in (?:(\d+)\s+hours?(?:\s+and\s+)?)?(?:(\d+)\s+minutes?)?`,
	)
	imageQuotaRetryAfterZhPattern = regexp.MustCompile(
		`(?:上限)?将在\s*(?:(\d+)\s*小时)?\s*(?:(\d+)\s*分钟)?\s*后重置`,
	)
)

// NewRunner 构造 Runner。
func NewRunner(sched *scheduler.Scheduler, dao *DAO) *Runner {
	return &Runner{sched: sched, dao: dao}
}

// SetQuotaDecrementor 注入额度扣减器。
func (r *Runner) SetQuotaDecrementor(qd QuotaDecrementor) { r.quotaDecr = qd }

func (r *Runner) SetAccountRecovery(recovery AccountRecovery) { r.accountRecovery = recovery }

type preImageAuthError struct {
	stage string
	cause error
}

func newPreImageAuthError(stage string, cause error) error {
	return &preImageAuthError{stage: stage, cause: cause}
}

func (e *preImageAuthError) Error() string {
	if e == nil || e.cause == nil {
		return "pre-image authentication failed"
	}
	return "pre-image " + e.stage + " authentication failed: " + e.cause.Error()
}

func (e *preImageAuthError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func isPreImageAuthError(err error) bool {
	var target *preImageAuthError
	return errors.As(err, &target)
}

// ReferenceImage 是图生图/编辑的一张参考图输入。
// 只需要提供原始字节 + 可选的文件名,Runner 会在运行时调用 chatgpt Client 上传。
type ReferenceImage struct {
	Data     []byte
	FileName string // 可选,未填时按长度 + 嗅探扩展名生成
}

// RunOptions 是单次生图的输入。
type RunOptions struct {
	TaskID                 string
	UserID                 uint64
	KeyID                  uint64
	ModelID                uint64
	UpstreamModel          string // 默认 "auto"(由上游根据 system_hints 挑选图像模型)
	Prompt                 string
	N                      int              // 期望返回的图片张数;够数 Poll 就立即返回(速度优先)
	MaxAttempts            int              // 跨账号重试次数,默认 1;用于硬错误和少数可确认瞬态的上游异步出图故障
	PerAttemptTimeout      time.Duration    // 单次尝试总超时,默认 2min(限制坏账号占用请求预算)
	SSEStartTimeout        time.Duration    // SSE 等待图片任务启动信号的超时,默认 45s
	ReferenceUploadTimeout time.Duration    // 单张参考图上传超时,默认 120s;覆盖 create/PUT/register 三步
	PollMaxWait            time.Duration    // SSE 没直出时,轮询 conversation 的最长等待,默认 2min
	References             []ReferenceImage // 图生图/编辑:参考图
}

// RunResult 是单次生图的输出。
type RunResult struct {
	Status         string // success / failed
	ConversationID string
	AccountID      uint64
	FileIDs        []string // chatgpt.com 侧的原始 ref("sed:" 前缀表示 sediment)
	SignedURLs     []string // 直接可访问的签名 URL(15 分钟有效)
	ContentTypes   []string
	ErrorCode      string
	ErrorMessage   string
	Attempts       int // 跨账号尝试次数(runOnce 次数)
	DurationMs     int64
}

func persistTaskWrite(taskID, op string, fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fn(ctx); err != nil {
		logger.L().Warn("image task state write failed",
			zap.String("task_id", taskID),
			zap.String("op", op),
			zap.Error(err),
		)
	}
}

func normalizeRunOptions(opt RunOptions) RunOptions {
	if opt.MaxAttempts <= 0 {
		// 默认只跑 1 次,不为"没命中"做跨账号重试。
		// 仅当首轮因为没调度到账号 / 账号被硬限流 / 命中特定可重试的上游异步生图故障时,
		// 才会用 MaxAttempts>1 做换账号重试。
		opt.MaxAttempts = 1
	}
	if opt.PerAttemptTimeout <= 0 {
		opt.PerAttemptTimeout = 2 * time.Minute
	}
	if opt.SSEStartTimeout <= 0 {
		opt.SSEStartTimeout = 45 * time.Second
	}
	if opt.ReferenceUploadTimeout <= 0 {
		opt.ReferenceUploadTimeout = 120 * time.Second
	}
	if opt.PollMaxWait <= 0 {
		opt.PollMaxWait = 2 * time.Minute
	}
	if opt.UpstreamModel == "" {
		opt.UpstreamModel = "auto"
	}
	if opt.N <= 0 {
		opt.N = 1
	}
	return opt
}

func resolveUpstreamImageModel(requested string) string {
	if requested == "" || requested == "auto" {
		return "auto"
	}
	return requested
}

func buildImageConversationOptions(
	opt RunOptions,
	requirements *chatgpt.ChatRequirementsResp,
	proofToken string,
	references []*chatgpt.UploadedFile,
	parentMessageID string,
	messageID string,
) chatgpt.ImageConvOpts {
	return chatgpt.ImageConvOpts{
		Prompt:         opt.Prompt,
		UpstreamModel:  resolveUpstreamImageModel(opt.UpstreamModel),
		ParentMsgID:    parentMessageID,
		MessageID:      messageID,
		ChatToken:      requirements.Token,
		ProofToken:     proofToken,
		TurnstileToken: requirements.TurnstileToken,
		SOToken:        requirements.SOToken,
		References:     references,
	}
}

func effectiveProxyURL(dispatchProxy string) string {
	if strings.TrimSpace(dispatchProxy) != "" {
		return dispatchProxy
	}
	return strings.TrimSpace(os.Getenv("GPT2API_FORCE_PROXY_URL"))
}

func collectVerifiedImageDownloads(ctx context.Context, cli imageDownloadClient, convID string, fileRefs []string) ([]verifiedImageDownload, error) {
	downloads := make([]verifiedImageDownload, 0, len(fileRefs))
	var lastErr error
	for _, ref := range fileRefs {
		url, err := cli.ImageDownloadURL(ctx, convID, ref)
		if err != nil {
			lastErr = err
			logger.L().Warn("image runner download url failed",
				zap.String("ref", ref), zap.Error(err))
			continue
		}
		body, ct, err := fetchVerifiedImageWithRetry(ctx, cli, url, maxVerifiedImageBytes)
		if err != nil {
			lastErr = err
			logger.L().Warn("image runner fetch image failed",
				zap.String("ref", ref), zap.Error(err))
			continue
		}
		if ct == "" {
			ct = "image/png"
		}
		downloads = append(downloads, verifiedImageDownload{
			Ref:         ref,
			URL:         url,
			ContentType: ct,
			Data:        body,
		})
	}
	if len(downloads) == 0 {
		if lastErr != nil {
			return nil, &verifiedImageDownloadsError{cause: lastErr}
		}
		return nil, &verifiedImageDownloadsError{}
	}
	return downloads, nil
}

func classifyImageDownloadFailure(err error) string {
	if err == nil {
		return ErrDownload
	}
	var upstreamErr *chatgpt.UpstreamError
	if errors.As(err, &upstreamErr) {
		switch {
		case upstreamErr.IsUnauthorized():
			return ErrAuthRequired
		case upstreamErr.IsForbidden():
			return ErrUpstreamForbidden
		}
	}
	return ErrDownload
}

func fetchVerifiedImageWithRetry(ctx context.Context, cli imageDownloadClient, url string, maxBytes int64) ([]byte, string, error) {
	const maxAttempts = 3

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		body, ct, err := cli.FetchImage(ctx, url, maxBytes)
		if err == nil {
			return body, ct, nil
		}
		lastErr = err
		if !isRetryableImageFetchError(err) || attempt == maxAttempts {
			return nil, ct, err
		}
		if !sleepContext(ctx, time.Duration(attempt)*300*time.Millisecond) {
			return nil, ct, ctx.Err()
		}
	}
	return nil, "", lastErr
}

func parseImageSSEForAttempt(stream <-chan chatgpt.SSEEvent, expectedN int, maxWait time.Duration) chatgpt.ImageSSEResult {
	return chatgpt.ParseImageSSEUntilReady(stream, chatgpt.ImageSSEParseOpts{
		ExpectedN:          expectedN,
		ReturnWhenPollable: true,
		MaxWait:            maxWait,
	})
}

func isRetryableImageFetchError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var upstreamErr *chatgpt.UpstreamError
	if errors.As(err, &upstreamErr) {
		return upstreamErr.Status == 408 || upstreamErr.Status == 429 || upstreamErr.Status >= 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "unexpected eof") ||
		strings.Contains(text, "connection reset") ||
		strings.Contains(text, "connection refused") ||
		strings.Contains(text, "broken pipe") ||
		strings.Contains(text, "server closed idle connection") ||
		strings.Contains(text, "tls handshake timeout")
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func composeStoredTaskError(code, raw string) string {
	if raw == "" || raw == code {
		return code
	}
	return code + ": " + raw
}

func buildPollTimeoutMessage(sse chatgpt.ImageSSEResult, dbg chatgpt.PollDebug, turnstileRequired, freeAccount bool) string {
	parts := []string{"no_image_ref"}
	if turnstileRequired {
		parts = append(parts, "turnstile_required")
	}
	if freeAccount {
		parts = append(parts, "free_account")
	}
	if sse.ImageGenTaskID != "" {
		parts = append(parts, "image_gen_task_started")
	}
	if sse.FinishType != "" {
		parts = append(parts, "finish_type="+sse.FinishType)
	}
	if dbg.NewToolMessages == 0 {
		parts = append(parts, "no_tool_messages")
	} else {
		parts = append(parts, fmt.Sprintf("tool_messages=%d", dbg.NewToolMessages))
	}
	if dbg.AssistantReplyText != "" {
		parts = append(parts, "assistant_reply="+dbg.AssistantReplyText)
	}
	if dbg.Attempts > 0 {
		parts = append(parts, fmt.Sprintf("poll_attempts=%d", dbg.Attempts))
	}
	if dbg.ConversationErrors > 0 {
		parts = append(parts, fmt.Sprintf("conversation_errors=%d", dbg.ConversationErrors))
	}
	if dbg.RateLimitHits > 0 {
		parts = append(parts, fmt.Sprintf("poll_429=%d", dbg.RateLimitHits))
	}
	if dbg.LastToolModelSlug != "" {
		parts = append(parts, "last_tool_model="+dbg.LastToolModelSlug)
	}
	if dbg.LastToolRecipient != "" {
		parts = append(parts, "last_tool_recipient="+dbg.LastToolRecipient)
	}
	if dbg.LastError != "" {
		parts = append(parts, "last_error="+dbg.LastError)
	}
	return strings.Join(parts, "; ")
}

func classifyPollFailureCode(sse chatgpt.ImageSSEResult, dbg chatgpt.PollDebug, turnstileRequired, freeAccount bool) string {
	switch {
	case isImageQuotaDepletedMessage(dbg.AsyncErrorText) || isImageQuotaDepletedMessage(dbg.AssistantReplyText):
		return ErrQuotaDepleted
	case dbg.AsyncErrorType == "content_policy" || chatgpt.IsContentPolicyText(dbg.AsyncErrorText) || chatgpt.IsContentPolicyText(dbg.AssistantReplyText):
		return ErrContentPolicyRejected
	case dbg.AsyncErrorType != "":
		return ErrImageAsyncError
	case isRetryableImageAsyncErrorText(dbg.AssistantReplyText):
		return ErrImageAsyncError
	case dbg.AssistantReplyText != "" && sse.ImageGenTaskID == "":
		return ErrAssistantReply
	case dbg.ConversationErrors > 0:
		return ErrConversationPoll
	case dbg.NewToolMessages > 0:
		return ErrImageToolNoAsset
	case sse.ImageGenTaskID != "":
		return ErrImageTaskStalled
	case turnstileRequired:
		return ErrTurnstileStalled
	case freeAccount:
		return ErrFreeAccountStalled
	default:
		return ErrPollTimeout
	}
}

func classifySSEStartFailure(sse chatgpt.ImageSSEResult) (string, string) {
	if text := strings.TrimSpace(sse.AssistantText); text != "" {
		if isImageQuotaDepletedMessage(text) {
			return ErrQuotaDepleted, text
		}
		if isRetryableImageAsyncErrorText(text) {
			return ErrImageAsyncError, text
		}
		if chatgpt.IsContentPolicyText(text) {
			return ErrContentPolicyRejected, text
		}
		return ErrAssistantReply, text
	}

	parts := []string{"sse_no_image_task"}
	if sse.TimedOut {
		parts = append(parts, "sse_timed_out")
	}
	if sse.ConversationID == "" {
		parts = append(parts, "sse_no_conversation")
	}
	if sse.FinishType != "" {
		parts = append(parts, "finish_type="+sse.FinishType)
	}
	if sse.TurnUseCase != "" {
		parts = append(parts, "turn_use_case="+sse.TurnUseCase)
	}
	if !sse.ToolInvoked {
		parts = append(parts, "tool_invoked=false")
	}
	return ErrImageTaskStalled, strings.Join(parts, "; ")
}

func isImageQuotaDepletedMessage(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	if normalized == "" {
		return false
	}
	if strings.Contains(normalized, "free plan limit") &&
		(strings.Contains(normalized, "image generation") || strings.Contains(normalized, "image creation")) {
		return true
	}

	// 支持中文 Free / 免费套餐生图上限提示:
	// 例如: "你已达到 Free 套餐的图像生成请求上限。上限将在 24小时 后重置，届时可创建更多图像。"
	isFreeOrPlan := strings.Contains(normalized, "free") || strings.Contains(normalized, "免费")
	hasQuotaOrLimit := strings.Contains(normalized, "上限") || strings.Contains(normalized, "额度已用完") || strings.Contains(normalized, "重置")
	hasImageContext := strings.Contains(normalized, "图像") || strings.Contains(normalized, "图片") || strings.Contains(normalized, "生图")
	if isFreeOrPlan && hasQuotaOrLimit && hasImageContext {
		return true
	}

	if (strings.Contains(normalized, "free 套餐") || strings.Contains(normalized, "free计划") || strings.Contains(normalized, "免费套餐")) &&
		(strings.Contains(normalized, "上限") || strings.Contains(normalized, "次数已用尽") || strings.Contains(normalized, "后重置")) {
		return true
	}

	normalizedClean := strings.ReplaceAll(normalized, "-", " ")
	normalizedClean = strings.Join(strings.Fields(normalizedClean), " ")
	return strings.Contains(normalizedClean, "image generation limit has been reached")
}

func extractImageQuotaRetryAfter(text string) (time.Duration, bool) {
	if !isImageQuotaDepletedMessage(text) {
		return 0, false
	}

	if matches := imageQuotaRetryAfterPattern.FindStringSubmatch(text); matches != nil {
		hours, _ := strconv.Atoi(matches[1])
		minutes, _ := strconv.Atoi(matches[2])
		d := time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute
		if d > 0 {
			return d, true
		}
		return quotaDepletedCooldown, true
	}

	if matches := imageQuotaRetryAfterZhPattern.FindStringSubmatch(text); matches != nil {
		var hours, minutes int
		if matches[1] != "" {
			hours, _ = strconv.Atoi(matches[1])
		}
		if matches[2] != "" {
			minutes, _ = strconv.Atoi(matches[2])
		}
		d := time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute
		if d > 0 {
			return d, true
		}
		return quotaDepletedCooldown, true
	}

	return quotaDepletedCooldown, true
}

func (r *Runner) markRateLimitedFromMessage(ctx context.Context, accountID uint64, detail string) {
	if r.sched == nil || accountID == 0 {
		return
	}
	if retryAfter, ok := extractImageQuotaRetryAfter(detail); ok && retryAfter > 0 {
		r.sched.MarkRateLimitedFor(ctx, accountID, retryAfter)
		return
	}
	r.sched.MarkRateLimited(ctx, accountID)
}

func (r *Runner) markQuotaDepletedFromMessage(ctx context.Context, accountID uint64, detail string, now time.Time) {
	if accountID == 0 {
		return
	}
	cooldownFor := quotaCooldownFromMessage(detail)
	if r.sched != nil {
		r.sched.MarkRateLimitedFor(ctx, accountID, cooldownFor)
	}
	if r.quotaDecr == nil {
		return
	}
	recorder, ok := r.quotaDecr.(QuotaLimitRecorder)
	if !ok {
		return
	}
	reset := now.Add(cooldownFor)
	if err := recorder.MarkQuotaDepleted(ctx, accountID, &reset); err != nil {
		logger.L().Warn("image quota depleted state write failed",
			zap.Uint64("account_id", accountID),
			zap.Error(err),
		)
	}
}

func quotaCooldownFromMessage(detail string) time.Duration {
	if retryAfter, ok := extractImageQuotaRetryAfter(detail); ok && retryAfter > 0 {
		return retryAfter
	}
	return quotaDepletedCooldown
}

func (r *Runner) recordPollFailureAccountState(ctx context.Context, accountID uint64, code string, detail string, now time.Time) {
	switch code {
	case ErrQuotaDepleted:
		r.markQuotaDepletedFromMessage(ctx, accountID, detail, now)
	case ErrRateLimited:
		r.markRateLimitedFromMessage(ctx, accountID, detail)
	}
}

func (r *Runner) markAccountFailureState(ctx context.Context, taskID string, attempt int, accountID uint64, status string, err error) {
	if r.sched == nil || accountID == 0 {
		return
	}
	switch status {
	case ErrAuthRequired:
		r.sched.MarkDeadWithReason(ctx, accountID, "auth_required_401")
		logger.L().Warn("image runner auth required, mark account dead and retry",
			zap.String("task_id", taskID),
			zap.Int("attempt", attempt),
			zap.Uint64("account_id", accountID),
			zap.String("reason", "auth_required_401"))
	default:
		reason, ok := retryableAccountFailureReason(status, err)
		if !ok {
			return
		}
		r.sched.MarkSuspicious(ctx, accountID, reason)
		logger.L().Warn("image runner transient failure, mark account suspicious and retry",
			zap.String("task_id", taskID),
			zap.Int("attempt", attempt),
			zap.Uint64("account_id", accountID),
			zap.String("status", status),
			zap.String("reason", reason))
	}
}

func (r *Runner) markAccountSuccessState(ctx context.Context, accountID uint64) {
	if r.sched == nil || accountID == 0 {
		return
	}
	r.sched.RestoreHealthy(ctx, accountID)
}

func shouldRetryRunAttempt(status string, err error) bool {
	switch status {
	case ErrQuotaDepleted, ErrRateLimited, ErrAuthRequired:
		return true
	default:
		_, ok := retryableAccountFailureReason(status, err)
		return ok
	}
}

func retryableAccountFailureReason(status string, err error) (string, bool) {
	switch status {
	case ErrUpstreamForbidden:
		if isProxyConnectForbiddenError(err) {
			return "proxy_connect_403", true
		}
		if isPrepareForbiddenError(err) {
			return "prepare_403", true
		}
		return "upstream_403", true
	case ErrNetworkTransient:
		return "network_transient", true
	case ErrDownload:
		return "download_failed", true
	case ErrPollTimeout:
		return "poll_timeout", true
	case ErrConversationPoll:
		return "conversation_poll_error", true
	case ErrImageTaskStalled:
		return "image_task_stalled", true
	case ErrImageToolNoAsset:
		return "image_tool_without_asset", true
	case ErrTurnstileStalled:
		return "turnstile_stalled", true
	case ErrFreeAccountStalled:
		return "free_account_stalled", true
	case ErrPOWTimeout:
		return "pow_timeout", true
	case ErrPOWFailed:
		return "pow_failed", true
	case ErrUpstream:
		if isRetryableNetworkError(err) {
			return "upstream_network_transient", true
		}
	case ErrImageAsyncError:
		if isRetryableImageAsyncError(err) {
			return "image_async_transient", true
		}
	}
	return "", false
}

func isPrepareForbiddenError(err error) bool {
	if err == nil {
		return false
	}
	var upstream *chatgpt.UpstreamError
	if errors.As(err, &upstream) {
		return upstream.Status == 403 &&
			strings.Contains(strings.ToLower(upstream.Message), "chat-requirements/prepare failed")
	}
	return strings.Contains(strings.ToLower(err.Error()), "chat-requirements/prepare failed")
}

func isRetryableNetworkError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "upload reference") && strings.Contains(msg, "upload put failed") {
		return true
	}
	return strings.Contains(msg, "eof") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "bad gateway") ||
		strings.Contains(msg, "gateway timeout") ||
		strings.Contains(msg, "tls: unrecognized name") ||
		strings.Contains(msg, "utls handshake") ||
		strings.Contains(msg, "use of closed network connection")
}

func isProxyConnectForbiddenError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "proxy connect") &&
		strings.Contains(msg, "chatgpt.com:443") &&
		strings.Contains(msg, "403 forbidden")
}

func isRetryableImageAsyncError(err error) bool {
	if err == nil {
		return false
	}
	return isRetryableImageAsyncErrorText(err.Error())
}

func isRetryableImageAsyncErrorText(text string) bool {
	detail := strings.ToLower(strings.TrimSpace(text))
	if detail == "" {
		return false
	}
	if strings.Contains(detail, "we experienced an error when generating images") {
		return true
	}
	if strings.Contains(detail, "image creation is temporarily unavailable") ||
		strings.Contains(detail, "image creation tool is currently unavailable") {
		return true
	}
	return strings.Contains(detail, "发生了错误") &&
		(strings.Contains(detail, "未能生成图片") || strings.Contains(detail, "无法生成图片"))
}

func applyContextErrorToResult(result *RunResult, err error) {
	if err == nil {
		return
	}
	if result == nil {
		return
	}
	if result.ErrorCode != "" && result.ErrorCode != ErrUnknown {
		return
	}
	result.ErrorCode = ErrUnknown
	result.ErrorMessage = err.Error()
}

// Run 执行生图。会同步阻塞直到完成/失败;调用方自行做超时控制(传 ctx)。
func (r *Runner) Run(ctx context.Context, opt RunOptions) *RunResult {
	start := time.Now()
	opt = normalizeRunOptions(opt)

	result := &RunResult{Status: StatusFailed, ErrorCode: ErrUnknown}

	// 仅当有 DAO 和 taskID 时才落库
	if r.dao != nil && opt.TaskID != "" {
		persistTaskWrite(opt.TaskID, "mark_running", func(writeCtx context.Context) error {
			return r.dao.MarkRunning(writeCtx, opt.TaskID, 0)
		})
	}

	if opt.N > 1 {
		// 并发模式:N 个 goroutine 各独立出 1 张
		r.runParallel(ctx, opt, start, result)
	} else {
		// 串行模式(原逻辑):带跨账号重试
		for attempt := 1; attempt <= opt.MaxAttempts; attempt++ {
			result.Attempts = attempt
			if err := ctx.Err(); err != nil {
				applyContextErrorToResult(result, err)
				break
			}

			attemptCtx, cancel := context.WithTimeout(ctx, opt.PerAttemptTimeout)
			ok, status, err := r.runOnce(attemptCtx, opt, result)
			cancel()

			if ok {
				result.Status = StatusSuccess
				result.ErrorCode = ""
				result.ErrorMessage = ""
				r.markAccountSuccessState(context.Background(), result.AccountID)
				break
			}
			if err != nil {
				result.ErrorMessage = err.Error()
			}
			result.ErrorCode = status

			r.markAccountFailureState(context.Background(), opt.TaskID, attempt, result.AccountID, status, err)
			if attempt >= opt.MaxAttempts {
				break
			}
			// 仅对账号级硬错误、瞬态网络错误、以及少数明确可重试的上游异步出图故障做跨账号重试。
			// 其他错误(poll 超时 / 内容安全拒绝 / 助手文本拒绝 / 下载失败)直接退出,避免静默吞时间。
			retryable := shouldRetryRunAttempt(status, err)
			if !retryable {
				break
			}
			logger.L().Info("image runner retry with another account",
				zap.String("task_id", opt.TaskID),
				zap.String("reason", status),
				zap.Int("attempt", attempt))
		}
	}

	result.DurationMs = time.Since(start).Milliseconds()

	// 落库
	if r.dao != nil && opt.TaskID != "" {
		if result.Status == StatusSuccess {
			persistTaskWrite(opt.TaskID, "mark_success", func(writeCtx context.Context) error {
				return r.dao.MarkSuccess(writeCtx, opt.TaskID, result.ConversationID,
					result.FileIDs, result.SignedURLs, 0 /* credit_cost 由网关负责写 */)
			})
			if r.quotaDecr != nil && result.AccountID > 0 {
				n := len(result.FileIDs)
				if n == 0 {
					n = opt.N
				}
				_ = r.quotaDecr.DecrQuota(context.Background(), result.AccountID, n)
			}
		} else {
			persistTaskWrite(opt.TaskID, "mark_failed", func(writeCtx context.Context) error {
				return r.dao.MarkFailed(writeCtx, opt.TaskID,
					composeStoredTaskError(result.ErrorCode, result.ErrorMessage))
			})
		}
	}
	return result
}

// runParallel 并发启动 opt.N 个独立请求,每个各出 1 张图,最终合并到 result。
// 只要有 ≥1 张成功就算整体成功;全部失败才返回失败。
// 各 goroutine 不写 DAO(TaskID 置空),写库由外层 Run 统一完成。
func (r *Runner) runParallel(ctx context.Context, opt RunOptions, start time.Time, result *RunResult) {
	type subResult struct {
		ok         bool
		fileIDs    []string
		signedURLs []string
		convID     string
		accountID  uint64
		errCode    string
		errMsg     string
	}

	n := opt.N
	ch := make(chan subResult, n)

	// 子任务:单张、不写 DAO
	subOpt := opt
	subOpt.N = 1
	subOpt.TaskID = "" // 禁用 DAO,避免多 goroutine 互相覆盖

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub := &RunResult{Status: StatusFailed, ErrorCode: ErrUnknown}
			attemptCtx, cancel := context.WithTimeout(ctx, opt.PerAttemptTimeout)
			defer cancel()
			ok, status, err := r.runOnce(attemptCtx, subOpt, sub)
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if !ok && status == "" {
				status = sub.ErrorCode
			}
			ch <- subResult{
				ok:         ok,
				fileIDs:    sub.FileIDs,
				signedURLs: sub.SignedURLs,
				convID:     sub.ConversationID,
				accountID:  sub.AccountID,
				errCode:    status,
				errMsg:     msg,
			}
		}()
	}

	// 等待全部完成后关闭 channel
	go func() { wg.Wait(); close(ch) }()

	var (
		successCount int
		lastErrCode  string
		lastErrMsg   string
	)
	for sr := range ch {
		if sr.ok {
			successCount++
			r.markAccountSuccessState(context.Background(), sr.accountID)
			result.FileIDs = append(result.FileIDs, sr.fileIDs...)
			result.SignedURLs = append(result.SignedURLs, sr.signedURLs...)
			if result.ConversationID == "" {
				result.ConversationID = sr.convID
			}
			if result.AccountID == 0 {
				result.AccountID = sr.accountID
			}
		} else {
			lastErrCode = sr.errCode
			lastErrMsg = sr.errMsg
		}
	}
	result.Attempts = n

	if successCount > 0 {
		result.Status = StatusSuccess
		result.ErrorCode = ""
		result.ErrorMessage = ""
		logger.L().Info("image runner parallel done",
			zap.String("task_id", opt.TaskID),
			zap.Int("requested", n),
			zap.Int("succeeded", successCount),
			zap.Int("got_images", len(result.FileIDs)),
		)
	} else {
		result.ErrorCode = lastErrCode
		result.ErrorMessage = lastErrMsg
		logger.L().Warn("image runner parallel all failed",
			zap.String("task_id", opt.TaskID),
			zap.Int("requested", n),
			zap.String("last_err", lastErrCode),
		)
	}
}

type leaseAttemptExecutor func(
	context.Context,
	RunOptions,
	*RunResult,
	*scheduler.Lease,
) (bool, string, error)

// runOnce owns one dispatched account lease. A turn that never invoked the
// image tool gets one bounded retry on that same lease before account state or
// cross-account scheduling is allowed to change.
func (r *Runner) runOnce(ctx context.Context, opt RunOptions, result *RunResult) (bool, string, error) {
	return r.runOnceWithExecutor(ctx, opt, result, r.runLeaseAttempt)
}

func (r *Runner) runOnceWithExecutor(
	ctx context.Context,
	opt RunOptions,
	result *RunResult,
	execute leaseAttemptExecutor,
) (bool, string, error) {
	// 1) 调度账号
	lease, err := r.sched.Dispatch(ctx, "image")
	if err != nil {
		if errors.Is(err, scheduler.ErrNoAvailable) {
			return false, ErrNoAccount, err
		}
		return false, ErrUnknown, err
	}
	defer func() {
		_ = lease.Release(context.Background())
	}()
	result.AccountID = lease.Account.ID
	// 立刻把 account_id 写回 image_tasks,供后续图片代理端点按 task_id 解出 AT。
	// MarkRunning 在 status=running 时 WHERE 不命中,所以用专门的 SetAccount。
	if r.dao != nil && opt.TaskID != "" {
		persistTaskWrite(opt.TaskID, "set_account", func(writeCtx context.Context) error {
			return r.dao.SetAccount(writeCtx, opt.TaskID, lease.Account.ID)
		})
	}

	credentialRecoveryAttempted := false
	missingToolRetryAttempted := false
	for {
		ok, status, runErr := execute(ctx, opt, result, lease)
		if ok {
			return true, status, runErr
		}
		if status == ErrAuthRequired &&
			!credentialRecoveryAttempted &&
			isPreImageAuthError(runErr) &&
			r.accountRecovery != nil {
			credentialRecoveryAttempted = true
			authToken, cookies, recovered, recoverErr := r.accountRecovery.RecoverImageCredential(
				ctx,
				lease.Account.ID,
				effectiveProxyURL(lease.ProxyURL),
				lease.Cookies,
			)
			if recoverErr != nil {
				return false, ErrNetworkTransient, fmt.Errorf("recover image credential: %w", recoverErr)
			}
			if !recovered {
				return false, status, runErr
			}
			if strings.TrimSpace(authToken) == "" {
				return false, ErrNetworkTransient, errors.New("recovered image credential is empty")
			}
			lease.AuthToken = authToken
			if strings.TrimSpace(cookies) != "" {
				lease.Cookies = cookies
			}
			logger.L().Info("image runner recovered credential on same lease",
				zap.String("task_id", opt.TaskID),
				zap.Uint64("account_id", lease.Account.ID))
			resetSameAccountAttemptResult(result)
			continue
		}
		if !missingToolRetryAttempted && shouldRetrySameAccountImageTurn(status, runErr) {
			missingToolRetryAttempted = true
			logger.L().Info("image runner retry missing tool on same account",
				zap.String("task_id", opt.TaskID),
				zap.Uint64("account_id", lease.Account.ID))
			resetSameAccountAttemptResult(result)
			continue
		}
		return false, status, runErr
	}
}

func shouldRetrySameAccountImageTurn(status string, err error) bool {
	if status != ErrImageTaskStalled || err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "tool_invoked=false")
}

func resetSameAccountAttemptResult(result *RunResult) {
	if result == nil {
		return
	}
	result.ConversationID = ""
	result.FileIDs = nil
	result.SignedURLs = nil
	result.ContentTypes = nil
	result.ErrorCode = ""
	result.ErrorMessage = ""
}

func (r *Runner) runLeaseAttempt(
	ctx context.Context,
	opt RunOptions,
	result *RunResult,
	lease *scheduler.Lease,
) (bool, string, error) {

	// 2) 构造上游 client
	proxyURL := effectiveProxyURL(lease.ProxyURL)
	if proxyURL == "" {
		r.sched.MarkWarned(context.Background(), lease.Account.ID)
		return false, ErrNetworkTransient, errors.New("image account has no proxy binding")
	}
	cli, err := chatgpt.New(chatGPTOptionsForLease(lease, proxyURL))
	if err != nil {
		return false, ErrUnknown, fmt.Errorf("chatgpt client: %w", err)
	}
	defer persistLeaseCookies(lease, cli)

	// 3) ChatRequirements + POW(新两步 sentinel 流程:prepare + 本地 solver + finalize)
	cr, err := cli.ChatRequirementsV2(ctx)
	if err != nil {
		code := r.classifyUpstream(err)
		if code == ErrAuthRequired {
			return false, code, newPreImageAuthError("chat_requirements", err)
		}
		return false, code, err
	}
	var proofToken string
	if cr.Proofofwork.Required {
		proofToken = cr.ProofToken
		if proofToken == "" {
			proofCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			ch := make(chan string, 1)
			go func() { ch <- cr.SolveProof(cli.UserAgent()) }()
			select {
			case <-proofCtx.Done():
				cancel()
				r.sched.MarkWarned(context.Background(), lease.Account.ID)
				return false, ErrPOWTimeout, proofCtx.Err()
			case proofToken = <-ch:
				cancel()
			}
		}
		if proofToken == "" {
			r.sched.MarkWarned(context.Background(), lease.Account.ID)
			return false, ErrPOWFailed, errors.New("pow solver returned empty")
		}
	}
	logger.L().Info("image runner chat requirements",
		zap.String("task_id", opt.TaskID),
		zap.Uint64("account_id", lease.Account.ID),
		zap.String("persona", cr.Persona),
		zap.Bool("turnstile_required", cr.Turnstile.Required),
		zap.Bool("pow_required", cr.Proofofwork.Required),
		zap.Bool("has_so_token", cr.SOToken != ""),
	)
	if cr.Turnstile.Required {
		logger.L().Info("image turnstile solved",
			zap.Uint64("account_id", lease.Account.ID),
			zap.Bool("has_turnstile_token", cr.TurnstileToken != ""),
			zap.Int("turnstile_token_len", len(cr.TurnstileToken)))
	}

	// 4) 不再调用 /backend-api/conversation/init:
	// 浏览器实测路径是 prepare → chat-requirements → f/conversation 三步,init 是
	// 过时/冗余调用,在免费账号上还会返回 404 让整条链路 fail。system_hints=picture_v2
	// 会通过 f/conversation 的 payload 字段传达。

	// 4.5) 图生图:上传参考图。任何一张失败都直接整体 fail(上游后续会对不上 attachment)。
	var refs []*chatgpt.UploadedFile
	if len(opt.References) > 0 {
		for idx, r0 := range opt.References {
			upCtx, cancel := context.WithTimeout(ctx, opt.ReferenceUploadTimeout)
			up, err := cli.UploadFile(upCtx, r0.Data, r0.FileName)
			cancel()
			if err != nil {
				logger.L().Warn("image runner upload reference failed",
					zap.Int("idx", idx), zap.Error(err))
				if ue, ok := err.(*chatgpt.UpstreamError); ok && ue.IsRateLimited() {
					r.sched.MarkRateLimited(context.Background(), lease.Account.ID)
					return false, ErrRateLimited, err
				}
				if r.classifyUpstream(err) == ErrAuthRequired {
					return false, ErrAuthRequired, newPreImageAuthError("reference_upload", err)
				}
				return false, ErrUpstream, fmt.Errorf("upload reference %d: %w", idx, err)
			}
			refs = append(refs, up)
		}
		logger.L().Info("image runner references uploaded",
			zap.String("task_id", opt.TaskID), zap.Int("count", len(refs)))
	}

	// 注意:新会话不要本地生成 conversation_id,上游会 404。
	// 真正的 conv_id 由 SSE 的 resume_conversation_token / sseResult.ConversationID 返回。
	var convID string
	parentID := uuid.NewString()
	messageID := uuid.NewString()

	// 5) 单轮 picture_v2:SSE 里直接给图就走 SSE 结果,没给就短轮询补一下。
	// IMG2 已正式上线,不再区分"终稿 / 预览",拿到就用,追求速度。
	convOpt := buildImageConversationOptions(
		opt,
		cr,
		proofToken,
		refs,
		parentID,
		messageID,
	)

	// Prepare(conduit_token;拿不到也能降级继续)
	if ct, err := cli.PrepareFConversation(ctx, convOpt); err == nil {
		convOpt.ConduitToken = ct
		logger.L().Info("image runner prepare ok",
			zap.String("task_id", opt.TaskID),
			zap.Uint64("account_id", lease.Account.ID),
			zap.Bool("has_conduit_token", ct != ""))
	} else if r.classifyUpstream(err) == ErrAuthRequired {
		return false, ErrAuthRequired, newPreImageAuthError("prepare", err)
	} else if ue, ok := err.(*chatgpt.UpstreamError); ok && ue.IsRateLimited() {
		r.sched.MarkRateLimited(context.Background(), lease.Account.ID)
		return false, ErrRateLimited, err
	} else if err != nil {
		logger.L().Warn("image runner prepare failed",
			zap.String("task_id", opt.TaskID),
			zap.Uint64("account_id", lease.Account.ID),
			zap.Error(err))
	}

	// f/conversation SSE
	streamCtx, cancelStream := context.WithCancel(ctx)
	stream, err := cli.StreamFConversation(streamCtx, convOpt)
	if err != nil {
		cancelStream()
		code := r.classifyUpstream(err)
		logger.L().Error("image runner StreamFConversation failed",
			zap.String("task_id", opt.TaskID),
			zap.Uint64("account_id", lease.Account.ID),
			zap.String("code", code),
			zap.Error(err),
		)
		if code == ErrRateLimited {
			r.sched.MarkRateLimited(context.Background(), lease.Account.ID)
		}
		return false, code, err
	}
	sseResult := parseImageSSEForAttempt(stream, opt.N, opt.SSEStartTimeout)
	cancelStream()
	if sseResult.ConversationID != "" {
		convID = sseResult.ConversationID
		result.ConversationID = convID
	}

	logger.L().Info("image runner SSE parsed",
		zap.String("task_id", opt.TaskID),
		zap.Uint64("account_id", lease.Account.ID),
		zap.String("conv_id", convID),
		zap.String("finish_type", sseResult.FinishType),
		zap.String("image_gen_task_id", sseResult.ImageGenTaskID),
		zap.Bool("tool_invoked", sseResult.ToolInvoked),
		zap.String("turn_use_case", sseResult.TurnUseCase),
		zap.Bool("saw_handoff", sseResult.SawHandoff),
		zap.Bool("pollable", sseResult.ShouldPollForImage()),
		zap.Bool("image_task_started", sseResult.HasStartedImageTask()),
		zap.Bool("sse_timed_out", sseResult.TimedOut),
		zap.String("sse_termination", sseResult.Termination),
		zap.String("sse_read_failure", string(sseResult.ReadFailure)),
		zap.Int("assistant_text_len", len(sseResult.AssistantText)),
		zap.Int("sse_fids", len(sseResult.FileIDs)),
		zap.Strings("sse_fids_list", sseResult.FileIDs),
		zap.Int("sse_sids", len(sseResult.SedimentIDs)),
		zap.Strings("sse_sids_list", sseResult.SedimentIDs),
	)

	// 聚合 SSE 阶段的所有引用:file-service 优先,sediment 补位
	var fileRefs []string
	fileRefs = append(fileRefs, sseResult.FileIDs...)
	for _, s := range sseResult.SedimentIDs {
		fileRefs = append(fileRefs, "sed:"+s)
	}

	if len(fileRefs) == 0 && !sseResult.ShouldPollForImage() {
		code, detail := classifySSEStartFailure(sseResult)
		logger.L().Warn("image runner SSE did not start image task",
			zap.String("task_id", opt.TaskID),
			zap.Uint64("account_id", lease.Account.ID),
			zap.String("conv_id", convID),
			zap.String("code", code),
			zap.String("detail", detail),
		)
		r.recordPollFailureAccountState(context.Background(), lease.Account.ID, code, detail, time.Now())
		return false, code, errors.New(detail)
	}

	// SSE 已经把期望数量的图带回来了 → 直接下载,跳过 Poll,省时间
	if len(fileRefs) >= opt.N {
		logger.L().Info("image runner enough refs from SSE, skip polling",
			zap.String("task_id", opt.TaskID),
			zap.Uint64("account_id", lease.Account.ID),
			zap.String("conv_id", convID),
			zap.Int("refs", len(fileRefs)),
			zap.Strings("refs_list", fileRefs),
		)
	} else {
		// SSE 没给够(常见于 IMG2 只走 tool 消息场景)→ 短轮询补齐。
		// 单轮新会话,不需要 baseline:conversation 里出现的每条 image_gen tool 消息
		// 都是本次请求的产物。
		pollOpt := chatgpt.PollOpts{
			ExpectedN:        opt.N,
			MaxWait:          opt.PollMaxWait,
			ImageTaskStarted: sseResult.HasStartedImageTask(),
		}
		status, fids, sids, dbg := cli.PollConversationForImages(ctx, convID, pollOpt)
		logger.L().Info("image runner poll done",
			zap.String("task_id", opt.TaskID),
			zap.Uint64("account_id", lease.Account.ID),
			zap.String("conv_id", convID),
			zap.String("poll_status", string(status)),
			zap.Int("poll_fids", len(fids)),
			zap.Strings("poll_fids_list", fids),
			zap.Int("poll_sids", len(sids)),
			zap.Strings("poll_sids_list", sids),
			zap.Int("poll_attempts", dbg.Attempts),
			zap.Int("poll_conversation_errors", dbg.ConversationErrors),
			zap.Int("poll_429", dbg.RateLimitHits),
			zap.Int("poll_tool_messages", dbg.ToolMessages),
			zap.Int("poll_new_tool_messages", dbg.NewToolMessages),
			zap.Int("poll_error_messages", dbg.ErrorMessages),
			zap.Int("poll_assistant_replies", dbg.AssistantReplies),
			zap.String("poll_last_tool_model", dbg.LastToolModelSlug),
			zap.String("poll_last_tool_recipient", dbg.LastToolRecipient),
			zap.String("poll_last_tool_author", dbg.LastToolAuthor),
			zap.String("poll_last_tool_title", dbg.LastToolTitle),
			zap.String("poll_async_error_type", dbg.AsyncErrorType),
			zap.String("poll_async_error_message_id", dbg.AsyncErrorMessageID),
			zap.String("poll_async_error_text", dbg.AsyncErrorText),
			zap.String("poll_assistant_reply_message_id", dbg.AssistantReplyMessageID),
			zap.String("poll_assistant_reply_text", dbg.AssistantReplyText),
			zap.String("poll_last_error", dbg.LastError),
		)
		switch status {
		case chatgpt.PollStatusSuccess:
			// 去重合并:SSE 捕获的 sediment 可能在 mapping 里再被 Poll 扫一次
			seen := make(map[string]struct{}, len(fileRefs))
			for _, r := range fileRefs {
				seen[r] = struct{}{}
			}
			for _, f := range fids {
				if _, ok := seen[f]; ok {
					continue
				}
				seen[f] = struct{}{}
				fileRefs = append(fileRefs, f)
			}
			for _, s := range sids {
				key := "sed:" + s
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				fileRefs = append(fileRefs, key)
			}
		case chatgpt.PollStatusRejected:
			detail := strings.TrimSpace(dbg.AsyncErrorText)
			if detail == "" {
				detail = strings.TrimSpace(dbg.AssistantReplyText)
			}
			if detail == "" {
				detail = "upstream image task rejected"
			}
			code := classifyPollFailureCode(sseResult, dbg, cr.Turnstile.Required, cr.IsFreeAccount())
			r.recordPollFailureAccountState(context.Background(), lease.Account.ID, code, detail, time.Now())
			return false, code, errors.New(detail)
		case chatgpt.PollStatusTimeout:
			detail := buildPollTimeoutMessage(sseResult, dbg, cr.Turnstile.Required, cr.IsFreeAccount())
			code := classifyPollFailureCode(sseResult, dbg, cr.Turnstile.Required, cr.IsFreeAccount())
			r.recordPollFailureAccountState(context.Background(), lease.Account.ID, code, detail, time.Now())
			return false, code, errors.New(detail)
		default:
			reason := "poll error"
			if dbg.LastError != "" {
				reason += ": " + dbg.LastError
			}
			if dbg.ConversationErrors > 0 {
				return false, ErrConversationPoll, errors.New(reason)
			}
			return false, ErrUpstream, errors.New(reason)
		}
	}

	if len(fileRefs) == 0 {
		return false, ErrUpstream, errors.New("no image ref produced")
	}

	// 6) 对每个 ref 取签名 URL,并立即拉取图片字节验证。
	// 只拿到 download_url 不能算成功:沉淀文件下载需要 AT + 同账号代理,
	// 如果这里不验证,调用方会拿到一个后续 /p/img 可能 502 的链接。
	downloads, err := collectVerifiedImageDownloads(ctx, cli, convID, fileRefs)
	if err != nil {
		return false, classifyImageDownloadFailure(err), err
	}
	keep := min(opt.N, len(downloads))

	logger.L().Info("image runner result summary",
		zap.String("task_id", opt.TaskID),
		zap.Uint64("account_id", lease.Account.ID),
		zap.Bool("proxy_bound", proxyURL != ""),
		zap.String("conv_id", convID),
		zap.Int("refs", min(opt.N, len(fileRefs))),
		zap.Strings("refs_list", fileRefs[:min(opt.N, len(fileRefs))]),
		zap.Int("signed_count", len(downloads)),
		zap.Int("verified_count", keep),
	)

	result.FileIDs = make([]string, 0, keep)
	result.SignedURLs = make([]string, 0, keep)
	result.ContentTypes = make([]string, 0, keep)
	for idx, d := range downloads[:keep] {
		result.FileIDs = append(result.FileIDs, d.Ref)
		result.SignedURLs = append(result.SignedURLs, d.URL)
		result.ContentTypes = append(result.ContentTypes, d.ContentType)
		if opt.TaskID != "" {
			PutVerifiedImage(opt.TaskID, idx, d.Ref, d.Data, d.ContentType)
		}
	}
	return true, "", nil
}

func chatGPTOptionsForLease(lease *scheduler.Lease, proxyURL string) chatgpt.Options {
	return chatgpt.Options{
		AuthToken:      lease.AuthToken,
		DeviceID:       lease.DeviceID,
		SessionID:      lease.SessionID,
		ProxyURL:       proxyURL,
		Cookies:        lease.Cookies,
		BrowserProfile: lease.BrowserProfile,
	}
}

func persistLeaseCookies(lease *scheduler.Lease, client *chatgpt.Client) {
	if lease == nil || client == nil {
		return
	}
	raw, err := client.CookieJSON()
	if err != nil || raw == "" || raw == "[]" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lease.PersistCookies(ctx, raw); err != nil {
		accountID := uint64(0)
		if lease.Account != nil {
			accountID = lease.Account.ID
		}
		logger.L().Warn("persist image account cookies failed",
			zap.Uint64("account_id", accountID), zap.Error(err))
	}
}

// classifyUpstream 把上游错误转成内部 error code。
func (r *Runner) classifyUpstream(err error) string {
	if err == nil {
		return ""
	}
	var ue *chatgpt.UpstreamError
	if errors.As(err, &ue) {
		if ue.IsRateLimited() {
			return ErrRateLimited
		}
		if ue.IsUnauthorized() {
			return ErrAuthRequired
		}
		if ue.IsForbidden() {
			return ErrUpstreamForbidden
		}
		return ErrUpstream
	}
	msg := err.Error()
	if strings.Contains(msg, "deadline exceeded") {
		return ErrPollTimeout
	}
	if isProxyConnectForbiddenError(err) {
		return ErrUpstreamForbidden
	}
	// uTLS 握手被对端强制关闭 (EOF / connection reset) 属于瞬态网络故障,允许重试。
	if isRetryableNetworkError(err) {
		return ErrNetworkTransient
	}
	return ErrUpstream
}

// GenerateTaskID 生成对外 task_id。
func GenerateTaskID() string {
	return "img_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
}
