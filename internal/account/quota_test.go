package account

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

type quotaRoundTripFunc func(*http.Request) (*http.Response, error)

func (f quotaRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestQuotaClientRejectsInvalidBoundProxyWithoutDirectFallback protects the
// quota probe from changing an account's egress identity after a proxy parse
// failure. Expected: client construction fails before any direct request.
func TestQuotaClientRejectsInvalidBoundProxyWithoutDirectFallback(t *testing.T) {
	q := &QuotaProber{
		client:        &http.Client{Timeout: time.Second},
		proxyResolver: staticProxyResolver("://invalid-proxy"),
	}
	if _, err := q.clientFor(context.Background(), &Account{ID: 8}); err == nil {
		t.Fatal("quota client accepted an invalid bound proxy")
	}
}

// TestQuotaProbeBootstrapsBeforeConversationInit verifies the quota probe uses
// the same browser-session shape required by chatgpt.com: first establish
// cookies with GET /, then call /backend-api/conversation/init with the same
// account device fingerprint.
func TestQuotaProbeBootstrapsBeforeConversationInit(t *testing.T) {
	var paths []string
	var bootstrapDevice string
	var initDevice string
	var initAuth string
	var bootstrapUA string
	var initUA string
	var initPlatformVersion string

	client := &http.Client{
		Transport: quotaRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.URL.Path)
			switch req.URL.Path {
			case "/":
				bootstrapDevice = req.Header.Get("Oai-Device-Id")
				bootstrapUA = req.Header.Get("User-Agent")
				return quotaJSONResponse(200, `{}`), nil
			case "/backend-api/conversation/init":
				initDevice = req.Header.Get("Oai-Device-Id")
				initAuth = req.Header.Get("Authorization")
				initUA = req.Header.Get("User-Agent")
				initPlatformVersion = req.Header.Get("Sec-Ch-Ua-Platform-Version")
				return quotaJSONResponse(200, `{
					"default_model_slug": "auto",
					"blocked_features": [],
					"limits_progress": [
						{"feature_name": "image_gen", "remaining": 24, "reset_after": "2026-06-21T13:15:42Z"}
					]
				}`), nil
			default:
				t.Fatalf("unexpected quota request path: %s", req.URL.Path)
				return nil, nil
			}
		}),
	}

	q := &QuotaProber{}
	out, err := q.doProbeWithClient(context.Background(), &Account{
		ID:             971,
		OAIDeviceID:    "device-426d72d9cb",
		BrowserProfile: chatgpt.DefaultBrowserProfileJSON(),
	}, "access-token", client)
	if err != nil {
		t.Fatalf("doProbeWithClient returned error: %v", err)
	}

	if got, want := strings.Join(paths, ","), "/,/backend-api/conversation/init"; got != want {
		t.Fatalf("request order = %s, want %s", got, want)
	}
	if bootstrapDevice != "device-426d72d9cb" {
		t.Fatalf("bootstrap Oai-Device-Id = %q", bootstrapDevice)
	}
	if initDevice != "device-426d72d9cb" {
		t.Fatalf("init Oai-Device-Id = %q", initDevice)
	}
	if initAuth != "Bearer access-token" {
		t.Fatalf("init Authorization = %q", initAuth)
	}
	if bootstrapUA != chatgpt.DefaultUserAgent || initUA != chatgpt.DefaultUserAgent {
		t.Fatalf("quota UAs = bootstrap %q init %q, want canonical Web UA", bootstrapUA, initUA)
	}
	if initPlatformVersion != `"19.0.0"` {
		t.Fatalf("quota platform version = %q, want canonical Web platform", initPlatformVersion)
	}
	if out.remaining != 24 {
		t.Fatalf("remaining = %d, want 24", out.remaining)
	}
	if out.defaultModel != "auto" {
		t.Fatalf("default model = %q, want auto", out.defaultModel)
	}
}

// TestQuotaProbeAcceptsObjectBlockedFeatures verifies conversation/init
// responses with object-shaped blocked_features still count as valid quota
// results. Expected: quota parsing keeps the image remaining value and stores a
// readable feature marker instead of failing the whole probe.
func TestQuotaProbeAcceptsObjectBlockedFeatures(t *testing.T) {
	client := &http.Client{
		Transport: quotaRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/":
				return quotaJSONResponse(200, `{}`), nil
			case "/backend-api/conversation/init":
				return quotaJSONResponse(200, `{
					"default_model_slug": "auto",
					"blocked_features": [
						{"feature_name": "image_gen", "reason": "safety_hold"}
					],
					"limits_progress": [
						{"feature_name": "image_gen", "remaining": 19, "reset_after": "2026-06-21T13:15:42Z"}
					]
				}`), nil
			default:
				t.Fatalf("unexpected quota request path: %s", req.URL.Path)
				return nil, nil
			}
		}),
	}

	q := &QuotaProber{}
	out, err := q.doProbeWithClient(context.Background(), &Account{
		ID:             972,
		OAIDeviceID:    "device-obj-blocked",
		BrowserProfile: chatgpt.DefaultBrowserProfileJSON(),
	}, "access-token", client)
	if err != nil {
		t.Fatalf("doProbeWithClient returned error: %v", err)
	}

	if out.remaining != 19 {
		t.Fatalf("remaining = %d, want 19", out.remaining)
	}
	if got, want := strings.Join(out.blockedFeatures, ","), "image_gen"; got != want {
		t.Fatalf("blocked features = %q, want %q", got, want)
	}
}

func quotaJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
