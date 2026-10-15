//go:build live

package chatgpt

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveChatRequirementsV2WithHTTP2 validates the live ChatGPT sentinel
// prepare/finalize flow with the browser-like HTTP/2 path. It requires explicit
// credentials in the environment and is intentionally excluded from normal test
// runs.
func TestLiveChatRequirementsV2WithHTTP2(t *testing.T) {
	authToken := os.Getenv("GPT2API_LIVE_AUTH_TOKEN")
	deviceID := os.Getenv("GPT2API_LIVE_DEVICE_ID")
	proxyURL := os.Getenv("GPT2API_LIVE_PROXY_URL")
	if authToken == "" || deviceID == "" || proxyURL == "" {
		t.Skip("set GPT2API_LIVE_AUTH_TOKEN, GPT2API_LIVE_DEVICE_ID, and GPT2API_LIVE_PROXY_URL")
	}

	previousForceH1 := forceH1
	forceH1 = false
	t.Cleanup(func() { forceH1 = previousForceH1 })

	client, err := New(Options{
		AuthToken: authToken,
		DeviceID:  deviceID,
		ProxyURL:  proxyURL,
		Timeout:   45 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	reqs, err := client.ChatRequirementsV2(ctx)
	if err != nil {
		t.Fatalf("ChatRequirementsV2 over HTTP/2 failed: %v", err)
	}
	if reqs.Token == "" {
		t.Fatal("ChatRequirementsV2 returned empty token")
	}
}
