package image

import (
	"testing"
	"time"
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
