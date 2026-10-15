package chatgpt

import (
	"net/http"
	"testing"
	"time"
)

// TestApplyDefaultBrowserFingerprintHeaders defines the one browser identity
// shared by normal Web calls, bootstrap, quota probing, and refresh validation.
// Expected: UA reduction, brand versions, platform version, and language remain
// internally coherent instead of each call site maintaining divergent literals.
func TestApplyDefaultBrowserFingerprintHeaders(t *testing.T) {
	header := make(http.Header)
	ApplyDefaultBrowserFingerprintHeaders(header)

	want := map[string]string{
		"User-Agent":                  DefaultUserAgent,
		"Accept-Language":             "zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6",
		"Sec-Ch-Ua":                   `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`,
		"Sec-Ch-Ua-Full-Version-List": `"Microsoft Edge";v="143.0.3650.96", "Chromium";v="143.0.7499.147", "Not A(Brand";v="24.0.0.0"`,
		"Sec-Ch-Ua-Mobile":            "?0",
		"Sec-Ch-Ua-Platform":          `"Windows"`,
		"Sec-Ch-Ua-Platform-Version":  `"19.0.0"`,
	}
	for key, value := range want {
		if got := header.Get(key); got != value {
			t.Fatalf("%s = %q, want %q", key, got, value)
		}
	}
}

// TestAccountBrowserProfileOwnsHeadersAndTimezone reproduces the registration
// profile loss that made production requests revert to one unrelated global
// identity. Expected: the persisted account profile applies only allowlisted
// headers and computes the browser's real JavaScript timezone offset.
func TestAccountBrowserProfileOwnsHeadersAndTimezone(t *testing.T) {
	raw := `{"version":1,"headers":{"Accept-Language":"en-US,en;q=0.9","Sec-Ch-Ua":"\"Chromium\";v=\"146\", \"Google Chrome\";v=\"146\"","Sec-Ch-Ua-Mobile":"?0","Sec-Ch-Ua-Platform":"\"Windows\"","User-Agent":"Mozilla/5.0 Chrome/146.0.0.0 Safari/537.36"},"language":"en-US","timezone":"America/Los_Angeles"}`
	profile, canonical, err := ParseBrowserProfileJSON(raw)
	if err != nil {
		t.Fatalf("ParseBrowserProfileJSON() error = %v", err)
	}
	if canonical != raw {
		t.Fatalf("canonical profile = %q, want stable JSON", canonical)
	}

	header := make(http.Header)
	ApplyBrowserFingerprintHeaders(header, profile)
	if got := header.Get("User-Agent"); got != "Mozilla/5.0 Chrome/146.0.0.0 Safari/537.36" {
		t.Fatalf("User-Agent = %q, want account browser UA", got)
	}
	if got := header.Get("Accept-Language"); got != "en-US,en;q=0.9" {
		t.Fatalf("Accept-Language = %q, want account browser language", got)
	}
	if got := header.Get("Sec-Ch-Ua-Full-Version"); got != "" {
		t.Fatalf("uncaptured high-entropy header = %q, want absent", got)
	}

	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	if got := profile.TimezoneOffsetMinutes(at); got != 420 {
		t.Fatalf("timezone offset = %d, want 420 for PDT", got)
	}
}
