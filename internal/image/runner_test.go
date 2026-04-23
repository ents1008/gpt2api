package image

import (
	"testing"
	"time"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

func TestNormalizeRunOptionsDefaultsUseFiveMinutePolling(t *testing.T) {
	got := normalizeRunOptions(RunOptions{})

	if got.MaxAttempts != 1 {
		t.Fatalf("MaxAttempts = %d, want 1", got.MaxAttempts)
	}
	if got.PerAttemptTimeout != 6*time.Minute {
		t.Fatalf("PerAttemptTimeout = %s, want 6m", got.PerAttemptTimeout)
	}
	if got.PollMaxWait != 5*time.Minute {
		t.Fatalf("PollMaxWait = %s, want 5m", got.PollMaxWait)
	}
	if got.UpstreamModel != "auto" {
		t.Fatalf("UpstreamModel = %q, want auto", got.UpstreamModel)
	}
	if got.N != 1 {
		t.Fatalf("N = %d, want 1", got.N)
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
			name: "generic image async error gets dedicated code",
			dbg: chatgpt.PollDebug{
				AsyncErrorType: "async_error",
				AsyncErrorText: "上游图片任务失败",
			},
			want: ErrImageAsyncError,
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
			name: "turnstile stalled task gets dedicated code",
			sse: chatgpt.ImageSSEResult{
				ImageGenTaskID: "task_123",
			},
			turnstileRequired: true,
			want:              ErrTurnstileStalled,
		},
		{
			name: "free account stalled task gets dedicated code",
			sse: chatgpt.ImageSSEResult{
				ImageGenTaskID: "task_123",
			},
			freeAccount: true,
			want:        ErrFreeAccountStalled,
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
