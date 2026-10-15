package account

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/publicsuffix"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

// AccountProxyResolver 把账号 ID 映射成代理 URL(形如 http(s)://user:pass@host:port)。
// 由 main.go 用 account.Service + proxy.Service 组装后注入;未绑定或禁用返回 ""。
type AccountProxyResolver interface {
	ProxyURLForAccount(ctx context.Context, accountID uint64) string
}

// RefreshSettings 热更新参数。由 settings.Service 实现。
type RefreshSettings interface {
	AccountRefreshEnabled() bool
	AccountRefreshIntervalSec() int
	AccountRefreshAheadSec() int
	AccountRefreshConcurrency() int
	AccountDefaultClientID() string
}

// RefreshResult 刷新结果。
type RefreshResult struct {
	AccountID uint64    `json:"account_id"`
	Email     string    `json:"email"`
	OK        bool      `json:"ok"`
	Source    string    `json:"source"` // rt / st / failed
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Error     string    `json:"error,omitempty"`
	RTRotated bool      `json:"rt_rotated,omitempty"`
	// ATVerified 表示新 AT 已通过图片链路的 chat-requirements/prepare。
	// 只要 Source=st 必为 true;Source=rt 时取决于 verify 结果;Source=failed 时为 false。
	ATVerified bool `json:"at_verified"`
	// WebUnauthorized 为 true 表示 RT 换出来的 AT 被 chatgpt.com 以 401 拒绝
	// (iOS OAuth 作用域 vs Web 作用域不一致)。前端据此提示用户补充 Session Token。
	WebUnauthorized bool `json:"web_unauthorized,omitempty"`
}

// Refresher 负责把账号的 AT 通过 RT 或 ST 刷新成新的 AT。
type Refresher struct {
	svc      *Service
	settings RefreshSettings
	log      *zap.Logger
	client   *http.Client

	proxyResolver AccountProxyResolver
	refreshLocks  sync.Map

	kick chan struct{}
}

// NewRefresher 构造。
// HTTP client 默认直连(uTLS transport + InsecureSkipVerify,兼容 SSL Inspection 代理);
// 如果注入了 AccountProxyResolver,则每次刷新会优先使用账号绑定的代理。
func NewRefresher(svc *Service, settings RefreshSettings, logger *zap.Logger) *Refresher {
	// 直连时也用 uTLS transport,保持与探测/出图一致的 TLS 指纹,
	// 同时 InsecureSkipVerify=true 兼容 SSL Inspection 代理环境。
	defaultTr, err := chatgpt.NewUTLSTransport("", 30*time.Second)
	if err != nil {
		// 理论上不会失败(空代理 URL);降级到裸 http.Transport
		defaultTr = http.DefaultTransport
		logger.Warn("refresher: failed to create uTLS transport, fallback to default", zap.Error(err))
	}
	return &Refresher{
		svc:      svc,
		settings: settings,
		log:      logger,
		client: &http.Client{
			Transport: defaultTr,
			Timeout:   30 * time.Second,
		},
		kick: make(chan struct{}, 1),
	}
}

// Kick 立刻触发一次扫描(批量刷新按钮触发时调)。
// SetProxyResolver 注入账号代理解析器。
// 未调用时 Refresher 保持直连行为(兼容无代理池环境)。
func (r *Refresher) SetProxyResolver(pr AccountProxyResolver) { r.proxyResolver = pr }

// clientFor 根据账号 ID 选择合适的 http.Client:
//   - proxyResolver 未注入或返回空 URL → 用默认直连 client(uTLS)
//   - 返回非空 URL → 构造一次性带代理的 uTLS client(结束后 GC)
//
// 全部使用 chatgpt.NewUTLSTransport,原因:
//  1. 伪装 Chrome TLS 指纹,避免 Cloudflare 拦截 auth.openai.com / chatgpt.com 请求;
//  2. InsecureSkipVerify=true 兼容做 SSL Inspection 的商业代理(证书由代理重签)。
//
// 代理 URL 无效时直接返回错误，禁止把账号静默切到不同出口。
func (r *Refresher) clientFor(ctx context.Context, accountID uint64) (*http.Client, error) {
	return r.clientForProxy(ctx, accountID, nil)
}

func (r *Refresher) clientForProxy(ctx context.Context, accountID uint64, fixedProxyURL *string) (*http.Client, error) {
	if fixedProxyURL != nil {
		pu := strings.TrimSpace(*fixedProxyURL)
		if pu == "" {
			return nil, fmt.Errorf("fixed image lease proxy is empty for account=%d", accountID)
		}
		tr, err := chatgpt.NewUTLSTransport(pu, 30*time.Second)
		if err != nil {
			return nil, fmt.Errorf("invalid fixed proxy url for refresh account=%d: %w", accountID, err)
		}
		return &http.Client{Transport: tr, Timeout: r.client.Timeout}, nil
	}
	if r.proxyResolver == nil {
		return r.client, nil
	}
	pu := r.proxyResolver.ProxyURLForAccount(ctx, accountID)
	if pu == "" {
		return nil, fmt.Errorf("account proxy binding unavailable for refresh account=%d", accountID)
	}
	tr, err := chatgpt.NewUTLSTransport(pu, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy url for refresh account=%d: %w", accountID, err)
	}
	return &http.Client{Transport: tr, Timeout: r.client.Timeout}, nil
}

func (r *Refresher) lockAccountRefresh(accountID uint64) func() {
	value, _ := r.refreshLocks.LoadOrStore(accountID, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func accountRefreshClientID(a *Account, fallback string) string {
	if a != nil {
		if value := strings.TrimSpace(a.RefreshClientID); value != "" {
			return value
		}
		if value := strings.TrimSpace(a.ClientID); value != "" {
			return value
		}
	}
	return strings.TrimSpace(fallback)
}

func (r *Refresher) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run 后台循环。
func (r *Refresher) Run(ctx context.Context) {
	r.log.Info("account refresher started")
	defer r.log.Info("account refresher stopped")

	// 启动延迟 5s,避免和 migration 抢锁
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
	}

	for {
		interval := time.Duration(r.settings.AccountRefreshIntervalSec()) * time.Second
		if interval < 30*time.Second {
			interval = 30 * time.Second
		}

		if r.settings.AccountRefreshEnabled() {
			r.scanOnce(ctx)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		case <-r.kick:
		}
	}
}

func (r *Refresher) scanOnce(ctx context.Context) {
	ahead := r.settings.AccountRefreshAheadSec()
	conc := r.settings.AccountRefreshConcurrency()

	rows, err := r.svc.dao.ListNeedRefresh(ctx, ahead, 256)
	if err != nil {
		r.log.Warn("list need-refresh accounts failed", zap.Error(err))
		return
	}
	if len(rows) == 0 {
		return
	}
	r.log.Info("refreshing accounts", zap.Int("count", len(rows)), zap.Int("ahead_sec", ahead), zap.Int("concurrency", conc))

	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for _, a := range rows {
		a := a
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			_, _ = r.RefreshAuto(ctx, a)
		}()
	}
	wg.Wait()
}

// RefreshByID 指定 id 刷新。
func (r *Refresher) RefreshByID(ctx context.Context, id uint64) (*RefreshResult, error) {
	a, err := r.svc.dao.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return r.RefreshAuto(ctx, a)
}

// RefreshAuto 优先 RT,失败/没有 RT 回退 ST;都失败则 markDead。
//
// 判定规则(严格):
//
//  1. RT → AT HTTP 成功后,立即调用 chat-requirements/prepare 做图片链路校验
//     - 200:写库,Source=rt,ATVerified=true,返回成功
//     - 非 200(含 401/403/429/5xx/网络错误):**视为 RT 不可用**,丢弃本次 AT,
//     不写库,继续尝试 ST
//  2. ST → AT HTTP 成功:写库,Source=st,ATVerified=true,返回成功
//  3. 两条路径都未拿到可用 AT:返回失败(ok=false),账号标 warned(RT 被 web 拒)
//     或 dead(完全没可用 token)。不会把"无法校验通过"的 AT 悄悄写进库。
//
// 这条规则确保前端看到「刷新成功」时,AT 一定是 chatgpt.com web 后端接受的,
// 后续探测 / 聊天 / 图像请求不会因作用域不匹配而 401。
type refreshScope struct {
	fixedProxyURL  *string
	browserCookies string
}

func (r *Refresher) RefreshAuto(ctx context.Context, a *Account) (*RefreshResult, error) {
	if a == nil {
		return nil, errors.New("account is nil")
	}
	release := r.lockAccountRefresh(a.ID)
	defer release()
	latest, err := r.svc.dao.GetByID(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	return r.refreshAutoUnlocked(ctx, latest, refreshScope{})
}

func (r *Refresher) ValidateAccountForImage(ctx context.Context, accountID uint64) (int, error) {
	release := r.lockAccountRefresh(accountID)
	defer release()
	a, err := r.svc.dao.GetByID(ctx, accountID)
	if err != nil {
		return 0, err
	}
	authToken, err := r.svc.DecryptAuthToken(a)
	if err != nil {
		return 0, err
	}
	cookies, err := r.svc.DecryptCookies(ctx, accountID)
	if err != nil {
		return 0, err
	}
	return r.verifyATForImage(ctx, a, authToken, cookies)
}

func (r *Refresher) RecoverAccountForImage(ctx context.Context, accountID uint64) (bool, error) {
	release := r.lockAccountRefresh(accountID)
	defer release()
	a, err := r.svc.dao.GetByID(ctx, accountID)
	if err != nil {
		return false, err
	}
	res, err := r.refreshAutoUnlocked(ctx, a, refreshScope{})
	if err != nil {
		return false, err
	}
	return res != nil && res.OK, nil
}

func (r *Refresher) RecoverImageCredential(
	ctx context.Context,
	accountID uint64,
	proxyURL string,
	cookies string,
) (string, string, bool, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return "", "", false, errors.New("image lease has no fixed proxy")
	}
	release := r.lockAccountRefresh(accountID)
	defer release()
	a, err := r.svc.dao.GetByID(ctx, accountID)
	if err != nil {
		return "", "", false, err
	}
	res, err := r.refreshAutoUnlocked(ctx, a, refreshScope{
		fixedProxyURL:  &proxyURL,
		browserCookies: cookies,
	})
	if err != nil {
		return "", "", false, err
	}
	if res == nil || !res.OK {
		return "", "", false, nil
	}
	latest, err := r.svc.dao.GetByID(ctx, accountID)
	if err != nil {
		return "", "", false, err
	}
	authToken, err := r.svc.DecryptAuthToken(latest)
	if err != nil {
		return "", "", false, err
	}
	latestCookies, err := r.svc.DecryptCookies(ctx, accountID)
	if err != nil {
		return "", "", false, err
	}
	if strings.TrimSpace(latestCookies) == "" {
		latestCookies = cookies
	}
	return authToken, latestCookies, true, nil
}

func (r *Refresher) refreshAutoUnlocked(ctx context.Context, a *Account, scope refreshScope) (*RefreshResult, error) {
	res := &RefreshResult{AccountID: a.ID, Email: a.Email}
	if isImmutableQuarantinedWebCredential(a.CredentialProvenance, a.AdmissionState) {
		res.Source = "skipped"
		res.Error = "Browser Web account is under immutable admission observation"
		return res, nil
	}
	browserCookies := strings.TrimSpace(scope.browserCookies)
	if r.svc != nil {
		if browserCookies == "" {
			var cookieErr error
			browserCookies, cookieErr = r.svc.DecryptCookies(ctx, a.ID)
			if cookieErr != nil {
				r.log.Warn("decrypt account cookies for refresh failed",
					zap.Uint64("id", a.ID), zap.Error(cookieErr))
			}
		}
	}

	// 标记:RT 换出的 AT 是否被 web 后端以 401 拒绝(iOS scope vs web scope 不匹配)
	var rtRejectedByWeb bool

	// 尝试 RT
	if a.RefreshTokenEnc.Valid && a.RefreshTokenEnc.String != "" {
		rt, err := r.svc.cipher.DecryptString(a.RefreshTokenEnc.String)
		if err == nil && rt != "" {
			clientID := accountRefreshClientID(a, r.settings.AccountDefaultClientID())
			newAT, newRT, expAt, err := r.rtToATWithProxy(ctx, a.ID, rt, clientID, scope.fixedProxyURL)
			if err == nil && newAT != "" {
				// RT → AT HTTP 200,现在校验 AT 能否被 chatgpt.com web 后端接受
				verifyStatus, verifyErr := r.verifyATForImageWithProxy(ctx, a, newAT, browserCookies, scope.fixedProxyURL)
				if (verifyErr != nil || verifyStatus != http.StatusOK) && newRT != "" {
					if persistErr := r.persistRotatedRefreshToken(a, newRT, "rt_at_unverified"); persistErr != nil {
						return nil, persistErr
					}
				}
				switch {
				case verifyErr == nil && verifyStatus == 200:
					res.ATVerified = true
					return r.applyRefresh(ctx, a, newAT, newRT, expAt, RefreshSourceRT, res)
				case verifyStatus == 401:
					// AT 作用域不是 web:不写库,走 ST 回退
					rtRejectedByWeb = true
					r.log.Warn("RT-AT rejected by chatgpt.com web (401), fallback to ST",
						zap.Uint64("id", a.ID), zap.String("email", a.Email))
					res.Error = "RT 换出的 AT 被 chatgpt.com 拒绝(iOS 作用域)"
				default:
					// 其他错误:不写库,走 ST 回退
					r.log.Warn("RT-AT verify failed, fallback to ST",
						zap.Uint64("id", a.ID), zap.Int("status", verifyStatus),
						zap.Error(verifyErr))
					if verifyErr != nil {
						res.Error = "RT 换出的 AT 校验失败:" + friendlyRefreshErr(verifyErr)
					} else {
						res.Error = fmt.Sprintf("RT 换出的 AT 校验失败(HTTP %d)", verifyStatus)
					}
				}
			} else {
				// RT → AT HTTP 本身失败,回退 ST
				r.log.Warn("RT refresh failed, fallback to ST", zap.Uint64("id", a.ID), zap.Error(err))
				res.Error = friendlyRefreshErr(err)
			}
		}
	}

	// 尝试 ST。session 接口返回 AT 后仍必须通过图片 prepare，不能仅凭响应写库。
	if a.SessionTokenEnc.Valid && a.SessionTokenEnc.String != "" {
		st, err := r.svc.cipher.DecryptString(a.SessionTokenEnc.String)
		if err == nil && st != "" {
			newAT, expAt, err := r.stToATWithProxy(ctx, a, st, browserCookies, scope.fixedProxyURL)
			if err == nil && newAT != "" {
				verifyStatus, verifyErr := r.verifyATForImageWithProxy(ctx, a, newAT, browserCookies, scope.fixedProxyURL)
				if verifyErr == nil && verifyStatus == http.StatusOK {
					res.ATVerified = true
					return r.applyRefresh(ctx, a, newAT, "", expAt, RefreshSourceST, res)
				}
				if verifyErr != nil {
					err = fmt.Errorf("ST 换出的 AT 图片验证失败: %w", verifyErr)
				} else {
					err = fmt.Errorf("ST 换出的 AT 图片验证失败(HTTP %d)", verifyStatus)
				}
			}
			if res.Error == "" {
				res.Error = friendlyRefreshErr(err)
			} else {
				res.Error += " / ST:" + friendlyRefreshErr(err)
			}
		}
	}

	// 都不行:区分两种失败语义
	if rtRejectedByWeb {
		// RT 本身可以刷出 AT,但作用域不兼容 web,且没有 ST 或 ST 也失败。
		// 对本系统而言账号完全不可用,标 dead,调度器不再挑中它。
		// 用户补充 Session Token 后再次手动刷新,ApplyRefreshResult 会把状态恢复成 healthy。
		res.WebUnauthorized = true
		if !a.SessionTokenEnc.Valid || a.SessionTokenEnc.String == "" {
			res.Error = "RT 换出的 AT 被 chatgpt.com 拒绝(iOS 作用域不兼容 web),请为该账号补充 Session Token"
		}
		_ = r.svc.dao.RecordRefreshError(ctx, a.ID, RefreshSourceRT, res.Error, true)
		res.Source = "failed"
		return res, nil
	}

	if res.Error == "" {
		res.Error = "账号既无可用 RT 也无可用 ST"
	}
	_ = r.svc.dao.RecordRefreshError(ctx, a.ID, RefreshSourceRT, res.Error, true)
	res.Source = "failed"
	return res, nil
}

func (r *Refresher) persistRotatedRefreshToken(a *Account, newRT, reason string) error {
	if a == nil || !a.RefreshTokenEnc.Valid || a.RefreshTokenEnc.String == "" || newRT == "" {
		return nil
	}
	newRTEnc, err := r.svc.cipher.EncryptString(newRT)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.svc.dao.RotateRefreshTokenCAS(
		writeCtx,
		a.ID,
		a.RefreshTokenEnc.String,
		newRTEnc,
		reason,
	); err != nil {
		return err
	}
	a.RefreshTokenEnc.String = newRTEnc
	a.RefreshTokenEnc.Valid = true
	return nil
}

type imageATPreparer interface {
	ChatRequirementsPrepare(context.Context) (*chatgpt.ChatRequirementsPrepareResp, error)
}

func imageATVerificationOptions(a *Account, accessToken, proxyURL, cookies string) chatgpt.Options {
	options := chatgpt.Options{
		AuthToken: accessToken,
		ProxyURL:  proxyURL,
		Cookies:   cookies,
		Timeout:   12 * time.Second,
	}
	if a != nil {
		options.DeviceID = a.OAIDeviceID
		options.SessionID = a.OAISessionID
		options.BrowserProfile = a.BrowserProfile
	}
	return options
}

func verifyImageATWithPreparer(ctx context.Context, preparer imageATPreparer) (int, error) {
	if preparer == nil {
		return 0, errors.New("image AT preparer is nil")
	}
	_, err := preparer.ChatRequirementsPrepare(ctx)
	if err == nil {
		return http.StatusOK, nil
	}
	var upstream *chatgpt.UpstreamError
	if errors.As(err, &upstream) {
		return upstream.Status, err
	}
	return 0, err
}

// verifyATForImage checks the exact authenticated prepare endpoint required by
// image generation, using the account's stable proxy, device, session, and
// cookies. It does not create an image task or consume image quota.
func (r *Refresher) verifyATForImage(ctx context.Context, a *Account, accessToken, cookies string) (int, error) {
	return r.verifyATForImageWithProxy(ctx, a, accessToken, cookies, nil)
}

func (r *Refresher) verifyATForImageWithProxy(
	ctx context.Context,
	a *Account,
	accessToken string,
	cookies string,
	fixedProxyURL *string,
) (int, error) {
	if a == nil {
		return 0, errors.New("account is nil")
	}
	vctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	proxyURL := ""
	if fixedProxyURL != nil {
		proxyURL = strings.TrimSpace(*fixedProxyURL)
		if proxyURL == "" {
			return 0, errors.New("fixed image lease proxy is empty")
		}
	} else if r.proxyResolver != nil {
		proxyURL = strings.TrimSpace(r.proxyResolver.ProxyURLForAccount(vctx, a.ID))
		if proxyURL == "" {
			return 0, errors.New("account proxy binding unavailable for image verification")
		}
	}
	client, err := chatgpt.New(imageATVerificationOptions(a, accessToken, proxyURL, cookies))
	if err != nil {
		return 0, err
	}
	status, verifyErr := verifyImageATWithPreparer(vctx, client)
	if status == http.StatusOK && verifyErr == nil && r.svc != nil {
		if raw, snapshotErr := client.CookieJSON(); snapshotErr == nil && raw != "" && raw != "[]" {
			if persistErr := r.svc.StoreCookies(vctx, a.ID, raw); persistErr != nil && r.log != nil {
				r.log.Warn("persist verified account cookies failed",
					zap.Uint64("id", a.ID), zap.Error(persistErr))
			}
		}
	}
	return status, verifyErr
}

func (r *Refresher) applyRefresh(
	ctx context.Context, a *Account,
	newAT, newRT string, expAt time.Time, source string,
	res *RefreshResult,
) (*RefreshResult, error) {
	atEnc, err := r.svc.cipher.EncryptString(newAT)
	if err != nil {
		return nil, err
	}
	var rtEnc string
	if newRT != "" {
		enc, err := r.svc.cipher.EncryptString(newRT)
		if err != nil {
			return nil, err
		}
		rtEnc = enc
	}
	if expAt.IsZero() {
		expAt = parseJWTExp(newAT)
	}
	expectedRTEnc := ""
	if source == RefreshSourceRT && a.RefreshTokenEnc.Valid {
		expectedRTEnc = a.RefreshTokenEnc.String
	}
	if err := r.svc.dao.ApplyRefreshResult(
		ctx,
		a.ID,
		expectedRTEnc,
		atEnc,
		rtEnc,
		expAt,
		source,
	); err != nil {
		return nil, err
	}
	res.OK = true
	res.Source = source
	res.ExpiresAt = expAt
	res.Error = ""
	res.RTRotated = rtEnc != ""
	return res, nil
}

// rtToAT POST https://auth.openai.com/oauth/token
func (r *Refresher) rtToAT(ctx context.Context, accountID uint64, refreshToken, clientID string) (newAT, newRT string, expAt time.Time, err error) {
	return r.rtToATWithProxy(ctx, accountID, refreshToken, clientID, nil)
}

func (r *Refresher) rtToATWithProxy(
	ctx context.Context,
	accountID uint64,
	refreshToken string,
	clientID string,
	fixedProxyURL *string,
) (newAT, newRT string, expAt time.Time, err error) {
	req, err := newOAuthRefreshRequest(ctx, refreshToken, clientID)
	if err != nil {
		return
	}

	client, err := r.clientForProxy(ctx, accountID, fixedProxyURL)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		err = fmt.Errorf("rt refresh http=%d body=%s", resp.StatusCode, truncate(string(data), 200))
		return
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err = json.Unmarshal(data, &out); err != nil {
		return
	}
	if out.AccessToken == "" {
		err = errors.New("rt refresh: missing access_token in response")
		return
	}
	newAT = out.AccessToken
	newRT = out.RefreshToken
	if out.ExpiresIn > 0 {
		expAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	} else {
		expAt = parseJWTExp(newAT)
	}
	return
}

const (
	codexOAuthClientID         = "app_EMoamEEZ73f0CkXaXp7hrann"
	platformOAuthClientID      = "app_2SKx67EdpoN0G6j64rFvigXD"
	chatGPTIOSOAuthRedirectURI = "com.openai.chat://auth0.openai.com/ios/com.openai.chat/callback"
	chatGPTIOSOAuthUserAgent   = "ChatGPT/1.2025.122 (iOS 18.2; iPhone15,2; build 15096)"
)

// newOAuthRefreshRequest keeps each issued RT on its client-specific wire
// contract. Codex uses its official three-field JSON request. The app_2SK RTs
// issued by Platform consent use a three-field form request without a redirect.
func newOAuthRefreshRequest(ctx context.Context, refreshToken, clientID string) (*http.Request, error) {
	var (
		body        io.Reader
		contentType string
		userAgent   string
	)
	if clientID == codexOAuthClientID {
		data, err := json.Marshal(map[string]string{
			"client_id":     clientID,
			"grant_type":    "refresh_token",
			"refresh_token": refreshToken,
		})
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(string(data))
		contentType = "application/json"
		userAgent = chatgpt.DefaultUserAgent
	} else if clientID == platformOAuthClientID {
		form := url.Values{}
		form.Set("client_id", clientID)
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", refreshToken)
		body = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
		userAgent = chatgpt.DefaultUserAgent
	} else {
		redirectURI, oauthUserAgent := oauthParamsForClient(clientID)
		form := url.Values{}
		form.Set("client_id", clientID)
		form.Set("grant_type", "refresh_token")
		form.Set("redirect_uri", redirectURI)
		form.Set("refresh_token", refreshToken)
		body = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
		userAgent = oauthUserAgent
	}

	req, err := http.NewRequestWithContext(
		ctx,
		"POST",
		"https://auth.openai.com/oauth/token",
		body,
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	applyOAuthBrowserHeaders(req.Header, userAgent)
	return req, nil
}

// oauthParamsForClient 根据 client_id 返回对应的 redirect_uri 和 User-Agent。
//
// OpenAI 非 Codex/app_2SK OAuth client 使用各自签发时的 redirect URI。两个
// 已知 client 由 newOAuthRefreshRequest 直接处理，不进入这条映射。
//
// RT 必须用签发它的 client 的参数来刷新，否则 OpenAI 返回 401 invalid_grant。
func oauthParamsForClient(clientID string) (redirectURI, userAgent string) {
	// 其他 client_id：前缀判断（以 app_2S 开头的视为 Platform 系列）
	if len(clientID) >= 6 && clientID[:6] == "app_2S" {
		return "https://platform.openai.com/auth/callback",
			chatgpt.DefaultUserAgent
	}
	// 默认 iOS App
	return chatGPTIOSOAuthRedirectURI, chatGPTIOSOAuthUserAgent
}

func applyOAuthBrowserHeaders(header http.Header, userAgent string) {
	if userAgent == chatgpt.DefaultUserAgent {
		chatgpt.ApplyDefaultBrowserFingerprintHeaders(header)
		return
	}
	header.Set("User-Agent", userAgent)
}

// stToAT GET https://chatgpt.com/api/auth/session  Cookie: __Secure-next-auth.session-token=ST
func (r *Refresher) stToAT(ctx context.Context, a *Account, sessionToken, cookies string) (newAT string, expAt time.Time, err error) {
	return r.stToATWithProxy(ctx, a, sessionToken, cookies, nil)
}

func (r *Refresher) stToATWithProxy(
	ctx context.Context,
	a *Account,
	sessionToken string,
	cookies string,
	fixedProxyURL *string,
) (newAT string, expAt time.Time, err error) {
	if a == nil {
		err = errors.New("account is nil")
		return
	}
	client, clientErr := r.accountWebClientWithProxy(ctx, a, cookies, fixedProxyURL)
	if clientErr != nil {
		err = clientErr
		return
	}
	sessionURL, parseErr := url.Parse(chatgpt.BaseURL + "/api/auth/session")
	if parseErr != nil {
		err = parseErr
		return
	}
	client.Jar.SetCookies(sessionURL, []*http.Cookie{{
		Name: "__Secure-next-auth.session-token", Value: sessionToken,
		Domain: sessionURL.Hostname(), Path: "/", Secure: true, HttpOnly: true,
	}})
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://chatgpt.com/api/auth/session", nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://chatgpt.com/")
	profile, _, profileErr := chatgpt.ParseBrowserProfileJSON(a.BrowserProfile)
	if profileErr != nil {
		err = profileErr
		return
	}
	chatgpt.ApplyBrowserFingerprintHeaders(req.Header, profile)
	setAccountWebIdentityHeaders(req.Header, a)

	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		err = fmt.Errorf("st refresh http=%d body=%s", resp.StatusCode, truncate(string(data), 200))
		return
	}
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "{}" {
		err = errors.New("ST 已过期或无效,响应为空")
		return
	}
	var out struct {
		AccessToken string `json:"accessToken"`
		Expires     string `json:"expires"`
	}
	if err = json.Unmarshal([]byte(raw), &out); err != nil {
		return
	}
	if out.AccessToken == "" {
		err = errors.New("响应缺少 accessToken 字段")
		return
	}
	r.persistWebClientCookies(ctx, a.ID, client)
	newAT = out.AccessToken
	if out.Expires != "" {
		if t, e := time.Parse(time.RFC3339, out.Expires); e == nil {
			expAt = t
		}
	}
	if expAt.IsZero() {
		expAt = parseJWTExp(newAT)
	}
	return
}

func (r *Refresher) accountWebClient(ctx context.Context, a *Account, cookies string) (*http.Client, error) {
	return r.accountWebClientWithProxy(ctx, a, cookies, nil)
}

func (r *Refresher) accountWebClientWithProxy(
	ctx context.Context,
	a *Account,
	cookies string,
	fixedProxyURL *string,
) (*http.Client, error) {
	base, err := r.clientForProxy(ctx, a.ID, fixedProxyURL)
	if err != nil {
		return nil, err
	}
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cookies) != "" {
		if err := chatgpt.LoadCookies(jar, chatgpt.BaseURL, cookies); err != nil {
			return nil, fmt.Errorf("load account cookies: %w", err)
		}
	}
	return &http.Client{Transport: base.Transport, Timeout: base.Timeout, Jar: jar}, nil
}

func setAccountWebIdentityHeaders(header http.Header, a *Account) {
	if a == nil {
		return
	}
	if value := strings.TrimSpace(a.OAIDeviceID); value != "" {
		header.Set("Oai-Device-Id", value)
	}
	if value := strings.TrimSpace(a.OAISessionID); value != "" {
		header.Set("Oai-Session-Id", value)
	}
}

func (r *Refresher) persistWebClientCookies(ctx context.Context, accountID uint64, client *http.Client) {
	if r.svc == nil || client == nil || client.Jar == nil {
		return
	}
	raw, err := chatgpt.CookieJarJSON(client.Jar, chatgpt.BaseURL)
	if err != nil || raw == "" || raw == "[]" {
		return
	}
	if err := r.svc.StoreCookies(ctx, accountID, raw); err != nil {
		r.log.Warn("persist refreshed account cookies failed",
			zap.Uint64("id", accountID), zap.Error(err))
	}
}

// parseJWTExp 解 JWT payload 里的 exp(秒级)。失败返回 +24h。
func parseJWTExp(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Now().Add(24 * time.Hour)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// 尝试 StdEncoding(容错)
		raw, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Now().Add(24 * time.Hour)
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp == 0 {
		return time.Now().Add(24 * time.Hour)
	}
	return time.Unix(claims.Exp, 0)
}

func friendlyRefreshErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "http=401"), strings.Contains(low, "invalid_grant"):
		return "RT 已失效(401)"
	case strings.Contains(low, "http=403"):
		return "上游拒绝访问(403)"
	case strings.Contains(low, "http=429"):
		return "触发速率限制(429)"
	case strings.Contains(low, "timeout"), strings.Contains(low, "deadline exceeded"):
		return "刷新请求超时"
	case strings.Contains(low, "proxyconnect") && strings.Contains(low, "no such host"):
		return "代理域名无法解析"
	case strings.Contains(low, "proxyconnect tcp"):
		return "代理握手失败"
	case strings.Contains(low, "no such host"):
		return "DNS 解析失败"
	case strings.Contains(low, "connection refused"):
		return "连接被拒绝"
	case strings.Contains(low, "connection reset"):
		return "连接被重置"
	case strings.Contains(low, "unexpected eof"), strings.HasSuffix(low, ": eof"):
		return "连接被对端关闭"
	case strings.Contains(low, "tls"), strings.Contains(low, "x509"):
		return "TLS 握手失败"
	case strings.Contains(low, "missing access_token"), strings.Contains(s, "ST 已过期"):
		return stripHTTPPrefix(s)
	default:
		return "刷新失败:" + stripHTTPPrefix(s)
	}
}

// stripHTTPPrefix 去掉 Go net/http 错误里形如
//
//	Post "https://auth.openai.com/oauth/token": dial tcp: ...
//
// 的 URL 前缀,只保留后面真正的原因,避免把敏感/冗长的 URL 暴露给前端。
func stripHTTPPrefix(s string) string {
	// 典型前缀: Get/Post/Put "https://...": rest
	if i := strings.Index(s, `": `); i > 0 && i < len(s)-3 {
		s = s[i+3:]
	}
	// 再剥一层常见的 "dial tcp: " / "proxyconnect tcp: " 类前缀最靠前的修饰,
	// 保留中间有用的原因(如 lookup xxx: no such host)。
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
