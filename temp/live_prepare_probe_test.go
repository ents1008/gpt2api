package main

import (
	"context"
	"errors"
	"testing"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

// TestOptionsForProbePreservesBrowserIdentity verifies that the diagnostic
// requirements check carries the same browser-profile field as production.
func TestOptionsForProbePreservesBrowserIdentity(t *testing.T) {
	account := probeAccount{
		AuthToken:      "secret-auth",
		DeviceID:       "device-id",
		SessionID:      "session-id",
		Cookies:        `[{"name":"cookie","value":"secret"}]`,
		ProxyURL:       "http://proxy-user:proxy-password@example.invalid:8080",
		BrowserProfile: chatgpt.DefaultBrowserProfileJSON(),
	}

	got := optionsForProbe(account)
	if got.AuthToken != account.AuthToken || got.DeviceID != account.DeviceID ||
		got.SessionID != account.SessionID || got.Cookies != account.Cookies ||
		got.ProxyURL != account.ProxyURL || got.BrowserProfile != account.BrowserProfile {
		t.Fatal("probe options did not preserve the complete browser identity")
	}
	if got.Timeout != probeTimeout {
		t.Fatalf("probe timeout = %s, want %s", got.Timeout, probeTimeout)
	}
}

// TestValidateProbeAccountRejectsIdentityFallbacks verifies that a stability
// check cannot silently use direct egress or the client's default profile.
func TestValidateProbeAccountRejectsIdentityFallbacks(t *testing.T) {
	complete := probeAccount{
		AuthToken:      "auth",
		DeviceID:       "device",
		SessionID:      "session",
		Cookies:        "[]",
		ProxyURL:       "http://proxy.example:8080",
		BrowserProfile: chatgpt.DefaultBrowserProfileJSON(),
	}
	if got := validateProbeAccount(complete); got != "" {
		t.Fatalf("complete identity rejected as %q", got)
	}

	tests := []struct {
		name string
		edit func(*probeAccount)
		want string
	}{
		{name: "auth", edit: func(a *probeAccount) { a.AuthToken = "" }, want: "missing_auth_token"},
		{name: "device", edit: func(a *probeAccount) { a.DeviceID = "" }, want: "missing_device_id"},
		{name: "session", edit: func(a *probeAccount) { a.SessionID = "" }, want: "missing_session_id"},
		{name: "cookies", edit: func(a *probeAccount) { a.Cookies = "" }, want: "missing_cookies"},
		{name: "proxy", edit: func(a *probeAccount) { a.ProxyURL = "" }, want: "missing_proxy"},
		{name: "profile", edit: func(a *probeAccount) { a.BrowserProfile = "" }, want: "missing_browser_profile"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			account := complete
			test.edit(&account)
			if got := validateProbeAccount(account); got != test.want {
				t.Fatalf("validateProbeAccount() = %q, want %q", got, test.want)
			}
		})
	}
}

// TestClassifyProbeErrorReturnsOnlyTypedMetadata verifies that upstream bodies
// and messages cannot enter the diagnostic result surface.
func TestClassifyProbeErrorReturnsOnlyTypedMetadata(t *testing.T) {
	secret := "credential-that-must-not-be-rendered"
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantClass  string
	}{
		{name: "unauthorized", err: &chatgpt.UpstreamError{Status: 401, Message: secret, Body: secret}, wantStatus: 401, wantClass: "unauthorized"},
		{name: "forbidden", err: &chatgpt.UpstreamError{Status: 403, Message: secret, Body: secret}, wantStatus: 403, wantClass: "forbidden"},
		{name: "rate limited", err: &chatgpt.UpstreamError{Status: 429, Message: secret, Body: secret}, wantStatus: 429, wantClass: "rate_limited"},
		{name: "other upstream", err: &chatgpt.UpstreamError{Status: 502, Message: secret, Body: secret}, wantStatus: 502, wantClass: "upstream_error"},
		{name: "timeout", err: context.DeadlineExceeded, wantClass: "timeout"},
		{name: "other", err: errors.New(secret), wantClass: "requirements_error"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, class := classifyProbeError(test.err)
			if status != test.wantStatus || class != test.wantClass {
				t.Fatalf("classifyProbeError() = (%d, %q), want (%d, %q)", status, class, test.wantStatus, test.wantClass)
			}
			if class == secret {
				t.Fatal("probe classification leaked the source error")
			}
		})
	}
}

// TestClassifyProbeStageExposesOnlyKnownProtocolStages verifies diagnostics can
// distinguish prepare from finalize without returning upstream error text.
func TestClassifyProbeStageExposesOnlyKnownProtocolStages(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "prepare", err: &chatgpt.UpstreamError{Status: 401, Message: "chat-requirements/prepare failed", Body: "secret"}, want: "requirements_prepare"},
		{name: "finalize", err: &chatgpt.UpstreamError{Status: 401, Message: "chat-requirements/finalize failed", Body: "secret"}, want: "requirements_finalize"},
		{name: "unknown upstream", err: &chatgpt.UpstreamError{Status: 500, Message: "secret", Body: "secret"}, want: "requirements"},
		{name: "local", err: errors.New("secret"), want: "requirements"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyProbeStage(test.err); got != test.want {
				t.Fatalf("classifyProbeStage() = %q, want %q", got, test.want)
			}
		})
	}
}
