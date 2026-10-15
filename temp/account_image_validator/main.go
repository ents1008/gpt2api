package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/432539/gpt2api/internal/account"
	"github.com/432539/gpt2api/internal/config"
	"github.com/432539/gpt2api/internal/db"
	"github.com/432539/gpt2api/internal/proxy"
	"github.com/432539/gpt2api/pkg/crypto"
)

type refreshSettings struct{}

func (refreshSettings) AccountRefreshEnabled() bool    { return false }
func (refreshSettings) AccountRefreshIntervalSec() int { return 0 }
func (refreshSettings) AccountRefreshAheadSec() int    { return 0 }
func (refreshSettings) AccountRefreshConcurrency() int { return 1 }
func (refreshSettings) AccountDefaultClientID() string { return "" }

type accountProxyResolver struct {
	accounts *account.Service
	proxies  *proxy.Service
}

func (r *accountProxyResolver) ProxyURLForAccount(
	ctx context.Context,
	accountID uint64,
) string {
	binding, err := r.accounts.GetBinding(ctx, accountID)
	if err != nil || binding == nil {
		return ""
	}
	boundProxy, err := r.proxies.Get(ctx, binding.ProxyID)
	if err != nil || boundProxy == nil || !boundProxy.Enabled {
		return ""
	}
	proxyURL, err := r.proxies.BuildURL(boundProxy)
	if err != nil {
		return ""
	}
	return proxyURL
}

type result struct {
	AccountID  uint64 `json:"account_id"`
	HTTPStatus int    `json:"http_status"`
	OK         bool   `json:"ok"`
	ErrorKind  string `json:"error_kind,omitempty"`
}

func main() {
	configPath := flag.String("c", "configs/config.yaml", "config file path")
	accountID := flag.Uint64("account", 0, "account id to validate")
	flag.Parse()

	if *accountID == 0 {
		writeResult(result{ErrorKind: "invalid_account_id"})
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		writeResult(result{AccountID: *accountID, ErrorKind: "config_load_failed"})
		os.Exit(1)
	}
	sqlDB, err := db.NewMySQL(cfg.MySQL)
	if err != nil {
		writeResult(result{AccountID: *accountID, ErrorKind: "database_unavailable"})
		os.Exit(1)
	}
	defer sqlDB.Close()

	cipher, err := crypto.NewAESGCM(cfg.Crypto.AESKey)
	if err != nil {
		writeResult(result{AccountID: *accountID, ErrorKind: "crypto_init_failed"})
		os.Exit(1)
	}

	accountService := account.NewService(account.NewDAO(sqlDB), cipher)
	proxyService := proxy.NewService(proxy.NewDAO(sqlDB), cipher)
	refresher := account.NewRefresher(
		accountService,
		refreshSettings{},
		zap.NewNop(),
	)
	refresher.SetProxyResolver(&accountProxyResolver{
		accounts: accountService,
		proxies:  proxyService,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	status, validateErr := refresher.ValidateAccountForImage(ctx, *accountID)
	probeResult := result{
		AccountID:  *accountID,
		HTTPStatus: status,
		OK:         validateErr == nil && status == http.StatusOK,
	}
	if !probeResult.OK {
		probeResult.ErrorKind = classifyError(status)
	}
	writeResult(probeResult)
	if !probeResult.OK {
		os.Exit(1)
	}
}

func classifyError(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case 0:
		return "transport_or_local_failure"
	default:
		return fmt.Sprintf("upstream_http_%d", status)
	}
}

func writeResult(value result) {
	_ = json.NewEncoder(os.Stdout).Encode(value)
}
