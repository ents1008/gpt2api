package image

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/432539/gpt2api/internal/account"
	"github.com/432539/gpt2api/internal/scheduler"
	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

type recordingImageDownloader struct {
	downloadURLs map[string]string
	fetchErr     error
	fetchErrs    []error
	fetchedURLs  []string
}

// TestBuildImageConversationOptionsPropagatesFinalizedSentinelIdentity verifies
// the production Runner uses one options owner for prepare and stream. Expected:
// all finalized requirement tokens, including SO, enter the shared image turn.
func TestBuildImageConversationOptionsPropagatesFinalizedSentinelIdentity(t *testing.T) {
	requirements := &chatgpt.ChatRequirementsResp{
		Token:          "chat-token",
		ProofToken:     "proof-token",
		TurnstileToken: "turnstile-token",
		SOToken:        "so-token",
	}
	got := buildImageConversationOptions(
		RunOptions{Prompt: "prompt", UpstreamModel: "gpt-5-3"},
		requirements,
		"proof-token",
		nil,
		"parent-id",
		"message-id",
	)
	if got.ChatToken != "chat-token" || got.ProofToken != "proof-token" ||
		got.TurnstileToken != "turnstile-token" || got.SOToken != "so-token" {
		t.Fatalf("sentinel identity was not preserved: %+v", got)
	}
}

func (r *recordingImageDownloader) ImageDownloadURL(_ context.Context, _ string, ref string) (string, error) {
	if r.downloadURLs == nil {
		return "", errors.New("missing download url")
	}
	url, ok := r.downloadURLs[ref]
	if !ok {
		return "", errors.New("missing download url")
	}
	return url, nil
}

func (r *recordingImageDownloader) FetchImage(_ context.Context, url string, _ int64) ([]byte, string, error) {
	r.fetchedURLs = append(r.fetchedURLs, url)
	if len(r.fetchErrs) > 0 {
		err := r.fetchErrs[0]
		r.fetchErrs = r.fetchErrs[1:]
		if err != nil {
			return nil, "", err
		}
		return []byte("png-bytes"), "image/png", nil
	}
	if r.fetchErr != nil {
		return nil, "", r.fetchErr
	}
	return []byte("png-bytes"), "image/png", nil
}

// TestCollectVerifiedImageDownloadsFailsWhenFetchedBytesAreUnavailable verifies
// a signed download URL is not enough to mark image generation successful. The
// runner must fetch real image bytes first, and every failed byte fetch must
// fail the verified-download step so callers never receive a dead /p/img URL.
func TestCollectVerifiedImageDownloadsFailsWhenFetchedBytesAreUnavailable(t *testing.T) {
	d := &recordingImageDownloader{
		downloadURLs: map[string]string{
			"sed:file_1": "https://example.invalid/file_1",
		},
		fetchErr: errors.New("chatgpt upstream 403: files/download failed"),
	}

	downloads, err := collectVerifiedImageDownloads(context.Background(), d, "conv_1", []string{"sed:file_1"})

	if err == nil {
		t.Fatal("collectVerifiedImageDownloads should fail when every signed URL cannot be fetched")
	}
	if !errors.Is(err, errNoVerifiedImageDownloads) {
		t.Fatalf("error = %v, want errNoVerifiedImageDownloads", err)
	}
	if len(downloads) != 0 {
		t.Fatalf("downloads = %d, want 0", len(downloads))
	}
	if len(d.fetchedURLs) != 1 {
		t.Fatalf("FetchImage calls = %d, want 1", len(d.fetchedURLs))
	}
}

// TestCollectVerifiedImageDownloadsRetriesUnexpectedEOF verifies a transient
// body read failure from a signed image URL is retried before the already
// generated image is reported as failed to the bot.
func TestCollectVerifiedImageDownloadsRetriesUnexpectedEOF(t *testing.T) {
	d := &recordingImageDownloader{
		downloadURLs: map[string]string{
			"sed:file_1": "https://example.invalid/file_1",
		},
		fetchErrs: []error{io.ErrUnexpectedEOF, nil},
	}

	downloads, err := collectVerifiedImageDownloads(context.Background(), d, "conv_1", []string{"sed:file_1"})

	if err != nil {
		t.Fatalf("collectVerifiedImageDownloads error = %v, want nil", err)
	}
	if len(downloads) != 1 {
		t.Fatalf("downloads = %d, want 1", len(downloads))
	}
	if len(d.fetchedURLs) != 2 {
		t.Fatalf("FetchImage calls = %d, want 2", len(d.fetchedURLs))
	}
}

// TestCollectVerifiedImageDownloadsDoesNotRetryForbiddenFetch verifies
// authorization failures from the signed image URL are not retried as network
// transients, preserving the real account/proxy failure signal.
func TestCollectVerifiedImageDownloadsDoesNotRetryForbiddenFetch(t *testing.T) {
	d := &recordingImageDownloader{
		downloadURLs: map[string]string{
			"sed:file_1": "https://example.invalid/file_1",
		},
		fetchErr: &chatgpt.UpstreamError{Status: 403, Message: "fetch image failed"},
	}

	downloads, err := collectVerifiedImageDownloads(context.Background(), d, "conv_1", []string{"sed:file_1"})

	if err == nil {
		t.Fatal("collectVerifiedImageDownloads should fail on forbidden fetch")
	}
	if !errors.Is(err, errNoVerifiedImageDownloads) {
		t.Fatalf("error = %v, want errNoVerifiedImageDownloads", err)
	}
	if len(downloads) != 0 {
		t.Fatalf("downloads = %d, want 0", len(downloads))
	}
	if len(d.fetchedURLs) != 1 {
		t.Fatalf("FetchImage calls = %d, want 1", len(d.fetchedURLs))
	}
}

// TestCollectVerifiedImageDownloadsPreservesUpstreamStatus verifies an
// attachment URL failure keeps its original upstream status through the
// verified-download wrapper. The runner needs that status to distinguish a
// revoked token from a transient image download failure.
func TestCollectVerifiedImageDownloadsPreservesUpstreamStatus(t *testing.T) {
	upstreamErr := &chatgpt.UpstreamError{
		Status:  401,
		Message: "files/download failed",
		Body:    `{"error":{"code":"token_revoked"}}`,
	}
	d := &recordingImageDownloader{
		downloadURLs: map[string]string{"sed:file_1": "https://example.invalid/file_1"},
		fetchErr:     upstreamErr,
	}

	_, err := collectVerifiedImageDownloads(context.Background(), d, "conv_1", []string{"sed:file_1"})
	if err == nil {
		t.Fatal("collectVerifiedImageDownloads should fail")
	}
	if !errors.Is(err, errNoVerifiedImageDownloads) {
		t.Fatalf("error = %v, want errNoVerifiedImageDownloads", err)
	}
	var got *chatgpt.UpstreamError
	if !errors.As(err, &got) {
		t.Fatal("verified download error lost the upstream status")
	}
	if got.Status != 401 || got.Body != upstreamErr.Body {
		t.Fatalf("upstream error = %#v, want status 401 and original body", got)
	}
}

// TestClassifyImageDownloadFailureSeparatesAuthAndForbidden verifies image
// result retrieval uses the same 401/403 account state contract as prepare and
// SSE requests instead of collapsing both into download_failed.
func TestClassifyImageDownloadFailureSeparatesAuthAndForbidden(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "revoked token",
			err:  &chatgpt.UpstreamError{Status: 401, Message: "files/download failed"},
			want: ErrAuthRequired,
		},
		{
			name: "upstream forbidden",
			err:  &chatgpt.UpstreamError{Status: 403, Message: "files/download failed"},
			want: ErrUpstreamForbidden,
		},
		{
			name: "network failure",
			err:  errors.New("connection reset by peer"),
			want: ErrDownload,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyImageDownloadFailure(tt.err); got != tt.want {
				t.Fatalf("classifyImageDownloadFailure() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNormalizeRunOptionsDefaultsBoundOneBadAccount verifies one bad account
// cannot consume most of the gateway request budget before the retry path gets
// a chance to use a healthier account.
func TestNormalizeRunOptionsDefaultsBoundOneBadAccount(t *testing.T) {
	got := normalizeRunOptions(RunOptions{})

	if got.MaxAttempts != 1 {
		t.Fatalf("MaxAttempts = %d, want 1", got.MaxAttempts)
	}
	if got.PerAttemptTimeout != 2*time.Minute {
		t.Fatalf("PerAttemptTimeout = %s, want 2m", got.PerAttemptTimeout)
	}
	if got.PollMaxWait != 2*time.Minute {
		t.Fatalf("PollMaxWait = %s, want 2m", got.PollMaxWait)
	}
	if got.ReferenceUploadTimeout != 120*time.Second {
		t.Fatalf("ReferenceUploadTimeout = %s, want 120s", got.ReferenceUploadTimeout)
	}
	if got.SSEStartTimeout != 45*time.Second {
		t.Fatalf("SSEStartTimeout = %s, want 45s", got.SSEStartTimeout)
	}
	if got.UpstreamModel != "auto" {
		t.Fatalf("UpstreamModel = %q, want auto", got.UpstreamModel)
	}
	if got.N != 1 {
		t.Fatalf("N = %d, want 1", got.N)
	}
}

// TestApplyContextErrorToResultPreservesConcreteAttemptFailure verifies the
// outer request timeout cannot erase the account-level image failure that was
// already observed. When the last attempted account reached SSE but never
// returned an image task, callers need the stable image_task_stalled code rather
// than a generic context deadline.
func TestApplyContextErrorToResultPreservesConcreteAttemptFailure(t *testing.T) {
	result := &RunResult{
		Status:       StatusFailed,
		ErrorCode:    ErrImageTaskStalled,
		ErrorMessage: "sse_no_image_task; sse_timed_out; tool_invoked=false",
	}

	applyContextErrorToResult(result, context.DeadlineExceeded)

	if result.ErrorCode != ErrImageTaskStalled {
		t.Fatalf("ErrorCode = %q, want %q", result.ErrorCode, ErrImageTaskStalled)
	}
	if result.ErrorMessage != "sse_no_image_task; sse_timed_out; tool_invoked=false" {
		t.Fatalf("ErrorMessage = %q, want concrete attempt failure", result.ErrorMessage)
	}
}

// TestApplyContextErrorToResultUsesUnknownWithoutPriorFailure verifies a
// request canceled before any concrete image attempt still reports the context
// failure as unknown because there is no better account-level signal.
func TestApplyContextErrorToResultUsesUnknownWithoutPriorFailure(t *testing.T) {
	result := &RunResult{Status: StatusFailed}

	applyContextErrorToResult(result, context.DeadlineExceeded)

	if result.ErrorCode != ErrUnknown {
		t.Fatalf("ErrorCode = %q, want %q", result.ErrorCode, ErrUnknown)
	}
	if result.ErrorMessage != context.DeadlineExceeded.Error() {
		t.Fatalf("ErrorMessage = %q, want context deadline", result.ErrorMessage)
	}
}

func TestClassifyUpstreamDistinguishes401And403(t *testing.T) {
	r := &Runner{}

	if got := r.classifyUpstream(&chatgpt.UpstreamError{Status: 401, Message: "unauthorized"}); got != ErrAuthRequired {
		t.Fatalf("401 classifyUpstream = %q, want %q", got, ErrAuthRequired)
	}
	if got := r.classifyUpstream(&chatgpt.UpstreamError{Status: 403, Message: "forbidden"}); got != ErrUpstreamForbidden {
		t.Fatalf("403 classifyUpstream = %q, want %q", got, ErrUpstreamForbidden)
	}
}

// TestClassifyUpstreamTreatsProxyConnect403AsForbidden verifies proxy-layer
// CONNECT rejection is treated as an account/proxy path failure. Expected: the
// runner marks the account suspicious and retries another account instead of
// leaving the bad proxy-bound account healthy.
func TestClassifyUpstreamTreatsProxyConnect403AsForbidden(t *testing.T) {
	r := &Runner{}
	err := errors.New(`Post "https://chatgpt.com/backend-api/sentinel/chat-requirements": proxy CONNECT chatgpt.com:443 → 403 Forbidden`)

	if got := r.classifyUpstream(err); got != ErrUpstreamForbidden {
		t.Fatalf("proxy CONNECT 403 classifyUpstream = %q, want %q", got, ErrUpstreamForbidden)
	}
	reason, ok := retryableAccountFailureReason(ErrUpstreamForbidden, err)
	if !ok {
		t.Fatal("proxy CONNECT 403 should be retryable")
	}
	if reason != "proxy_connect_403" {
		t.Fatalf("proxy CONNECT 403 reason = %q, want proxy_connect_403", reason)
	}
}

func TestRunnerMarksForbiddenAsSuspiciousNotDead(t *testing.T) {
	sched := &recordingScheduler{}
	r := &Runner{sched: sched}

	r.markAccountFailureState(context.Background(), "task_403", 1, 42,
		ErrUpstreamForbidden, &chatgpt.UpstreamError{Status: 403, Message: "forbidden"})

	if sched.deadAccountID != 0 {
		t.Fatalf("403 should not mark account dead, got dead account %d", sched.deadAccountID)
	}
	if sched.suspiciousAccountID != 42 {
		t.Fatalf("403 suspicious account = %d, want 42", sched.suspiciousAccountID)
	}
	if sched.suspiciousReason != "upstream_403" {
		t.Fatalf("403 suspicious reason = %q, want upstream_403", sched.suspiciousReason)
	}
}

func TestPrepareForbiddenUsesShortSuspiciousReason(t *testing.T) {
	err := &chatgpt.UpstreamError{Status: 403, Message: "chat-requirements/prepare failed"}

	reason, ok := retryableAccountFailureReason(ErrUpstreamForbidden, err)
	if !ok {
		t.Fatal("prepare 403 should be retryable")
	}
	if reason != "prepare_403" {
		t.Fatalf("prepare 403 reason = %q, want prepare_403", reason)
	}
}

func TestRunnerMarks401AuthRequiredAsDeadWithReason(t *testing.T) {
	sched := &recordingScheduler{}
	r := &Runner{sched: sched}

	r.markAccountFailureState(context.Background(), "task_401", 1, 43, ErrAuthRequired, nil)

	if sched.deadAccountID != 43 {
		t.Fatalf("401 dead account = %d, want 43", sched.deadAccountID)
	}
	if sched.deadReason != "auth_required_401" {
		t.Fatalf("401 dead reason = %q, want auth_required_401", sched.deadReason)
	}
	if sched.suspiciousAccountID != 0 {
		t.Fatalf("401 should not mark suspicious, got account %d", sched.suspiciousAccountID)
	}
}

// TestRunnerRestoresSuccessfulAccountToHealthy verifies a real successful image
// run clears a previous suspicious/warned account state. Without this, accounts
// that recover after a temporary 403 or upload timeout stay in the low-priority
// retry pool forever even after they have produced a valid image.
func TestRunnerRestoresSuccessfulAccountToHealthy(t *testing.T) {
	sched := &recordingScheduler{}
	r := &Runner{sched: sched}

	r.markAccountSuccessState(context.Background(), 59)

	if sched.restoredAccountID != 59 {
		t.Fatalf("restored account = %d, want 59", sched.restoredAccountID)
	}
}

// TestChatGPTOptionsForLeaseIncludesPersistedCookies verifies that image
// generation uses the account's persisted ChatGPT browser cookies when they
// exist. Expected: the client receives AuthToken, stable device/session ids,
// proxy, and cookie JSON from the scheduler lease.
func TestChatGPTOptionsForLeaseIncludesPersistedCookies(t *testing.T) {
	lease := &scheduler.Lease{
		AuthToken:      "at",
		DeviceID:       "device-123",
		SessionID:      "session-456",
		Cookies:        `[{"name":"oai-did","value":"device-123","domain":".chatgpt.com"}]`,
		BrowserProfile: `{"version":1,"headers":{"Accept-Language":"en-US,en;q=0.9","Sec-Ch-Ua":"\"Chromium\";v=\"146\", \"Google Chrome\";v=\"146\"","Sec-Ch-Ua-Mobile":"?0","Sec-Ch-Ua-Platform":"\"Windows\"","User-Agent":"Mozilla/5.0 Chrome/146.0.0.0 Safari/537.36"},"language":"en-US","timezone":"America/Los_Angeles"}`,
	}

	opts := chatGPTOptionsForLease(lease, "http://127.0.0.1:2260")

	if opts.AuthToken != "at" {
		t.Fatalf("AuthToken = %q, want at", opts.AuthToken)
	}
	if opts.DeviceID != "device-123" {
		t.Fatalf("DeviceID = %q, want device-123", opts.DeviceID)
	}
	if opts.SessionID != "session-456" {
		t.Fatalf("SessionID = %q, want session-456", opts.SessionID)
	}
	if opts.ProxyURL != "http://127.0.0.1:2260" {
		t.Fatalf("ProxyURL = %q, want bound proxy", opts.ProxyURL)
	}
	if opts.Cookies != lease.Cookies {
		t.Fatalf("Cookies = %q, want persisted cookie JSON", opts.Cookies)
	}
	if opts.BrowserProfile != lease.BrowserProfile {
		t.Fatalf("BrowserProfile = %q, want persisted account profile", opts.BrowserProfile)
	}
}

func TestParseImageSSEForAttemptReturnsOnPollableSignal(t *testing.T) {
	stream := make(chan chatgpt.SSEEvent, 4)
	stream <- chatgpt.SSEEvent{Data: []byte(`{"type":"resume_conversation_token","conversation_id":"conv_runner_poll"}`)}
	stream <- chatgpt.SSEEvent{Data: []byte(`{"type":"server_ste_metadata","conversation_id":"conv_runner_poll","metadata":{"tool_invoked":true,"turn_use_case":"image gen"}}`)}

	done := make(chan chatgpt.ImageSSEResult, 1)
	go func() {
		done <- parseImageSSEForAttempt(stream, 1, 0)
	}()

	select {
	case got := <-done:
		if got.ConversationID != "conv_runner_poll" {
			t.Fatalf("ConversationID = %q, want conv_runner_poll", got.ConversationID)
		}
		if !got.ShouldPollForImage() {
			t.Fatal("ShouldPollForImage() = false, want true")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("parseImageSSEForAttempt did not return after pollable image metadata")
	}
}

// TestClassifySSEStartFailureRetriesNonPollableNoTask verifies the production
// shape where SSE exposes no image task/ref/pollable signal is treated as a
// retryable account startup failure, not a long conversation poll.
func TestClassifySSEStartFailureRetriesNonPollableNoTask(t *testing.T) {
	code, detail := classifySSEStartFailure(chatgpt.ImageSSEResult{
		ConversationID: "conv_no_task",
		TimedOut:       true,
	})

	if code != ErrImageTaskStalled {
		t.Fatalf("classifySSEStartFailure code = %q, want %q", code, ErrImageTaskStalled)
	}
	if !contains(detail, "sse_no_image_task") {
		t.Fatalf("classifySSEStartFailure detail = %q, want sse_no_image_task marker", detail)
	}
	if !contains(detail, "sse_timed_out") {
		t.Fatalf("classifySSEStartFailure detail = %q, want sse_timed_out marker", detail)
	}
	if retryable := shouldRetryRunAttempt(code, errors.New(detail)); !retryable {
		t.Fatal("non-pollable SSE start failure should retry another account")
	}
}

// TestClassifySSEStartFailureKeepsAssistantTextTerminal verifies assistant text
// returned before any image task remains a terminal request error, preventing a
// policy/refusal response from burning additional accounts.
func TestClassifySSEStartFailureKeepsAssistantTextTerminal(t *testing.T) {
	code, detail := classifySSEStartFailure(chatgpt.ImageSSEResult{
		ConversationID: "conv_text",
		AssistantText:  "I can't create that image.",
	})

	if code != ErrAssistantReply {
		t.Fatalf("classifySSEStartFailure code = %q, want %q", code, ErrAssistantReply)
	}
	if detail != "I can't create that image." {
		t.Fatalf("classifySSEStartFailure detail = %q, want assistant text", detail)
	}
	if retryable := shouldRetryRunAttempt(code, errors.New(detail)); retryable {
		t.Fatal("plain assistant text should not retry another account")
	}
}

// TestClassifySSEStartFailureRetriesConfirmedTechnicalTerminal verifies the
// observed upstream image failure text is treated as an account-local async
// failure. Expected: it returns immediately and remains eligible for another
// account attempt instead of becoming a non-retryable generic assistant reply.
func TestClassifySSEStartFailureRetriesConfirmedTechnicalTerminal(t *testing.T) {
	text := "由于我这边发生了错误，我未能生成图片。"
	code, detail := classifySSEStartFailure(chatgpt.ImageSSEResult{
		ConversationID: "conv_technical_terminal",
		TurnUseCase:    "image gen",
		AssistantText:  text,
	})

	if code != ErrImageAsyncError {
		t.Fatalf("classifySSEStartFailure code = %q, want %q", code, ErrImageAsyncError)
	}
	if detail != text {
		t.Fatalf("classifySSEStartFailure detail = %q, want exact upstream text", detail)
	}
	if retryable := shouldRetryRunAttempt(code, errors.New(detail)); !retryable {
		t.Fatal("confirmed technical terminal should retry another account")
	}
}

// TestResolveUpstreamImageModelPreservesExplicitModel protects the configured
// image model from account-tier rewrites. A free account must send the same
// explicit upstream slug selected by the model configuration; only an empty or
// explicit auto value may resolve to auto.
func TestResolveUpstreamImageModelPreservesExplicitModel(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		want      string
	}{
		{
			name:      "explicit gpt-5-4 is preserved",
			requested: "gpt-5-4",
			want:      "gpt-5-4",
		},
		{
			name:      "explicit gpt-5-3 is preserved",
			requested: "gpt-5-3",
			want:      "gpt-5-3",
		},
		{
			name:      "explicit gpt-5.6-luna is preserved",
			requested: "gpt-5.6-luna",
			want:      "gpt-5.6-luna",
		},
		{
			name:      "empty requested model stays auto",
			requested: "",
			want:      "auto",
		},
		{
			name:      "explicit auto stays auto",
			requested: "auto",
			want:      "auto",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveUpstreamImageModel(tt.requested)
			if got != tt.want {
				t.Fatalf("resolveUpstreamImageModel(%q) = %q, want %q",
					tt.requested, got, tt.want)
			}
		})
	}
}

func TestBuildPollTimeoutMessageIncludesStageSignals(t *testing.T) {
	msg := buildPollTimeoutMessage(
		chatgpt.ImageSSEResult{
			ImageGenTaskID: "task_123",
			FinishType:     "stop",
		},
		chatgpt.PollDebug{
			Attempts:           17,
			ConversationErrors: 2,
			ToolMessages:       0,
			NewToolMessages:    0,
			LastError:          "chatgpt upstream 500: boom",
		},
		true,
		true,
	)

	for _, want := range []string{
		"no_image_ref",
		"turnstile_required",
		"free_account",
		"image_gen_task_started",
		"finish_type=stop",
		"no_tool_messages",
		"poll_attempts=17",
		"conversation_errors=2",
		"last_error=chatgpt upstream 500: boom",
	} {
		if !contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
}

func TestComposeStoredTaskErrorIncludesDetail(t *testing.T) {
	got := composeStoredTaskError(ErrPollTimeout, "no_image_ref; turnstile_required")
	want := "poll_timeout: no_image_ref; turnstile_required"
	if got != want {
		t.Fatalf("composeStoredTaskError = %q, want %q", got, want)
	}
}

func TestClassifyPollFailureCode(t *testing.T) {
	tests := []struct {
		name              string
		sse               chatgpt.ImageSSEResult
		dbg               chatgpt.PollDebug
		turnstileRequired bool
		freeAccount       bool
		want              string
	}{
		{
			name: "content policy async rejection gets dedicated code",
			dbg: chatgpt.PollDebug{
				AsyncErrorType: "content_policy",
				AsyncErrorText: "非常抱歉，该提示可能违反了我们的内容政策。",
			},
			want: ErrContentPolicyRejected,
		},
		{
			name: "guardrails fraud and scam activity rejection is content policy rejection",
			dbg: chatgpt.PollDebug{
				AsyncErrorType: "async_error",
				AsyncErrorText: "We’re so sorry, but the image we created may violate our guardrails around potential fraudulent or scam activity. If you think we got it wrong, please retry or edit your prompt.",
			},
			want: ErrContentPolicyRejected,
		},
		{
			name: "guardrails rejection in assistant reply text is content policy rejection",
			dbg: chatgpt.PollDebug{
				AssistantReplyText: "We’re so sorry, but the image we created may violate our guardrails around potential fraudulent or scam activity. If you think we got it wrong, please retry or edit your prompt.",
			},
			want: ErrContentPolicyRejected,
		},
		{
			name: "content policies rejection in async error text is content policy rejection",
			dbg: chatgpt.PollDebug{
				AsyncErrorType: "async_error",
				AsyncErrorText: "We’re so sorry, but the prompt may violate our content policies. If you think we got it wrong, please retry or edit your prompt.",
			},
			want: ErrContentPolicyRejected,
		},
		{
			name: "generic image async error gets dedicated code",
			dbg: chatgpt.PollDebug{
				AsyncErrorType: "async_error",
				AsyncErrorText: "上游图片任务失败",
			},
			want: ErrImageAsyncError,
		},
		{
			name: "plain assistant text reply gets dedicated code",
			dbg: chatgpt.PollDebug{
				AssistantReplyText: "I can't create that image.",
			},
			want: ErrAssistantReply,
		},
		{
			name: "confirmed technical assistant reply is retryable async failure",
			dbg: chatgpt.PollDebug{
				AssistantReplyText: "由于我这边发生了错误，我未能生成图片。",
			},
			want: ErrImageAsyncError,
		},
		{
			name: "free plan limit assistant reply is account quota depletion",
			dbg: chatgpt.PollDebug{
				AssistantReplyText: "You've hit the free plan limit for image generation requests. You can create more images when the limit resets in 15 minutes.",
			},
			freeAccount: true,
			want:        ErrQuotaDepleted,
		},
		{
			name: "chinese free plan limit assistant reply is account quota depletion",
			dbg: chatgpt.PollDebug{
				AssistantReplyText: "你已达到 Free 套餐的图像生成请求上限。上限将在 24小时 后重置，届时可创建更多图像。",
			},
			freeAccount: true,
			want:        ErrQuotaDepleted,
		},
		{
			name: "assistant prompt json after started image task is not a terminal assistant reply",
			sse: chatgpt.ImageSSEResult{
				ImageGenTaskID: "task_123",
			},
			dbg: chatgpt.PollDebug{
				AssistantReplyText: `{"prompt":"A poster","size":"1024x1024"}`,
			},
			want: ErrImageTaskStalled,
		},
		{
			name: "assistant refusal after started image task also waits for structured result",
			sse: chatgpt.ImageSSEResult{
				ImageGenTaskID: "task_123",
			},
			dbg: chatgpt.PollDebug{
				AssistantReplyText: "I can't create that image.",
			},
			want: ErrImageTaskStalled,
		},
		{
			name: "conversation poll error wins",
			dbg: chatgpt.PollDebug{
				ConversationErrors: 2,
				NewToolMessages:    0,
			},
			want: ErrConversationPoll,
		},
		{
			name: "tool message without asset gets dedicated code",
			dbg: chatgpt.PollDebug{
				NewToolMessages: 1,
			},
			want: ErrImageToolNoAsset,
		},
		{
			name: "started image task wins over stale turnstile and free account signals",
			sse: chatgpt.ImageSSEResult{
				ImageGenTaskID: "task_123",
			},
			turnstileRequired: true,
			freeAccount:       true,
			want:              ErrImageTaskStalled,
		},
		{
			name:              "turnstile blocks before image task id gets dedicated code",
			turnstileRequired: true,
			want:              ErrTurnstileStalled,
		},
		{
			name: "started image task wins over stale free account signal",
			sse: chatgpt.ImageSSEResult{
				ImageGenTaskID: "task_123",
			},
			freeAccount: true,
			want:        ErrImageTaskStalled,
		},
		{
			name: "started task without richer signal is generic stalled task",
			sse: chatgpt.ImageSSEResult{
				ImageGenTaskID: "task_123",
			},
			want: ErrImageTaskStalled,
		},
		{
			name: "plain timeout remains poll timeout",
			want: ErrPollTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyPollFailureCode(tt.sse, tt.dbg, tt.turnstileRequired, tt.freeAccount)
			if got != tt.want {
				t.Fatalf("classifyPollFailureCode = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMiniImageLimitResponseEntersQuotaDispatchPath reproduces the exact
// production response emitted by the Mini model after an account exhausts its
// instant image quota. The response must be classified as account quota
// depletion in both poll and SSE terminal paths, recorded against that account,
// cooled out of dispatch, and allowed to retry with another account.
func TestMiniImageLimitResponseEntersQuotaDispatchPath(t *testing.T) {
	const message = "It looks like image creation is temporarily unavailable. Do you want to try something else?\n\n" +
		"The image creation tool is currently unavailable because the instant image-generation limit has been reached, " +
		"and this Mini model is text-only right now. I can't generate or edit the image in this turn."

	pollCode := classifyPollFailureCode(
		chatgpt.ImageSSEResult{},
		chatgpt.PollDebug{AssistantReplyText: message},
		true,
		true,
	)
	if pollCode != ErrQuotaDepleted {
		t.Fatalf("poll classification = %q, want %q", pollCode, ErrQuotaDepleted)
	}

	sseCode, sseDetail := classifySSEStartFailure(chatgpt.ImageSSEResult{AssistantText: message})
	if sseCode != ErrQuotaDepleted {
		t.Fatalf("SSE classification = %q, want %q", sseCode, ErrQuotaDepleted)
	}
	if sseDetail != message {
		t.Fatalf("SSE detail changed: got %q", sseDetail)
	}

	recorder := &recordingQuotaRecorder{}
	sched := &recordingScheduler{}
	runner := &Runner{sched: sched, quotaDecr: recorder}
	now := time.Date(2026, 8, 7, 16, 36, 14, 0, time.FixedZone("HKT", 8*60*60))
	runner.recordPollFailureAccountState(context.Background(), 1536, pollCode, message, now)

	if recorder.accountID != 1536 || recorder.remaining != 0 {
		t.Fatalf("quota record = account %d remaining %d, want account 1536 remaining 0", recorder.accountID, recorder.remaining)
	}
	if sched.rateLimitedAccountID != 1536 || sched.rateLimitedFor != quotaDepletedCooldown {
		t.Fatalf("scheduler cooldown = account %d for %s, want account 1536 for %s",
			sched.rateLimitedAccountID, sched.rateLimitedFor, quotaDepletedCooldown)
	}
	if !shouldRetryRunAttempt(pollCode, errors.New(message)) {
		t.Fatal("quota-depleted account must retry the request with another account")
	}
}

// TestChineseFreePlanLimitResponseEntersQuotaDispatchPath reproduces the exact
// production response emitted by ChatGPT Free accounts when exhausting their daily image quota:
// "你已达到 Free 套餐的图像生成请求上限。上限将在 24小时 后重置，届时可创建更多图像。"
func TestChineseFreePlanLimitResponseEntersQuotaDispatchPath(t *testing.T) {
	testCases := []struct {
		name         string
		message      string
		wantDuration time.Duration
	}{
		{
			name:         "24 hours reset",
			message:      "你已达到 Free 套餐的图像生成请求上限。上限将在 24小时 后重置，届时可创建更多图像。",
			wantDuration: 24 * time.Hour,
		},
		{
			name:         "12 hours reset",
			message:      "你已达到 Free 套餐的图像生成请求上限。上限将在 12小时 后重置，届时可创建更多图像。",
			wantDuration: 12 * time.Hour,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pollCode := classifyPollFailureCode(
				chatgpt.ImageSSEResult{},
				chatgpt.PollDebug{AssistantReplyText: tc.message},
				true,
				true,
			)
			if pollCode != ErrQuotaDepleted {
				t.Fatalf("poll classification = %q, want %q", pollCode, ErrQuotaDepleted)
			}

			sseCode, sseDetail := classifySSEStartFailure(chatgpt.ImageSSEResult{AssistantText: tc.message})
			if sseCode != ErrQuotaDepleted {
				t.Fatalf("SSE classification = %q, want %q", sseCode, ErrQuotaDepleted)
			}
			if sseDetail != tc.message {
				t.Fatalf("SSE detail changed: got %q", sseDetail)
			}

			retryAfter, ok := extractImageQuotaRetryAfter(tc.message)
			if !ok || retryAfter != tc.wantDuration {
				t.Fatalf("extractImageQuotaRetryAfter() = (%s, %v), want (%s, true)", retryAfter, ok, tc.wantDuration)
			}

			recorder := &recordingQuotaRecorder{}
			sched := &recordingScheduler{}
			runner := &Runner{sched: sched, quotaDecr: recorder}
			now := time.Date(2026, 9, 4, 9, 25, 15, 0, time.FixedZone("HKT", 8*60*60))
			runner.recordPollFailureAccountState(context.Background(), 1498, pollCode, tc.message, now)

			if recorder.accountID != 1498 || recorder.remaining != 0 {
				t.Fatalf("quota record = account %d remaining %d, want account 1498 remaining 0", recorder.accountID, recorder.remaining)
			}
			if sched.rateLimitedAccountID != 1498 || sched.rateLimitedFor != tc.wantDuration {
				t.Fatalf("scheduler cooldown = account %d for %s, want account 1498 for %s",
					sched.rateLimitedAccountID, sched.rateLimitedFor, tc.wantDuration)
			}
			if !shouldRetryRunAttempt(pollCode, errors.New(tc.message)) {
				t.Fatal("quota-depleted account must retry the request with another account")
			}
		})
	}
}

// TestTemporaryImageUnavailableResponseRetriesAnotherAccount reproduces the
// shorter production response that omits the quota explanation. Because this
// sentence is an account-local temporary image failure rather than a definitive
// quota signal, it must use the short suspicious cooldown and retry another
// account without recording zero quota.
func TestTemporaryImageUnavailableResponseRetriesAnotherAccount(t *testing.T) {
	const message = "It looks like image creation is temporarily unavailable. Do you want to try something else?"

	code := classifyPollFailureCode(
		chatgpt.ImageSSEResult{},
		chatgpt.PollDebug{AssistantReplyText: message},
		true,
		true,
	)
	if code != ErrImageAsyncError {
		t.Fatalf("poll classification = %q, want %q", code, ErrImageAsyncError)
	}

	sched := &recordingScheduler{}
	runner := &Runner{sched: sched}
	runner.markAccountFailureState(context.Background(), "img_short_unavailable", 1, 1536, code, errors.New(message))
	if sched.suspiciousAccountID != 1536 || sched.suspiciousReason != "image_async_transient" {
		t.Fatalf("suspicious state = account %d reason %q, want account 1536 reason image_async_transient",
			sched.suspiciousAccountID, sched.suspiciousReason)
	}
	if sched.rateLimitedAccountID != 0 {
		t.Fatalf("temporary unavailable response must not record quota depletion, got account %d", sched.rateLimitedAccountID)
	}
	if !shouldRetryRunAttempt(code, errors.New(message)) {
		t.Fatal("temporary unavailable response must retry with another account")
	}
}

func TestExtractImageQuotaRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		text string
		want time.Duration
		ok   bool
	}{
		{
			name: "minutes only",
			text: "You've hit the free plan limit for image generation requests. You can create more images when the limit resets in 15 minutes.",
			want: 15 * time.Minute,
			ok:   true,
		},
		{
			name: "hours and minutes",
			text: "You've hit the free plan limit for image generation requests. You can create more images when the limit resets in 4 hours and 15 minutes.",
			want: 4*time.Hour + 15*time.Minute,
			ok:   true,
		},
		{
			name: "not a limit message",
			text: "I can't create that image.",
			want: 0,
			ok:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractImageQuotaRetryAfter(tt.text)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("extractImageQuotaRetryAfter() = (%s, %v), want (%s, %v)",
					got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestShouldRetryRunAttempt(t *testing.T) {
	tests := []struct {
		name   string
		status string
		err    error
		want   bool
	}{
		{
			name:   "rate limit retries",
			status: ErrRateLimited,
			want:   true,
		},
		{
			name:   "known quota depleted retries with another account",
			status: ErrQuotaDepleted,
			want:   true,
		},
		{
			name:   "no account stops after scheduler queue is exhausted",
			status: ErrNoAccount,
			want:   false,
		},
		{
			name:   "auth required retries",
			status: ErrAuthRequired,
			want:   true,
		},
		{
			name:   "upstream forbidden retries with another account",
			status: ErrUpstreamForbidden,
			want:   true,
		},
		{
			name:   "network transient retries",
			status: ErrNetworkTransient,
			want:   true,
		},
		{
			name:   "download failure retries with another account",
			status: ErrDownload,
			err:    errors.New("no verified image downloads: chatgpt upstream 403: files/download failed"),
			want:   true,
		},
		{
			name:   "poll timeout retries with another account",
			status: ErrPollTimeout,
			err:    errors.New("no_image_ref; poll_attempts=40"),
			want:   true,
		},
		{
			name:   "started image task stalled retries with another account",
			status: ErrImageTaskStalled,
			err:    errors.New("no_image_ref; image_gen_task_started"),
			want:   true,
		},
		{
			name:   "turnstile stalled retries with another account",
			status: ErrTurnstileStalled,
			err:    errors.New("no_image_ref; turnstile_required"),
			want:   true,
		},
		{
			name:   "free account stalled retries with another account",
			status: ErrFreeAccountStalled,
			err:    errors.New("no_image_ref; free_account"),
			want:   true,
		},
		{
			name:   "conversation poll errors retry with another account",
			status: ErrConversationPoll,
			err:    errors.New("conversation get failed"),
			want:   true,
		},
		{
			name:   "image tool without asset retries with another account",
			status: ErrImageToolNoAsset,
			err:    errors.New("tool message returned without image asset"),
			want:   true,
		},
		{
			name:   "reference upload EOF wrapped as upstream retries",
			status: ErrUpstream,
			err:    errors.New(`upload reference 0: upload PUT: Put "https://sdmntprnortheu.oaiusercontent.com/files/x/raw": EOF`),
			want:   true,
		},
		{
			name:   "reference upload proxy gateway timeout wrapped as upstream retries",
			status: ErrUpstream,
			err:    errors.New(`upload reference 0: upload PUT: Put "https://sdmntprnortheu.oaiusercontent.com/files/x/raw": proxy CONNECT sdmntprnortheu.oaiusercontent.com:443 -> 504 Gateway Timeout`),
			want:   true,
		},
		{
			name:   "reference upload tls sni failure wrapped as upstream retries",
			status: ErrUpstream,
			err:    errors.New(`upload reference 0: upload PUT: Put "https://sdmntprukwest.oaiusercontent.com/files/x/raw": utls handshake sdmntprukwest.oaiusercontent.com: remote error: tls: unrecognized name`),
			want:   true,
		},
		{
			name:   "reference upload closed connection wrapped as upstream retries",
			status: ErrUpstream,
			err:    errors.New(`upload reference 0: upload PUT: Put "https://sdmntprnortheu.oaiusercontent.com/files/x/raw": use of closed network connection`),
			want:   true,
		},
		{
			name:   "reference upload put failed wrapped as upstream retries",
			status: ErrUpstream,
			err:    errors.New("upload reference 0: chatgpt upstream 400: upload PUT failed"),
			want:   true,
		},
		{
			name:   "generic upstream async image failure retries",
			status: ErrImageAsyncError,
			err:    errors.New("We experienced an error when generating images."),
			want:   true,
		},
		{
			name:   "generic upstream async image failure retries case-insensitively",
			status: ErrImageAsyncError,
			err:    errors.New("we experienced an error when generating images"),
			want:   true,
		},
		{
			name:   "content policy async failure does not retry",
			status: ErrImageAsyncError,
			err:    errors.New("非常抱歉，该提示可能违反了我们的内容政策。"),
			want:   false,
		},
		{
			name:   "fraud-style async failure does not retry",
			status: ErrImageAsyncError,
			err:    errors.New("非常抱歉，生成的图片可能违反了关于潜在欺诈或诈骗活动的防护限制。"),
			want:   false,
		},
		{
			name:   "assistant reply does not retry",
			status: ErrAssistantReply,
			err:    errors.New("I can't create that image."),
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldRetryRunAttempt(tt.status, tt.err)
			if got != tt.want {
				t.Fatalf("shouldRetryRunAttempt(%q, %v) = %v, want %v", tt.status, tt.err, got, tt.want)
			}
		})
	}
}

// TestRunnerMarksRetryableShortFailuresSuspicious verifies account-local
// transient image failures enter scheduler cooldown before the request retries.
// These are not user prompt failures; they identify a bad account/proxy/runtime
// path and should not be selected again immediately.
func TestRunnerMarksRetryableShortFailuresSuspicious(t *testing.T) {
	tests := []struct {
		name   string
		status string
		err    error
		reason string
	}{
		{
			name:   "download failed",
			status: ErrDownload,
			err:    errors.New("no verified image downloads: chatgpt upstream 403: files/download failed"),
			reason: "download_failed",
		},
		{
			name:   "poll timeout",
			status: ErrPollTimeout,
			err:    errors.New("no_image_ref; poll_attempts=40"),
			reason: "poll_timeout",
		},
		{
			name:   "started image task stalled",
			status: ErrImageTaskStalled,
			err:    errors.New("no_image_ref; image_gen_task_started"),
			reason: "image_task_stalled",
		},
		{
			name:   "turnstile stalled",
			status: ErrTurnstileStalled,
			err:    errors.New("no_image_ref; turnstile_required"),
			reason: "turnstile_stalled",
		},
		{
			name:   "upstream upload timeout",
			status: ErrUpstream,
			err:    errors.New(`upload reference 0: upload PUT: Put "https://sdmntprnortheu.oaiusercontent.com/files/x/raw": context deadline exceeded`),
			reason: "upstream_network_transient",
		},
		{
			name:   "upstream upload tls sni failure",
			status: ErrUpstream,
			err:    errors.New(`upload reference 0: upload PUT: Put "https://sdmntprukwest.oaiusercontent.com/files/x/raw": utls handshake sdmntprukwest.oaiusercontent.com: remote error: tls: unrecognized name`),
			reason: "upstream_network_transient",
		},
		{
			name:   "upstream upload closed connection",
			status: ErrUpstream,
			err:    errors.New(`upload reference 0: upload PUT: Put "https://sdmntprnortheu.oaiusercontent.com/files/x/raw": use of closed network connection`),
			reason: "upstream_network_transient",
		},
		{
			name:   "upstream upload put failed",
			status: ErrUpstream,
			err:    errors.New("upload reference 0: chatgpt upstream 400: upload PUT failed"),
			reason: "upstream_network_transient",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sched := &recordingScheduler{}
			r := &Runner{sched: sched}

			r.markAccountFailureState(context.Background(), "task_short_failure", 2, 77, tt.status, tt.err)

			if sched.suspiciousAccountID != 77 {
				t.Fatalf("suspicious account = %d, want 77", sched.suspiciousAccountID)
			}
			if sched.suspiciousReason != tt.reason {
				t.Fatalf("suspicious reason = %q, want %q", sched.suspiciousReason, tt.reason)
			}
			if sched.deadAccountID != 0 {
				t.Fatalf("short failure should not mark dead, got account %d", sched.deadAccountID)
			}
		})
	}
}

// TestRunOnceRetriesMissingToolOnTheSameLease verifies a transient SSE result
// with no image-tool invocation does not immediately release or downgrade the
// selected account. Expected trajectory: one dispatch, first turn reports
// tool_invoked=false, attempt-local state is cleared, and a second turn on the
// same account succeeds before the outer cross-account state machine runs.
func TestRunOnceRetriesMissingToolOnTheSameLease(t *testing.T) {
	sched := &recordingScheduler{
		lease: &scheduler.Lease{
			Account:  &account.Account{ID: 77},
			ProxyURL: "http://bound-proxy:8080",
		},
	}
	runner := &Runner{sched: sched}
	result := &RunResult{}
	turns := 0

	ok, status, err := runner.runOnceWithExecutor(
		context.Background(),
		RunOptions{TaskID: "task_same_account_retry"},
		result,
		func(_ context.Context, _ RunOptions, got *RunResult, lease *scheduler.Lease) (bool, string, error) {
			turns++
			if lease.Account.ID != 77 {
				t.Fatalf("lease account = %d, want 77", lease.Account.ID)
			}
			switch turns {
			case 1:
				got.ConversationID = "stale-conversation"
				got.FileIDs = []string{"stale-file"}
				return false, ErrImageTaskStalled, errors.New("sse_no_image_task; tool_invoked=false")
			case 2:
				if got.ConversationID != "" || len(got.FileIDs) != 0 {
					t.Fatalf("second turn inherited stale result: %#v", got)
				}
				got.ConversationID = "successful-conversation"
				got.FileIDs = []string{"file-1"}
				return true, "", nil
			default:
				t.Fatalf("turns = %d, want at most 2", turns)
				return false, ErrUnknown, errors.New("unexpected extra turn")
			}
		},
	)

	if !ok || status != "" || err != nil {
		t.Fatalf("runOnceWithExecutor = (%v, %q, %v), want success", ok, status, err)
	}
	if turns != 2 {
		t.Fatalf("turns = %d, want 2", turns)
	}
	if sched.dispatchCount != 1 {
		t.Fatalf("dispatches = %d, want 1", sched.dispatchCount)
	}
	if result.AccountID != 77 || result.ConversationID != "successful-conversation" {
		t.Fatalf("result = %#v, want account 77 successful second turn", result)
	}
	if sched.suspiciousAccountID != 0 {
		t.Fatalf("first missing-tool turn must not mark suspicious, got %d", sched.suspiciousAccountID)
	}
}

// TestRunOnceDoesNotRetryAStartedImageTask verifies the same-account retry is
// limited to a turn that never invoked the image tool. Expected: a stalled task
// that already started returns after one execution so duplicate images are not
// generated for an in-flight upstream task.
func TestRunOnceDoesNotRetryAStartedImageTask(t *testing.T) {
	sched := &recordingScheduler{
		lease: &scheduler.Lease{
			Account:  &account.Account{ID: 78},
			ProxyURL: "http://bound-proxy:8080",
		},
	}
	runner := &Runner{sched: sched}
	turns := 0

	ok, status, err := runner.runOnceWithExecutor(
		context.Background(),
		RunOptions{TaskID: "task_started_image"},
		&RunResult{},
		func(context.Context, RunOptions, *RunResult, *scheduler.Lease) (bool, string, error) {
			turns++
			return false, ErrImageTaskStalled, errors.New("no_image_ref; image_gen_task_started")
		},
	)

	if ok || status != ErrImageTaskStalled || err == nil {
		t.Fatalf("runOnceWithExecutor = (%v, %q, %v), want started-task failure", ok, status, err)
	}
	if turns != 1 || sched.dispatchCount != 1 {
		t.Fatalf("turns=%d dispatches=%d, want 1 and 1", turns, sched.dispatchCount)
	}
}

// TestRunOnceRecoversPreImage401OnTheSameLease verifies the recovery point is
// inside the existing account lease. Expected trajectory: one dispatch, one
// recovery using the same account/proxy/cookies, replacement credentials on
// the second execution, and no cross-account retry.
func TestRunOnceRecoversPreImage401OnTheSameLease(t *testing.T) {
	sched := &recordingScheduler{
		lease: &scheduler.Lease{
			Account:   &account.Account{ID: 79},
			AuthToken: "old-at",
			ProxyURL:  "http://bound-proxy:8080",
			Cookies:   "old-cookies",
		},
	}
	recovery := &recordingAccountRecovery{
		authToken: "new-at",
		cookies:   "new-cookies",
		recovered: true,
	}
	runner := &Runner{sched: sched, accountRecovery: recovery}
	turns := 0

	ok, status, err := runner.runOnceWithExecutor(
		context.Background(),
		RunOptions{TaskID: "task_auth_recovery"},
		&RunResult{},
		func(_ context.Context, _ RunOptions, _ *RunResult, lease *scheduler.Lease) (bool, string, error) {
			turns++
			if lease.Account.ID != 79 || lease.ProxyURL != "http://bound-proxy:8080" {
				t.Fatalf("lease identity changed: %+v", lease)
			}
			if turns == 1 {
				return false, ErrAuthRequired, newPreImageAuthError(
					"prepare",
					&chatgpt.UpstreamError{Status: 401, Message: "token invalidated"},
				)
			}
			if lease.AuthToken != "new-at" || lease.Cookies != "new-cookies" {
				t.Fatalf("second turn credentials = (%q, %q)", lease.AuthToken, lease.Cookies)
			}
			return true, "", nil
		},
	)

	if !ok || status != "" || err != nil {
		t.Fatalf("runOnceWithExecutor = (%v, %q, %v), want success", ok, status, err)
	}
	if turns != 2 || sched.dispatchCount != 1 || recovery.calls != 1 {
		t.Fatalf("turns=%d dispatches=%d recoveries=%d", turns, sched.dispatchCount, recovery.calls)
	}
	if recovery.accountID != 79 || recovery.proxyURL != "http://bound-proxy:8080" || recovery.cookiesIn != "old-cookies" {
		t.Fatalf("recovery identity = %+v", recovery)
	}
}

// TestRunOnceAttemptsPreImage401RecoveryOnlyOnce prevents an invalid token
// family from creating a refresh loop. Expected: the second 401 returns after
// one recovery, one lease, and two executions.
func TestRunOnceAttemptsPreImage401RecoveryOnlyOnce(t *testing.T) {
	sched := &recordingScheduler{
		lease: &scheduler.Lease{
			Account:   &account.Account{ID: 80},
			AuthToken: "old-at",
			ProxyURL:  "http://bound-proxy:8080",
		},
	}
	recovery := &recordingAccountRecovery{authToken: "new-at", recovered: true}
	runner := &Runner{sched: sched, accountRecovery: recovery}
	turns := 0

	ok, status, err := runner.runOnceWithExecutor(
		context.Background(),
		RunOptions{TaskID: "task_auth_recovery_once"},
		&RunResult{},
		func(context.Context, RunOptions, *RunResult, *scheduler.Lease) (bool, string, error) {
			turns++
			return false, ErrAuthRequired, newPreImageAuthError(
				"prepare",
				&chatgpt.UpstreamError{Status: 401, Message: "token invalidated"},
			)
		},
	)

	if ok || status != ErrAuthRequired || err == nil {
		t.Fatalf("runOnceWithExecutor = (%v, %q, %v), want final auth failure", ok, status, err)
	}
	if turns != 2 || sched.dispatchCount != 1 || recovery.calls != 1 {
		t.Fatalf("turns=%d dispatches=%d recoveries=%d", turns, sched.dispatchCount, recovery.calls)
	}
}

func TestMarkQuotaDepletedFromMessage(t *testing.T) {
	recorder := &recordingQuotaRecorder{}
	sched := &recordingScheduler{}
	r := &Runner{sched: sched, quotaDecr: recorder}
	now := time.Date(2026, 5, 28, 21, 43, 0, 0, time.UTC)

	r.markQuotaDepletedFromMessage(
		context.Background(),
		37,
		"You've hit the free plan limit for image generations requests. You can create more images when the limit resets in 23 hours and 27 minutes.",
		now,
	)

	if recorder.accountID != 37 {
		t.Fatalf("accountID = %d, want 37", recorder.accountID)
	}
	if recorder.remaining != 0 {
		t.Fatalf("remaining = %d, want 0", recorder.remaining)
	}
	if recorder.total != -1 {
		t.Fatalf("total = %d, want -1 to preserve existing capacity", recorder.total)
	}
	wantCooldown := 23*time.Hour + 27*time.Minute
	wantReset := now.Add(wantCooldown)
	if recorder.resetAt == nil || !recorder.resetAt.Equal(wantReset) {
		t.Fatalf("resetAt = %v, want %v", recorder.resetAt, wantReset)
	}
	if sched.rateLimitedAccountID != 37 {
		t.Fatalf("rate limited account = %d, want 37", sched.rateLimitedAccountID)
	}
	if sched.rateLimitedFor != wantCooldown {
		t.Fatalf("quota cooldown = %s, want %s", sched.rateLimitedFor, wantCooldown)
	}
}

// TestRecordPollFailureAccountStateMarksQuotaOnTimeout verifies the production
// log shape where ChatGPT returns a free-plan-limit assistant reply during
// polling, but the poll loop exits as timeout rather than rejected. That signal
// is still definitive account quota exhaustion and must cool the account before
// the next dispatch.
func TestRecordPollFailureAccountStateMarksQuotaOnTimeout(t *testing.T) {
	recorder := &recordingQuotaRecorder{}
	sched := &recordingScheduler{}
	r := &Runner{sched: sched, quotaDecr: recorder}
	now := time.Date(2026, 6, 25, 9, 41, 43, 0, time.UTC)
	dbg := chatgpt.PollDebug{
		AssistantReplyText: "You've hit the Free plan limit for image generations requests. You can create more images when the limit resets in 12 hours and 30 minutes.",
	}
	code := classifyPollFailureCode(chatgpt.ImageSSEResult{}, dbg, true, true)
	detail := buildPollTimeoutMessage(chatgpt.ImageSSEResult{}, dbg, true, true)

	r.recordPollFailureAccountState(context.Background(), 1116, code, detail, now)

	if recorder.accountID != 1116 {
		t.Fatalf("quota depleted account = %d, want 1116", recorder.accountID)
	}
	if recorder.remaining != 0 {
		t.Fatalf("remaining = %d, want 0", recorder.remaining)
	}
	if sched.rateLimitedAccountID != 1116 {
		t.Fatalf("rate limited account = %d, want 1116", sched.rateLimitedAccountID)
	}
	wantCooldown := 12*time.Hour + 30*time.Minute
	if sched.rateLimitedFor != wantCooldown {
		t.Fatalf("quota cooldown = %s, want %s", sched.rateLimitedFor, wantCooldown)
	}
}

type recordingQuotaRecorder struct {
	accountID uint64
	remaining int
	total     int
	resetAt   *time.Time
}

func (r *recordingQuotaRecorder) DecrQuota(_ context.Context, _ uint64, _ int) error {
	return nil
}

func (r *recordingQuotaRecorder) MarkQuotaDepleted(_ context.Context, accountID uint64, resetAt *time.Time) error {
	r.accountID = accountID
	r.remaining = 0
	r.total = -1
	if resetAt != nil {
		v := *resetAt
		r.resetAt = &v
	}
	return nil
}

type recordingScheduler struct {
	rateLimitedAccountID uint64
	rateLimitedFor       time.Duration
	deadAccountID        uint64
	deadReason           string
	suspiciousAccountID  uint64
	suspiciousReason     string
	restoredAccountID    uint64
	lease                *scheduler.Lease
	dispatchCount        int
}

type recordingAccountRecovery struct {
	accountID uint64
	proxyURL  string
	cookiesIn string
	authToken string
	cookies   string
	recovered bool
	err       error
	calls     int
}

func (r *recordingAccountRecovery) RecoverImageCredential(
	_ context.Context,
	accountID uint64,
	proxyURL string,
	cookies string,
) (string, string, bool, error) {
	r.calls++
	r.accountID = accountID
	r.proxyURL = proxyURL
	r.cookiesIn = cookies
	return r.authToken, r.cookies, r.recovered, r.err
}

func (r *recordingScheduler) Dispatch(context.Context, string) (*scheduler.Lease, error) {
	r.dispatchCount++
	if r.lease != nil {
		return r.lease, nil
	}
	return nil, scheduler.ErrNoAvailable
}

func (r *recordingScheduler) MarkRateLimited(context.Context, uint64) {}

func (r *recordingScheduler) MarkRateLimitedFor(_ context.Context, accountID uint64, cooldownFor time.Duration) {
	r.rateLimitedAccountID = accountID
	r.rateLimitedFor = cooldownFor
}

func (r *recordingScheduler) MarkWarned(context.Context, uint64) {}

func (r *recordingScheduler) MarkDead(_ context.Context, accountID uint64) {
	r.deadAccountID = accountID
}

func (r *recordingScheduler) MarkDeadWithReason(_ context.Context, accountID uint64, reason string) {
	r.deadAccountID = accountID
	r.deadReason = reason
}

func (r *recordingScheduler) MarkSuspicious(_ context.Context, accountID uint64, reason string) {
	r.suspiciousAccountID = accountID
	r.suspiciousReason = reason
}

func (r *recordingScheduler) RestoreHealthy(_ context.Context, accountID uint64) {
	r.restoredAccountID = accountID
}

func TestEffectiveProxyURL(t *testing.T) {
	t.Run("uses dispatch proxy when present", func(t *testing.T) {
		t.Setenv("GPT2API_FORCE_PROXY_URL", "http://127.0.0.1:10810")
		got := effectiveProxyURL("http://bound-proxy:8080")
		if got != "http://bound-proxy:8080" {
			t.Fatalf("effectiveProxyURL = %q, want dispatch proxy", got)
		}
	})

	t.Run("falls back to forced proxy when dispatch proxy missing", func(t *testing.T) {
		t.Setenv("GPT2API_FORCE_PROXY_URL", "http://127.0.0.1:10810")
		got := effectiveProxyURL("")
		if got != "http://127.0.0.1:10810" {
			t.Fatalf("effectiveProxyURL = %q, want forced proxy", got)
		}
	})

	t.Run("empty when neither proxy exists", func(t *testing.T) {
		t.Setenv("GPT2API_FORCE_PROXY_URL", "")
		got := effectiveProxyURL("")
		if got != "" {
			t.Fatalf("effectiveProxyURL = %q, want empty", got)
		}
	})
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && stringIndex(s, sub) >= 0)
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
