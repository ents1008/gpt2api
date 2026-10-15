package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

// QuotaSettings 热更新参数。
type QuotaSettings interface {
	AccountQuotaProbeEnabled() bool
	AccountQuotaProbeIntervalSec() int
	AccountRefreshConcurrency() int // 复用刷新并发上限
}

// QuotaResult 探测结果。
type QuotaResult struct {
	AccountID       uint64    `json:"account_id"`
	Email           string    `json:"email"`
	OK              bool      `json:"ok"`
	Remaining       int       `json:"remaining"`
	Total           int       `json:"total"`
	ResetAt         time.Time `json:"reset_at,omitempty"`
	DefaultModel    string    `json:"default_model,omitempty"`    // 如 gpt-5-3
	BlockedFeatures []string  `json:"blocked_features,omitempty"` // 被风控限制的功能列表
	Error           string    `json:"error,omitempty"`
}

// QuotaProber 后台定期探测账号图片剩余额度。
type QuotaProber struct {
	svc      *Service
	settings QuotaSettings
	log      *zap.Logger
	client   *http.Client

	proxyResolver AccountProxyResolver

	kick chan struct{}
}

func NewQuotaProber(svc *Service, settings QuotaSettings, logger *zap.Logger) *QuotaProber {
	return &QuotaProber{
		svc:      svc,
		settings: settings,
		log:      logger,
		// client 仅作为"没代理也没 uTLS transport 构造失败"的退路,一般不会走到
		client: newQuotaHTTPClient(nil, 20*time.Second),
		kick:   make(chan struct{}, 1),
	}
}

// SetProxyResolver 注入账号代理解析器;未注入则直连。
func (q *QuotaProber) SetProxyResolver(pr AccountProxyResolver) { q.proxyResolver = pr }

// clientFor 返回一个带 uTLS(伪 Chrome ClientHello)的 http.Client。
// 代理配置错误直接返回，不把账号静默切到直连出口。
//
// 历史背景:探测额度的落点是 POST /backend-api/conversation/init,这条路径和
// 生图走的 f/conversation 同一道 Cloudflare 指纹校验。用 Go 默认 net/http 的
// 标准 TLS 指纹发请求,JA3/JA4 立刻对不上 Chrome,被 403 挡住;而生图之所以
// 没事是因为它走的是 chatgpt.NewUTLSTransport(内部 parrot 成 Chrome)。
// 这里统一复用同一套 uTLS transport,保持整个"后台对 chatgpt.com 的触碰"指纹一致。
func (q *QuotaProber) clientFor(ctx context.Context, a *Account) (*http.Client, error) {
	accountID := uint64(0)
	if a != nil {
		accountID = a.ID
	}
	proxyURL := ""
	if q.proxyResolver != nil {
		proxyURL = q.proxyResolver.ProxyURLForAccount(ctx, accountID)
	}
	tr, err := chatgpt.NewUTLSTransport(proxyURL, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("build utls transport for quota probe account=%d: %w", accountID, err)
	}
	client := newQuotaHTTPClient(tr, q.client.Timeout)
	if q.svc != nil && a != nil {
		if cookies, err := q.svc.DecryptCookies(ctx, a.ID); err == nil && cookies != "" {
			if err := chatgpt.LoadCookies(client.Jar, chatgpt.BaseURL, cookies); err != nil {
				if q.log != nil {
					q.log.Warn("load account cookies for quota probe failed",
						zap.Uint64("account_id", a.ID), zap.Error(err))
				}
			}
		}
	}
	return client, nil
}

func newQuotaHTTPClient(tr http.RoundTripper, timeout time.Duration) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Transport: tr, Timeout: timeout, Jar: jar}
}

func (q *QuotaProber) Kick() {
	select {
	case q.kick <- struct{}{}:
	default:
	}
}

// Run 后台循环。
func (q *QuotaProber) Run(ctx context.Context) {
	q.log.Info("account quota prober started")
	defer q.log.Info("account quota prober stopped")

	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
	}

	// 扫描循环固定 60s 一轮。注意"扫描周期"和"账号探测最小间隔"是两件事:
	//   - 扫描周期 = prober goroutine 多久检查一次 DB,有没有候选要打;
	//   - 探测最小间隔 = 同一账号两次探测之间的最短间隔(5h,由 DAO SQL 决定)。
	// 绑定后者会让 5h 场景下每 100 分钟才扫一次 → "额度=0 补探"分支最长延迟 100 分钟,
	// 达不到用户想要的"归零后尽快更新"。固定 60s 扫描 + SQL WHERE 过滤几乎零成本。
	const scanInterval = 60 * time.Second

	for {
		if q.settings.AccountQuotaProbeEnabled() {
			q.scanOnce(ctx)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(scanInterval):
		case <-q.kick:
		}
	}
}

func (q *QuotaProber) scanOnce(ctx context.Context) {
	if _, err := q.ProbeDue(ctx, 256); err != nil {
		q.log.Warn("quota probe scan failed", zap.Error(err))
	}
}

// ProbeByID 指定账号探测。
func (q *QuotaProber) ProbeByID(ctx context.Context, id uint64) (*QuotaResult, error) {
	a, err := q.svc.dao.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return q.ProbeOne(ctx, a)
}

// ProbeDue 探测按配置到期的健康候选账号。后台循环只调用这一条路径。
func (q *QuotaProber) ProbeDue(ctx context.Context, limit int) ([]*QuotaResult, error) {
	if limit <= 0 {
		limit = 256
	}
	rows, err := q.svc.dao.ListNeedProbeQuota(ctx, q.quotaProbeIntervalSec(), limit)
	if err != nil {
		return nil, err
	}
	return q.probeAccounts(ctx, rows), nil
}

// ProbeAllActive 手动探测所有未软删账号。limit=0 表示不限制数量。
func (q *QuotaProber) ProbeAllActive(ctx context.Context, limit int) ([]*QuotaResult, error) {
	rows, err := q.svc.dao.ListAllActive(ctx, limit)
	if err != nil {
		return nil, err
	}
	return q.probeAccounts(ctx, rows), nil
}

func (q *QuotaProber) probeAccounts(ctx context.Context, rows []*Account) []*QuotaResult {
	results := make([]*QuotaResult, len(rows))
	if len(rows) == 0 {
		return results
	}

	conc := q.quotaProbeConcurrency()
	if conc > len(rows) {
		conc = len(rows)
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, a := range rows {
		i, a := i, a
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res, err := q.ProbeOne(ctx, a)
			if res == nil {
				res = &QuotaResult{AccountID: a.ID, Email: a.Email}
			}
			if err != nil && res.Error == "" {
				res.Error = friendlyProbeErr(err)
			}
			results[i] = res
		}()
	}
	wg.Wait()
	return results
}

func (q *QuotaProber) quotaProbeIntervalSec() int {
	if q.settings == nil {
		return 18000
	}
	n := q.settings.AccountQuotaProbeIntervalSec()
	if n <= 0 {
		return 18000
	}
	return n
}

func (q *QuotaProber) quotaProbeConcurrency() int {
	if q.settings == nil {
		return 4
	}
	n := q.settings.AccountRefreshConcurrency()
	if n <= 0 {
		return 4
	}
	if n > 32 {
		return 32
	}
	return n
}

// ProbeOne 执行一次探测。
// 访问 https://chatgpt.com/backend-api/rate_limits(需要 AT),挑选 image 相关条目汇总。
func (q *QuotaProber) ProbeOne(ctx context.Context, a *Account) (*QuotaResult, error) {
	res := &QuotaResult{AccountID: a.ID, Email: a.Email}
	at, err := q.svc.cipher.DecryptString(a.AuthTokenEnc)
	if err != nil || at == "" {
		res.Error = "AT 解密失败"
		_ = q.svc.dao.ApplyQuotaResult(ctx, a.ID, -1, -1, nil)
		return res, errors.New(res.Error)
	}

	probe, probeErr := q.doProbe(ctx, a, at)
	if probeErr != nil {
		res.Error = friendlyProbeErr(probeErr)
		_ = q.svc.dao.ApplyQuotaResult(ctx, a.ID, -1, -1, nil)
		return res, probeErr
	}

	var resetPtr *time.Time
	if !probe.resetAt.IsZero() {
		resetPtr = &probe.resetAt
	}
	if err := q.svc.dao.ApplyQuotaResult(ctx, a.ID, probe.remaining, probe.total, resetPtr); err != nil {
		res.Error = "写库失败:" + err.Error()
		return res, err
	}
	res.OK = true
	res.Remaining = probe.remaining
	res.Total = probe.total
	res.ResetAt = probe.resetAt
	res.DefaultModel = probe.defaultModel
	res.BlockedFeatures = probe.blockedFeatures
	return res, nil
}

type probeOutcome struct {
	remaining       int
	total           int
	resetAt         time.Time
	defaultModel    string
	blockedFeatures []string
}

// doProbe 调 /backend-api/conversation/init。
//
// 这是 ChatGPT 网页左下角「今日还剩 XX 张图」的数据源,官方不会把这次调用计入额度消耗,
// 适合用于后台定时探测。
//
// 请求 body 参照抓包样例;响应关心的字段是:
//   - limits_progress[].feature_name == "image_gen" → remaining / reset_after
//   - default_model_slug  → 账号默认模型
//   - blocked_features    → 被风控限制的功能;非空需要关注
//
// 指纹注意事项(曾在这里踩过 403):
//   - TLS ClientHello 必须是 Chrome parrot(uTLS),由 q.clientFor 返回的 transport 提供;
//   - HTTP 头部必须对齐 chatgpt.Client.commonHeaders(全套 sec-ch-ua-* / sec-fetch-*
//     / Oai-Device-Id / Oai-Client-Version),少任何一项都可能被 Cloudflare / 业务风控
//     当成脚本直接 403;
//   - User-Agent / sec-ch-ua 两者的版本号必须一致(都是 Edge 143),否则"指纹冲突"。
func (q *QuotaProber) doProbe(ctx context.Context, a *Account, accessToken string) (out probeOutcome, err error) {
	client, clientErr := q.clientFor(ctx, a)
	if clientErr != nil {
		return out, clientErr
	}
	return q.doProbeWithClient(ctx, a, accessToken, client)
}

func (q *QuotaProber) doProbeWithClient(ctx context.Context, a *Account, accessToken string, client *http.Client) (out probeOutcome, err error) {
	out.remaining = -1
	out.total = -1
	if client == nil {
		client = q.client
	}
	if client == nil {
		client = newQuotaHTTPClient(nil, 20*time.Second)
	}

	profile, _, profileErr := chatgpt.ParseBrowserProfileJSON(a.BrowserProfile)
	if profileErr != nil {
		err = profileErr
		return
	}
	reqBody, marshalErr := json.Marshal(map[string]interface{}{
		"gizmo_id":                nil,
		"requested_default_model": nil,
		"conversation_id":         nil,
		"timezone_offset_min":     profile.TimezoneOffsetMinutes(time.Now()),
		"system_hints":            []string{"picture_v2"},
	})
	if marshalErr != nil {
		err = marshalErr
		return
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://chatgpt.com/backend-api/conversation/init", bytes.NewReader(reqBody))
	if err != nil {
		return
	}
	deviceID := quotaDeviceID(a)
	if e := bootstrapQuotaSession(ctx, client, deviceID, profile); e != nil {
		err = e
		return
	}

	// ── 基础头 ──
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", chatgpt.BaseURL)
	req.Header.Set("Referer", chatgpt.BaseURL+"/")
	chatgpt.ApplyBrowserFingerprintHeaders(req.Header, profile)

	// ── Client-Hints(Edge 143 @ Windows 11,必须跟 UA 版本一致)──

	// ── Fetch 元数据(同源 XHR)──
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Priority", "u=1, i")

	// ── Oai-* 业务指纹(chatgpt.com 自己埋的)──
	// DeviceID 缺失会触发降权/频繁 403,这里用账号绑定的;实在没存就退回随机填充,
	// 起码让请求长得像浏览器。
	req.Header.Set("Oai-Device-Id", deviceID)
	if sid := strings.TrimSpace(a.OAISessionID); sid != "" {
		req.Header.Set("Oai-Session-Id", sid)
	}
	req.Header.Set("Oai-Language", profile.Language)
	req.Header.Set("Oai-Client-Version", chatgpt.DefaultClientVersion)
	req.Header.Set("Oai-Client-Build-Number", chatgpt.DefaultClientBuildNum)

	// ── X-Openai-Target-* (chatgpt web 每请求必带,值就是 URL path)──
	req.Header.Set("X-Openai-Target-Path", req.URL.Path)
	req.Header.Set("X-Openai-Target-Route", req.URL.Path)

	resp, e := client.Do(req)
	if e != nil {
		err = e
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		err = fmt.Errorf("conversation/init http=%d body=%s", resp.StatusCode, truncate(string(data), 200))
		return
	}

	var payload struct {
		Type             string             `json:"type"`
		BlockedFeatures  flexibleStringList `json:"blocked_features"`
		DefaultModelSlug string             `json:"default_model_slug"`
		LimitsProgress   []struct {
			FeatureName string `json:"feature_name"`
			Remaining   *int   `json:"remaining"`
			ResetAfter  string `json:"reset_after"`
		} `json:"limits_progress"`
	}
	if err = json.Unmarshal(data, &payload); err != nil {
		return
	}
	out.defaultModel = payload.DefaultModelSlug
	out.blockedFeatures = payload.BlockedFeatures

	for _, item := range payload.LimitsProgress {
		if !isImageFeature(item.FeatureName) {
			continue
		}
		if item.Remaining != nil {
			if out.remaining < 0 || *item.Remaining < out.remaining {
				out.remaining = *item.Remaining
			}
		}
		if item.ResetAfter != "" {
			if t, e := time.Parse(time.RFC3339, item.ResetAfter); e == nil {
				if out.resetAt.IsZero() || t.Before(out.resetAt) {
					out.resetAt = t
				}
			}
		}
	}
	return
}

type flexibleStringList []string

func (l *flexibleStringList) UnmarshalJSON(data []byte) error {
	var raw []interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		switch v := item.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				out = append(out, s)
			}
		case map[string]interface{}:
			if s := blockedFeatureObjectName(v); s != "" {
				out = append(out, s)
				continue
			}
			if b, err := json.Marshal(v); err == nil {
				out = append(out, truncate(string(b), 120))
			}
		}
	}
	*l = out
	return nil
}

func blockedFeatureObjectName(v map[string]interface{}) string {
	for _, key := range []string{"feature_name", "name", "id", "key", "type"} {
		if raw, ok := v[key]; ok {
			if s, ok := raw.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func quotaDeviceID(a *Account) string {
	if a == nil {
		return fallbackDeviceID(0)
	}
	deviceID := strings.TrimSpace(a.OAIDeviceID)
	if deviceID == "" {
		deviceID = fallbackDeviceID(a.ID)
	}
	return deviceID
}

func bootstrapQuotaSession(ctx context.Context, client *http.Client, deviceID string, profile chatgpt.BrowserProfile) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chatgpt.BaseURL+"/", nil)
	if err != nil {
		return err
	}
	chatgpt.ApplyBrowserFingerprintHeaders(req.Header, profile)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Oai-Device-Id", deviceID)
	req.Header.Set("Oai-Language", profile.Language)
	req.Header.Set("Oai-Client-Version", chatgpt.DefaultClientVersion)
	req.Header.Set("Oai-Client-Build-Number", chatgpt.DefaultClientBuildNum)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("bootstrap http=%d body=%s", resp.StatusCode, truncate(string(data), 160))
	}
	return nil
}

// fallbackDeviceID 兜底 Oai-Device-Id:用账号 ID 拼一个固定的 uuid-like 字符串,
// 保证同一账号每次探测都发一样的 device-id(这跟"用户在浏览器里的固定 LocalStorage"
// 语义一致),避免被风控标成"跳跃式设备"。生产环境建议在首次登录时就把真实
// device-id 写进 oai_accounts.oai_device_id。
func fallbackDeviceID(accountID uint64) string {
	// 形如 00000000-0000-4000-8000-xxxxxxxxxxxx(变体位正确,RFC 4122 v4)
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", accountID%1_000_000_000_000)
}

func isImageFeature(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "image_gen", "image_generation", "image_edit", "img_gen":
		return true
	}
	return strings.Contains(n, "image_gen") || strings.Contains(n, "img_gen")
}

func friendlyProbeErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "http=401"):
		return "AT 已过期,无法探测额度"
	case strings.Contains(low, "http=403"):
		return "上游拒绝访问(403)"
	case strings.Contains(low, "http=429"):
		return "上游速率限制(429)"
	case strings.Contains(low, "timeout"), strings.Contains(low, "deadline exceeded"):
		return "探测超时"
	case strings.Contains(low, "connection refused"), strings.Contains(low, "no such host"):
		return "网络不通"
	default:
		if len(s) > 160 {
			s = s[:160] + "…"
		}
		return "探测失败:" + s
	}
}
