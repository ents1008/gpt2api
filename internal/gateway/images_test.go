package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/432539/gpt2api/internal/image"
	"github.com/gin-gonic/gin"
)

func TestImageAsyncErrorUsesClientErrorStatus(t *testing.T) {
	if got := imageErrorHTTPStatus(image.ErrImageAsyncError); got != http.StatusBadRequest {
		t.Fatalf("image async errors should be returned as client errors, got %d", got)
	}
}

// TestAuthRequiredImageErrorsUseServiceUnavailable verifies account-auth failures
// stay a structured JSON service error. Returning 502 here lets Cloudflare
// replace the body with an HTML bad-gateway page, so AstrBot loses the error
// code it needs for stable user-facing mapping.
func TestAuthRequiredImageErrorsUseServiceUnavailable(t *testing.T) {
	if got := imageErrorHTTPStatus(image.ErrAuthRequired); got != http.StatusServiceUnavailable {
		t.Fatalf("auth required image errors should be returned as service unavailable, got %d", got)
	}
}

func TestForbiddenImageErrorsHideTechnicalDetail(t *testing.T) {
	if got := imageErrorHTTPStatus(image.ErrUpstreamForbidden); got != http.StatusServiceUnavailable {
		t.Fatalf("forbidden image errors should be returned as service unavailable, got %d", got)
	}
	got := localizeImageErr(image.ErrUpstreamForbidden, "chatgpt upstream 403: forbidden")
	want := "图片服务状态异常,请稍后重试"
	if got != want {
		t.Fatalf("forbidden image message = %q, want %q", got, want)
	}
}

// TestImageGenerationMaxAttemptsPolicy verifies one user request has enough
// room to skip a short run of fast prepare 403 accounts without sweeping the
// whole pool. The outer request timeout still bounds slow upload/poll failures.
func TestImageGenerationMaxAttemptsPolicy(t *testing.T) {
	if got := imageGenerationMaxAttempts(0); got != 8 {
		t.Fatalf("text-to-image max attempts = %d, want 8", got)
	}
	if got := imageGenerationMaxAttempts(1); got != 8 {
		t.Fatalf("reference-image max attempts = %d, want 8", got)
	}
}

// TestImageRunTimeoutReturnsBeforeAstrBotClientTimeout verifies gpt2api returns
// a structured JSON error before AstrBot's 300 second HTTP read timeout can
// convert the failure into a client-side read timeout.
func TestImageRunTimeoutReturnsBeforeAstrBotClientTimeout(t *testing.T) {
	got := imageRunTimeout()
	if got >= 300*time.Second {
		t.Fatalf("image run timeout = %s, want below AstrBot 300s read timeout", got)
	}
	if got > 210*time.Second {
		t.Fatalf("image run timeout = %s, want bounded degraded-pool wait", got)
	}
	if got < 3*time.Minute {
		t.Fatalf("image run timeout = %s, want enough room for three account attempts", got)
	}
}

// TestImageRunContextFollowsRequestCancellation verifies image generation work
// stops when the HTTP client is gone. AstrBot has a much shorter timeout than
// the historical 8-minute backend ceiling; using a detached background context
// keeps burning account slots after the bot has already returned an error.
func TestImageRunContextFollowsRequestCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	runCtx, cancelRun := imageRunContext(parent, time.Minute)
	defer cancelRun()

	cancelParent()

	select {
	case <-runCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("image run context did not follow request cancellation")
	}
}

func TestImageAsyncErrorMessageKeepsUpstreamDetail(t *testing.T) {
	raw := "非常抱歉，生成的图片可能违反了关于潜在欺诈或诈骗活动的防护限制。"
	got := localizeImageErr(image.ErrImageAsyncError, raw)
	if !strings.Contains(got, raw) {
		t.Fatalf("localized message should include upstream detail, got %q", got)
	}
}

func TestImageRejectionDetailsPreservePublicText(t *testing.T) {
	cases := []struct {
		code string
		raw  string
		want string
	}{
		{
			code: image.ErrImageAsyncError,
			raw:  "上游拒绝: 该提示包含受限内容，请修改后重试。 当前账号",
			want: "该提示包含受限内容，请修改后重试。",
		},
	}
	for _, tc := range cases {
		got := localizeImageErr(tc.code, tc.raw)
		if !strings.Contains(got, tc.want) {
			t.Fatalf("message for %s should preserve public detail %q, got %q", tc.code, tc.want, got)
		}
	}
}

func TestContentPolicyRejectedMessageIsConcise(t *testing.T) {
	raw := "非常抱歉，该提示可能违反了我们的内容政策。如果你认为此判断有误，请重试或修改提示语。"
	got := localizeImageErr(image.ErrContentPolicyRejected, raw)
	want := "提示词可能违反内容安全策略,请修改后重试"
	if got != want {
		t.Fatalf("content policy message = %q, want %q", got, want)
	}
	if strings.Contains(got, raw) || strings.Contains(got, ":") {
		t.Fatalf("content policy message should not append verbose raw detail, got %q", got)
	}
}

func TestGuardrailsFraudScamRejectedMessageIsLocalized(t *testing.T) {
	raw := "We’re so sorry, but the image we created may violate our guardrails around potential fraudulent or scam activity. If you think we got it wrong, please retry or edit your prompt."
	got := localizeImageErr(image.ErrContentPolicyRejected, raw)
	want := "提示词可能违反内容安全策略,请修改后重试"
	if got != want {
		t.Fatalf("guardrails content policy message = %q, want %q", got, want)
	}

	got2 := localizeImageErr(image.ErrImageAsyncError, raw)
	if got2 != want {
		t.Fatalf("guardrails image async message = %q, want %q", got2, want)
	}
}

func TestContentPoliciesPluralRejectedMessageIsLocalized(t *testing.T) {
	raw := "We’re so sorry, but the prompt may violate our content policies. If you think we got it wrong, please retry or edit your prompt."
	got := localizeImageErr(image.ErrContentPolicyRejected, raw)
	want := "提示词可能违反内容安全策略,请修改后重试"
	if got != want {
		t.Fatalf("content policies message = %q, want %q", got, want)
	}

	got2 := localizeImageErr(image.ErrImageAsyncError, raw)
	if got2 != want {
		t.Fatalf("content policies image async message = %q, want %q", got2, want)
	}
}

func TestQuotaDepletedMessageIsInternalPoolFailure(t *testing.T) {
	got := localizeImageErr(image.ErrQuotaDepleted, "scheduler: known image quota depleted")
	want := "图片服务繁忙,请稍后重试"
	if got != want {
		t.Fatalf("quota depleted message = %q, want %q", got, want)
	}
}

func TestImageErrorMessagesDoNotExposeInternalProviderWording(t *testing.T) {
	codes := []string{
		image.ErrNoAccount,
		image.ErrQuotaDepleted,
		image.ErrRateLimited,
		image.ErrContentPolicyRejected,
		image.ErrImageAsyncError,
		image.ErrAssistantReply,
		image.ErrConversationPoll,
		image.ErrImageTaskStalled,
		image.ErrImageToolNoAsset,
		image.ErrTurnstileStalled,
		image.ErrFreeAccountStalled,
		image.ErrUnknown,
		image.ErrUpstream,
	}
	for _, code := range codes {
		got := localizeImageErr(code, "")
		for _, forbidden := range []string{"上游", "账号池", "免费账号"} {
			if strings.Contains(got, forbidden) {
				t.Fatalf("message for %s exposes %q: %q", code, forbidden, got)
			}
		}
	}
}

func TestUploadReferenceUpstreamErrorHidesPresignedURL(t *testing.T) {
	raw := `upload reference 0: upload PUT: Put "https://sdmntprukwest.oaiusercontent.com/files/x/raw?sig=secret": utls handshake sdmntprukwest.oaiusercontent.com: remote error: tls: unrecognized name`
	got := localizeImageErr(image.ErrUpstream, raw)
	if strings.Contains(got, "https://") || strings.Contains(got, "sdmntpr") || strings.Contains(got, "upload reference") {
		t.Fatalf("upload reference upstream error should hide internal URL/detail, got %q", got)
	}
	want := "图片服务网络波动,请重试"
	if got != want {
		t.Fatalf("upload reference upstream error message = %q, want %q", got, want)
	}
}

func TestChatEmptyReplyMessagesDoNotExposeInternalProviderWording(t *testing.T) {
	cases := []struct {
		freeAccount      bool
		silentlyRejected bool
	}{
		{freeAccount: false, silentlyRejected: false},
		{freeAccount: true, silentlyRejected: false},
		{freeAccount: false, silentlyRejected: true},
		{freeAccount: true, silentlyRejected: true},
	}
	for _, tc := range cases {
		got := emptyReplyMessage(tc.freeAccount, tc.silentlyRejected)
		for _, forbidden := range []string{"上游", "账号", "chatgpt-freeaccount"} {
			if strings.Contains(got, forbidden) {
				t.Fatalf("message for free=%v silent=%v exposes %q: %q",
					tc.freeAccount, tc.silentlyRejected, forbidden, got)
			}
		}
	}
}

func TestPublicOpenAIErrorCodeDoesNotExposeInternalProviderWording(t *testing.T) {
	cases := map[string]string{
		"upstream_error":       "service_error",
		"upstream_init_error":  "service_init_error",
		"upstream_timeout":     "service_timeout",
		"upstream_unavailable": "service_unavailable",
		"image_async_error":    "image_async_error",
	}
	for in, want := range cases {
		got := publicOpenAIErrorCode(in)
		if got != want {
			t.Fatalf("public code for %q = %q, want %q", in, got, want)
		}
		if strings.Contains(got, "upstream") {
			t.Fatalf("public code for %q exposes upstream wording: %q", in, got)
		}
	}
}

func TestImageRejectionDetailsDoNotExposeInternalProviderWording(t *testing.T) {
	cases := []struct {
		code string
		raw  string
	}{
		{
			code: image.ErrContentPolicyRejected,
			raw:  "上游拒绝: upstream content policy; chatgpt-freeaccount 当前账号",
		},
		{
			code: image.ErrImageAsyncError,
			raw:  "上游图片任务失败, 当前账号为 ChatGPT 免费版",
		},
		{
			code: image.ErrAssistantReply,
			raw:  "upstream image task rejected",
		},
	}
	for _, tc := range cases {
		got := localizeImageErr(tc.code, tc.raw)
		for _, forbidden := range []string{"上游", "upstream", "ChatGPT", "chatgpt", "账号池", "当前账号", "免费账号"} {
			if strings.Contains(got, forbidden) {
				t.Fatalf("message for %s exposes %q: %q", tc.code, forbidden, got)
			}
		}
	}
}

func TestAssistantReplyMessageKeepsPlainTextDetail(t *testing.T) {
	raw := "I can't create that specific image, but if you have any other requests or ideas, feel free to share them!"
	got := localizeImageErr(image.ErrAssistantReply, raw)
	want := "图像助手回复:" + raw
	if got != want {
		t.Fatalf("assistant reply message = %q, want %q", got, want)
	}
}

func TestAssistantReplyMessageHidesJSONRewriteDetail(t *testing.T) {
	raw := `{"prompt":"A cute orange kitten","size":"1024x1024"}`
	got := localizeImageErr(image.ErrAssistantReply, raw)
	want := "图片生成未启动,返回了文本回复"
	if got != want {
		t.Fatalf("assistant reply JSON message = %q, want %q", got, want)
	}
}

func TestAuthRequiredMessageHidesTechnicalDetail(t *testing.T) {
	raw := "chatgpt upstream 401: chat-requirements failed"
	got := localizeImageErr(image.ErrAuthRequired, raw)
	want := "图片服务授权状态异常,请稍后重试"
	if got != want {
		t.Fatalf("auth required message = %q, want %q", got, want)
	}
}

func TestTurnstileStalledMessageHidesTechnicalDetail(t *testing.T) {
	raw := "no_image_ref; turnstile_required; free_account; no_tool_messages; poll_attempts=88"
	got := localizeImageErr(image.ErrTurnstileStalled, raw)
	want := "图片服务验证卡住,请稍后重试"
	if got != want {
		t.Fatalf("turnstile stalled message = %q, want %q", got, want)
	}
}

func TestImageOpenAIErrorOmitsCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	imageOpenAIError(c, http.StatusBadRequest, image.ErrContentPolicyRejected, "提示词可能违反内容安全策略,请修改后重试")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var body struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if _, ok := body.Error["code"]; ok {
		t.Fatalf("image error response should omit code, got %v", body.Error["code"])
	}
	if got, _ := body.Error["message"].(string); got != "提示词可能违反内容安全策略,请修改后重试" {
		t.Fatalf("message = %q", got)
	}
}
