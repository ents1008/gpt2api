package channel

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
)

var ErrNotFound = errors.New("channel: not found")

type DAO struct{ db *sqlx.DB }

func NewDAO(db *sqlx.DB) *DAO { return &DAO{db: db} }

// ---------- Channel CRUD ----------

func (d *DAO) Create(ctx context.Context, c *Channel) (uint64, error) {
	res, err := d.db.ExecContext(ctx, `
INSERT INTO upstream_channels
  (name, type, base_url, api_key_enc, enabled, priority, weight, timeout_s, ratio, extra, status, remark)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.Type, c.BaseURL, c.APIKeyEnc, c.Enabled, c.Priority, c.Weight,
		c.TimeoutS, c.Ratio, c.Extra, c.Status, c.Remark,
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return uint64(id), nil
}

func (d *DAO) Update(ctx context.Context, c *Channel) error {
	_, err := d.db.ExecContext(ctx, `
UPDATE upstream_channels
   SET name=?, type=?, base_url=?, api_key_enc=?, enabled=?, priority=?, weight=?,
       timeout_s=?, ratio=?, extra=?, remark=?
 WHERE id=? AND deleted_at IS NULL`,
		c.Name, c.Type, c.BaseURL, c.APIKeyEnc, c.Enabled, c.Priority, c.Weight,
		c.TimeoutS, c.Ratio, c.Extra, c.Remark, c.ID,
	)
	return err
}

func (d *DAO) SoftDelete(ctx context.Context, id uint64) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE upstream_channels SET deleted_at=? WHERE id=?`, time.Now(), id)
	return err
}

func (d *DAO) GetByID(ctx context.Context, id uint64) (*Channel, error) {
	var c Channel
	err := d.db.GetContext(ctx, &c,
		`SELECT * FROM upstream_channels WHERE id=? AND deleted_at IS NULL`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (d *DAO) List(ctx context.Context, offset, limit int) ([]*Channel, int64, error) {
	var total int64
	if err := d.db.GetContext(ctx, &total,
		`SELECT COUNT(*) FROM upstream_channels WHERE deleted_at IS NULL`); err != nil {
		return nil, 0, err
	}
	rows := make([]*Channel, 0, limit)
	err := d.db.SelectContext(ctx, &rows,
		`SELECT * FROM upstream_channels
          WHERE deleted_at IS NULL
          ORDER BY priority ASC, id ASC
          LIMIT ? OFFSET ?`, limit, offset)
	return rows, total, err
}

// ListEnabled 返回所有启用的渠道(用于路由选择)。
func (d *DAO) ListEnabled(ctx context.Context) ([]*Channel, error) {
	rows := make([]*Channel, 0, 16)
	err := d.db.SelectContext(ctx, &rows,
		`SELECT * FROM upstream_channels
          WHERE deleted_at IS NULL AND enabled = 1
          ORDER BY priority ASC, id ASC`)
	return rows, err
}

// UpdateHealth 记录探测结果。
func (d *DAO) UpdateHealth(ctx context.Context, id uint64, ok bool, errMsg string, failCount int, status string) error {
	_, err := d.db.ExecContext(ctx, `
UPDATE upstream_channels
   SET last_test_at=?, last_test_ok=?, last_test_error=?, fail_count=?, status=?
 WHERE id=?`, time.Now(), ok, truncateErr(errMsg, 500), failCount, status, id)
	return err
}

// ---------- Mapping CRUD ----------

func (d *DAO) CreateMapping(ctx context.Context, m *Mapping) (uint64, error) {
	res, err := d.db.ExecContext(ctx, `
INSERT INTO channel_model_mappings
  (channel_id, local_model, upstream_model, modality, enabled, priority)
VALUES (?, ?, ?, ?, ?, ?)`,
		m.ChannelID, m.LocalModel, m.UpstreamModel, m.Modality, m.Enabled, m.Priority)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return uint64(id), nil
}

func (d *DAO) UpdateMapping(ctx context.Context, m *Mapping) error {
	_, err := d.db.ExecContext(ctx, `
UPDATE channel_model_mappings
   SET local_model=?, upstream_model=?, modality=?, enabled=?, priority=?
 WHERE id=?`,
		m.LocalModel, m.UpstreamModel, m.Modality, m.Enabled, m.Priority, m.ID)
	return err
}

func (d *DAO) DeleteMapping(ctx context.Context, id uint64) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM channel_model_mappings WHERE id=?`, id)
	return err
}

// DeleteMappingsByChannel 当渠道软删时不需调用(外键 CASCADE),
// 但这里保留方法以便手动清理。
func (d *DAO) DeleteMappingsByChannel(ctx context.Context, channelID uint64) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM channel_model_mappings WHERE channel_id=?`, channelID)
	return err
}

func (d *DAO) ListMappingsByChannel(ctx context.Context, channelID uint64) ([]*Mapping, error) {
	rows := make([]*Mapping, 0, 8)
	err := d.db.SelectContext(ctx, &rows,
		`SELECT * FROM channel_model_mappings WHERE channel_id=? ORDER BY priority ASC, id ASC`,
		channelID)
	return rows, err
}

// ListMappingsByLocalModel 路由热路径:按本地模型 + 模态查所有启用的映射,
// 并关联出渠道本身的 enabled、priority,在业务层组合选择。
//
// 返回顺序:
//  1. upstream_channels.enabled = 1 且 deleted_at IS NULL
//  2. channel_model_mappings.enabled = 1
//  3. ORDER BY channel.priority ASC, mapping.priority ASC, mapping.id ASC
type MappingWithChannel struct {
	Mapping
	Channel Channel `db:"c"`
}

func (d *DAO) ResolveByLocalModel(ctx context.Context, localModel, modality string) ([]*MappingWithChannel, error) {
	rows := make([]*MappingWithChannel, 0, 8)
	err := d.db.SelectContext(ctx, &rows, `
SELECT m.id            AS id,
       m.channel_id    AS channel_id,
       m.local_model   AS local_model,
       m.upstream_model AS upstream_model,
       m.modality      AS modality,
       m.enabled       AS enabled,
       m.priority      AS priority,
       m.created_at    AS created_at,
       m.updated_at    AS updated_at,
       c.id                AS "c.id",
       c.name              AS "c.name",
       c.type              AS "c.type",
       c.base_url          AS "c.base_url",
       c.api_key_enc       AS "c.api_key_enc",
       c.enabled           AS "c.enabled",
       c.priority          AS "c.priority",
       c.weight            AS "c.weight",
       c.timeout_s         AS "c.timeout_s",
       c.ratio             AS "c.ratio",
       c.extra             AS "c.extra",
       c.status            AS "c.status",
       c.fail_count        AS "c.fail_count",
       c.last_test_at      AS "c.last_test_at",
       c.last_test_ok      AS "c.last_test_ok",
       c.last_test_error   AS "c.last_test_error",
       c.remark            AS "c.remark",
       c.created_at        AS "c.created_at",
       c.updated_at        AS "c.updated_at",
       c.deleted_at        AS "c.deleted_at"
  FROM channel_model_mappings m
  JOIN upstream_channels c ON c.id = m.channel_id
 WHERE m.local_model = ?
   AND m.modality    = ?
   AND m.enabled     = 1
   AND c.enabled     = 1
   AND c.deleted_at IS NULL
 ORDER BY c.priority ASC, m.priority ASC, m.id ASC`, localModel, modality)
	return rows, err
}

func truncateErr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
