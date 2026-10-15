package account

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	admissionMinimumAge   = time.Hour
	admissionRetryDelay   = 30 * time.Minute
	admissionClaimTTL     = 2 * time.Minute
	admissionScanInterval = time.Minute
	admissionCheckTimeout = 30 * time.Second
	admissionScanLimit    = 64
	admissionConcurrency  = 4
)

type admissionTransition struct {
	State       string
	Stage       int
	StartedAt   sql.NullTime
	NextCheckAt sql.NullTime
	Reason      string
}

func resetAdmissionObservation(now time.Time) admissionTransition {
	return admissionTransition{
		State:       AdmissionStateQuarantined,
		Stage:       AdmissionStageInitial,
		StartedAt:   nullTime(now),
		NextCheckAt: nullTime(now.Add(admissionMinimumAge)),
		Reason:      AdmissionReasonRecoveryReset,
	}
}

func newAdmissionObservation(now time.Time) admissionTransition {
	transition := resetAdmissionObservation(now)
	transition.Reason = AdmissionReasonPending1h
	return transition
}

func nextAdmissionSuccess(a *Account, now time.Time) (admissionTransition, error) {
	if a == nil || !a.AdmissionStartedAt.Valid {
		return admissionTransition{}, errors.New("admission observation has no start time")
	}
	startedAt := a.AdmissionStartedAt.Time
	transition := admissionTransition{
		State:     AdmissionStateQuarantined,
		Stage:     a.AdmissionStage,
		StartedAt: a.AdmissionStartedAt,
	}

	switch a.AdmissionStage {
	case AdmissionStageInitial:
		if now.Before(startedAt.Add(admissionMinimumAge)) {
			return admissionTransition{}, errors.New("one hour admission checkpoint is not due")
		}
		transition.State = AdmissionStateAdmitted
		transition.Stage = AdmissionStageAdmitted
		transition.Reason = AdmissionReasonChecksPassed
	case AdmissionStageAdmitted:
		return admissionTransition{}, errors.New("account is already admitted")
	default:
		return admissionTransition{}, fmt.Errorf("invalid admission stage %d", a.AdmissionStage)
	}
	return transition, nil
}

func applyAdmissionTransition(a *Account, transition admissionTransition) {
	if a == nil {
		return
	}
	a.AdmissionState = transition.State
	a.AdmissionStage = transition.Stage
	a.AdmissionStartedAt = transition.StartedAt
	a.AdmissionNextCheckAt = transition.NextCheckAt
	a.AdmissionLastCheckAt = sql.NullTime{}
	a.AdmissionClaimedUntil = sql.NullTime{}
	a.AdmissionReason = transition.Reason
}

type admissionStore interface {
	ListDueAdmissions(context.Context, time.Time, int) ([]*Account, error)
	ClaimAdmissionCheck(context.Context, uint64, int, time.Time, time.Time, time.Time) (bool, error)
	ApplyAdmissionTransition(context.Context, uint64, int, time.Time, admissionTransition, time.Time) error
	RetryAdmissionCheck(context.Context, uint64, int, time.Time, time.Time, string) error
	RejectAdmission(context.Context, uint64, int, time.Time, string) error
}

type admissionCredentialVerifier interface {
	ValidateAccountForImage(context.Context, uint64) (int, error)
	RecoverAccountForImage(context.Context, uint64) (bool, error)
}

type AdmissionMonitor struct {
	store    admissionStore
	verifier admissionCredentialVerifier
	log      *zap.Logger
	now      func() time.Time
}

func NewAdmissionMonitor(store admissionStore, verifier admissionCredentialVerifier, logger *zap.Logger) *AdmissionMonitor {
	return &AdmissionMonitor{
		store:    store,
		verifier: verifier,
		log:      logger,
		now:      time.Now,
	}
}

func (m *AdmissionMonitor) Run(ctx context.Context) {
	if m == nil || m.store == nil || m.verifier == nil {
		return
	}
	m.log.Info("account admission monitor started")
	defer m.log.Info("account admission monitor stopped")

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}

	ticker := time.NewTicker(admissionScanInterval)
	defer ticker.Stop()
	for {
		m.scanOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *AdmissionMonitor) scanOnce(ctx context.Context) {
	now := m.now()
	accounts, err := m.store.ListDueAdmissions(ctx, now, admissionScanLimit)
	if err != nil {
		m.log.Warn("list due account admissions failed", zap.Error(err))
		return
	}
	sem := make(chan struct{}, admissionConcurrency)
	var wg sync.WaitGroup
	for _, candidate := range accounts {
		candidate := candidate
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			m.processOne(ctx, candidate)
		}()
	}
	wg.Wait()
}

func (m *AdmissionMonitor) processOne(parent context.Context, a *Account) {
	if a == nil {
		return
	}
	if !a.AdmissionStartedAt.Valid {
		return
	}
	now := m.now()
	claimed, err := m.store.ClaimAdmissionCheck(
		parent,
		a.ID,
		a.AdmissionStage,
		a.AdmissionStartedAt.Time,
		now,
		now.Add(admissionClaimTTL),
	)
	if err != nil || !claimed {
		if err != nil {
			m.log.Warn("claim account admission failed", zap.Uint64("account_id", a.ID), zap.Error(err))
		}
		return
	}

	ctx, cancel := context.WithTimeout(parent, admissionCheckTimeout)
	defer cancel()
	status, verifyErr := m.verifier.ValidateAccountForImage(ctx, a.ID)
	if verifyErr == nil && status == http.StatusOK {
		transition, nextErr := nextAdmissionSuccess(a, now)
		if nextErr != nil {
			_ = m.store.RetryAdmissionCheck(parent, a.ID, a.AdmissionStage, a.AdmissionStartedAt.Time, now.Add(admissionRetryDelay), "invalid_admission_state")
			return
		}
		if err := m.store.ApplyAdmissionTransition(parent, a.ID, a.AdmissionStage, a.AdmissionStartedAt.Time, transition, now); err != nil {
			m.log.Warn("advance account admission failed", zap.Uint64("account_id", a.ID), zap.Error(err))
		}
		return
	}

	if status == http.StatusUnauthorized {
		if isImmutableQuarantinedWebCredential(a.CredentialProvenance, a.AdmissionState) {
			_ = m.store.RejectAdmission(parent, a.ID, a.AdmissionStage, a.AdmissionStartedAt.Time, "auth_required_401")
			return
		}
		recovered, recoverErr := m.verifier.RecoverAccountForImage(ctx, a.ID)
		switch {
		case recoverErr != nil:
			_ = m.store.RetryAdmissionCheck(parent, a.ID, a.AdmissionStage, a.AdmissionStartedAt.Time, now.Add(admissionRetryDelay), "credential_recovery_transient")
		case !recovered:
			_ = m.store.RejectAdmission(parent, a.ID, a.AdmissionStage, a.AdmissionStartedAt.Time, "auth_required_401")
		default:
			transition := resetAdmissionObservation(now)
			if err := m.store.ApplyAdmissionTransition(parent, a.ID, a.AdmissionStage, a.AdmissionStartedAt.Time, transition, now); err != nil {
				m.log.Warn("reset recovered account admission failed", zap.Uint64("account_id", a.ID), zap.Error(err))
			}
		}
		return
	}

	reason := "verification_transient"
	if status > 0 {
		reason = fmt.Sprintf("verification_http_%d", status)
	}
	_ = m.store.RetryAdmissionCheck(parent, a.ID, a.AdmissionStage, a.AdmissionStartedAt.Time, now.Add(admissionRetryDelay), reason)
}
