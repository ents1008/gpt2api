package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/432539/gpt2api/internal/account"
	"github.com/432539/gpt2api/internal/config"
	"github.com/432539/gpt2api/internal/db"
	"github.com/432539/gpt2api/internal/proxy"
	"github.com/432539/gpt2api/internal/upstream/chatgpt"
	appcrypto "github.com/432539/gpt2api/pkg/crypto"
)

type result struct {
	Endpoint      string `json:"endpoint"`
	Status        int    `json:"status"`
	ContentType   string `json:"content_type,omitempty"`
	BodyLength    int    `json:"body_length"`
	BodySHA256    string `json:"body_sha256"`
	DownloadKind  string `json:"download_kind,omitempty"`
	DownloadHost  string `json:"download_host,omitempty"`
	DownloadQuery bool   `json:"download_query,omitempty"`
	Format        string `json:"format,omitempty"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
}

func main() {
	configPath := flag.String("config", "/etc/gpt2api/config.yaml", "gpt2api config path")
	accountID := flag.Uint64("account", 0, "account id")
	conversationID := flag.String("conversation", "", "conversation id")
	fileID := flag.String("file", "", "sediment/file id")
	flag.Parse()
	if *accountID == 0 || *conversationID == "" || *fileID == "" {
		fatal("account, conversation and file are required")
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
	authToken, err := accountService.DecryptAuthToken(acc)
	if err != nil {
		fatal("decrypt token: %v", err)
	}
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
	hc := &http.Client{Transport: transport, Jar: jar, Timeout: 30 * time.Second}
	paths := []struct {
		name string
		path string
	}{
		{"conversation_attachment", fmt.Sprintf("/backend-api/conversation/%s/attachment/%s/download", url.PathEscape(*conversationID), url.PathEscape(*fileID))},
		{"legacy_files_download", fmt.Sprintf("/backend-api/files/download/%s", url.PathEscape(*fileID))},
		{"standard_files_download", fmt.Sprintf("/backend-api/files/%s/download", url.PathEscape(*fileID))},
	}
	var downloadURL string
	for _, candidate := range paths {
		res, resolvedURL, err := requestPath(ctx, hc, candidate.name, candidate.path, authToken, acc.OAIDeviceID, acc.OAISessionID)
		if err != nil {
			fmt.Printf("{\"endpoint\":%q,\"error\":\"request_failed\"}\n", candidate.name)
			continue
		}
		if downloadURL == "" && resolvedURL != "" {
			downloadURL = resolvedURL
		}
		encoded, _ := json.Marshal(res)
		fmt.Println(string(encoded))
	}
	if downloadURL == "" {
		return
	}
	directTransport, err := chatgpt.NewUTLSTransport("", 30*time.Second)
	if err != nil {
		fatal("init direct transport")
	}
	publicClient := &http.Client{Transport: directTransport, Timeout: 30 * time.Second}
	publicResult, publicErr := requestUnauthenticatedAsset(ctx, publicClient, downloadURL)
	if publicErr == nil {
		encoded, _ := json.Marshal(publicResult)
		fmt.Println(string(encoded))
		return
	}
	bearerResult, bearerErr := requestBearerAsset(ctx, publicClient, downloadURL, authToken)
	if bearerErr == nil {
		encoded, _ := json.Marshal(bearerResult)
		fmt.Println(string(encoded))
		return
	}
	authenticatedResult, authenticatedErr := requestAuthenticatedAsset(
		ctx, publicClient, downloadURL, authToken, acc.OAIDeviceID, acc.OAISessionID,
	)
	if authenticatedErr == nil {
		authenticatedResult.Endpoint = "direct_authenticated_no_cookie"
		authenticatedResult.DownloadKind = "direct_authenticated_no_cookie"
		encoded, _ := json.Marshal(authenticatedResult)
		fmt.Println(string(encoded))
		return
	}
	directClient := &http.Client{Transport: directTransport, Jar: jar, Timeout: 30 * time.Second}
	directResult, err := requestAuthenticatedAsset(
		ctx, directClient, downloadURL, authToken, acc.OAIDeviceID, acc.OAISessionID,
	)
	if err != nil {
		fmt.Println(`{"endpoint":"direct_authenticated_asset","error":"request_failed"}`)
		return
	}
	encoded, _ := json.Marshal(directResult)
	fmt.Println(string(encoded))
}

func requestPath(ctx context.Context, hc *http.Client, endpoint, path, token, deviceID, sessionID string) (result, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chatgpt.BaseURL+path, nil)
	if err != nil {
		return result{}, "", err
	}
	applyAuthenticatedHeaders(req, token, deviceID, sessionID)
	response, err := hc.Do(req)
	if err != nil {
		return result{}, "", err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 128*1024))
	if readErr != nil {
		return result{}, "", readErr
	}
	hash := sha256.Sum256(body)
	downloadKind := ""
	downloadHost := ""
	downloadQuery := false
	downloadURL := ""
	var payload struct {
		DownloadURL string `json:"download_url"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.DownloadURL != "" {
		if parsed, parseErr := url.Parse(payload.DownloadURL); parseErr == nil {
			downloadURL = payload.DownloadURL
			downloadHost = parsed.Hostname()
			downloadQuery = parsed.RawQuery != ""
			if parsed.Hostname() == "chatgpt.com" {
				downloadKind = "authenticated_internal"
			} else {
				downloadKind = "external_signed"
			}
		}
	}
	return result{
		Endpoint: endpoint, Status: response.StatusCode, ContentType: response.Header.Get("Content-Type"),
		BodyLength: len(body), BodySHA256: hex.EncodeToString(hash[:]),
		DownloadKind: downloadKind, DownloadHost: downloadHost, DownloadQuery: downloadQuery,
	}, downloadURL, nil
}

func requestAuthenticatedAsset(
	ctx context.Context,
	hc *http.Client,
	downloadURL string,
	token string,
	deviceID string,
	sessionID string,
) (result, error) {
	return requestImageAsset(ctx, hc, downloadURL, "direct_authenticated_asset", func(req *http.Request) {
		applyAuthenticatedHeaders(req, token, deviceID, sessionID)
	})
}

func requestUnauthenticatedAsset(
	ctx context.Context,
	hc *http.Client,
	downloadURL string,
) (result, error) {
	return requestImageAsset(ctx, hc, downloadURL, "direct_signed_asset", func(req *http.Request) {
		chatgpt.ApplyDefaultBrowserFingerprintHeaders(req.Header)
		req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		req.Header.Set("Sec-Fetch-Dest", "image")
		req.Header.Set("Sec-Fetch-Mode", "no-cors")
		req.Header.Set("Sec-Fetch-Site", "same-site")
	})
}

func requestBearerAsset(
	ctx context.Context,
	hc *http.Client,
	downloadURL string,
	token string,
) (result, error) {
	return requestImageAsset(ctx, hc, downloadURL, "direct_bearer_asset", func(req *http.Request) {
		chatgpt.ApplyDefaultBrowserFingerprintHeaders(req.Header)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", chatgpt.BaseURL)
		req.Header.Set("Referer", chatgpt.BaseURL+"/")
		req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		req.Header.Set("Sec-Fetch-Dest", "image")
		req.Header.Set("Sec-Fetch-Mode", "no-cors")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	})
}

func requestImageAsset(
	ctx context.Context,
	hc *http.Client,
	downloadURL string,
	endpoint string,
	applyHeaders func(*http.Request),
) (result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return result{}, err
	}
	applyHeaders(req)
	response, err := hc.Do(req)
	if err != nil {
		return result{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16*1024*1024+1))
	if err != nil || len(body) > 16*1024*1024 {
		return result{}, fmt.Errorf("invalid asset body")
	}
	imageConfig, format, err := image.DecodeConfig(bytes.NewReader(body))
	if err != nil || imageConfig.Width <= 0 || imageConfig.Height <= 0 {
		return result{}, fmt.Errorf("invalid image body")
	}
	hash := sha256.Sum256(body)
	return result{
		Endpoint: endpoint, Status: response.StatusCode,
		ContentType: response.Header.Get("Content-Type"), BodyLength: len(body),
		BodySHA256: hex.EncodeToString(hash[:]), DownloadKind: endpoint,
		Format: format, Width: imageConfig.Width, Height: imageConfig.Height,
	}, nil
}

func applyAuthenticatedHeaders(req *http.Request, token, deviceID, sessionID string) {
	chatgpt.ApplyDefaultBrowserFingerprintHeaders(req.Header)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", chatgpt.BaseURL)
	req.Header.Set("Referer", chatgpt.BaseURL+"/")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Priority", "u=1, i")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Oai-Device-Id", deviceID)
	if sessionID != "" {
		req.Header.Set("Oai-Session-Id", sessionID)
	}
	req.Header.Set("Oai-Language", chatgpt.DefaultLanguage)
	req.Header.Set("Oai-Client-Version", chatgpt.DefaultClientVersion)
	req.Header.Set("Oai-Client-Build-Number", chatgpt.DefaultClientBuildNum)
	req.Header.Set("X-Openai-Target-Path", req.URL.Path)
	req.Header.Set("X-Openai-Target-Route", req.URL.Path)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
