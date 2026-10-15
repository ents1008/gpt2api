package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	accountservice "github.com/432539/gpt2api/internal/account"
	"github.com/432539/gpt2api/internal/config"
	"github.com/432539/gpt2api/internal/db"
)

var configPath = flag.String("c", "configs/config.yaml", "config file path")

type candidate struct {
	ID                  uint64     `json:"id"`
	Status              string     `json:"status"`
	ImageQuotaRemaining int        `json:"image_quota_remaining"`
	LastUsedAt          *time.Time `json:"last_used_at,omitempty"`
	CooldownUntil       *time.Time `json:"cooldown_until,omitempty"`
	RefreshError        string     `json:"refresh_error,omitempty"`
}

type probeResult struct {
	RunningImageTasks      int64       `json:"running_image_tasks"`
	RunningLast2Minutes    int64       `json:"running_last_2_minutes"`
	RunningLast5Minutes    int64       `json:"running_last_5_minutes"`
	RunningLast15Minutes   int64       `json:"running_last_15_minutes"`
	OldestRunningStartedAt *time.Time  `json:"oldest_running_started_at,omitempty"`
	NewestRunningStartedAt *time.Time  `json:"newest_running_started_at,omitempty"`
	Candidates             []candidate `json:"candidates"`
}

type runningTaskStats struct {
	Total           int64        `db:"total"`
	Last2Minutes    int64        `db:"last_2_minutes"`
	Last5Minutes    int64        `db:"last_5_minutes"`
	Last15Minutes   int64        `db:"last_15_minutes"`
	OldestStartedAt sql.NullTime `db:"oldest_started_at"`
	NewestStartedAt sql.NullTime `db:"newest_started_at"`
}

func main() {
	flag.Parse()
	cfg, err := config.Load(*configPath)
	if err != nil {
		fail(err)
	}
	database, err := db.NewMySQL(cfg.MySQL)
	if err != nil {
		fail(err)
	}
	defer database.Close()
	var running runningTaskStats
	if err := database.GetContext(
		context.Background(),
		&running,
		`SELECT COUNT(*) AS total,
                COALESCE(SUM(started_at >= DATE_SUB(NOW(), INTERVAL 2 MINUTE)), 0) AS last_2_minutes,
                COALESCE(SUM(started_at >= DATE_SUB(NOW(), INTERVAL 5 MINUTE)), 0) AS last_5_minutes,
                COALESCE(SUM(started_at >= DATE_SUB(NOW(), INTERVAL 15 MINUTE)), 0) AS last_15_minutes,
                MIN(started_at) AS oldest_started_at,
                MAX(started_at) AS newest_started_at
           FROM image_tasks
          WHERE status = 'running'`,
	); err != nil {
		fail(err)
	}

	accounts, err := accountservice.NewDAO(database).ListDispatchable(context.Background(), 20)
	if err != nil {
		fail(err)
	}
	result := make([]candidate, 0, len(accounts))
	for _, account := range accounts {
		item := candidate{
			ID:                  account.ID,
			Status:              account.Status,
			ImageQuotaRemaining: account.ImageQuotaRemaining,
			RefreshError:        account.RefreshError,
		}
		if account.LastUsedAt.Valid {
			value := account.LastUsedAt.Time
			item.LastUsedAt = &value
		}
		if account.CooldownUntil.Valid {
			value := account.CooldownUntil.Time
			item.CooldownUntil = &value
		}
		result = append(result, item)
	}
	output := probeResult{
		RunningImageTasks:    running.Total,
		RunningLast2Minutes:  running.Last2Minutes,
		RunningLast5Minutes:  running.Last5Minutes,
		RunningLast15Minutes: running.Last15Minutes,
		Candidates:           result,
	}
	if running.OldestStartedAt.Valid {
		value := running.OldestStartedAt.Time
		output.OldestRunningStartedAt = &value
	}
	if running.NewestStartedAt.Valid {
		value := running.NewestStartedAt.Time
		output.NewestRunningStartedAt = &value
	}
	if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
		fail(err)
	}
}

func fail(err error) {
	_, _ = fmt.Fprintln(os.Stderr, "candidate probe failed:", err)
	os.Exit(1)
}
