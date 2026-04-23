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
