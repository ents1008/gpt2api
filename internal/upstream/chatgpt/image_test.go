package chatgpt

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizePollOptsDefaultsUseFiveMinuteWait(t *testing.T) {
	got := normalizePollOpts(PollOpts{})

	if got.ExpectedN != 1 {
		t.Fatalf("ExpectedN = %d, want 1", got.ExpectedN)
	}
	if got.MaxWait != 5*time.Minute {
		t.Fatalf("MaxWait = %s, want 5m", got.MaxWait)
	}
	if got.Interval != 3*time.Second {
		t.Fatalf("Interval = %s, want 3s", got.Interval)
	}
}

func TestExtractImageErrorMsgsDetectsContentPolicyAsyncError(t *testing.T) {
	mapping := map[string]interface{}{
		"hidden": map[string]interface{}{
			"message": map[string]interface{}{
				"author": map[string]interface{}{"role": "assistant"},
				"content": map[string]interface{}{
					"content_type": "text",
					"parts":        []interface{}{"非常抱歉，该提示可能违反了我们的内容政策。如果你认为此判断有误，请重试或修改提示语。"},
				},
				"status":    "finished_successfully",
				"end_turn":  true,
				"recipient": "all",
				"metadata": map[string]interface{}{
					"is_visually_hidden_from_conversation": true,
				},
			},
		},
		"async_err": map[string]interface{}{
			"message": map[string]interface{}{
				"author": map[string]interface{}{"role": "assistant"},
				"content": map[string]interface{}{
					"content_type": "text",
					"parts":        []interface{}{"非常抱歉，该提示可能违反了我们的内容政策。如果你认为此判断有误，请重试或修改提示语。"},
				},
				"status":    "finished_successfully",
				"end_turn":  true,
				"recipient": "all",
				"metadata": map[string]interface{}{
					"async_task_type": "image_gen",
					"async_task_id":   "imagegen_123",
					"is_error":        true,
				},
			},
		},
	}

	msgs := ExtractImageErrorMsgs(mapping)
	if len(msgs) != 2 {
		t.Fatalf("len(ExtractImageErrorMsgs) = %d, want 2", len(msgs))
	}

	best := pickBestImageError(msgs)
	if best == nil {
		t.Fatal("pickBestImageError returned nil")
	}
	if best.AsyncTaskType != "image_gen" {
		t.Fatalf("best.AsyncTaskType = %q, want image_gen", best.AsyncTaskType)
	}
	if !best.IsError {
		t.Fatal("best.IsError = false, want true")
	}
	if best.ErrorType != "content_policy" {
		t.Fatalf("best.ErrorType = %q, want content_policy", best.ErrorType)
	}
	if !strings.Contains(best.Text, "内容政策") {
		t.Fatalf("best.Text = %q, want content policy refusal text", best.Text)
	}
}
