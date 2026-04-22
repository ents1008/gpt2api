package auth

import (
	"errors"
	"testing"
	"time"
)

func TestEvaluateRegisterIPPolicy_AllowsWhenPolicyDisabled(t *testing.T) {
	now := time.Date(2026, 4, 22, 20, 0, 0, 0, time.UTC)
	last := now.Add(-30 * time.Second)

	if err := evaluateRegisterIPPolicy(now, &last, 99, 0, 0); err != nil {
		t.Fatalf("expected policy-disabled registration to pass, got %v", err)
	}
}

func TestEvaluateRegisterIPPolicy_RejectsWithinCooldown(t *testing.T) {
	now := time.Date(2026, 4, 22, 20, 0, 0, 0, time.UTC)
	last := now.Add(-30 * time.Second)

	err := evaluateRegisterIPPolicy(now, &last, 1, 60, 0)
	if !errors.Is(err, ErrRegisterIPCooldown) {
		t.Fatalf("expected ErrRegisterIPCooldown, got %v", err)
	}
}

func TestEvaluateRegisterIPPolicy_RejectsWhenDailyLimitReached(t *testing.T) {
	now := time.Date(2026, 4, 22, 20, 0, 0, 0, time.UTC)

	err := evaluateRegisterIPPolicy(now, nil, 3, 0, 3)
	if !errors.Is(err, ErrRegisterIPLimit) {
		t.Fatalf("expected ErrRegisterIPLimit, got %v", err)
	}
}

func TestEvaluateRegisterIPPolicy_AllowsWhenOutsideCooldownAndBelowLimit(t *testing.T) {
	now := time.Date(2026, 4, 22, 20, 0, 0, 0, time.UTC)
	last := now.Add(-2 * time.Hour)

	if err := evaluateRegisterIPPolicy(now, &last, 2, 600, 3); err != nil {
		t.Fatalf("expected registration to pass, got %v", err)
	}
}
