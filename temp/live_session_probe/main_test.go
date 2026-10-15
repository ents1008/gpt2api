package main

import (
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

// TestValidateSessionProbeInputRejectsIdentityFallbacks verifies a live session
// check cannot silently omit a browser or account-bound identity field.
func TestValidateSessionProbeInputRejectsIdentityFallbacks(t *testing.T) {
	complete := sessionProbeInput{
		AuthToken: "auth", DeviceID: "device", SessionID: "session",
		SessionToken: "next-auth", Cookies: "[]", ProxyURL: "http://proxy.example:8080",
		BrowserProfile: chatgpt.DefaultBrowserProfileJSON(),
	}
	if got := validateSessionProbeInput(complete); got != "" {
		t.Fatalf("complete identity rejected as %q", got)
	}
	tests := []struct {
		name string
		edit func(*sessionProbeInput)
		want string
	}{
		{name: "auth", edit: func(v *sessionProbeInput) { v.AuthToken = "" }, want: "missing_auth_token"},
		{name: "device", edit: func(v *sessionProbeInput) { v.DeviceID = "" }, want: "missing_device_id"},
		{name: "session", edit: func(v *sessionProbeInput) { v.SessionID = "" }, want: "missing_session_id"},
		{name: "session token", edit: func(v *sessionProbeInput) { v.SessionToken = "" }, want: "missing_session_token"},
		{name: "cookies", edit: func(v *sessionProbeInput) { v.Cookies = "" }, want: "missing_cookies"},
		{name: "proxy", edit: func(v *sessionProbeInput) { v.ProxyURL = "" }, want: "missing_proxy"},
		{name: "profile", edit: func(v *sessionProbeInput) { v.BrowserProfile = "" }, want: "missing_browser_profile"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := complete
			test.edit(&input)
			if got := validateSessionProbeInput(input); got != test.want {
				t.Fatalf("validateSessionProbeInput() = %q, want %q", got, test.want)
			}
		})
	}
}

// TestTokenSecondsUntilExpiryUsesJWTExp verifies the probe reports only a TTL,
// never token contents or claims.
func TestTokenSecondsUntilExpiryUsesJWTExp(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, now.Unix()+600)))
	token := "header." + payload + ".signature"
	if got := tokenSecondsUntilExpiry(token, now); got != 600 {
		t.Fatalf("tokenSecondsUntilExpiry() = %d, want 600", got)
	}
	if got := tokenSecondsUntilExpiry("not-a-jwt", now); got != 0 {
		t.Fatalf("invalid token TTL = %d, want 0", got)
	}
}

// TestConstantTokenEqualComparesWithoutRendering verifies token comparison is
// boolean-only and distinguishes changed session-issued access tokens.
func TestConstantTokenEqualComparesWithoutRendering(t *testing.T) {
	if !constantTokenEqual("same-secret", "same-secret") {
		t.Fatal("equal tokens were not recognized")
	}
	if constantTokenEqual("old-secret", "new-secret") {
		t.Fatal("different tokens were treated as equal")
	}
}
