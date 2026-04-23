package gateway

import (
	"net/http"
	"strings"
	"testing"

	"github.com/432539/gpt2api/internal/image"
)

func TestImageAsyncErrorUsesClientErrorStatus(t *testing.T) {
	if got := imageErrorHTTPStatus(image.ErrImageAsyncError); got != http.StatusBadRequest {
		t.Fatalf("image async errors should be returned as client errors, got %d", got)
	}
}

func TestImageAsyncErrorMessageKeepsUpstreamDetail(t *testing.T) {
	raw := "非常抱歉，生成的图片可能违反了关于潜在欺诈或诈骗活动的防护限制。"
	got := localizeImageErr(image.ErrImageAsyncError, raw)
	if !strings.Contains(got, raw) {
		t.Fatalf("localized message should include upstream detail, got %q", got)
	}
}
