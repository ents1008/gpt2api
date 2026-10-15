package account

import (
	"context"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestAdmissionSuccessRequiresOneHourObservation defines the production
// admission boundary. Expected: a check before one full hour is rejected by
// the clock, while the first successful check at one hour admits the account
// without scheduling any additional probes.
func TestAdmissionSuccessRequiresOneHourObservation(t *testing.T) {
	startedAt := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	a := &Account{
		AdmissionState:     AdmissionStateQuarantined,
		AdmissionStage:     AdmissionStageInitial,
		AdmissionStartedAt: nullTime(startedAt),
	}

	if _, err := nextAdmissionSuccess(a, startedAt.Add(time.Hour-time.Nanosecond)); err == nil {
		t.Fatal("checkpoint before one hour unexpectedly succeeded")
	}

	transition, err := nextAdmissionSuccess(a, startedAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("one-hour checkpoint: %v", err)
	}
	if transition.State != AdmissionStateAdmitted || transition.Stage != AdmissionStageAdmitted ||
		transition.NextCheckAt.Valid || transition.Reason != AdmissionReasonChecksPassed {
		t.Fatalf("one-hour transition = %+v", transition)
	}
}

// TestAdmissionLateCheckAdmitsOnceAgeReached covers a delayed monitor restart.
// Expected: an account already older than one hour needs one successful check,
// then becomes admitted immediately instead of accumulating artificial delays.
func TestAdmissionLateCheckAdmitsOnceAgeReached(t *testing.T) {
	startedAt := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	a := &Account{
		AdmissionState:     AdmissionStateQuarantined,
		AdmissionStage:     AdmissionStageInitial,
		AdmissionStartedAt: nullTime(startedAt),
	}
	lateFirstAt := startedAt.Add(8 * time.Hour)

	transition, err := nextAdmissionSuccess(a, lateFirstAt)
	if err != nil {
		t.Fatalf("late one-hour checkpoint: %v", err)
	}
	if transition.State != AdmissionStateAdmitted || transition.Stage != AdmissionStageAdmitted ||
		transition.NextCheckAt.Valid || transition.Reason != AdmissionReasonChecksPassed {
		t.Fatalf("late transition = %+v", transition)
	}
}

// TestAdmissionRecoveryRestartsObservation prevents a freshly rotated bearer
// from inheriting the old token's accumulated age. Expected: a recoverable 401
// resets stage zero and schedules a new one-hour proof.
func TestAdmissionRecoveryRestartsObservation(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	transition := resetAdmissionObservation(now)

	if transition.State != AdmissionStateQuarantined || transition.Stage != AdmissionStageInitial {
		t.Fatalf("reset transition = %+v", transition)
	}
	if !transition.StartedAt.Valid || !transition.StartedAt.Time.Equal(now) {
		t.Fatalf("started_at = %v, want %v", transition.StartedAt, now)
	}
	if !transition.NextCheckAt.Valid || !transition.NextCheckAt.Time.Equal(now.Add(time.Hour)) {
		t.Fatalf("next_check_at = %v", transition.NextCheckAt)
	}
	if transition.Reason != AdmissionReasonRecoveryReset {
		t.Fatalf("reason = %q", transition.Reason)
	}
}

// TestAdmissionMonitorResetsRecovered401Observation exercises the complete
// monitor decision rather than only the clock helper. Expected: a due 401 is
// claimed, recovery runs once, and the old stage is replaced by a fresh one-hour
// quarantine observation without rejection or production admission.
func TestAdmissionMonitorResetsRecovered401Observation(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-6 * time.Hour)
	store := &recordingAdmissionStore{claimResult: true}
	verifier := &recordingAdmissionVerifier{
		status:    http.StatusUnauthorized,
		recovered: true,
	}
	monitor := NewAdmissionMonitor(store, verifier, zap.NewNop())
	monitor.now = func() time.Time { return now }
	a := &Account{
		ID:                 1941,
		AdmissionState:     AdmissionStateQuarantined,
		AdmissionStage:     AdmissionStageInitial,
		AdmissionStartedAt: nullTime(startedAt),
	}

	monitor.processOne(context.Background(), a)

	if verifier.recoveryCalls != 1 || store.applyCalls != 1 || store.rejectCalls != 0 {
		t.Fatalf("recovery=%d apply=%d reject=%d", verifier.recoveryCalls, store.applyCalls, store.rejectCalls)
	}
	if store.transition.Stage != AdmissionStageInitial ||
		!store.transition.StartedAt.Time.Equal(now) ||
		!store.transition.NextCheckAt.Time.Equal(now.Add(time.Hour)) ||
		store.transition.Reason != AdmissionReasonRecoveryReset {
		t.Fatalf("reset transition = %+v", store.transition)
	}
}

// TestAdmissionMonitorRejectsUnrecoverable401 proves an account cannot remain
// in a retry loop after both RT and ST recovery fail. Expected: exactly one
// recovery attempt and an auth_required_401 rejection, with no admission
// transition applied.
func TestAdmissionMonitorRejectsUnrecoverable401(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-30 * time.Minute)
	store := &recordingAdmissionStore{claimResult: true}
	verifier := &recordingAdmissionVerifier{status: http.StatusUnauthorized}
	monitor := NewAdmissionMonitor(store, verifier, zap.NewNop())
	monitor.now = func() time.Time { return now }
	a := &Account{
		ID:                 1942,
		AdmissionState:     AdmissionStateQuarantined,
		AdmissionStage:     AdmissionStageInitial,
		AdmissionStartedAt: nullTime(startedAt),
	}

	monitor.processOne(context.Background(), a)

	if verifier.recoveryCalls != 1 || store.rejectCalls != 1 || store.applyCalls != 0 {
		t.Fatalf("recovery=%d reject=%d apply=%d", verifier.recoveryCalls, store.rejectCalls, store.applyCalls)
	}
	if store.rejectReason != "auth_required_401" {
		t.Fatalf("reject reason = %q", store.rejectReason)
	}
}

// TestAdmissionMonitorRejectsBrowser401WithoutRecovery protects the one-hour
// stability observation from credential mutation. Expected: a browser-owned
// account that loses its original bearer is rejected immediately; recovery is
// never called and cannot reset or contaminate the observation clock.
func TestAdmissionMonitorRejectsBrowser401WithoutRecovery(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-30 * time.Minute)
	store := &recordingAdmissionStore{claimResult: true}
	verifier := &recordingAdmissionVerifier{status: http.StatusUnauthorized, recovered: true}
	monitor := NewAdmissionMonitor(store, verifier, zap.NewNop())
	monitor.now = func() time.Time { return now }
	a := &Account{
		ID:                   1943,
		CredentialProvenance: CredentialProvenanceBrowserWeb,
		AdmissionState:       AdmissionStateQuarantined,
		AdmissionStage:       AdmissionStageInitial,
		AdmissionStartedAt:   nullTime(startedAt),
	}

	monitor.processOne(context.Background(), a)

	if verifier.recoveryCalls != 0 || store.rejectCalls != 1 || store.applyCalls != 0 {
		t.Fatalf("recovery=%d reject=%d apply=%d", verifier.recoveryCalls, store.rejectCalls, store.applyCalls)
	}
	if store.rejectReason != "auth_required_401" {
		t.Fatalf("reject reason = %q", store.rejectReason)
	}
}

type recordingAdmissionStore struct {
	claimResult  bool
	applyCalls   int
	rejectCalls  int
	retryCalls   int
	transition   admissionTransition
	rejectReason string
}

func (s *recordingAdmissionStore) ListDueAdmissions(context.Context, time.Time, int) ([]*Account, error) {
	return nil, nil
}

func (s *recordingAdmissionStore) ClaimAdmissionCheck(
	context.Context,
	uint64,
	int,
	time.Time,
	time.Time,
	time.Time,
) (bool, error) {
	return s.claimResult, nil
}

func (s *recordingAdmissionStore) ApplyAdmissionTransition(
	_ context.Context,
	_ uint64,
	_ int,
	_ time.Time,
	transition admissionTransition,
	_ time.Time,
) error {
	s.applyCalls++
	s.transition = transition
	return nil
}

func (s *recordingAdmissionStore) RetryAdmissionCheck(
	context.Context,
	uint64,
	int,
	time.Time,
	time.Time,
	string,
) error {
	s.retryCalls++
	return nil
}

func (s *recordingAdmissionStore) RejectAdmission(
	_ context.Context,
	_ uint64,
	_ int,
	_ time.Time,
	reason string,
) error {
	s.rejectCalls++
	s.rejectReason = reason
	return nil
}

type recordingAdmissionVerifier struct {
	status        int
	verifyErr     error
	recovered     bool
	recoverErr    error
	recoveryCalls int
}

func (v *recordingAdmissionVerifier) ValidateAccountForImage(context.Context, uint64) (int, error) {
	return v.status, v.verifyErr
}

func (v *recordingAdmissionVerifier) RecoverAccountForImage(context.Context, uint64) (bool, error) {
	v.recoveryCalls++
	return v.recovered, v.recoverErr
}
