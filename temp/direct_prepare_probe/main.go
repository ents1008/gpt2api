package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/432539/gpt2api/internal/account"
	"github.com/432539/gpt2api/internal/config"
	"github.com/432539/gpt2api/internal/db"
	"github.com/432539/gpt2api/internal/proxy"
	"github.com/432539/gpt2api/internal/upstream/chatgpt"
	appcrypto "github.com/432539/gpt2api/pkg/crypto"
)

type probeResult struct {
	AccountID           uint64            `json:"account_id"`
	LocalStatus         string            `json:"local_status"`
	AdmissionState      string            `json:"admission_state"`
	AdmissionStage      int               `json:"admission_stage"`
	AdmissionReason     string            `json:"admission_reason,omitempty"`
	AdmissionNextCheck  string            `json:"admission_next_check,omitempty"`
	CredentialOrigin    string            `json:"credential_origin"`
	RefreshClientID     string            `json:"refresh_client_id"`
	TokenExpiresAt      string            `json:"token_expires_at,omitempty"`
	LastRefreshAt       string            `json:"last_refresh_at,omitempty"`
	ProxyBound          bool              `json:"proxy_bound"`
	DeviceIDPresent     bool              `json:"device_id_present"`
	CookieCanonical     bool              `json:"cookie_canonical"`
	CookieCount         int               `json:"cookie_count"`
	CookieDeviceMatch   bool              `json:"cookie_device_match"`
	CookieSnapshotCount int               `json:"cookie_snapshot_count"`
	HTTPStatus          int               `json:"http_status"`
	PrepareOK           bool              `json:"prepare_ok"`
	Persona             string            `json:"persona,omitempty"`
	Turnstile           bool              `json:"turnstile_required,omitempty"`
	ProofOfWork         bool              `json:"pow_required,omitempty"`
	Error               string            `json:"error,omitempty"`
	BodyLength          int               `json:"body_length,omitempty"`
	BodySHA256          string            `json:"body_sha256,omitempty"`
	BodyFields          map[string]string `json:"body_fields,omitempty"`
	BodyMarkers         []string          `json:"body_markers,omitempty"`
}

func main() {
	configPath := flag.String("config", "/etc/gpt2api/config.yaml", "gpt2api config path")
	accountID := flag.Uint64("account", 0, "account id")
	flag.Parse()
	if *accountID == 0 {
		fail("account id is required")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fail("load config: %v", err)
	}
	database, err := db.NewMySQL(cfg.MySQL)
	if err != nil {
		fail("open database: %v", err)
	}
	defer database.Close()

	cipher, err := appcrypto.NewAESGCM(cfg.Crypto.AESKey)
	if err != nil {
		fail("init cipher: %v", err)
	}
	accountService := account.NewService(account.NewDAO(database), cipher)
	proxyService := proxy.NewService(proxy.NewDAO(database), cipher)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	acc, err := accountService.Get(ctx, *accountID)
	if err != nil {
		fail("load account: %v", err)
	}
	result := probeResult{
		AccountID:        acc.ID,
		LocalStatus:      acc.Status,
		AdmissionState:   acc.AdmissionState,
		AdmissionStage:   acc.AdmissionStage,
		AdmissionReason:  acc.AdmissionReason,
		CredentialOrigin: acc.CredentialProvenance,
		RefreshClientID:  acc.RefreshClientID,
		DeviceIDPresent:  strings.TrimSpace(acc.OAIDeviceID) != "",
	}
	if acc.AdmissionNextCheckAt.Valid {
		result.AdmissionNextCheck = acc.AdmissionNextCheckAt.Time.Format(time.RFC3339)
	}
	if acc.TokenExpiresAt.Valid {
		result.TokenExpiresAt = acc.TokenExpiresAt.Time.Format(time.RFC3339)
	}
	if acc.LastRefreshAt.Valid {
		result.LastRefreshAt = acc.LastRefreshAt.Time.Format(time.RFC3339)
	}

	authToken, err := accountService.DecryptAuthToken(acc)
	if err != nil {
		fail("decrypt account token: %v", err)
	}
	cookies, err := accountService.DecryptCookies(ctx, acc.ID)
	if err != nil {
		fail("decrypt account cookies: %v", err)
	}
	result.CookieCanonical, result.CookieCount, result.CookieDeviceMatch = cookieSummary(cookies, acc.OAIDeviceID)
	binding, err := accountService.GetBinding(ctx, acc.ID)
	if err != nil {
		fail("load proxy binding: %v", err)
	}
	proxyURL := ""
	if binding != nil {
		boundProxy, getErr := proxyService.Get(ctx, binding.ProxyID)
		if getErr != nil {
			fail("load bound proxy: %v", getErr)
		}
		if boundProxy.Enabled {
			proxyURL, err = proxyService.BuildURL(boundProxy)
			if err != nil {
				fail("build bound proxy: %v", err)
			}
			result.ProxyBound = proxyURL != ""
		}
	}

	client, err := chatgpt.New(chatgpt.Options{
		AuthToken: authToken,
		DeviceID:  acc.OAIDeviceID,
		SessionID: acc.OAISessionID,
		ProxyURL:  proxyURL,
		Cookies:   cookies,
		Timeout:   30 * time.Second,
	})
	if err != nil {
		fail("init upstream client: %v", err)
	}
	prepare, err := client.ChatRequirementsPrepare(ctx)
	if snapshot, snapshotErr := client.CookieJSON(); snapshotErr == nil {
		_, result.CookieSnapshotCount, _ = cookieSummary(snapshot, acc.OAIDeviceID)
	}
	if err == nil {
		result.HTTPStatus = 200
		result.PrepareOK = true
		result.Persona = prepare.Persona
		result.Turnstile = prepare.Turnstile.Required
		result.ProofOfWork = prepare.Proofofwork.Required
		writeResult(result)
		return
	}

	var upstream *chatgpt.UpstreamError
	if !errors.As(err, &upstream) {
		result.Error = err.Error()
		writeResult(result)
		return
	}
	result.HTTPStatus = upstream.Status
	result.Error = upstream.Message
	result.BodyLength = len(upstream.Body)
	sum := sha256.Sum256([]byte(upstream.Body))
	result.BodySHA256 = hex.EncodeToString(sum[:])
	result.BodyFields = safeBodyFields(upstream.Body)
	result.BodyMarkers = bodyMarkers(upstream.Body)
	writeResult(result)
}

func cookieSummary(raw, deviceID string) (canonical bool, count int, deviceMatch bool) {
	normalized, err := chatgpt.NormalizeCookieJSON(raw, chatgpt.BaseURL)
	if err != nil {
		return false, 0, false
	}
	var cookies []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if json.Unmarshal([]byte(normalized), &cookies) != nil {
		return false, 0, false
	}
	for _, cookie := range cookies {
		if cookie.Name == "oai-did" && cookie.Value == deviceID {
			deviceMatch = true
		}
	}
	return strings.TrimSpace(raw) == normalized, len(cookies), deviceMatch
}

func safeBodyFields(body string) map[string]string {
	var value any
	if json.Unmarshal([]byte(body), &value) != nil {
		return nil
	}
	allowed := map[string]bool{
		"code": true, "detail": true, "error_code": true, "error_type": true,
		"message": true, "status": true, "type": true,
	}
	out := make(map[string]string)
	var walk func(string, any)
	walk = func(path string, current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				next := key
				if path != "" {
					next = path + "." + key
				}
				walk(next, child)
			}
		case string:
			key := path
			if index := strings.LastIndexByte(key, '.'); index >= 0 {
				key = key[index+1:]
			}
			if allowed[strings.ToLower(key)] {
				out[path] = safeText(typed)
			}
		case float64, bool:
			key := path
			if index := strings.LastIndexByte(key, '.'); index >= 0 {
				key = key[index+1:]
			}
			if allowed[strings.ToLower(key)] {
				out[path] = fmt.Sprint(typed)
			}
		}
	}
	walk("", value)
	if len(out) == 0 {
		return nil
	}
	return out
}

func bodyMarkers(body string) []string {
	lower := strings.ToLower(body)
	known := []string{
		"auth_required", "expired", "invalid_grant", "invalid_token",
		"session", "token_invalidated", "unauthorized",
	}
	found := make([]string, 0, len(known))
	for _, marker := range known {
		if strings.Contains(lower, marker) {
			found = append(found, marker)
		}
	}
	sort.Strings(found)
	return found
}

func safeText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 400 {
		return value[:400]
	}
	return value
}

func writeResult(result probeResult) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(result); err != nil {
		fail("encode result: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
