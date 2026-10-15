package chatgpt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Purpose: pin the read-only image-entitlement diagnostic to the same browser
// identity owner used by image turns. Expected: it sends the account device and
// session headers and returns only image-related feature names.
func TestGetAccountFeaturesUsesBrowserIdentityAndFiltersFeatures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/backend-api/accounts/check/v4-2023-04-27" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Oai-Device-Id") != "device-id" || request.Header.Get("Oai-Session-Id") != "session-id" {
			t.Fatalf("browser identity headers are incomplete")
		}
		if request.Header.Get("X-Oai-Is-Client-Observation") != "true" {
			t.Fatalf("client observation header is missing")
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accounts": map[string]interface{}{
				"default": map[string]interface{}{
					"account":                 map[string]bool{"is_deactivated": false},
					"can_access_with_session": true,
					"features": []interface{}{
						"memory", "gpt_image_1",
						map[string]string{"name": "image_gen_v2"},
						map[string]string{"feature_name": "image_gen_v2"},
					},
				},
			},
		})
	}))
	defer server.Close()

	client, err := New(Options{
		BaseURL: server.URL, AuthToken: "auth", DeviceID: "device-id", SessionID: "session-id",
	})
	if err != nil {
		t.Fatal(err)
	}
	features, err := client.GetAccountFeatures(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if features.IsDeactivated || !features.CanAccessWithSession || features.FeatureCount != 4 || !features.ImageGenerationEnabled {
		t.Fatalf("account features = %+v", features)
	}
	if got := len(features.ImageFeatures); got != 2 || features.ImageFeatures[0] != "gpt_image_1" || features.ImageFeatures[1] != "image_gen_v2" {
		t.Fatalf("image features = %#v", features.ImageFeatures)
	}
}

// Purpose: distinguish image-generation entitlement from unrelated image
// upload features. Expected: voice_image_upload remains observable but cannot
// admit an account to a real image turn.
func TestImageGenerationFeatureRejectsUploadOnlyCapabilities(t *testing.T) {
	if isImageGenerationFeatureName("voice_image_upload") {
		t.Fatal("voice image upload was treated as image generation")
	}
	for _, feature := range []string{"image_gen_tool_enabled", "gpt_image_1", "image_gen_v2"} {
		if !isImageGenerationFeatureName(feature) {
			t.Fatalf("generation feature %q was rejected", feature)
		}
	}
}

// Purpose: prevent malformed account projections from being mistaken for an
// account with no image entitlement. Expected: missing default account fails.
func TestGetAccountFeaturesRequiresDefaultAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"accounts":{}}`))
	}))
	defer server.Close()
	client, err := New(Options{BaseURL: server.URL, AuthToken: "auth", DeviceID: "device"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetAccountFeatures(context.Background()); err == nil {
		t.Fatal("missing default account was accepted")
	}
}
