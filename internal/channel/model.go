// Package channel 实现"外置上游渠道"的统一管理与路由。
//
// 渠道(Channel)指除 ChatGPT 账号池之外的 API 供应商:
//   - openai: 兼容 OpenAI /v1/chat/completions、/v1/images/generations 等
//   - gemini: 兼容 Google Generative Language API v1beta
//
// 每个渠道有自己的 base_url、api_key、倍率、优先级与健康状态。
// 模型映射(Mapping)把本地模型名映射到指定渠道上的上游模型名,
// 同一个本地模型可以在多个渠道上映射以实现负载均衡/故障转移。
package channel

import (
	"database/sql"
	"time"
)

// 渠道类型。
const (
	TypeOpenAI = "openai"
	TypeGemini = "gemini"
)

// 模态。
const (
	ModalityText  = "text"
	ModalityImage = "image"
	ModalityVideo = "video"
)

// 健康状态。
const (
	StatusHealthy   = "healthy"
	StatusUnhealthy = "unhealthy"
)

// Channel 对应 upstream_channels 表。ApiKeyEnc 是 AES-GCM 密文,
// 读取后需要由 Service.DecryptKey 解密再使用。
type Channel struct {
	ID            uint64         `db:"id"               json:"id"`
	Name          string         `db:"name"             json:"name"`
	Type          string         `db:"type"             json:"type"`
	BaseURL       string         `db:"base_url"         json:"base_url"`
	APIKeyEnc     string         `db:"api_key_enc"      json:"-"`
	APIKeyMasked  string         `db:"-"                json:"api_key_masked,omitempty"`
	Enabled       bool           `db:"enabled"          json:"enabled"`
	Priority      int            `db:"priority"         json:"priority"`
	Weight        int            `db:"weight"           json:"weight"`
	TimeoutS      int            `db:"timeout_s"        json:"timeout_s"`
	Ratio         float64        `db:"ratio"            json:"ratio"`
	Extra         sql.NullString `db:"extra"            json:"-"`
	ExtraJSON     string         `db:"-"                json:"extra,omitempty"`
	Status        string         `db:"status"           json:"status"`
	FailCount     int            `db:"fail_count"       json:"fail_count"`
	LastTestAt    sql.NullTime   `db:"last_test_at"     json:"last_test_at,omitempty"`
	LastTestOK    sql.NullBool   `db:"last_test_ok"     json:"last_test_ok,omitempty"`
	LastTestError string         `db:"last_test_error"  json:"last_test_error,omitempty"`
	Remark        string         `db:"remark"           json:"remark"`
	CreatedAt     time.Time      `db:"created_at"       json:"created_at"`
	UpdatedAt     time.Time      `db:"updated_at"       json:"updated_at"`
	DeletedAt     sql.NullTime   `db:"deleted_at"       json:"-"`
}

// Mapping 对应 channel_model_mappings 表。
type Mapping struct {
	ID            uint64    `db:"id"             json:"id"`
	ChannelID     uint64    `db:"channel_id"     json:"channel_id"`
	ChannelName   string    `db:"-"              json:"channel_name,omitempty"`
	LocalModel    string    `db:"local_model"    json:"local_model"`
	UpstreamModel string    `db:"upstream_model" json:"upstream_model"`
	Modality      string    `db:"modality"       json:"modality"`
	Enabled       bool      `db:"enabled"        json:"enabled"`
	Priority      int       `db:"priority"       json:"priority"`
	CreatedAt     time.Time `db:"created_at"     json:"created_at"`
	UpdatedAt     time.Time `db:"updated_at"     json:"updated_at"`
}
