package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/432539/gpt2api/internal/account"
	"github.com/432539/gpt2api/internal/config"
	"github.com/432539/gpt2api/internal/db"
	"github.com/432539/gpt2api/internal/proxy"
	"github.com/432539/gpt2api/internal/upstream/chatgpt"
	appcrypto "github.com/432539/gpt2api/pkg/crypto"
)

type probeResult struct {
	SessionHTTPStatus int               `json:"session_http_status"`
	SessionBodyLen    int               `json:"session_body_len"`
	SessionBodyHash   string            `json:"session_body_sha256"`
	SessionBodyFields map[string]string `json:"session_body_fields,omitempty"`
	FreshATLength     int               `json:"fresh_at_length,omitempty"`
	FreshATHash       string            `json:"fresh_at_sha256,omitempty"`
	StoredATHash      string            `json:"stored_at_sha256,omitempty"`
	FreshATMatchesStored bool           `json:"fresh_at_matches_stored,omitempty"`
	PrepareHTTPStatus int               `json:"prepare_http_status,omitempty"`
	PrepareError      string            `json:"prepare_error,omitempty"`
	PreparePersona    string            `json:"prepare_persona,omitempty"`
}

func main() {
	configPath := flag.String("config", "/etc/gpt2api/config.yaml", "gpt2api config path")
	accountID := flag.Uint64("account", 0, "account id")
	flag.Parse()
	if *accountID == 0 {
		fatal("account id is required")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config: %v", err)
	}
	database, err := db.NewMySQL(cfg.MySQL)
	if err != nil {
		fatal("open database: %v", err)
	}
	defer database.Close()
	cipher, err := appcrypto.NewAESGCM(cfg.Crypto.AESKey)
	if err != nil {
		fatal("init cipher: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	accountService := account.NewService(account.NewDAO(database), cipher)
	proxyService := proxy.NewService(proxy.NewDAO(database), cipher)
	acc, err := accountService.Get(ctx, *accountID)
	if err != nil {
		fatal("load account: %v", err)
	}
	secrets, err := accountService.GetSecrets(ctx, acc.ID)
	if err != nil {
		fatal("decrypt account secrets: %v", err)
	}
	st := secrets.SessionToken
	cookies, err := accountService.DecryptCookies(ctx, acc.ID)
	if err != nil {
		fatal("decrypt cookies: %v", err)
	}
	proxyURL := ""
	if binding, bindingErr := accountService.GetBinding(ctx, acc.ID); bindingErr == nil && binding != nil {
		boundProxy, getErr := proxyService.Get(ctx, binding.ProxyID)
		if getErr != nil {
			fatal("load proxy: %v", getErr)
		}
		if boundProxy.Enabled {
			proxyURL, err = proxyService.BuildURL(boundProxy)
			if err != nil {
				fatal("build proxy: %v", err)
			}
		}
	}
	transport, err := chatgpt.NewUTLSTransport(proxyURL, 30*time.Second)
	if err != nil {
		fatal("init transport: %v", err)
	}
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		fatal("init cookie jar: %v", err)
	}
	if err := chatgpt.LoadCookies(jar, chatgpt.BaseURL, cookies); err != nil {
		fatal("load cookies: %v", err)
	}
	sessionURL, err := url.Parse(chatgpt.BaseURL + "/api/auth/session")
	if err != nil {
		fatal("parse session url: %v", err)
	}
	jar.SetCookies(sessionURL, []*http.Cookie{
		{Name: "__Secure-next-auth.session-token", Value: st, Domain: sessionURL.Hostname(), Path: "/", Secure: true, HttpOnly: true},
	})
	hc := &http.Client{Transport: transport, Jar: jar, Timeout: 30 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sessionURL.String(), nil)
	if err != nil {
		fatal("build session request: %v", err)
	}
	chatgpt.ApplyDefaultBrowserFingerprintHeaders(request.Header)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Referer", chatgpt.BaseURL+"/")
	request.Header.Set("Oai-Device-Id", acc.OAIDeviceID)
	if acc.OAISessionID != "" {
		request.Header.Set("Oai-Session-Id", acc.OAISessionID)
	}
	response, err := hc.Do(request)
	if err != nil {
		fatal("session request: %v", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 128*1024))
	response.Body.Close()
	if readErr != nil {
		fatal("read session response: %v", readErr)
	}
	hash := sha256.Sum256(body)
	result := probeResult{
		SessionHTTPStatus: response.StatusCode,
		SessionBodyLen:    len(body),
		SessionBodyHash:   hex.EncodeToString(hash[:]),
		SessionBodyFields: safeFields(body),
	}
	if secrets.AuthToken != "" {
		result.StoredATHash = shortHash(secrets.AuthToken)
	}
	var session struct {
		AccessToken string `json:"accessToken"`
	}
	if json.Unmarshal(body, &session) == nil && session.AccessToken != "" {
		atHash := sha256.Sum256([]byte(session.AccessToken))
		result.FreshATLength = len(session.AccessToken)
		result.FreshATHash = hex.EncodeToString(atHash[:])
		result.FreshATMatchesStored = secrets.AuthToken == session.AccessToken
		client, clientErr := chatgpt.New(chatgpt.Options{
			AuthToken: session.AccessToken,
			DeviceID:  acc.OAIDeviceID,
			SessionID: acc.OAISessionID,
			ProxyURL:  proxyURL,
			Cookies:   cookies,
			Timeout:   30 * time.Second,
		})
		if clientErr != nil {
			result.PrepareError = clientErr.Error()
		} else {
			prepare, prepareErr := client.ChatRequirementsPrepare(ctx)
			if prepareErr != nil {
				result.PrepareError = prepareErr.Error()
				if upstream, ok := prepareErr.(*chatgpt.UpstreamError); ok {
					result.PrepareHTTPStatus = upstream.Status
				}
			} else {
				result.PrepareHTTPStatus = http.StatusOK
				result.PreparePersona = prepare.Persona
			}
		}
	}
	encoded, _ := json.Marshal(result)
	fmt.Println(string(encoded))
}

func safeFields(body []byte) map[string]string {
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		return nil
	}
	out := make(map[string]string)
	for _, key := range []string{"accessToken", "expires", "error", "message", "status", "user"} {
		if raw, ok := value[key]; ok {
			switch item := raw.(type) {
			case string:
				out[key] = fmt.Sprintf("len=%d sha256=%s", len(item), shortHash(item))
			default:
				out[key] = fmt.Sprintf("type=%T", raw)
			}
		}
	}
	return out
}

func shortHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])[:16]
}

func fatal(format string, args ...any) {
	message := strings.TrimSpace(fmt.Sprintf(format, args...))
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
