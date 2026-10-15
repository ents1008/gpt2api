// Package scheduler 负责 chatgpt.com 账号的并发安全调度。
//
// 核心规则(参考 RISK_AND_SAAS.md):
//  1. 一号一锁:同账号同时只允许 1 个请求占用(Redis SETNX)。
//  2. 最小间隔:同账号相邻请求 >= min_interval_sec。
//  3. 状态机:healthy -> warned -> throttled -> suspicious -> dead,冷却过期自动恢复。
//  4. 选择策略:status=healthy + cooldown 到期 + last_used_at 最早的优先。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/432539/gpt2api/internal/account"
	"github.com/432539/gpt2api/internal/config"
	"github.com/432539/gpt2api/internal/proxy"
	"github.com/432539/gpt2api/pkg/lock"
	"github.com/432539/gpt2api/pkg/logger"

	"go.uber.org/zap"
)

// ErrNoAvailable 没有任何账号可用。
var ErrNoAvailable = errors.New("scheduler: no available account")

// Lease 代表一次账号占用的租约。
type Lease struct {
	Account            *account.Account
	AuthToken          string // 已解密
	ProxyURL           string // 已带密码
	ProxyID            uint64
	DeviceID           string
	SessionID          string // oai_session_id(按账号稳定)
	Cookies            string // JSON cookies,可选;用于复用账号注册/浏览器会话
	BrowserProfile     string // 账号注册浏览器的无凭据运行画像
	lockKey            string
	lockToken          string
	persistCookiesFunc func(context.Context, string) error
	releaseFunc        func(context.Context) error
}

// PersistCookies commits the browser cookie snapshot while this lease still
// owns the account lock. Callers should invoke it before Release.
func (l *Lease) PersistCookies(ctx context.Context, raw string) error {
	if strings.TrimSpace(raw) == "" || raw == l.Cookies {
		return nil
	}
	if l.persistCookiesFunc != nil {
		if err := l.persistCookiesFunc(ctx, raw); err != nil {
			return err
		}
		l.Cookies = raw
	}
	return nil
}

// Release 释放锁并更新账号 last_used_at / today_used。
func (l *Lease) Release(ctx context.Context) error {
	if l.releaseFunc != nil {
		return l.releaseFunc(ctx)
	}
	return nil
}

// RuntimeParams 调度器运行期可热更的参数。
//   - 由外部 settings.Service 提供回调,每次读都取最新值;
//   - 回调未注入时回退到 cfg 的静态值。
type RuntimeParams struct {
	// 为 nil 时 Scheduler 使用 cfg 里的静态值。
	DailyUsageRatio func() float64
	Cooldown429Sec  func() int
	WarnedPauseHrs  func() int
	// QueueWaitSec 拿不到空闲账号时最长排队等待秒数,≤0 表示不排队(老语义)。
	QueueWaitSec func() int
}

// Scheduler 账号调度器。
type Scheduler struct {
	accSvc   *account.Service
	proxySvc *proxy.Service
	lock     *lock.RedisLock
	cfg      config.SchedulerConfig
	rt       RuntimeParams
}

func New(
	accSvc *account.Service,
	proxySvc *proxy.Service,
	rl *lock.RedisLock,
	cfg config.SchedulerConfig,
) *Scheduler {
	if cfg.LockTTLSec <= 0 {
		cfg.LockTTLSec = 180
	}
	if cfg.MinIntervalSec <= 0 {
		cfg.MinIntervalSec = 5
	}
	if cfg.DailyUsageRatio <= 0 {
		cfg.DailyUsageRatio = 0.8
	}
	if cfg.Cooldown429Sec <= 0 {
		cfg.Cooldown429Sec = 300
	}
	return &Scheduler{accSvc: accSvc, proxySvc: proxySvc, lock: rl, cfg: cfg}
}

// SetRuntime 注入运行期可热更的参数。建议在 main 里一次性设置:
//
//	sched.SetRuntime(scheduler.RuntimeParams{
//	    DailyUsageRatio: settingsSvc.DailyUsageRatio,
//	    Cooldown429Sec:  settingsSvc.Cooldown429Sec,
//	    WarnedPauseHrs:  settingsSvc.WarnedPauseHours,
//	})
func (s *Scheduler) SetRuntime(p RuntimeParams) { s.rt = p }

// 下面三个 getter 用于内部调用,保证"有回调用回调,否则用 cfg"。
func (s *Scheduler) dailyUsageRatio() float64 {
	if s.rt.DailyUsageRatio != nil {
		if v := s.rt.DailyUsageRatio(); v > 0 && v <= 1 {
			return v
		}
	}
	return s.cfg.DailyUsageRatio
}
func (s *Scheduler) cooldown429() time.Duration {
	if s.rt.Cooldown429Sec != nil {
		if v := s.rt.Cooldown429Sec(); v > 0 {
			return time.Duration(v) * time.Second
		}
	}
	return time.Duration(s.cfg.Cooldown429Sec) * time.Second
}
func (s *Scheduler) warnedPause() time.Duration {
	if s.rt.WarnedPauseHrs != nil {
		if v := s.rt.WarnedPauseHrs(); v > 0 {
			return time.Duration(v) * time.Hour
		}
	}
	return time.Duration(s.cfg.WarnedPauseHours) * time.Hour
}
func (s *Scheduler) suspiciousPause() time.Duration {
	return s.cooldown429()
}

// queueWait 拿不到账号时的最长排队等待时间。
// 返回 0 表示关闭排队(立即返回 ErrNoAvailable)。
func (s *Scheduler) queueWait() time.Duration {
	if s.rt.QueueWaitSec != nil {
		if v := s.rt.QueueWaitSec(); v >= 0 {
			return time.Duration(v) * time.Second
		}
	}
	return 120 * time.Second
}

// Dispatch 为本次请求挑选一个账号并加锁。调用方必须 defer lease.Release(ctx)。
//
// 语义(一号一任务 + 排队):
//   - 同账号同时只允许 1 个请求持有 Redis 锁(acct:lock:{id},SETNX+TTL)。
//   - 扫一遍所有 candidate 都被锁住 / 不满足 min_interval 时,
//     不立即返回失败,而是按指数退避轮询重试,直到拿到锁或超过 queueWait。
//   - queueWait=0 时退化为老语义(扫一次,失败即返回 ErrNoAvailable)。
func (s *Scheduler) Dispatch(ctx context.Context, modelType string) (*Lease, error) {
	deadline := time.Now().Add(s.queueWait())

	const (
		minBackoff = 200 * time.Millisecond
		maxBackoff = 2 * time.Second
	)
	backoff := minBackoff

	attempt := 0
	start := time.Now()

	for {
		attempt++
		lease, err := s.tryDispatchOnce(ctx, modelType)
		if err == nil {
			if attempt > 1 {
				logger.L().Info("scheduler queued dispatch ok",
					zap.Int("attempt", attempt),
					zap.Duration("waited", time.Since(start)),
					zap.Uint64("account_id", lease.Account.ID))
			}
			return lease, nil
		}
		if !errors.Is(err, ErrNoAvailable) {
			return nil, err
		}

		// 所有候选都忙或不就绪:排队等待。
		if !time.Now().Before(deadline) {
			return nil, ErrNoAvailable
		}
		wait := backoff
		if remain := time.Until(deadline); remain < wait {
			wait = remain
		}
		if wait <= 0 {
			return nil, ErrNoAvailable
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		// 指数退避(×1.5)
		backoff += backoff / 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// tryDispatchOnce 扫一遍 candidate,尝试为其中一个加锁;
// 全部 candidate 都被锁 / 不满足 min_interval 时返回 ErrNoAvailable。
func (s *Scheduler) tryDispatchOnce(ctx context.Context, modelType string) (*Lease, error) {
	limit := 30
	dao := s.accSvc.DAO()
	candidates, err := dao.ListDispatchable(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("scheduler list: %w", err)
	}
	if len(candidates) == 0 {
		return nil, ErrNoAvailable
	}

	now := time.Now()
	minInterval := time.Duration(s.cfg.MinIntervalSec) * time.Second

	for _, acc := range candidates {
		if shouldSkipDispatchCandidate(acc, now, minInterval) {
			continue
		}
		lease, err := s.tryLock(ctx, acc)
		if err == nil {
			if shouldSkipLeaseForMissingProxy(modelType, lease.ProxyURL) {
				logger.L().Warn("scheduler image account missing proxy binding",
					zap.Uint64("account_id", acc.ID))
				s.markImageAccountMissingProxy(ctx, acc.ID)
				_ = lease.Release(ctx)
				continue
			}
			return lease, nil
		}
		if errors.Is(err, lock.ErrNotAcquired) {
			// 被别的请求占用,下一个候选
			continue
		}
		logger.L().Warn("scheduler tryLock error",
			zap.Uint64("account_id", acc.ID), zap.Error(err))
	}
	return nil, ErrNoAvailable
}

func shouldSkipDispatchCandidate(acc *account.Account, now time.Time, minInterval time.Duration) bool {
	if acc == nil {
		return true
	}
	return acc.LastUsedAt.Valid && now.Sub(acc.LastUsedAt.Time) < minInterval
}

func shouldSkipLeaseForMissingProxy(modelType, proxyURL string) bool {
	return modelType == "image" && strings.TrimSpace(proxyURL) == ""
}

func missingProxyBindingPauseUntil(now time.Time) time.Time {
	return now.Add(24 * time.Hour)
}

func (s *Scheduler) markImageAccountMissingProxy(ctx context.Context, accountID uint64) {
	if s == nil || s.accSvc == nil || accountID == 0 {
		return
	}
	pauseUntil := missingProxyBindingPauseUntil(time.Now())
	_ = s.accSvc.DAO().SetStatusReason(ctx, accountID, account.StatusThrottled, &pauseUntil, "missing_proxy_binding")
}

func (s *Scheduler) tryLock(ctx context.Context, acc *account.Account) (*Lease, error) {
	key := fmt.Sprintf("acct:lock:%d", acc.ID)
	token := uuid.NewString()
	ttl := time.Duration(s.cfg.LockTTLSec) * time.Second
	if err := s.lock.Acquire(ctx, key, token, ttl); err != nil {
		return nil, err
	}

	authToken, err := s.accSvc.DecryptAuthToken(acc)
	if err != nil {
		_ = s.lock.Release(ctx, key, token)
		return nil, fmt.Errorf("decrypt auth_token: %w", err)
	}

	// 首次使用时为账号补发一个持久化的 oai_device_id(导入时常为空)。
	// chatgpt.com 要求请求头带 oai-device-id,等同于浏览器首访拿到的 oai-did cookie;
	// 一次生成后持久化,账号绑定的"设备身份"保持稳定,避免每次换 id 触发风控。
	deviceID := acc.OAIDeviceID
	if deviceID == "" {
		gen := uuid.NewString()
		if fixed, err := s.accSvc.DAO().EnsureDeviceID(ctx, acc.ID, gen); err == nil && fixed != "" {
			deviceID = fixed
			acc.OAIDeviceID = fixed
		} else {
			deviceID = gen
		}
	}

	// oai_session_id:真实浏览器是"每打开页面生成一次"。为了保持账号行为稳定
	// (风控倾向于把频繁变换 session_id 的账号识别为脚本),我们按账号持久化,
	// 与 device_id 同策略。
	sessionID := acc.OAISessionID
	if sessionID == "" {
		gen := uuid.NewString()
		if fixed, err := s.accSvc.DAO().EnsureSessionID(ctx, acc.ID, gen); err == nil && fixed != "" {
			sessionID = fixed
			acc.OAISessionID = fixed
		} else {
			sessionID = gen
		}
	}

	var proxyURL string
	var proxyID uint64
	if b, _ := s.accSvc.GetBinding(ctx, acc.ID); b != nil {
		p, err := s.proxySvc.Get(ctx, b.ProxyID)
		if err == nil && p != nil && p.Enabled {
			if u, err := s.proxySvc.BuildURL(p); err == nil {
				proxyURL = u
				proxyID = p.ID
			}
		}
	}
	cookies, err := s.accSvc.DecryptCookies(ctx, acc.ID)
	if err != nil {
		logger.L().Warn("scheduler decrypt account cookies failed",
			zap.Uint64("account_id", acc.ID), zap.Error(err))
	}

	accCopy := acc
	lease := &Lease{
		Account:        accCopy,
		AuthToken:      authToken,
		ProxyURL:       proxyURL,
		ProxyID:        proxyID,
		DeviceID:       deviceID,
		SessionID:      sessionID,
		Cookies:        cookies,
		BrowserProfile: acc.BrowserProfile,
		lockKey:        key,
		lockToken:      token,
	}
	lease.persistCookiesFunc = func(c context.Context, raw string) error {
		return s.accSvc.StoreCookies(c, accCopy.ID, raw)
	}
	lease.releaseFunc = func(c context.Context) error {
		today := truncateDay(time.Now())
		_ = s.accSvc.DAO().MarkUsed(c, accCopy.ID, today)
		return s.lock.Release(c, key, token)
	}
	return lease, nil
}

// MarkRateLimited 上游 429:标记账号冷却并降级状态。
func (s *Scheduler) MarkRateLimited(ctx context.Context, accountID uint64) {
	s.MarkRateLimitedFor(ctx, accountID, s.cooldown429())
}

// MarkRateLimitedFor 标记账号按上游明确 reset 时间冷却。
func (s *Scheduler) MarkRateLimitedFor(ctx context.Context, accountID uint64, cooldownFor time.Duration) {
	if cooldownFor <= 0 {
		cooldownFor = s.cooldown429()
	}
	cooldown := time.Now().Add(cooldownFor)
	_ = s.accSvc.DAO().SetStatus(ctx, accountID, account.StatusThrottled, &cooldown)
}

// MarkWarned 上游返回 suspicious 横幅时降级。
func (s *Scheduler) MarkWarned(ctx context.Context, accountID uint64) {
	pause := time.Now().Add(s.warnedPause())
	_ = s.accSvc.DAO().SetStatus(ctx, accountID, account.StatusWarned, &pause)
}

// MarkSuspicious 标记账号遇到可恢复的风控/环境异常,冷却后可由恢复流程重新确认。
func (s *Scheduler) MarkSuspicious(ctx context.Context, accountID uint64, reason string) {
	pause := time.Now().Add(s.suspiciousPauseForReason(reason))
	_ = s.accSvc.DAO().SetStatusReason(ctx, accountID, account.StatusSuspicious, &pause, reason)
}

func (s *Scheduler) suspiciousPauseForReason(reason string) time.Duration {
	switch strings.TrimSpace(reason) {
	case "upstream_403",
		"proxy_connect_403",
		"turnstile_stalled":
		return 2 * time.Hour
	case "upstream_network_transient",
		"download_failed",
		"poll_timeout",
		"conversation_poll_error",
		"image_task_stalled",
		"image_tool_without_asset",
		"free_account_stalled",
		"pow_timeout",
		"pow_failed",
		"image_async_transient":
		return 30 * time.Minute
	default:
		return s.suspiciousPause()
	}
}

// MarkDead 账号彻底不可用。
func (s *Scheduler) MarkDead(ctx context.Context, accountID uint64) {
	s.MarkDeadWithReason(ctx, accountID, "mark_dead")
}

// MarkDeadWithReason 账号彻底不可用,并记录机器可读原因。
func (s *Scheduler) MarkDeadWithReason(ctx context.Context, accountID uint64, reason string) {
	_ = s.accSvc.DAO().SetStatusReason(ctx, accountID, account.StatusDead, nil, reason)
}

// RestoreHealthy 调度成功后回归健康。这里只恢复 suspicious 账号:
// 成功出图能证明 403/上传超时这类临时异常已恢复,但不能证明 dead/RT
// 失效或长期 throttled 的根因已经消失。
func (s *Scheduler) RestoreHealthy(ctx context.Context, accountID uint64) {
	_ = s.accSvc.DAO().RestoreHealthy(ctx, accountID)
}

// ------ helpers ------

func truncateDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day()
}
