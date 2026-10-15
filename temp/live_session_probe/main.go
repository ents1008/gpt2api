package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

type sessionProbeInput struct {
	AuthToken      string `json:"auth_token"`
	DeviceID       string `json:"device_id"`
	SessionID      string `json:"session_id"`
	SessionToken   string `json:"session_token"`
	Cookies        string `json:"cookies"`
	ProxyURL       string `json:"proxy_url"`
	BrowserProfile string `json:"browser_profile"`
}

type sessionProbeResult struct {
	OK                   bool   `json:"ok"`
	Stage                string `json:"stage"`
	Status               int    `json:"status,omitempty"`
	ErrorClass           string `json:"error_class,omitempty"`
	AccessTokenPresent   bool   `json:"access_token_present,omitempty"`
	AccessTokenUnchanged bool   `json:"access_token_unchanged,omitempty"`
	AccessTokenExpiresIn int64  `json:"access_token_expires_in,omitempty"`
}

func main() {
	var input sessionProbeInput
	if err := json.Unmarshal([]byte(os.Getenv("GPT2API_LIVE_SESSION_PROBE")), &input); err != nil {
		writeSessionProbeResult(sessionProbeResult{Stage: "input", ErrorClass: "invalid_json"})
		os.Exit(2)
	}
	if errorClass := validateSessionProbeInput(input); errorClass != "" {
		writeSessionProbeResult(sessionProbeResult{Stage: "input", ErrorClass: errorClass})
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	writeSessionProbeResult(runSessionProbe(ctx, input))
}

func validateSessionProbeInput(input sessionProbeInput) string {
	switch {
	case strings.TrimSpace(input.AuthToken) == "":
		return "missing_auth_token"
	case strings.TrimSpace(input.DeviceID) == "":
		return "missing_device_id"
	case strings.TrimSpace(input.SessionID) == "":
		return "missing_session_id"
	case strings.TrimSpace(input.SessionToken) == "":
		return "missing_session_token"
	case strings.TrimSpace(input.Cookies) == "":
		return "missing_cookies"
	case strings.TrimSpace(input.ProxyURL) == "":
		return "missing_proxy"
	case strings.TrimSpace(input.BrowserProfile) == "":
		return "missing_browser_profile"
	default:
		return ""
	}
}

func runSessionProbe(ctx context.Context, input sessionProbeInput) sessionProbeResult {
	transport, err := chatgpt.NewUTLSTransport(input.ProxyURL, 15*time.Second)
	if err != nil {
		return sessionProbeResult{Stage: "init", ErrorClass: "transport_init_failed"}
	}
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return sessionProbeResult{Stage: "init", ErrorClass: "cookie_jar_failed"}
	}
	if err := chatgpt.LoadCookies(jar, chatgpt.BaseURL, input.Cookies); err != nil {
		return sessionProbeResult{Stage: "input", ErrorClass: "invalid_cookies"}
	}
	sessionURL, err := url.Parse(chatgpt.BaseURL + "/api/auth/session")
	if err != nil {
		return sessionProbeResult{Stage: "init", ErrorClass: "invalid_session_url"}
	}
	jar.SetCookies(sessionURL, []*http.Cookie{{
		Name: "__Secure-next-auth.session-token", Value: input.SessionToken,
		Domain: sessionURL.Hostname(), Path: "/", Secure: true, HttpOnly: true,
	}})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sessionURL.String(), nil)
	if err != nil {
		return sessionProbeResult{Stage: "init", ErrorClass: "request_build_failed"}
	}
	profile, _, err := chatgpt.ParseBrowserProfileJSON(input.BrowserProfile)
	if err != nil {
		return sessionProbeResult{Stage: "input", ErrorClass: "invalid_browser_profile"}
	}
	chatgpt.ApplyBrowserFingerprintHeaders(request.Header, profile)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Referer", chatgpt.BaseURL+"/")
	request.Header.Set("Oai-Device-Id", input.DeviceID)
	request.Header.Set("Oai-Session-Id", input.SessionID)
	response, err := (&http.Client{Transport: transport, Jar: jar, Timeout: 25 * time.Second}).Do(request)
	if err != nil {
		return sessionProbeResult{Stage: "session", ErrorClass: classifySessionTransportError(err)}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 128*1024))
	if err != nil {
		return sessionProbeResult{Stage: "session", Status: response.StatusCode, ErrorClass: "response_read_failed"}
	}
	if response.StatusCode != http.StatusOK {
		return sessionProbeResult{Stage: "session", Status: response.StatusCode, ErrorClass: classifySessionStatus(response.StatusCode)}
	}
	var payload struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return sessionProbeResult{Stage: "session", Status: response.StatusCode, ErrorClass: "invalid_response_json"}
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return sessionProbeResult{Stage: "session", Status: response.StatusCode, ErrorClass: "access_token_missing"}
	}
	return sessionProbeResult{
		OK:                   true,
		Stage:                "session",
		Status:               response.StatusCode,
		AccessTokenPresent:   true,
		AccessTokenUnchanged: constantTokenEqual(payload.AccessToken, input.AuthToken),
		AccessTokenExpiresIn: tokenSecondsUntilExpiry(payload.AccessToken, time.Now()),
	}
}

func classifySessionStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusTooManyRequests:
		return "rate_limited"
	default:
		return "upstream_error"
	}
}

func classifySessionTransportError(err error) string {
	if err == context.DeadlineExceeded || strings.Contains(strings.ToLower(err.Error()), "timeout") {
		return "timeout"
	}
	return "transport_error"
}

func constantTokenEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return leftHash == rightHash
}

func tokenSecondsUntilExpiry(token string, now time.Time) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return 0
	}
	return claims.Exp - now.Unix()
}

func writeSessionProbeResult(result sessionProbeResult) {
	raw, err := json.Marshal(result)
	if err != nil {
		fmt.Println(`{"ok":false,"stage":"output","error_class":"encode_failed"}`)
		return
	}
	fmt.Println(string(raw))
}
