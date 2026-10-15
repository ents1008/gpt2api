package chatgpt

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

type cookieRecordForTest struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

// TestNormalizeCookieJSONAcceptsRegistrationCookieHeader reproduces the live
// account-import shape emitted by register_bridge before this repair. Expected:
// the structured normalizer converts the header into deterministic JSON so the
// existing account can load its device/session cookies without token re-import.
func TestNormalizeCookieJSONAcceptsRegistrationCookieHeader(t *testing.T) {
	raw := "oai-did=device-for-test; __Secure-next-auth.session-token=session-for-test"
	normalized, err := NormalizeCookieJSON(raw, BaseURL)
	if err != nil {
		t.Fatalf("NormalizeCookieJSON() error = %v", err)
	}

	var records []cookieRecordForTest
	if err := json.Unmarshal([]byte(normalized), &records); err != nil {
		t.Fatalf("normalized cookies are not JSON: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("cookie count = %d, want 2", len(records))
	}
	if records[0].Name != "__Secure-next-auth.session-token" || records[1].Name != "oai-did" {
		t.Fatalf("cookie order/names = %q, %q", records[0].Name, records[1].Name)
	}
	for _, record := range records {
		if record.Domain != "chatgpt.com" || record.Path != "/" {
			t.Fatalf("cookie scope = %q %q, want chatgpt.com /", record.Domain, record.Path)
		}
	}
}

// TestClientCookieJSONCapturesRuntimeUpdates verifies browser cookies received
// during bootstrap are not lost when the per-attempt HTTP client is discarded.
// Expected: the exported snapshot contains both the imported device cookie and
// a new upstream cookie in deterministic canonical JSON.
func TestClientCookieJSONCapturesRuntimeUpdates(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	if err := loadCookies(jar, BaseURL, `[{"name":"oai-did","value":"device-for-test","domain":"chatgpt.com","path":"/"}]`); err != nil {
		t.Fatalf("loadCookies() error = %v", err)
	}
	u, _ := url.Parse(BaseURL)
	jar.SetCookies(u, []*http.Cookie{{Name: "runtime-cookie", Value: "runtime-for-test", Path: "/"}})
	client := &Client{opts: Options{BaseURL: BaseURL}, hc: &http.Client{Jar: jar}}

	raw, err := client.CookieJSON()
	if err != nil {
		t.Fatalf("CookieJSON() error = %v", err)
	}
	var records []cookieRecordForTest
	if err := json.Unmarshal([]byte(raw), &records); err != nil {
		t.Fatalf("CookieJSON() returned invalid JSON: %v", err)
	}
	if len(records) != 2 || records[0].Name != "oai-did" || records[1].Name != "runtime-cookie" {
		t.Fatalf("cookie snapshot = %#v, want oai-did + runtime-cookie", records)
	}
}

// TestNormalizeCookieJSONRejectsMalformedJSON prevents corrupt cookie state
// from being encrypted and silently ignored at runtime. Expected: malformed
// JSON-looking input is rejected rather than reinterpreted as a Cookie header.
func TestNormalizeCookieJSONRejectsMalformedJSON(t *testing.T) {
	if _, err := NormalizeCookieJSON(`[{"name":`, BaseURL); err == nil {
		t.Fatal("NormalizeCookieJSON() accepted malformed JSON")
	}
}

// TestNormalizeCookieJSONDropsForeignOAuthCookies reproduces the legacy
// account snapshot that blocked account 1226 before any upstream request.
// Expected: auth.openai.com cookies are discarded while valid chatgpt.com
// cookies remain loadable and deterministic.
func TestNormalizeCookieJSONDropsForeignOAuthCookies(t *testing.T) {
	raw := `[
		{"name":"login_session","value":"oauth-only","domain":"auth.openai.com","path":"/"},
		{"name":"oai-did","value":"device-for-test","domain":"chatgpt.com","path":"/"}
	]`
	normalized, err := NormalizeCookieJSON(raw, BaseURL)
	if err != nil {
		t.Fatalf("NormalizeCookieJSON() error = %v", err)
	}
	var records []cookieRecordForTest
	if err := json.Unmarshal([]byte(normalized), &records); err != nil {
		t.Fatalf("normalized cookies are not JSON: %v", err)
	}
	if len(records) != 1 || records[0].Name != "oai-did" || records[0].Domain != "chatgpt.com" {
		t.Fatalf("normalized cookies = %#v, want only chatgpt.com oai-did", records)
	}
}

// TestLoadCookiesTreatsForeignOnlySnapshotAsEmpty keeps obsolete OAuth
// cookies from making an otherwise valid AT/RT account unusable. Expected: an
// all-foreign legacy snapshot loads as an empty jar so bootstrap can seed the
// current ChatGPT cookie set.
func TestLoadCookiesTreatsForeignOnlySnapshotAsEmpty(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	raw := `[{"name":"login_session","value":"oauth-only","domain":"auth.openai.com","path":"/"}]`
	if err := loadCookies(jar, BaseURL, raw); err != nil {
		t.Fatalf("loadCookies() error = %v", err)
	}
	u, _ := url.Parse(BaseURL)
	if cookies := jar.Cookies(u); len(cookies) != 0 {
		t.Fatalf("foreign-only snapshot loaded cookies: %#v", cookies)
	}
}

// TestLoadCookiesDropsOnlyRFC6265UnsendableBrowserCookies reproduces a browser
// export containing quoted login-state JSON. Expected: the unsendable g_state
// entry never reaches net/http's mutating Cookie header path, while the valid
// ChatGPT session and device cookies retain their exact values.
func TestLoadCookiesDropsOnlyRFC6265UnsendableBrowserCookies(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New() error = %v", err)
	}
	raw := `[
		{"name":"g_state","value":"{\"i_l\":0,\"i_ll\":0}","domain":".chatgpt.com","path":"/"},
		{"name":"oai-did","value":"device-id","domain":".chatgpt.com","path":"/"},
		{"name":"__Secure-next-auth.session-token","value":"session-token","domain":".chatgpt.com","path":"/"}
	]`
	if err := loadCookies(jar, "https://chatgpt.com", raw); err != nil {
		t.Fatalf("loadCookies() error = %v", err)
	}

	request, err := http.NewRequest(http.MethodGet, "https://chatgpt.com/", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	for _, cookie := range jar.Cookies(request.URL) {
		request.AddCookie(cookie)
	}
	got := request.Header.Get("Cookie")
	if strings.Contains(got, "g_state=") {
		t.Fatalf("Cookie header contains unsendable g_state: %q", got)
	}
	if !strings.Contains(got, "oai-did=device-id") {
		t.Fatalf("Cookie header lost device cookie: %q", got)
	}
	if !strings.Contains(got, "__Secure-next-auth.session-token=session-token") {
		t.Fatalf("Cookie header lost session cookie: %q", got)
	}
}
