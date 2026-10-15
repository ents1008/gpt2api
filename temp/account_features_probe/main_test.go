package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

func completeProbeJSON(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(probeInput{
		AuthToken: "auth", DeviceID: "device", SessionID: "session",
		Cookies: "[]", ProxyURL: "http://proxy.example:8080",
		BrowserProfile: chatgpt.DefaultBrowserProfileJSON(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Purpose: guarantee the read-only feature probe cannot accept an account
// collection or silently fall back to direct egress/default browser identity.
// Expected: exactly one complete identity is required before network I/O.
func TestReadProbeInputRequiresOneCompleteIdentity(t *testing.T) {
	if _, err := readProbeInput(bytes.NewReader(completeProbeJSON(t))); err != nil {
		t.Fatal(err)
	}
	if _, err := readProbeInput(strings.NewReader("[{}]")); err == nil {
		t.Fatal("array input was accepted")
	}
	var missing map[string]interface{}
	_ = json.Unmarshal(completeProbeJSON(t), &missing)
	delete(missing, "session_id")
	raw, _ := json.Marshal(missing)
	if _, err := readProbeInput(bytes.NewReader(raw)); err == nil || err.Error() != "missing_session_id" {
		t.Fatalf("missing session error = %v", err)
	}
}

// Purpose: prevent upstream bodies or credential-bearing messages from
// reaching the probe output. Expected: errors collapse to fixed classes.
func TestClassifyProbeErrorReturnsTypedMetadataOnly(t *testing.T) {
	secret := "token-cookie-proxy-secret"
	status, class := classifyProbeError(&chatgpt.UpstreamError{Status: 403, Message: secret, Body: secret})
	if status != 403 || class != "forbidden" || strings.Contains(class, secret) {
		t.Fatalf("classification = (%d, %q)", status, class)
	}
	status, class = classifyProbeError(context.DeadlineExceeded)
	if status != 0 || class != "timeout" {
		t.Fatalf("timeout classification = (%d, %q)", status, class)
	}
	status, class = classifyProbeError(errors.New(secret))
	if status != 0 || class != "transport_or_decode_error" {
		t.Fatalf("local classification = (%d, %q)", status, class)
	}
}
