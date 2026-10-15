package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/432539/gpt2api/internal/account"
	"github.com/432539/gpt2api/internal/config"
	"github.com/432539/gpt2api/internal/db"
)

func main() {
	configPath := flag.String("config", "/etc/gpt2api/config.yaml", "gpt2api config path")
	accountID := flag.Uint64("account", 0, "account id")
	status := flag.String("status", "", "target account status")
	reason := flag.String("reason", "", "status reason")
	flag.Parse()
	if *accountID == 0 || *status == "" || *reason == "" {
		fail("account, status, and reason are required")
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
	dao := account.NewDAO(database)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before, err := dao.GetByID(ctx, *accountID)
	if err != nil {
		fail("load account: %v", err)
	}
	if err := dao.SetStatusReason(ctx, *accountID, *status, nil, *reason); err != nil {
		fail("set status: %v", err)
	}
	after, err := dao.GetByID(ctx, *accountID)
	if err != nil {
		fail("reload account: %v", err)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"account_id": *accountID,
		"before":     before.Status,
		"after":      after.Status,
		"reason":     after.RefreshError,
	})
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
