package chatgpt

import (
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
