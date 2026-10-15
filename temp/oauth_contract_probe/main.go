package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/432539/gpt2api/internal/account"
	"github.com/432539/gpt2api/internal/config"
	"github.com/432539/gpt2api/internal/db"
	"github.com/432539/gpt2api/internal/proxy"
	"github.com/432539/gpt2api/internal/upstream/chatgpt"
	appcrypto "github.com/432539/gpt2api/pkg/crypto"
)

const (
	clientID    = "app_2SKx67EdpoN0G6j64rFvigXD"
	redirectURI = "com.openai.chat://auth0.openai.com/ios/com.openai.chat/callback"
	tokenURL    = "https://auth.openai.com/oauth/token"
)

type probeResult struct {
	AccountID       uint64 `json:"account_id"`
	ProxyID         uint64 `json:"proxy_id"`
	Contract        string `json:"contract"`
	HTTPStatus      int    `json:"http_status"`
	ErrorCode       string `json:"error_code,omitempty"`
	ErrorType       string `json:"error_type,omitempty"`
	HasAccessToken  bool   `json:"has_access_token"`
	RotatedRTSaved  bool   `json:"rotated_rt_saved"`
	ResponseElapsed int64  `json:"response_elapsed_ms"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Error        struct {
		Code string `json:"code"`
		Type string `json:"type"`
	} `json:"error"`
}

func main() {
	configPath := flag.String("config", "/etc/gpt2api/config.yaml", "gpt2api config path")
	accountID := flag.Uint64("account", 0, "account id")
	contract := flag.String("contract", "ios-json", "refresh contract: ios-json or platform-form")
	flag.Parse()
	if *accountID == 0 {
		fail("account id is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
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
	accountDAO := account.NewDAO(database)
	accountService := account.NewService(accountDAO, cipher)
	proxyService := proxy.NewService(proxy.NewDAO(database), cipher)
	item, err := accountService.Get(ctx, *accountID)
	if err != nil {
		fail("load account: %v", err)
	}
	if strings.TrimSpace(item.RefreshClientID) != clientID {
		fail("account refresh client does not match app_2SK")
	}
	secrets, err := accountService.GetSecrets(ctx, *accountID)
	if err != nil || strings.TrimSpace(secrets.RefreshToken) == "" {
		fail("load account RT: %v", err)
	}
	binding, err := accountService.GetBinding(ctx, *accountID)
	if err != nil || binding == nil {
		fail("load proxy binding: %v", err)
	}
	proxyItem, err := proxyService.Get(ctx, binding.ProxyID)
	if err != nil || proxyItem == nil || !proxyItem.Enabled {
		fail("load enabled proxy: %v", err)
	}
	proxyURL, err := proxyService.BuildURL(proxyItem)
	if err != nil {
		fail("build proxy URL: %v", err)
	}

	var body io.Reader
	contentType := ""
	switch *contract {
	case "ios-json":
		encoded, err := json.Marshal(map[string]string{
			"client_id":     clientID,
			"grant_type":    "refresh_token",
			"redirect_uri":  redirectURI,
			"refresh_token": secrets.RefreshToken,
		})
		if err != nil {
			fail("encode request: %v", err)
		}
		body = strings.NewReader(string(encoded))
		contentType = "application/json"
	case "platform-form":
		form := url.Values{}
		form.Set("client_id", clientID)
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", secrets.RefreshToken)
		body = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	default:
		fail("unsupported contract")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, body)
	if err != nil {
		fail("build request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)

	transport, err := chatgpt.NewUTLSTransport(proxyURL, 20*time.Second)
	if err != nil {
		fail("build transport: %v", err)
	}
	started := time.Now()
	resp, err := (&http.Client{Transport: transport, Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		fail("request OAuth token: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		fail("read OAuth response: %v", err)
	}
	var payload tokenResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		fail("decode OAuth response: HTTP %d", resp.StatusCode)
	}

	result := probeResult{
		AccountID:       item.ID,
		ProxyID:         binding.ProxyID,
		Contract:        *contract,
		HTTPStatus:      resp.StatusCode,
		ErrorCode:       payload.Error.Code,
		ErrorType:       payload.Error.Type,
		HasAccessToken:  strings.TrimSpace(payload.AccessToken) != "",
		ResponseElapsed: time.Since(started).Milliseconds(),
	}
	if resp.StatusCode == http.StatusOK && strings.TrimSpace(payload.RefreshToken) != "" {
		rotated, err := cipher.EncryptString(payload.RefreshToken)
		if err != nil {
			fail("encrypt rotated RT: %v", err)
		}
		if err := accountDAO.RotateRefreshTokenCAS(
			ctx,
			item.ID,
			item.RefreshTokenEnc.String,
			rotated,
			"oauth_contract_probe",
		); err != nil {
			fail("persist rotated RT: %v", err)
		}
		result.RotatedRTSaved = true
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fail("encode result: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
