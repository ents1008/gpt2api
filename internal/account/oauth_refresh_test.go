package account

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type staticProxyResolver string

func (r staticProxyResolver) ProxyURLForAccount(context.Context, uint64) string {
	return string(r)
}

type fakeImageATPreparer struct {
	err error
}

func (p fakeImageATPreparer) ChatRequirementsPrepare(context.Context) (*chatgpt.ChatRequirementsPrepareResp, error) {
	return &chatgpt.ChatRequirementsPrepareResp{}, p.err
}

// TestImageATVerificationUsesPrepareStatus defines the authoritative refresh
// acceptance check. Expected: a successful prepare verifies the AT, while the
// exact upstream token_invalidated 401 remains a failed verification and can
// never restore the account to healthy.
func TestImageATVerificationUsesPrepareStatus(t *testing.T) {
	if status, err := verifyImageATWithPreparer(context.Background(), fakeImageATPreparer{}); err != nil || status != http.StatusOK {
		t.Fatalf("successful prepare = status %d err %v", status, err)
	}
	upstream := &chatgpt.UpstreamError{Status: http.StatusUnauthorized, Message: "chat-requirements/prepare failed"}
	if status, err := verifyImageATWithPreparer(context.Background(), fakeImageATPreparer{err: upstream}); err == nil || status != http.StatusUnauthorized {
		t.Fatalf("401 prepare = status %d err %v", status, err)
	}
}

// TestImageATVerificationOptionsPreserveAccountIdentity ensures the no-quota
// refresh verification path uses the same account-local proxy, device,
// session, and cookie state as normal image generation.
func TestImageATVerificationOptionsPreserveAccountIdentity(t *testing.T) {
	a := &Account{OAIDeviceID: "device-for-test", OAISessionID: "session-for-test"}
	options := imageATVerificationOptions(a, "at-for-test", "http://proxy-for-test:8080", `[{"name":"oai-did"}]`)
	if options.AuthToken != "at-for-test" || options.DeviceID != "device-for-test" || options.SessionID != "session-for-test" || options.ProxyURL != "http://proxy-for-test:8080" || options.Cookies != `[{"name":"oai-did"}]` {
		t.Fatalf("verification options lost account identity: %+v", options)
	}
}

// TestRefresherRejectsInvalidBoundProxyWithoutDirectFallback protects account
// IP continuity. Expected: an invalid non-empty bound proxy is returned as an
// error and never replaced by the default direct client.
func TestRefresherRejectsInvalidBoundProxyWithoutDirectFallback(t *testing.T) {
	r := &Refresher{
		client:        &http.Client{},
		proxyResolver: staticProxyResolver("://invalid-proxy"),
	}
	if _, err := r.clientFor(context.Background(), 7); err == nil {
		t.Fatal("clientFor accepted an invalid bound proxy")
	}
}

// TestRefresherRejectsMissingBoundProxyWithoutDirectFallback protects network
// identity continuity when the resolver is configured but a binding vanishes.
// Expected: refresh fails closed instead of silently using the server's IP.
func TestRefresherRejectsMissingBoundProxyWithoutDirectFallback(t *testing.T) {
	r := &Refresher{
		client:        &http.Client{},
		proxyResolver: staticProxyResolver(""),
	}
	if _, err := r.clientFor(context.Background(), 7); err == nil {
		t.Fatal("clientFor silently fell back to direct transport")
	}
}

// TestAccountRefreshClientIDSeparatesRuntimeAndRecoveryOwnership ensures a
// ChatGPT Web runtime client can coexist with a Platform-issued RT. Expected:
// refresh uses refresh_client_id and only falls back for legacy records.
func TestAccountRefreshClientIDSeparatesRuntimeAndRecoveryOwnership(t *testing.T) {
	a := &Account{ClientID: "app_web", RefreshClientID: "app_platform"}
	if got := accountRefreshClientID(a, "app_default"); got != "app_platform" {
		t.Fatalf("refresh client = %q, want app_platform", got)
	}
	a.RefreshClientID = ""
	if got := accountRefreshClientID(a, "app_default"); got != "app_web" {
		t.Fatalf("legacy refresh client = %q, want app_web", got)
	}
}

// TestBrowserQuarantineRefreshIsImmutable protects the uncontaminated one-hour
// survival control. Expected: every refresh entrypoint reaches this shared
// boundary and returns skipped before decrypting or exchanging AT, RT, or ST.
func TestBrowserQuarantineRefreshIsImmutable(t *testing.T) {
	r := &Refresher{}
	a := &Account{
		ID:                   1939,
		CredentialProvenance: CredentialProvenanceBrowserWeb,
		AdmissionState:       AdmissionStateQuarantined,
	}

	result, err := r.refreshAutoUnlocked(context.Background(), a, refreshScope{})
	if err != nil {
		t.Fatalf("refreshAutoUnlocked returned error: %v", err)
	}
	if result == nil || result.OK || result.Source != "skipped" {
		t.Fatalf("refresh result = %+v, want immutable skipped result", result)
	}
}

// TestRefresherSerializesSameAccountRefresh protects rotating RT ownership
// inside one process. Expected: two refresh paths for the same account never
// overlap the exchange/verify/persist critical section.
func TestRefresherSerializesSameAccountRefresh(t *testing.T) {
	r := &Refresher{}
	var active int32
	var maxActive int32
	var wg sync.WaitGroup

	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := r.lockAccountRefresh(77)
			current := atomic.AddInt32(&active, 1)
			for {
				observed := atomic.LoadInt32(&maxActive)
				if current <= observed || atomic.CompareAndSwapInt32(&maxActive, observed, current) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			release()
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxActive); got != 1 {
		t.Fatalf("maximum concurrent refreshes = %d, want 1", got)
	}
}

// TestRTToATUsesPlatformFormOAuthRefresh protects the refresh contract proven
// by a Platform-consent app_2SK RT. Expected: the request is form encoded with
// exactly client_id, grant_type, and refresh_token; redirect_uri is absent.
func TestRTToATUsesPlatformFormOAuthRefresh(t *testing.T) {
	var captured url.Values
	r := &Refresher{
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			assertPlatformBrowserHeaders(t, req.Header)
			if got := req.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
				t.Fatalf("Content-Type = %q, want application/x-www-form-urlencoded", got)
			}
			raw, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			captured, err = url.ParseQuery(string(raw))
			if err != nil {
				t.Fatalf("parse form body: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"access_token":"access.test.token","refresh_token":"rotated-rt","expires_in":3600}`)),
				Request:    req,
			}, nil
		})},
	}

	at, rt, _, err := r.rtToAT(context.Background(), 1, "original-rt", "app_2SKx67EdpoN0G6j64rFvigXD")
	if err != nil {
		t.Fatalf("rtToAT returned error: %v", err)
	}
	if at != "access.test.token" || rt != "rotated-rt" {
		t.Fatalf("tokens = (%q, %q), want access.test.token / rotated-rt", at, rt)
	}
	assertOAuthRefreshForm(t, captured, "original-rt", "app_2SKx67EdpoN0G6j64rFvigXD")
}

// TestImportTokenRTExchangeUsesPlatformFormOAuthRefresh verifies that RT
// import and background refresh cannot diverge. Expected: import sends the
// same three-field app_2SK Platform form grant as normal account refresh.
func TestImportTokenRTExchangeUsesPlatformFormOAuthRefresh(t *testing.T) {
	var captured url.Values
	httpc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		assertPlatformBrowserHeaders(t, req.Header)
		if got := req.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("Content-Type = %q, want application/x-www-form-urlencoded", got)
		}
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		captured, err = url.ParseQuery(string(raw))
		if err != nil {
			t.Fatalf("parse form body: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"access.test.token","refresh_token":"rotated-rt","expires_in":3600}`)),
			Request:    req,
		}, nil
	})}

	at, rt, _, err := rtExchange(context.Background(), httpc, "original-rt", "app_2SKx67EdpoN0G6j64rFvigXD")
	if err != nil {
		t.Fatalf("rtExchange returned error: %v", err)
	}
	if at != "access.test.token" || rt != "rotated-rt" {
		t.Fatalf("tokens = (%q, %q), want access.test.token / rotated-rt", at, rt)
	}
	assertOAuthRefreshForm(t, captured, "original-rt", "app_2SKx67EdpoN0G6j64rFvigXD")
}

// TestCodexOAuthRefreshMatchesOfficialClientContract protects RTs issued by
// the official Codex client. Expected: app_EM refresh uses the same JSON body
// as Codex itself and never sends a redirect_uri from an unrelated iOS flow.
func TestCodexOAuthRefreshMatchesOfficialClientContract(t *testing.T) {
	req, err := newOAuthRefreshRequest(
		context.Background(),
		"original-rt",
		"app_EMoamEEZ73f0CkXaXp7hrann",
	)
	if err != nil {
		t.Fatalf("newOAuthRefreshRequest returned error: %v", err)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		t.Fatalf("decode refresh request: %v", err)
	}
	want := map[string]string{
		"client_id":     "app_EMoamEEZ73f0CkXaXp7hrann",
		"grant_type":    "refresh_token",
		"refresh_token": "original-rt",
	}
	if len(body) != len(want) {
		t.Fatalf("refresh body keys = %v, want exactly %v", body, want)
	}
	for key, value := range want {
		if body[key] != value {
			t.Fatalf("body[%s] = %q, want %q", key, body[key], value)
		}
	}
	if _, exists := body["redirect_uri"]; exists {
		t.Fatalf("Codex refresh body unexpectedly contains redirect_uri: %v", body)
	}
}

// TestSTToATReusesAccountBrowserSession proves Session Token recovery preserves
// the same Web identity as image requests. Expected: the request carries the
// persisted device/session headers, canonical browser profile, existing
// device cookie, and the explicitly supplied NextAuth session token.
func TestSTToATReusesAccountBrowserSession(t *testing.T) {
	r := &Refresher{
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			assertPlatformBrowserHeaders(t, req.Header)
			if got := req.Header.Get("Oai-Device-Id"); got != "device-for-test" {
				t.Fatalf("Oai-Device-Id = %q", got)
			}
			if got := req.Header.Get("Oai-Session-Id"); got != "session-for-test" {
				t.Fatalf("Oai-Session-Id = %q", got)
			}
			if cookie, err := req.Cookie("oai-did"); err != nil || cookie.Value != "device-for-test" {
				t.Fatalf("oai-did cookie = %v, err = %v", cookie, err)
			}
			if cookie, err := req.Cookie("__Secure-next-auth.session-token"); err != nil || cookie.Value != "st-for-test" {
				t.Fatalf("session cookie = %v, err = %v", cookie, err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"accessToken":"access.test.token"}`)),
				Request:    req,
			}, nil
		})},
	}
	account := &Account{
		ID:             7,
		OAIDeviceID:    "device-for-test",
		OAISessionID:   "session-for-test",
		BrowserProfile: chatgpt.DefaultBrowserProfileJSON(),
	}
	cookies := `[{"name":"oai-did","value":"device-for-test","domain":"chatgpt.com","path":"/"}]`

	at, _, err := r.stToAT(context.Background(), account, "st-for-test", cookies)
	if err != nil {
		t.Fatalf("stToAT returned error: %v", err)
	}
	if at != "access.test.token" {
		t.Fatalf("access token = %q", at)
	}
}

func assertPlatformBrowserHeaders(t *testing.T, header http.Header) {
	t.Helper()
	if got := header.Get("User-Agent"); got != chatgpt.DefaultUserAgent {
		t.Fatalf("User-Agent = %q, want canonical Web UA", got)
	}
	if got := header.Get("Sec-Ch-Ua-Platform-Version"); got != `"19.0.0"` {
		t.Fatalf("Sec-Ch-Ua-Platform-Version = %q, want canonical Web platform", got)
	}
}

func assertOAuthRefreshForm(t *testing.T, form url.Values, refreshToken, clientID string) {
	t.Helper()
	want := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     clientID,
	}
	if len(form) != len(want) {
		t.Fatalf("refresh form keys = %v, want exactly %v", form, want)
	}
	for key, value := range want {
		if got := form.Get(key); got != value {
			t.Fatalf("form[%s] = %q, want %q; full form=%v", key, got, value, form)
		}
	}
}
