package image

import (
	"testing"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

func TestShouldFallbackToLastPreview(t *testing.T) {
	t.Run("no preview means no fallback", func(t *testing.T) {
		if shouldFallbackToLastPreview(chatgpt.PollStatusError, nil, nil) {
			t.Fatal("expected false when there is no preview image")
		}
	})

	t.Run("poll error falls back when preview already exists", func(t *testing.T) {
		if !shouldFallbackToLastPreview(chatgpt.PollStatusError, []string{"file_1"}, nil) {
			t.Fatal("expected true for poll error with preview image")
		}
	})

	t.Run("poll timeout falls back when preview already exists", func(t *testing.T) {
		if !shouldFallbackToLastPreview(chatgpt.PollStatusTimeout, nil, []string{"sed_1"}) {
			t.Fatal("expected true for poll timeout with preview image")
		}
	})
}
