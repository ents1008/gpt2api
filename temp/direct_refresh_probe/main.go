package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/432539/gpt2api/internal/account"
	"github.com/432539/gpt2api/internal/config"
	"github.com/432539/gpt2api/internal/db"
	"github.com/432539/gpt2api/internal/proxy"
	"github.com/432539/gpt2api/internal/settings"
	appcrypto "github.com/432539/gpt2api/pkg/crypto"
)

type accountProxyResolver struct {
	accounts *account.Service
	proxies  *proxy.Service
}

func (r *accountProxyResolver) ProxyURLForAccount(ctx context.Context, accountID uint64) string {
	binding, err := r.accounts.GetBinding(ctx, accountID)
	if err != nil || binding == nil {
		return ""
	}
	item, err := r.proxies.Get(ctx, binding.ProxyID)
	if err != nil || item == nil || !item.Enabled {
		return ""
	}
	value, err := r.proxies.BuildURL(item)
	if err != nil {
		return ""
	}
	return value
}

type probeResult struct {
	AccountID          uint64 `json:"account_id"`
	BeforeStatus       string `json:"before_status"`
	BeforeHasRT        bool   `json:"before_has_rt"`
	BeforeHasST        bool   `json:"before_has_st"`
	BeforeLastRefresh  string `json:"before_last_refresh,omitempty"`
	RefreshOK          bool   `json:"refresh_ok"`
	RefreshSource      string `json:"refresh_source,omitempty"`
	ATVerified         bool   `json:"at_verified"`
	RTRotated          bool   `json:"rt_rotated"`
	WebUnauthorized    bool   `json:"web_unauthorized"`
	RefreshExpiresAt   string `json:"refresh_expires_at,omitempty"`
	RefreshError       string `json:"refresh_error,omitempty"`
	AfterStatus        string `json:"after_status"`
	AfterHasRT         bool   `json:"after_has_rt"`
	AfterHasAT         bool   `json:"after_has_at"`
	AfterLastRefresh   string `json:"after_last_refresh,omitempty"`
	AfterRefreshSource string `json:"after_refresh_source,omitempty"`
	AfterRefreshError  string `json:"after_refresh_error,omitempty"`
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
	settingService := settings.NewService(settings.NewDAO(database))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := settingService.Reload(ctx); err != nil {
		fail("load settings: %v", err)
	}

	before, err := accountService.Get(ctx, *accountID)
	if err != nil {
		fail("load account before refresh: %v", err)
	}
	result := probeResult{
		AccountID:    before.ID,
		BeforeStatus: before.Status,
		BeforeHasRT:  before.RefreshTokenEnc.Valid && strings.TrimSpace(before.RefreshTokenEnc.String) != "",
		BeforeHasST:  before.SessionTokenEnc.Valid && strings.TrimSpace(before.SessionTokenEnc.String) != "",
	}
	if before.LastRefreshAt.Valid {
		result.BeforeLastRefresh = before.LastRefreshAt.Time.Format(time.RFC3339)
	}

	refresher := account.NewRefresher(accountService, settingService, zap.NewNop())
	refresher.SetProxyResolver(&accountProxyResolver{accounts: accountService, proxies: proxyService})
	refresh, refreshErr := refresher.RefreshByID(ctx, *accountID)
	if refreshErr != nil {
		result.RefreshError = refreshErr.Error()
	} else if refresh != nil {
		result.RefreshOK = refresh.OK
		result.RefreshSource = refresh.Source
		result.ATVerified = refresh.ATVerified
		result.RTRotated = refresh.RTRotated
		result.WebUnauthorized = refresh.WebUnauthorized
		result.RefreshExpiresAt = formatTime(refresh.ExpiresAt)
		result.RefreshError = refresh.Error
	}

	after, err := accountService.Get(ctx, *accountID)
	if err != nil {
		fail("load account after refresh: %v", err)
	}
	result.AfterStatus = after.Status
	result.AfterHasRT = after.RefreshTokenEnc.Valid && strings.TrimSpace(after.RefreshTokenEnc.String) != ""
	result.AfterHasAT = strings.TrimSpace(after.AuthTokenEnc) != ""
	result.AfterRefreshSource = after.LastRefreshSource
	result.AfterRefreshError = after.RefreshError
	if after.LastRefreshAt.Valid {
		result.AfterLastRefresh = after.LastRefreshAt.Time.Format(time.RFC3339)
	}
	writeResult(result)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339)
}

func writeResult(value probeResult) {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		fail("encode result: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
