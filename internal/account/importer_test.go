package account

import (
	"strings"
	"testing"
)

const testBrowserProfileJSON = `{"version":1,"headers":{"Accept-Language":"en-US,en;q=0.9","Sec-Ch-Ua":"\"Chromium\";v=\"146\", \"Google Chrome\";v=\"146\"","Sec-Ch-Ua-Mobile":"?0","Sec-Ch-Ua-Platform":"\"Windows\"","User-Agent":"Mozilla/5.0 Chrome/146.0.0.0 Safari/537.36"},"language":"en-US","timezone":"America/Los_Angeles"}`

// TestParseJSONBlobPreservesImageRuntimeMetadata verifies that token JSON
// produced by the registration pipeline keeps the browser/runtime identity
// needed by the ChatGPT image path. Expected: device id, session id, and cookie
// JSON survive parsing instead of being discarded before ImportBatch.
func TestParseJSONBlobPreservesImageRuntimeMetadata(t *testing.T) {
	raw := `{
		"email": "user@example.com",
		"access_token": "at",
		"refresh_token": "rt",
		"client_id": "app_platform",
		"device_id": "device-123",
		"oai_session_id": "session-456",
		"cookies": [{"name":"oai-did","value":"device-123","domain":".chatgpt.com"}],
		"browser_profile": {"version":1,"headers":{"Accept-Language":"en-US,en;q=0.9","Sec-Ch-Ua":"\"Chromium\";v=\"146\", \"Google Chrome\";v=\"146\"","Sec-Ch-Ua-Mobile":"?0","Sec-Ch-Ua-Platform":"\"Windows\"","User-Agent":"Mozilla/5.0 Chrome/146.0.0.0 Safari/537.36"},"language":"en-US","timezone":"America/Los_Angeles"}
	}`

	items, err := ParseJSONBlob(raw)
	if err != nil {
		t.Fatalf("ParseJSONBlob returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	got := items[0]
	if got.DeviceID != "device-123" {
		t.Fatalf("DeviceID = %q, want device-123", got.DeviceID)
	}
	if got.SessionID != "session-456" {
		t.Fatalf("SessionID = %q, want session-456", got.SessionID)
	}
	if !strings.Contains(got.Cookies, "oai-did") || !strings.Contains(got.Cookies, "device-123") {
		t.Fatalf("Cookies = %q, want serialized oai-did cookie JSON", got.Cookies)
	}
	if got.BrowserProfile != testBrowserProfileJSON {
		t.Fatalf("BrowserProfile = %q, want canonical browser profile", got.BrowserProfile)
	}
}
