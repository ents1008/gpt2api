package account

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
)

var (
	ErrNotFound          = errors.New("账号不存在")
	ErrRefreshConflict   = errors.New("account refresh was superseded by another writer")
	ErrAdmissionConflict = errors.New("account admission observation was superseded by another writer")
)

type DAO struct{ db *sqlx.DB }

func NewDAO(db *sqlx.DB) *DAO { return &DAO{db: db} }

// DB 暴露底层 handle 给刷新器 / 探测器用于直接原子更新(少量场景)。
func (d *DAO) DB() *sqlx.DB { return d.db }

// fill 填充非 db 列的辅助字段。
func fill(a *Account) {
	if a == nil {
		return
	}
	a.HasRT = a.RefreshTokenEnc.Valid && a.RefreshTokenEnc.String != ""
	a.HasST = a.SessionTokenEnc.Valid && a.SessionTokenEnc.String != ""
}

func fillAll(rows []*Account) {
	for _, r := range rows {
		fill(r)
	}
}

func (d *DAO) Create(ctx context.Context, a *Account) (uint64, error) {
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO oai_accounts
         (email, auth_token_enc, refresh_token_enc, session_token_enc, token_expires_at,
          oai_session_id, oai_device_id, client_id, refresh_client_id, credential_provenance, browser_profile,
          chatgpt_account_id, account_type,
          plan_type, daily_image_quota, status,
          admission_state, admission_stage, admission_started_at, admission_next_check_at,
          admission_last_check_at, admission_claimed_until, admission_reason, notes)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Email, a.AuthTokenEnc, a.RefreshTokenEnc, a.SessionTokenEnc, a.TokenExpiresAt,
		a.OAISessionID, a.OAIDeviceID, a.ClientID, a.RefreshClientID, a.CredentialProvenance, a.BrowserProfile,
		a.ChatGPTAccountID, a.AccountType,
		a.PlanType, a.DailyImageQuota, a.Status,
		a.AdmissionState, a.AdmissionStage, a.AdmissionStartedAt, a.AdmissionNextCheckAt,
		a.AdmissionLastCheckAt, a.AdmissionClaimedUntil, a.AdmissionReason, a.Notes,
	)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return uint64(id), nil
}

func (d *DAO) GetByID(ctx context.Context, id uint64) (*Account, error) {
	var a Account
	err := d.db.GetContext(ctx, &a,
		`SELECT * FROM oai_accounts WHERE id = ? AND deleted_at IS NULL`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	fill(&a)
	return &a, err
}

// GetByEmail 精确找;未命中返回 nil, nil(方便 importer 判 upsert)。
func (d *DAO) GetByEmail(ctx context.Context, email string) (*Account, error) {
	var a Account
	err := d.db.GetContext(ctx, &a,
		`SELECT * FROM oai_accounts WHERE email = ? AND deleted_at IS NULL LIMIT 1`, email)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fill(&a)
	return &a, nil
}

func (d *DAO) List(ctx context.Context, status string, keyword string, offset, limit int) ([]*Account, int64, error) {
	var total int64
	var err error
	var rows []*Account

	where := "deleted_at IS NULL"
	args := []interface{}{}
	if status != "" {
		where += " AND status = ?"
		args = append(args, status)
	}
	if keyword != "" {
		where += " AND (email LIKE ? OR notes LIKE ?)"
		like := "%" + keyword + "%"
		args = append(args, like, like)
	}

	if err = d.db.GetContext(ctx, &total, "SELECT COUNT(*) FROM oai_accounts WHERE "+where, args...); err != nil {
		return nil, 0, err
	}
	argsPage := append([]interface{}{}, args...)
	argsPage = append(argsPage, limit, offset)
	err = d.db.SelectContext(ctx, &rows,
		"SELECT * FROM oai_accounts WHERE "+where+" ORDER BY id DESC LIMIT ? OFFSET ?", argsPage...)
	fillAll(rows)
	return rows, total, err
}

// ListDispatchable 调度器专用:返回 AT 有效且 cooldown 到期的候选账号。
//
// 接受 status IN ('healthy', 'warned', 'suspicious')，以及带有明确且已
// 到期 cooldown 的 throttled 账号:
//   - healthy: 正常
//   - warned:  RT/ST 刷新失败但 AT 尚未到期,仍可继续出图直到 AT 过期
//   - suspicious: 403/风控等可恢复异常,冷却到期后低优先级复试
//   - throttled: 上游额度/429 临时限流,冷却到期后最低优先级复试
//
// cooldown 为空的 throttled 账号视为人工或长期停用，不自动恢复。
func (d *DAO) ListDispatchable(ctx context.Context, limit int) ([]*Account, error) {
	rows := make([]*Account, 0, limit)
	now := time.Now()
	recentSince := now.Add(-24 * time.Hour)
	err := d.db.SelectContext(ctx, &rows,
		`SELECT a.* FROM oai_accounts a
         LEFT JOIN (
             SELECT account_id,
                    SUM(CASE WHEN status='success' THEN 1 ELSE 0 END) AS recent_successes,
                    SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END) AS recent_failures,
                    MAX(CASE WHEN status='success' THEN finished_at ELSE NULL END) AS last_success_at,
                    MAX(CASE WHEN status='failed' THEN finished_at ELSE NULL END) AS last_failure_at
               FROM image_tasks
              WHERE created_at >= ?
                AND account_id > 0
              GROUP BY account_id
         ) recent ON recent.account_id = a.id
         WHERE a.deleted_at IS NULL
           AND a.admission_state = 'admitted'
           AND (a.status IN ('healthy', 'warned', 'suspicious')
                OR (a.status='throttled' AND a.cooldown_until IS NOT NULL))
           AND (a.cooldown_until IS NULL OR a.cooldown_until <= ?)
           AND (a.token_expires_at IS NULL OR a.token_expires_at > ?)
          ORDER BY
           CASE a.status WHEN 'healthy' THEN 0 WHEN 'warned' THEN 1 WHEN 'suspicious' THEN 2 WHEN 'throttled' THEN 3 ELSE 4 END,
           CASE
             WHEN a.status='suspicious'
              AND COALESCE(recent.recent_successes, 0) > 0
              AND (recent.last_failure_at IS NULL OR recent.last_success_at > recent.last_failure_at)
             THEN 0
             WHEN a.status='suspicious' THEN 1
             ELSE 0
           END,
           CASE
             WHEN a.status='suspicious'
              AND COALESCE(recent.recent_successes, 0) > 0
              AND (recent.last_failure_at IS NULL OR recent.last_success_at > recent.last_failure_at)
             THEN recent.last_success_at
             ELSE NULL
           END DESC,
           CASE
             WHEN a.status='suspicious' THEN
               CASE COALESCE(a.refresh_error, '')
                 WHEN 'upstream_network_transient' THEN 0
                 WHEN 'network_transient' THEN 1
                 WHEN 'download_failed' THEN 2
                 WHEN 'conversation_poll_error' THEN 3
                 WHEN 'image_task_stalled' THEN 4
                 WHEN 'image_tool_without_asset' THEN 5
                 WHEN 'poll_timeout' THEN 6
                 WHEN 'upstream_403' THEN 7
                 WHEN 'proxy_connect_403' THEN 8
                 WHEN 'turnstile_stalled' THEN 9
                 ELSE 10
               END
             ELSE 0
           END,
           CASE
             WHEN a.status='suspicious' THEN
               CASE
                 WHEN a.image_quota_remaining > 0 THEN 0
                 WHEN a.image_quota_remaining < 0 THEN 1
                 ELSE 2
               END
             ELSE 0
           END,
           CASE WHEN a.last_used_at IS NULL THEN 0 ELSE 1 END,
           a.last_used_at ASC
         LIMIT ?`, recentSince, now, now, limit)
	fillAll(rows)
	return rows, err
}

// ListNeedRefresh 返回需要预刷新的账号(AT 将在 aheadSec 秒内过期)。
// 按 token_expires_at 升序,最快过期的先刷。
func (d *DAO) ListNeedRefresh(ctx context.Context, aheadSec int, limit int) ([]*Account, error) {
	rows := make([]*Account, 0, limit)
	threshold := time.Now().Add(time.Duration(aheadSec) * time.Second)
	err := d.db.SelectContext(ctx, &rows,
		`SELECT * FROM oai_accounts
         WHERE deleted_at IS NULL
           AND status <> 'dead'
           AND (refresh_token_enc IS NOT NULL OR session_token_enc IS NOT NULL)
           AND NOT (admission_state = ? AND credential_provenance IN (?, ?))
           AND token_expires_at IS NOT NULL
           AND token_expires_at <= ?
         ORDER BY token_expires_at ASC
         LIMIT ?`,
		AdmissionStateQuarantined,
		CredentialProvenanceWebOnly,
		CredentialProvenanceBrowserWeb,
		threshold,
		limit,
	)
	fillAll(rows)
	return rows, err
}

func (d *DAO) ListDueAdmissions(ctx context.Context, now time.Time, limit int) ([]*Account, error) {
	rows := make([]*Account, 0, limit)
	err := d.db.SelectContext(ctx, &rows,
		`SELECT * FROM oai_accounts
         WHERE deleted_at IS NULL
           AND status <> 'dead'
           AND admission_state = 'quarantined'
           AND admission_next_check_at IS NOT NULL
           AND admission_next_check_at <= ?
           AND (admission_claimed_until IS NULL OR admission_claimed_until <= ?)
         ORDER BY admission_next_check_at ASC
         LIMIT ?`, now, now, limit)
	fillAll(rows)
	return rows, err
}

func (d *DAO) ClaimAdmissionCheck(
	ctx context.Context,
	id uint64,
	expectedStage int,
	expectedStartedAt time.Time,
	now time.Time,
	claimedUntil time.Time,
) (bool, error) {
	res, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts
         SET admission_claimed_until = ?
         WHERE id = ? AND deleted_at IS NULL
           AND admission_state = 'quarantined'
           AND admission_next_check_at IS NOT NULL
           AND admission_next_check_at <= ?
           AND admission_stage = ? AND admission_started_at = ?
           AND (admission_claimed_until IS NULL OR admission_claimed_until <= ?)`,
		claimedUntil, id, now, expectedStage, expectedStartedAt, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (d *DAO) ApplyAdmissionTransition(
	ctx context.Context,
	id uint64,
	expectedStage int,
	expectedStartedAt time.Time,
	transition admissionTransition,
	checkedAt time.Time,
) error {
	res, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts
         SET admission_state = ?, admission_stage = ?, admission_started_at = ?,
             admission_next_check_at = ?, admission_last_check_at = ?,
             admission_claimed_until = NULL, admission_reason = ?
         WHERE id = ? AND deleted_at IS NULL
           AND admission_state = 'quarantined' AND admission_stage = ?
           AND admission_started_at = ?`,
		transition.State, transition.Stage, transition.StartedAt,
		transition.NextCheckAt, checkedAt, transition.Reason, id, expectedStage, expectedStartedAt)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrAdmissionConflict
	}
	return nil
}

func (d *DAO) RetryAdmissionCheck(
	ctx context.Context,
	id uint64,
	expectedStage int,
	expectedStartedAt time.Time,
	next time.Time,
	reason string,
) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts
         SET admission_next_check_at = ?, admission_last_check_at = ?,
             admission_claimed_until = NULL, admission_reason = ?
         WHERE id = ? AND deleted_at IS NULL AND admission_state = 'quarantined'
           AND admission_stage = ? AND admission_started_at = ?`,
		next, time.Now(), reason, id, expectedStage, expectedStartedAt)
	return err
}

func (d *DAO) RejectAdmission(
	ctx context.Context,
	id uint64,
	expectedStage int,
	expectedStartedAt time.Time,
	reason string,
) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts
         SET admission_state = 'rejected', admission_next_check_at = NULL,
             admission_last_check_at = ?, admission_claimed_until = NULL,
             admission_reason = ?, status = 'dead', refresh_error = ?
         WHERE id = ? AND deleted_at IS NULL AND admission_state = 'quarantined'
           AND admission_stage = ? AND admission_started_at = ?`,
		time.Now(), reason, reason, id, expectedStage, expectedStartedAt)
	return err
}

// ListNeedProbeQuota 返回需要探测图片额度的账号。命中以下任一条件即纳入:
//
//	(a) 从未探测过(image_quota_updated_at IS NULL);
//	(b) 上次探测超过 minIntervalSec 秒(常规轮询);
//	(c) **剩余额度=0 且已过 reset_at**:这种"归零等重置"的账号要第一时间补探,
//	    不受 minIntervalSec 限制,避免 5 小时轮询间隔导致的额度恢复滞后显示。
func (d *DAO) ListNeedProbeQuota(ctx context.Context, minIntervalSec int, limit int) ([]*Account, error) {
	rows := make([]*Account, 0, limit)
	threshold := time.Now().Add(-time.Duration(minIntervalSec) * time.Second)
	err := d.db.SelectContext(ctx, &rows,
		`SELECT * FROM oai_accounts
         WHERE deleted_at IS NULL
           AND status = 'healthy'
           AND admission_state = 'admitted'
           AND (token_expires_at IS NULL OR token_expires_at > NOW())
           AND (
                image_quota_updated_at IS NULL
             OR image_quota_updated_at <= ?
             OR (image_quota_remaining = 0
                 AND (image_quota_reset_at IS NULL OR image_quota_reset_at <= NOW()))
           )
         ORDER BY CASE WHEN image_quota_updated_at IS NULL THEN 0 ELSE 1 END,
                  image_quota_updated_at ASC
         LIMIT ?`, threshold, limit)
	fillAll(rows)
	return rows, err
}

// ListAllActiveIDs 用于批量刷新 / 批量探测:返回未软删的所有 id。
func (d *DAO) ListAllActiveIDs(ctx context.Context) ([]uint64, error) {
	ids := make([]uint64, 0, 128)
	err := d.db.SelectContext(ctx, &ids,
		`SELECT id FROM oai_accounts WHERE deleted_at IS NULL ORDER BY id ASC`)
	return ids, err
}

// ListAllActive 返回未软删账号实体,用于管理员手动全量额度探测。
func (d *DAO) ListAllActive(ctx context.Context, limit int) ([]*Account, error) {
	rows := make([]*Account, 0, 128)
	var err error
	if limit > 0 {
		err = d.db.SelectContext(ctx, &rows,
			`SELECT * FROM oai_accounts WHERE deleted_at IS NULL ORDER BY id ASC LIMIT ?`, limit)
	} else {
		err = d.db.SelectContext(ctx, &rows,
			`SELECT * FROM oai_accounts WHERE deleted_at IS NULL ORDER BY id ASC`)
	}
	fillAll(rows)
	return rows, err
}

// QuotaSummary 全局额度汇总。
type QuotaSummary struct {
	TotalRemaining int64 `db:"total_remaining" json:"total_remaining"` // 所有未软删账号剩余额度之和
	TotalCapacity  int64 `db:"total_capacity"  json:"total_capacity"`  // 所有未软删账号上限之和
	ActiveAccounts int64 `db:"active_accounts" json:"active_accounts"` // 未软删账号总数
}

// SumQuota 汇总所有未软删账号的额度(含 dead/suspicious)。
// 账号失效只影响能否被调度出图,不影响其已探测到的额度数字;
// 全部纳入统计才能正确反映账号池的实际剩余容量。
func (d *DAO) SumQuota(ctx context.Context) (*QuotaSummary, error) {
	var s QuotaSummary
	err := d.db.GetContext(ctx, &s, `
SELECT
  COALESCE(SUM(image_quota_remaining), 0) AS total_remaining,
  COALESCE(SUM(image_quota_total),     0) AS total_capacity,
  COUNT(*)                                AS active_accounts
FROM oai_accounts
WHERE deleted_at IS NULL`)
	return &s, err
}

func (d *DAO) Update(ctx context.Context, a *Account) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts
         SET email=?, auth_token_enc=?, refresh_token_enc=?, session_token_enc=?, token_expires_at=?,
             oai_session_id=?, oai_device_id=?, client_id=?, refresh_client_id=?, credential_provenance=?, browser_profile=?,
             chatgpt_account_id=?, account_type=?,
             plan_type=?, daily_image_quota=?,
             status=?, admission_state=?, admission_stage=?, admission_started_at=?,
             admission_next_check_at=?, admission_last_check_at=?, admission_claimed_until=?,
             admission_reason=?, notes=?
         WHERE id = ? AND deleted_at IS NULL`,
		a.Email, a.AuthTokenEnc, a.RefreshTokenEnc, a.SessionTokenEnc, a.TokenExpiresAt,
		a.OAISessionID, a.OAIDeviceID, a.ClientID, a.RefreshClientID, a.CredentialProvenance, a.BrowserProfile,
		a.ChatGPTAccountID, a.AccountType,
		a.PlanType, a.DailyImageQuota,
		a.Status, a.AdmissionState, a.AdmissionStage, a.AdmissionStartedAt,
		a.AdmissionNextCheckAt, a.AdmissionLastCheckAt, a.AdmissionClaimedUntil,
		a.AdmissionReason, a.Notes, a.ID,
	)
	return err
}

func (d *DAO) SoftDelete(ctx context.Context, id uint64) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts SET deleted_at = ? WHERE id = ?`, time.Now(), id)
	return err
}

// SoftDeleteByStatus 按状态批量软删。status 为空时删除全部(调用方需二次确认)。
// 返回删除行数。
func (d *DAO) SoftDeleteByStatus(ctx context.Context, status string) (int64, error) {
	now := time.Now()
	if status == "" {
		res, err := d.db.ExecContext(ctx,
			`UPDATE oai_accounts SET deleted_at = ? WHERE deleted_at IS NULL`, now)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return n, nil
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts SET deleted_at = ? WHERE deleted_at IS NULL AND status = ?`,
		now, status)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// EnsureDeviceID 确保账号有 oai_device_id。
// 如果当前为空,原子写入给定的 deviceID;返回最终实际的 device_id(已有则原值)。
func (d *DAO) EnsureDeviceID(ctx context.Context, id uint64, deviceID string) (string, error) {
	_, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts SET oai_device_id = ?
         WHERE id = ? AND deleted_at IS NULL AND (oai_device_id = '' OR oai_device_id IS NULL)`,
		deviceID, id)
	if err != nil {
		return "", err
	}
	// 回读,兼容其他协程并发填写的情形
	var cur string
	if err := d.db.GetContext(ctx, &cur,
		`SELECT oai_device_id FROM oai_accounts WHERE id = ?`, id); err != nil {
		return "", err
	}
	return cur, nil
}

// EnsureSessionID 确保账号有 oai_session_id(按账号稳定复用)。
// 逻辑与 EnsureDeviceID 完全一致,单独一个函数是为了日志/审计区分用途。
func (d *DAO) EnsureSessionID(ctx context.Context, id uint64, sessionID string) (string, error) {
	_, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts SET oai_session_id = ?
         WHERE id = ? AND deleted_at IS NULL AND (oai_session_id = '' OR oai_session_id IS NULL)`,
		sessionID, id)
	if err != nil {
		return "", err
	}
	var cur string
	if err := d.db.GetContext(ctx, &cur,
		`SELECT oai_session_id FROM oai_accounts WHERE id = ?`, id); err != nil {
		return "", err
	}
	return cur, nil
}

// MarkUsed 更新 last_used_at + 今日计数。today 是当日零点(用于 today_used_date 比较)。
func (d *DAO) MarkUsed(ctx context.Context, id uint64, today time.Time) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts
         SET last_used_at = ?,
             today_used_count = CASE WHEN today_used_date = ? THEN today_used_count + 1 ELSE 1 END,
             today_used_date  = ?
         WHERE id = ?`,
		time.Now(), today, today, id)
	return err
}

// SetStatus 迁移状态,可选 cooldownUntil。
func (d *DAO) SetStatus(ctx context.Context, id uint64, status string, cooldownUntil *time.Time) error {
	return d.SetStatusReason(ctx, id, status, cooldownUntil, "")
}

// SetStatusReason 迁移状态并记录机器可读原因。原因写入 refresh_error,
// 让后台能直接看到账号为什么被降级/打死。
func (d *DAO) SetStatusReason(ctx context.Context, id uint64, status string, cooldownUntil *time.Time, reason string) error {
	if cooldownUntil != nil {
		if reason != "" {
			_, err := d.db.ExecContext(ctx,
				`UPDATE oai_accounts SET status=?, cooldown_until=?, refresh_error=? WHERE id=?`,
				status, *cooldownUntil, reason, id)
			return err
		}
		_, err := d.db.ExecContext(ctx,
			`UPDATE oai_accounts SET status=?, cooldown_until=? WHERE id=?`,
			status, *cooldownUntil, id)
		return err
	}
	if reason != "" {
		_, err := d.db.ExecContext(ctx,
			`UPDATE oai_accounts SET status=?, cooldown_until=NULL, refresh_error=? WHERE id=?`,
			status, reason, id)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts SET status=?, cooldown_until=NULL WHERE id=?`,
		status, id)
	return err
}

// RestoreHealthy clears transient image-runtime failures after a later
// successful dispatch. A successful image proves that both a suspicious
// runtime failure and an expired throttled quota/rate-limit state recovered.
// Dead accounts and throttled accounts without a dispatchable cooldown never
// reach this path and remain excluded.
func (d *DAO) RestoreHealthy(ctx context.Context, id uint64) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts
         SET status='healthy', cooldown_until=NULL, refresh_error=''
         WHERE id=? AND deleted_at IS NULL AND status IN ('suspicious', 'throttled')`,
		id)
	return err
}

// ApplyRefreshResult 原子更新 AT / RT + 过期时间 + 最近刷新信息。
// newRTEnc 为空字符串表示 RT 没有轮转,保持不变。
func (d *DAO) ApplyRefreshResult(
	ctx context.Context,
	id uint64,
	expectedRTEnc string,
	newATEnc string,
	newRTEnc string,
	expiresAt time.Time,
	source string,
) error {
	var (
		res sql.Result
		err error
	)
	if source == RefreshSourceRT && expectedRTEnc == "" {
		return ErrRefreshConflict
	}
	if newRTEnc != "" {
		res, err = d.db.ExecContext(ctx,
			`UPDATE oai_accounts
             SET auth_token_enc = ?,
                 refresh_token_enc = ?,
                 token_expires_at = ?,
                 last_refresh_at = ?,
                 last_refresh_source = ?,
                 refresh_error = '',
                 cooldown_until = CASE WHEN status='throttled' THEN cooldown_until ELSE NULL END,
                 warned_at = NULL,
                 status = CASE WHEN status IN ('dead','suspicious','warned') THEN 'healthy' ELSE status END
			 WHERE id = ? AND deleted_at IS NULL AND refresh_token_enc = ?`,
			newATEnc, newRTEnc, expiresAt, time.Now(), source, id, expectedRTEnc)
	} else if source == RefreshSourceRT {
		res, err = d.db.ExecContext(ctx,
			`UPDATE oai_accounts
             SET auth_token_enc = ?,
                 token_expires_at = ?,
                 last_refresh_at = ?,
                 last_refresh_source = ?,
                 refresh_error = '',
                 cooldown_until = CASE WHEN status='throttled' THEN cooldown_until ELSE NULL END,
                 warned_at = NULL,
                 status = CASE WHEN status IN ('dead','suspicious','warned') THEN 'healthy' ELSE status END
             WHERE id = ? AND deleted_at IS NULL AND refresh_token_enc = ?`,
			newATEnc, expiresAt, time.Now(), source, id, expectedRTEnc)
	} else {
		res, err = d.db.ExecContext(ctx,
			`UPDATE oai_accounts
             SET auth_token_enc = ?,
                 token_expires_at = ?,
                 last_refresh_at = ?,
                 last_refresh_source = ?,
                 refresh_error = '',
                 cooldown_until = CASE WHEN status='throttled' THEN cooldown_until ELSE NULL END,
                 warned_at = NULL,
                 status = CASE WHEN status IN ('dead','suspicious','warned') THEN 'healthy' ELSE status END
             WHERE id = ? AND deleted_at IS NULL`,
			newATEnc, expiresAt, time.Now(), source, id)
	}
	if err != nil {
		return err
	}
	if source == RefreshSourceRT {
		n, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		if n != 1 {
			return ErrRefreshConflict
		}
	}
	return nil
}

func (d *DAO) RotateRefreshTokenCAS(
	ctx context.Context,
	id uint64,
	expectedRTEnc string,
	newRTEnc string,
	reason string,
) error {
	if expectedRTEnc == "" || newRTEnc == "" {
		return ErrRefreshConflict
	}
	res, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts
         SET refresh_token_enc = ?, last_refresh_at = ?,
             last_refresh_source = ?, refresh_error = ?
         WHERE id = ? AND deleted_at IS NULL AND refresh_token_enc = ?`,
		newRTEnc, time.Now(), RefreshSourceRT, reason, id, expectedRTEnc)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrRefreshConflict
	}
	return nil
}

// RecordRefreshError 写入刷新失败原因,同时推进 last_refresh_at(避免 pressed-out 重试)。
//
// markDead=true 时:若当前 AT 尚未过期,只降为 'warned'(账号仍可被调度出图,AT 用完前
// 不要让它离线);若 AT 已过期或为空则真正标 'dead'。
// 这样避免"RT 失效但 AT 还有几天"的账号被提前下线。
func (d *DAO) RecordRefreshError(ctx context.Context, id uint64, source string, reason string, markDead bool) error {
	if markDead {
		_, err := d.db.ExecContext(ctx,
			`UPDATE oai_accounts
             SET last_refresh_at = ?, last_refresh_source = ?, refresh_error = ?,
                 status = CASE
                   WHEN status = 'dead' THEN 'dead'
                   WHEN token_expires_at IS NOT NULL AND token_expires_at > NOW() THEN 'warned'
                   ELSE 'dead'
                 END
             WHERE id = ? AND deleted_at IS NULL`,
			time.Now(), source, reason, id)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts
         SET last_refresh_at = ?, last_refresh_source = ?, refresh_error = ?
         WHERE id = ? AND deleted_at IS NULL`,
		time.Now(), source, reason, id)
	return err
}

// ApplyQuotaResult 更新图片额度探测结果;remaining/total = -1 表示保持原值。
func (d *DAO) ApplyQuotaResult(ctx context.Context, id uint64, remaining, total int, resetAt *time.Time) error {
	q := `UPDATE oai_accounts
          SET image_quota_remaining = CASE WHEN ? < 0 THEN image_quota_remaining ELSE ? END,
              image_quota_total     = CASE WHEN ? < 0 THEN image_quota_total     ELSE ? END,
              image_quota_reset_at  = ?,
              image_quota_updated_at = ?
          WHERE id = ? AND deleted_at IS NULL`
	var reset interface{}
	if resetAt != nil {
		reset = *resetAt
	} else {
		reset = nil
	}
	_, err := d.db.ExecContext(ctx, q, remaining, remaining, total, total, reset, time.Now(), id)
	return err
}

// MarkQuotaDepleted records a definitive upstream image quota limit result.
func (d *DAO) MarkQuotaDepleted(ctx context.Context, accountID uint64, resetAt *time.Time) error {
	return d.ApplyQuotaResult(ctx, accountID, 0, -1, resetAt)
}

// DecrQuota 生图成功后立即乐观扣减账号剩余额度。
// 这样无需等下次探测即可在前端看到正确剩余数字。
// n 为本次消耗张数;GREATEST(0,...) 防止出现负值。
func (d *DAO) DecrQuota(ctx context.Context, accountID uint64, n int) error {
	if n <= 0 {
		return nil
	}
	_, err := d.db.ExecContext(ctx,
		`UPDATE oai_accounts
         SET image_quota_remaining = GREATEST(0, image_quota_remaining - ?)
         WHERE id = ? AND deleted_at IS NULL`,
		n, accountID)
	return err
}

// ---- cookies ----

func (d *DAO) UpsertCookies(ctx context.Context, accountID uint64, cookieEnc string) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO oai_account_cookies (account_id, cookie_json_enc)
         VALUES (?, ?)
         ON DUPLICATE KEY UPDATE cookie_json_enc = VALUES(cookie_json_enc)`,
		accountID, cookieEnc)
	return err
}

func (d *DAO) GetCookies(ctx context.Context, accountID uint64) (string, error) {
	var enc string
	err := d.db.GetContext(ctx, &enc,
		`SELECT cookie_json_enc FROM oai_account_cookies WHERE account_id = ?`,
		accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return enc, err
}

// ---- bindings ----

func (d *DAO) SetBinding(ctx context.Context, accountID, proxyID uint64) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO account_proxy_bindings (account_id, proxy_id)
         VALUES (?, ?)
         ON DUPLICATE KEY UPDATE proxy_id = VALUES(proxy_id), bound_at = CURRENT_TIMESTAMP`,
		accountID, proxyID)
	return err
}

func (d *DAO) RemoveBinding(ctx context.Context, accountID uint64) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM account_proxy_bindings WHERE account_id = ?`, accountID)
	return err
}

func (d *DAO) GetBinding(ctx context.Context, accountID uint64) (*Binding, error) {
	var b Binding
	err := d.db.GetContext(ctx, &b,
		`SELECT * FROM account_proxy_bindings WHERE account_id = ?`, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &b, err
}
