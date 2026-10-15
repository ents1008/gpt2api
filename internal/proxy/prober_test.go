package proxy

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateProbeErrorPreservesUTF8 verifies proxy probe error text is
// shortened without cutting a multi-byte rune. Expected: the string remains
// valid UTF-8 before it is written to MySQL last_error.
func TestTruncateProbeErrorPreservesUTF8(t *testing.T) {
	input := strings.Repeat("目标站返回异常状态码", 40)

	got := truncateProbeError(input, 200)

	if len(got) > 200 {
		t.Fatalf("truncated probe error bytes = %d, want <= 200", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated probe error is not valid UTF-8: %q", got)
	}
}
