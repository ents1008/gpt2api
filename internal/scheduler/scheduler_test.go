package scheduler

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/432539/gpt2api/internal/account"
	"github.com/432539/gpt2api/internal/config"
)

// TestLeasePersistsCookieSnapshotBeforeRelease defines cookie-session ownership
// at the account lease boundary. Expected: the caller can commit one canonical
// snapshot while it still owns the account lock, before Release marks use and
// makes the account available to another request.
func TestLeasePersistsCookieSnapshotBeforeRelease(t *testing.T) {
	var events []string
	lease := &Lease{
		persistCookiesFunc: func(_ context.Context, raw string) error {
			events = append(events, "persist:"+raw)
			return nil
		},
		releaseFunc: func(context.Context) error {
			events = append(events, "release")
			return nil
		},
	}

	if err := lease.PersistCookies(context.Background(), `[{"name":"oai-did"}]`); err != nil {
		t.Fatalf("PersistCookies() error = %v", err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if len(events) != 2 || events[0] != `persist:[{"name":"oai-did"}]` || events[1] != "release" {
		t.Fatalf("events = %#v, want persist before release", events)
	}
}

// TestImageDispatchRequiresBoundProxy verifies image generation never leaves
// the scheduler with an account that has no usable proxy URL. The expected
// path is to skip that lease and keep searching, while non-image dispatches
// keep their existing direct/no-proxy behavior.
func TestImageDispatchRequiresBoundProxy(t *testing.T) {
	if !shouldSkipLeaseForMissingProxy("image", "") {
		t.Fatal("image dispatch should skip leases without a proxy URL")
	}
	if shouldSkipLeaseForMissingProxy("image", "http://127.0.0.1:2260") {
		t.Fatal("image dispatch should keep leases with a proxy URL")
	}
	if shouldSkipLeaseForMissingProxy("chat", "") {
		t.Fatal("non-image dispatch should not require a proxy URL")
	}
}

// TestMissingProxyImageAccountPauseDuration verifies an image account without a
// proxy binding is removed from near-term image scheduling. Releasing the lock
// and immediately continuing would re-scan the same broken account on every
// request, producing noisy retries without any chance of success.
func TestMissingProxyImageAccountPauseDuration(t *testing.T) {
	pause := missingProxyBindingPauseUntil(time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC))

	want := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	if !pause.Equal(want) {
		t.Fatalf("missing proxy pause = %s, want %s", pause, want)
	}
}

func TestImageQuotaProbeStateNeverBlocksDispatch(t *testing.T) {
	now := time.Date(2026, 5, 3, 15, 0, 0, 0, time.Local)

	tests := []struct {
		name string
		acc  account.Account
		want bool
	}{
		{
			name: "unknown quota is dispatchable",
			acc: account.Account{
				ImageQuotaRemaining: -1,
			},
			want: false,
		},
		{
			name: "positive remaining quota is dispatchable",
			acc: account.Account{
				ImageQuotaRemaining: 2,
				ImageQuotaResetAt:   nullableTime(now.Add(2 * time.Hour)),
				ImageQuotaUpdatedAt: nullableTime(now.Add(-10 * time.Minute)),
			},
			want: false,
		},
		{
			name: "zero remaining with future reset still dispatches to verify by real generation",
			acc: account.Account{
				ImageQuotaRemaining: 0,
				ImageQuotaResetAt:   nullableTime(now.Add(2 * time.Hour)),
				ImageQuotaUpdatedAt: nullableTime(now.Add(-10 * time.Minute)),
			},
			want: false,
		},
		{
			name: "zero remaining after reset needs reprobe instead of hard skip",
			acc: account.Account{
				ImageQuotaRemaining: 0,
				ImageQuotaResetAt:   nullableTime(now.Add(-time.Minute)),
				ImageQuotaUpdatedAt: nullableTime(now.Add(-2 * time.Hour)),
			},
			want: false,
		},
		{
			name: "zero remaining without reset time still dispatches to verify by real generation",
			acc: account.Account{
				ImageQuotaRemaining: 0,
				ImageQuotaUpdatedAt: nullableTime(now.Add(-10 * time.Minute)),
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldSkipDispatchCandidate(&tt.acc, now, 5*time.Second)
			if got != tt.want {
				t.Fatalf("shouldSkipDispatchCandidate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDispatchCandidateSkipIgnoresLocalDailyCounters(t *testing.T) {
	now := time.Date(2026, 5, 3, 15, 0, 0, 0, time.Local)
	acc := account.Account{
		DailyImageQuota: 1,
		TodayUsedCount:  1,
		TodayUsedDate:   nullableTime(now),
	}

	if got := shouldSkipDispatchCandidate(&acc, now, 5*time.Second); got {
		t.Fatal("local daily counters should not block dispatch; real generation decides quota")
	}
}

func TestWarnedImageAccountDoesNotNeedQuotaProbeBeforeDispatch(t *testing.T) {
	tests := []struct {
		name string
		acc  account.Account
		want bool
	}{
		{
			name: "warned unknown quota remains dispatchable",
			acc: account.Account{
				Status:              account.StatusWarned,
				ImageQuotaRemaining: -1,
			},
			want: false,
		},
		{
			name: "warned positive quota remains dispatchable",
			acc: account.Account{
				Status:              account.StatusWarned,
				ImageQuotaRemaining: 1,
			},
			want: false,
		},
		{
			name: "healthy unknown quota remains dispatchable",
			acc: account.Account{
				Status:              account.StatusHealthy,
				ImageQuotaRemaining: -1,
			},
			want: false,
		},
		{
			name: "suspicious positive quota remains dispatchable after cooldown",
			acc: account.Account{
				Status:              account.StatusSuspicious,
				ImageQuotaRemaining: 1,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldSkipDispatchCandidate(&tt.acc, time.Now(), 5*time.Second)
			if got != tt.want {
				t.Fatalf("shouldSkipDispatchCandidate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSuspiciousPauseUsesShortCooldownInsteadOfWarnedPause(t *testing.T) {
	s := &Scheduler{
		cfg: config.SchedulerConfig{
			Cooldown429Sec:   300,
			WarnedPauseHours: 24,
		},
		rt: RuntimeParams{
			Cooldown429Sec: func() int { return 600 },
			WarnedPauseHrs: func() int { return 24 },
		},
	}

	if got := s.suspiciousPause(); got != 10*time.Minute {
		t.Fatalf("suspiciousPause() = %s, want 10m short cooldown", got)
	}
}

// TestSuspiciousPauseForReasonKeepsBadImageAccountsOutLonger verifies repeated
// image-runtime account failures do not immediately recycle into the dispatch
// candidate pool. The fallback suspicious cooldown remains short for unknown
// reasons, while concrete 403/upload/download/poll failures get a longer
// account-local pause.
func TestSuspiciousPauseForReasonKeepsBadImageAccountsOutLonger(t *testing.T) {
	s := &Scheduler{
		cfg: config.SchedulerConfig{
			Cooldown429Sec:   300,
			WarnedPauseHours: 24,
		},
		rt: RuntimeParams{
			Cooldown429Sec: func() int { return 600 },
			WarnedPauseHrs: func() int { return 24 },
		},
	}

	tests := []struct {
		reason string
		want   time.Duration
	}{
		{reason: "prepare_403", want: 10 * time.Minute},
		{reason: "upstream_403", want: 2 * time.Hour},
		{reason: "proxy_connect_403", want: 2 * time.Hour},
		{reason: "turnstile_stalled", want: 2 * time.Hour},
		{reason: "upstream_network_transient", want: 30 * time.Minute},
		{reason: "download_failed", want: 30 * time.Minute},
		{reason: "poll_timeout", want: 30 * time.Minute},
		{reason: "image_task_stalled", want: 30 * time.Minute},
		{reason: "unknown_runtime_warning", want: 10 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			if got := s.suspiciousPauseForReason(tt.reason); got != tt.want {
				t.Fatalf("suspiciousPauseForReason(%q) = %s, want %s", tt.reason, got, tt.want)
			}
		})
	}
}

func nullableTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: true}
}
