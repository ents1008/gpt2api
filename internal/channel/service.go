package channel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/432539/gpt2api/pkg/crypto"
)

// Service 封装渠道 CRUD、API Key 加解密、模型映射维护。
type Service struct {
	dao    *DAO
	cipher *crypto.AESGCM
}

func NewService(dao *DAO, cipher *crypto.AESGCM) *Service {
	return &Service{dao: dao, cipher: cipher}
}

// DAO 暴露底层 DAO,供路由器等场景只读使用。
func (s *Service) DAO() *DAO { return s.dao }

// CreateInput Create 入参。APIKey 为明文,入库时加密。
type CreateInput struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	BaseURL  string  `json:"base_url"`
	APIKey   string  `json:"api_key"`
	Enabled  bool    `json:"enabled"`
	Priority int     `json:"priority"`
	Weight   int     `json:"weight"`
	TimeoutS int     `json:"timeout_s"`
	Ratio    float64 `json:"ratio"`
	Extra    string  `json:"extra"`
	Remark   string  `json:"remark"`
}

// UpdateInput Update 入参。APIKey 为空串表示保持不变。
type UpdateInput struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	BaseURL  string  `json:"base_url"`
	APIKey   string  `json:"api_key"`
	Enabled  bool    `json:"enabled"`
	Priority int     `json:"priority"`
	Weight   int     `json:"weight"`
	TimeoutS int     `json:"timeout_s"`
	Ratio    float64 `json:"ratio"`
	Extra    string  `json:"extra"`
	Remark   string  `json:"remark"`
}

// 校验合法的渠道类型。
func validType(t string) bool {
	switch t {
	case TypeOpenAI, TypeGemini:
		return true
	}
	return false
}

// 校验合法的模态。
func validModality(m string) bool {
	switch m {
	case ModalityText, ModalityImage, ModalityVideo:
		return true
	}
	return false
}

func (s *Service) Create(ctx context.Context, in CreateInput) (*Channel, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, errors.New("name 不能为空")
	}
	if !validType(in.Type) {
		return nil, fmt.Errorf("type 只能是 %s / %s", TypeOpenAI, TypeGemini)
	}
	if strings.TrimSpace(in.BaseURL) == "" {
		return nil, errors.New("base_url 不能为空")
	}
	if strings.TrimSpace(in.APIKey) == "" {
		return nil, errors.New("api_key 不能为空")
	}
	if in.Ratio <= 0 {
		in.Ratio = 1.0
	}
	if in.Weight <= 0 {
		in.Weight = 1
	}
	if in.Priority < 0 {
		in.Priority = 100
	}
	if in.TimeoutS <= 0 {
		in.TimeoutS = 120
	}
	enc, err := s.cipher.EncryptString(in.APIKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt api_key: %w", err)
	}
	c := &Channel{
		Name: in.Name, Type: in.Type,
		BaseURL:   strings.TrimRight(in.BaseURL, "/"),
		APIKeyEnc: enc,
		Enabled:   in.Enabled, Priority: in.Priority, Weight: in.Weight,
		TimeoutS: in.TimeoutS, Ratio: in.Ratio,
		Extra:  sql.NullString{String: in.Extra, Valid: in.Extra != ""},
		Status: StatusHealthy, Remark: in.Remark,
	}
	id, err := s.dao.Create(ctx, c)
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s *Service) Update(ctx context.Context, id uint64, in UpdateInput) (*Channel, error) {
	c, err := s.dao.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Name != "" {
		c.Name = in.Name
	}
	if in.Type != "" {
		if !validType(in.Type) {
			return nil, fmt.Errorf("type 只能是 %s / %s", TypeOpenAI, TypeGemini)
		}
		c.Type = in.Type
	}
	if in.BaseURL != "" {
		c.BaseURL = strings.TrimRight(in.BaseURL, "/")
	}
	if in.APIKey != "" {
		enc, err := s.cipher.EncryptString(in.APIKey)
		if err != nil {
			return nil, err
		}
		c.APIKeyEnc = enc
	}
	c.Enabled = in.Enabled
	if in.Priority >= 0 {
		c.Priority = in.Priority
	}
	if in.Weight > 0 {
		c.Weight = in.Weight
	}
	if in.TimeoutS > 0 {
		c.TimeoutS = in.TimeoutS
	}
	if in.Ratio > 0 {
		c.Ratio = in.Ratio
	}
	c.Extra = sql.NullString{String: in.Extra, Valid: in.Extra != ""}
	c.Remark = in.Remark

	if err := s.dao.Update(ctx, c); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s *Service) Delete(ctx context.Context, id uint64) error {
	return s.dao.SoftDelete(ctx, id)
}

// Get 返回带 masked / extra_json 的渠道。
func (s *Service) Get(ctx context.Context, id uint64) (*Channel, error) {
	c, err := s.dao.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	s.decorate(c)
	return c, nil
}

func (s *Service) List(ctx context.Context, offset, limit int) ([]*Channel, int64, error) {
	rows, total, err := s.dao.List(ctx, offset, limit)
	if err != nil {
		return nil, 0, err
	}
	for _, c := range rows {
		s.decorate(c)
	}
	return rows, total, nil
}

// DecryptKey 返回 API Key 明文。
func (s *Service) DecryptKey(c *Channel) (string, error) {
	if c.APIKeyEnc == "" {
		return "", nil
	}
	return s.cipher.DecryptString(c.APIKeyEnc)
}

// decorate 填充不落库的展示字段:api_key_masked / extra_json。
func (s *Service) decorate(c *Channel) {
	if c.APIKeyEnc != "" {
		if pt, err := s.cipher.DecryptString(c.APIKeyEnc); err == nil {
			c.APIKeyMasked = maskKey(pt)
		} else {
			c.APIKeyMasked = "(invalid)"
		}
	}
	if c.Extra.Valid {
		c.ExtraJSON = c.Extra.String
	}
}

func maskKey(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + "****" + s[len(s)-4:]
}

// ---------- Mapping ----------

// CreateMappingInput 映射入参。
type CreateMappingInput struct {
	ChannelID     uint64 `json:"channel_id"`
	LocalModel    string `json:"local_model"`
	UpstreamModel string `json:"upstream_model"`
	Modality      string `json:"modality"`
	Enabled       bool   `json:"enabled"`
	Priority      int    `json:"priority"`
}

func (s *Service) CreateMapping(ctx context.Context, in CreateMappingInput) (*Mapping, error) {
	if in.ChannelID == 0 {
		return nil, errors.New("channel_id 不能为空")
	}
	if strings.TrimSpace(in.LocalModel) == "" {
		return nil, errors.New("local_model 不能为空")
	}
	if strings.TrimSpace(in.UpstreamModel) == "" {
		return nil, errors.New("upstream_model 不能为空")
	}
	if in.Modality == "" {
		in.Modality = ModalityText
	}
	if !validModality(in.Modality) {
		return nil, fmt.Errorf("modality 只能是 text / image / video")
	}
	if in.Priority < 0 {
		in.Priority = 100
	}
	m := &Mapping{
		ChannelID: in.ChannelID, LocalModel: in.LocalModel,
		UpstreamModel: in.UpstreamModel, Modality: in.Modality,
		Enabled: in.Enabled, Priority: in.Priority,
	}
	id, err := s.dao.CreateMapping(ctx, m)
	if err != nil {
		return nil, err
	}
	m.ID = id
	return m, nil
}

func (s *Service) UpdateMapping(ctx context.Context, id uint64, in CreateMappingInput) (*Mapping, error) {
	if in.Modality == "" {
		in.Modality = ModalityText
	}
	if !validModality(in.Modality) {
		return nil, errors.New("modality 只能是 text / image / video")
	}
	m := &Mapping{
		ID: id, LocalModel: in.LocalModel, UpstreamModel: in.UpstreamModel,
		Modality: in.Modality, Enabled: in.Enabled, Priority: in.Priority,
	}
	if err := s.dao.UpdateMapping(ctx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Service) DeleteMapping(ctx context.Context, id uint64) error {
	return s.dao.DeleteMapping(ctx, id)
}

func (s *Service) ListMappings(ctx context.Context, channelID uint64) ([]*Mapping, error) {
	return s.dao.ListMappingsByChannel(ctx, channelID)
}

// MarkHealth 记录一次探测结果,超过阈值置 unhealthy。
func (s *Service) MarkHealth(ctx context.Context, c *Channel, ok bool, errMsg string) error {
	failCount := c.FailCount
	status := c.Status
	if ok {
		failCount = 0
		status = StatusHealthy
	} else {
		failCount++
		if failCount >= 3 {
			status = StatusUnhealthy
		}
	}
	return s.dao.UpdateHealth(ctx, c.ID, ok, errMsg, failCount, status)
}
