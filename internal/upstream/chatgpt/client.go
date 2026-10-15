// Package chatgpt 封装 chatgpt.com 的反向工程调用。
//
// 本包不关心调度与计费,只负责一次 HTTP 往返。
// 调用方(网关)负责:调度器拿 Lease -> 构造 Client -> 发请求 -> 转译响应。
package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/publicsuffix"

	"github.com/432539/gpt2api/pkg/logger"
)

func loggerL() *zap.Logger { return logger.L() }

// 固定请求头和客户端版本号。浏览器画像定义在 browser_profile.go。
const (
	DefaultClientVersion  = "prod-a194cd50d4416d3c0b47c740f206b12ce60f5887"
	DefaultClientBuildNum = "6708908"
	DefaultLanguage       = "zh-CN"
	BaseURL               = "https://chatgpt.com"
)

// Options 构造 Client 的参数。
type Options struct {
	BaseURL        string
	AuthToken      string // 完整 Bearer token(已解密)
	DeviceID       string
	SessionID      string
	ProxyURL       string        // http(s)://user:pass@host:port,为空则直连
	Timeout        time.Duration // HTTP 总超时,默认 120s
	SSETimeout     time.Duration // SSE 首 byte 超时,默认 60s
	Cookies        string        // JSON 字符串(可选),格式 [{"name":"x","value":"y","domain":".chatgpt.com"}]
	BrowserProfile string        // 注册浏览器采集的无凭据运行画像 JSON
	ClientVersion  string
	Language       string
	TrafficMeter   *TrafficMeter

	// TurnstileSolver 可选。正常路径优先使用内置 SolveTurnstileToken(dx, p),
	// 只有本地解释器返回空时才尝试外部 solver。
	TurnstileSolver TurnstileSolver
}

// Client 一个账号/代理/device 一次请求的上游客户端。可多次复用(建议 1 次请求 1 个)。
type Client struct {
	opts          Options
	hc            *http.Client
	assetHC       *http.Client
	bearerAssetHC *http.Client
	profile       BrowserProfile
}

// TrafficSnapshot separates paid account-proxy tunnel bytes from direct image
// body bytes. It is intended for isolated admission evidence; normal
// production clients leave the meter nil and incur no atomic operations.
type TrafficSnapshot struct {
	ProxyReadBytes          int64 `json:"proxy_read_bytes"`
	ProxyWrittenBytes       int64 `json:"proxy_written_bytes"`
	DirectAssetReadBytes    int64 `json:"direct_asset_read_bytes"`
	DirectAssetWrittenBytes int64 `json:"direct_asset_written_bytes"`
}

type TrafficCheckpoint struct {
	Stage             string          `json:"stage"`
	Traffic           TrafficSnapshot `json:"traffic"`
	ResponseStatus    int             `json:"response_status,omitempty"`
	SetCookieCount    int             `json:"set_cookie_count,omitempty"`
	ContentEncoding   string          `json:"content_encoding,omitempty"`
	ResponseBodyBytes int64           `json:"response_body_bytes,omitempty"`
	ContentLength     int64           `json:"content_length,omitempty"`
}

type TrafficMeter struct {
	proxyReadBytes          atomic.Int64
	proxyWrittenBytes       atomic.Int64
	directAssetReadBytes    atomic.Int64
	directAssetWrittenBytes atomic.Int64
	checkpointMu            sync.Mutex
	checkpoints             []TrafficCheckpoint
}

func (meter *TrafficMeter) Snapshot() TrafficSnapshot {
	if meter == nil {
		return TrafficSnapshot{}
	}
	return TrafficSnapshot{
		ProxyReadBytes:          meter.proxyReadBytes.Load(),
		ProxyWrittenBytes:       meter.proxyWrittenBytes.Load(),
		DirectAssetReadBytes:    meter.directAssetReadBytes.Load(),
		DirectAssetWrittenBytes: meter.directAssetWrittenBytes.Load(),
	}
}

func (meter *TrafficMeter) checkpoint(stage string) {
	meter.checkpointResponse(stage, 0, 0, "", 0, 0)
}

func (meter *TrafficMeter) checkpointResponse(
	stage string,
	responseStatus int,
	setCookieCount int,
	contentEncoding string,
	responseBodyBytes int64,
	contentLength int64,
) {
	if meter == nil {
		return
	}
	meter.checkpointMu.Lock()
	meter.checkpoints = append(meter.checkpoints, TrafficCheckpoint{
		Stage:             stage,
		Traffic:           meter.Snapshot(),
		ResponseStatus:    responseStatus,
		SetCookieCount:    setCookieCount,
		ContentEncoding:   contentEncoding,
		ResponseBodyBytes: responseBodyBytes,
		ContentLength:     contentLength,
	})
	meter.checkpointMu.Unlock()
}

func (meter *TrafficMeter) Checkpoints() []TrafficCheckpoint {
	if meter == nil {
		return nil
	}
	meter.checkpointMu.Lock()
	defer meter.checkpointMu.Unlock()
	return append([]TrafficCheckpoint(nil), meter.checkpoints...)
}

// TrafficSnapshot returns this client's current diagnostic counters. Normal
// production clients have no meter and therefore return an all-zero snapshot.
func (c *Client) TrafficSnapshot() TrafficSnapshot {
	if c == nil {
		return TrafficSnapshot{}
	}
	return c.opts.TrafficMeter.Snapshot()
}

func (c *Client) checkpointTraffic(stage string) {
	if c != nil {
		c.opts.TrafficMeter.checkpoint(stage)
	}
}

func (c *Client) checkpointResponseTraffic(
	stage string,
	response *http.Response,
	responseBodyBytes int64,
) {
	if c == nil || response == nil {
		return
	}
	c.opts.TrafficMeter.checkpointResponse(
		stage,
		response.StatusCode,
		len(response.Cookies()),
		response.Header.Get("Content-Encoding"),
		responseBodyBytes,
		response.ContentLength,
	)
}

// New 构造客户端。
func New(opt Options) (*Client, error) {
	if opt.AuthToken == "" {
		return nil, errors.New("auth_token required")
	}
	if opt.DeviceID == "" {
		return nil, errors.New("device_id required")
	}
	if opt.BaseURL == "" {
		opt.BaseURL = BaseURL
	}
	if opt.Timeout == 0 {
		opt.Timeout = 120 * time.Second
	}
	if opt.SSETimeout == 0 {
		opt.SSETimeout = 60 * time.Second
	}
	if opt.ClientVersion == "" {
		opt.ClientVersion = DefaultClientVersion
	}
	if opt.Language == "" {
		opt.Language = DefaultLanguage
	}
	profile := DefaultBrowserProfile()
	if strings.TrimSpace(opt.BrowserProfile) != "" {
		parsed, canonical, err := ParseBrowserProfileJSON(opt.BrowserProfile)
		if err != nil {
			return nil, err
		}
		profile = parsed
		opt.BrowserProfile = canonical
		opt.Language = profile.Language
	}

	// 直接用标准 net/http 会被 Cloudflare 按 JA3/JA4 指纹识别出不是浏览器(403 拦截页);
	// 这里换成 utls-based RoundTripper,ClientHello 伪装成 Chrome 120。
	// Proxy(HTTP / HTTPS CONNECT)在 transport 内部处理,不再走 http.ProxyURL。
	tr, err := newUTLSTransport(opt.ProxyURL, 30*time.Second, opt.TrafficMeter)
	if err != nil {
		return nil, fmt.Errorf("init utls transport: %w", err)
	}
	bearerAssetTransport, err := newUTLSTransport("", 30*time.Second, nil)
	if err != nil {
		return nil, fmt.Errorf("init bearer asset transport: %w", err)
	}

	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	hc := &http.Client{
		Transport: tr,
		Timeout:   opt.Timeout, // SSE 场景会关闭该 timeout(见 StreamConversation)
		Jar:       jar,
	}
	assetTransport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	assetHC := &http.Client{Transport: assetTransport, Timeout: opt.Timeout}
	bearerAssetHC := &http.Client{Transport: bearerAssetTransport, Timeout: opt.Timeout}
	if opt.Cookies != "" {
		if err := loadCookies(jar, opt.BaseURL, opt.Cookies); err != nil {
			return nil, fmt.Errorf("load cookies: %w", err)
		}
	}
	return &Client{
		opts: opt, hc: hc, assetHC: assetHC, bearerAssetHC: bearerAssetHC, profile: profile,
	}, nil
}

// CookieJSON returns the current ChatGPT cookie jar after bootstrap and other
// upstream requests have applied Set-Cookie updates.
func (c *Client) CookieJSON() (string, error) {
	if c == nil || c.hc == nil {
		return "", errors.New("chatgpt client is nil")
	}
	return cookieJarJSON(c.hc.Jar, c.opts.BaseURL)
}

// UserAgent returns the browser UA used by this client and its PoW solver.
func (c *Client) UserAgent() string {
	return c.profile.UserAgent()
}

// commonHeaders 设置所有 chatgpt.com 请求通用的头。
//
// 对齐真实浏览器(Edge 143 @ Windows)抓包:除了 PoW/turnstile 这类 sentinel 头
// 由具体端点自己加,其他客户端指纹头、Oai-* 头、sec-ch-ua 完整套件、
// X-Openai-Target-Path/Route 都在这里统一设置。X-Oai-Turn-Trace-Id 是"每 turn 一个"
// 的 UUID,由具体发送函数(StreamFChat / StreamFConversation)自己随机生成,
// 这里只填固定的。
//
// 真正的指纹差异在 HTTP/2 SETTINGS frame(JA4H),已在 utls_transport.go 中用
// forceH1 强制走 http/1.1 规避。
func (c *Client) commonHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.opts.AuthToken)
	ApplyBrowserFingerprintHeaders(req.Header, c.profile)
	req.Header.Set("Origin", c.opts.BaseURL)
	req.Header.Set("Referer", c.opts.BaseURL+"/")
	// 不设置 Accept-Encoding:Go net/http 会自动加 `Accept-Encoding: gzip` 并透明解压。
	// 若主动声明 br/zstd,Go 不会解压,body 会是压缩字节,SSE / JSON 解析全坏。
	// sec-ch-ua 完整套件(Edge 143 on Windows 11):真实浏览器每次都会带这整套,
	// 少其中任何一项都可能触发 bot 指纹识别。保持与 DefaultUserAgent 对齐。
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Priority", "u=1, i")
	// Oai-* 头:真实浏览器每请求必带。
	req.Header.Set("Oai-Device-Id", c.opts.DeviceID)
	if c.opts.SessionID != "" {
		req.Header.Set("Oai-Session-Id", c.opts.SessionID)
	}
	req.Header.Set("Oai-Language", c.opts.Language)
	req.Header.Set("Oai-Client-Version", c.opts.ClientVersion)
	req.Header.Set("Oai-Client-Build-Number", DefaultClientBuildNum)
	// X-Openai-Target-Path / Route:真实浏览器每请求必带,值就是请求 URL 的 path
	// (不含 query)。Route 通常是带占位符的形式(例如 /files/download/{file_id}),
	// 但后端对两个字段都是相等比较,填成 Path 也不触发 400;先统一 Path,以后
	// 发现特定端点报错再单独覆盖。
	if p := req.URL.Path; p != "" {
		req.Header.Set("X-Openai-Target-Path", p)
		req.Header.Set("X-Openai-Target-Route", p)
	}
	// Accept 默认值在各 endpoint 函数里会覆盖(比如 SSE 设成 text/event-stream)。
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "*/*")
	}
}

// UpstreamError 是一次 chatgpt.com 请求失败的结构化错误。
type UpstreamError struct {
	Status  int
	Message string
	Body    string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("chatgpt upstream %d: %s", e.Status, e.Message)
}

// IsRateLimited 对应 HTTP 429 / 资源耗尽。
func (e *UpstreamError) IsRateLimited() bool { return e.Status == 429 }

// IsUnauthorized 对应真正的鉴权失败。403 可能是风控/环境/阶段性拒绝,
// 不能等同于 token 失效。
func (e *UpstreamError) IsUnauthorized() bool { return e.Status == 401 }

// IsForbidden 对应 HTTP 403。调用方应按可恢复异常降级/冷却,不要直接打 dead。
func (e *UpstreamError) IsForbidden() bool { return e.Status == 403 }

// ChatRequirementsResp 对应响应(仅摘取关键字段)。
type ChatRequirementsResp struct {
	Token string `json:"token"` // chat_token
	// ProofToken / TurnstileToken 是本次 finalize 已经使用过的 sentinel token,
	// 后续 f/conversation 请求必须复用同一组 token。
	ProofToken     string `json:"-"`
	TurnstileToken string `json:"-"`
	SOToken        string `json:"-"`
	// Persona 常见取值:"chatgpt-freeaccount"(免费号)/ "chatgpt-paid"(Plus/Team)
	//              / "chatgpt-noauth"(未登录)
	// 当前仅把 persona 暴露给上层做观测与日志,具体请求模型是否可用交由上游自行判定。
	Persona     string `json:"persona"`
	Proofofwork struct {
		Required   bool   `json:"required"`
		Seed       string `json:"seed"`
		Difficulty string `json:"difficulty"`
	} `json:"proofofwork"`
	Turnstile struct {
		Required bool `json:"required"`
	} `json:"turnstile"`
}

// IsFreeAccount 判断当前账号是否为免费号(persona=chatgpt-freeaccount)。
func (r *ChatRequirementsResp) IsFreeAccount() bool {
	return r.Persona == "chatgpt-freeaccount"
}

// SolveProof 求解 POW,返回要放进 `Openai-Sentinel-Proof-Token` 的字符串。
// 若 Proofofwork.Required=false,返回空串。
func (r *ChatRequirementsResp) SolveProof(userAgent string) string {
	if !r.Proofofwork.Required {
		return ""
	}
	return SolveProofToken(r.Proofofwork.Seed, r.Proofofwork.Difficulty, userAgent)
}

// Bootstrap 对 chatgpt.com 的 / 发起 HEAD,让 cookie jar 收下 Cloudflare 的
// `__cf_bm` / `_cfuvid` 与 OpenAI 的 `oai-did` 等 cookie,但不下载未使用的首页正文。
//
// 关键作用:没有这些 cookie 时,chat-requirements 会直接要求 Turnstile(即使
// Bearer 合法),所以建议在每次新建 Client 后先调一次 Bootstrap,或者在
// ChatRequirements 内部第一次请求前自动调一次。
// 多次调用是幂等的(HTTP 200/3xx 均视为成功)。
func (c *Client) Bootstrap(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.opts.BaseURL+"/", nil)
	if err != nil {
		return err
	}
	ApplyBrowserFingerprintHeaders(req.Header, c.profile)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	res, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("bootstrap HEAD /: %w", err)
	}
	defer res.Body.Close()
	bodyBytes, _ := io.Copy(io.Discard, res.Body)
	c.checkpointResponseTraffic("requirements.bootstrap", res, bodyBytes)
	if res.StatusCode >= 500 {
		return &UpstreamError{Status: res.StatusCode, Message: "bootstrap failed"}
	}
	return nil
}

// SetLastUsedModelConfig 对齐 ChatGPT Web 的设置 PATCH,让 thinking 模型与强度在发消息前先落到用户配置。
func (c *Client) SetLastUsedModelConfig(ctx context.Context, modelSlug, thinkingEffort string) error {
	if strings.TrimSpace(modelSlug) == "" {
		return errors.New("model_slug required")
	}

	q := url.Values{}
	q.Set("model_slug", modelSlug)
	if strings.TrimSpace(thinkingEffort) != "" {
		q.Set("thinking_effort", thinkingEffort)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		c.opts.BaseURL+"/backend-api/settings/user_last_used_model_config?"+q.Encode(),
		nil)
	if err != nil {
		return err
	}
	c.commonHeaders(req)
	req.Header.Set("Accept", "*/*")

	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	buf, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		return &UpstreamError{
			Status:  res.StatusCode,
			Message: "settings/user_last_used_model_config failed",
			Body:    string(buf),
		}
	}
	return nil
}

// ChatRequirements 取 chat_token。
// 请求体必须带 `p` = 客户端预算的 requirements_token(前缀 gAAAAAC,固定难度 0fffff)。
// 否则上游会返回空 token 或 403。
func (c *Client) ChatRequirements(ctx context.Context) (*ChatRequirementsResp, error) {
	// 首次请求前顺手做一次浏览器首访,拿 __cf_bm / oai-did,避免 Turnstile。
	// jar 已经持有过则这次 GET 其实是 200,代价就是一个 RTT,可以接受。
	if err := c.Bootstrap(ctx); err != nil {
		// 拿不到 cookie 不致命,继续往下走;真不行再让 chat-requirements 自己报错。
		_ = err
	}
	reqToken := NewPOWConfig(DefaultUserAgent).RequirementsToken()
	body, _ := json.Marshal(map[string]string{"p": reqToken})
	req, err := http.NewRequestWithContext(ctx,
		http.MethodPost,
		c.opts.BaseURL+"/backend-api/sentinel/chat-requirements",
		strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	buf, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		return nil, &UpstreamError{Status: res.StatusCode, Message: "chat-requirements failed", Body: string(buf)}
	}
	var out ChatRequirementsResp
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, fmt.Errorf("decode chat-requirements: %w", err)
	}
	if logger := loggerL(); logger != nil {
		logger.Info("chat-requirements result",
			zap.String("persona", out.Persona),
			zap.Bool("turnstile_required", out.Turnstile.Required),
			zap.Bool("pow_required", out.Proofofwork.Required),
			zap.Bool("has_token", out.Token != ""))
	}
	return &out, nil
}

// ChatRequirementsPrepareResp 是 /sentinel/chat-requirements/prepare 的响应。
//
// 浏览器在每个 turn 前先调 prepare 拿到 challenge(turnstile.dx + pow.seed/difficulty),
// 本地计算后再调 finalize 拿最终 chat-requirements-token。
type ChatRequirementsPrepareResp struct {
	Persona      string `json:"persona"`
	PrepareToken string `json:"prepare_token"`
	SourceP      string `json:"-"`
	Turnstile    struct {
		Required bool   `json:"required"`
		DX       string `json:"dx"`
	} `json:"turnstile"`
	Proofofwork struct {
		Required   bool   `json:"required"`
		Seed       string `json:"seed"`
		Difficulty string `json:"difficulty"`
	} `json:"proofofwork"`
}

// ChatRequirementsPrepare 调 /backend-api/sentinel/chat-requirements/prepare。
// 请求体里的 `p` 字段仍是 18 元素 PoW(前缀 gAAAAAC),和老的单步接口同款。
func (c *Client) ChatRequirementsPrepare(ctx context.Context) (*ChatRequirementsPrepareResp, error) {
	reqToken := NewPOWConfig(DefaultUserAgent).RequirementsToken()
	body, _ := json.Marshal(map[string]string{"p": reqToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.opts.BaseURL+"/backend-api/sentinel/chat-requirements/prepare",
		strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	buf, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		return nil, &UpstreamError{Status: res.StatusCode, Message: "chat-requirements/prepare failed", Body: string(buf)}
	}
	var out ChatRequirementsPrepareResp
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, fmt.Errorf("decode chat-requirements/prepare: %w", err)
	}
	out.SourceP = reqToken
	return &out, nil
}

// ChatRequirementsFinalize 调 /backend-api/sentinel/chat-requirements/finalize。
// 入参:prepare_token(来自 Prepare),proofToken(本地解,13 元素),
// turnstileResp(由 SolveTurnstileToken 解出;没有则传空串,上游可能拒绝)。
// 返回最终的 chat-requirements-token。
func (c *Client) ChatRequirementsFinalize(
	ctx context.Context,
	prepareToken, proofToken, turnstileResp string,
) (string, string, string, error) {
	payload := map[string]interface{}{
		"prepare_token":   prepareToken,
		"proof_token":     proofToken,
		"turnstile_token": turnstileResp,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.opts.BaseURL+"/backend-api/sentinel/chat-requirements/finalize",
		strings.NewReader(string(body)))
	if err != nil {
		return "", "", "", err
	}
	c.commonHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	res, err := c.hc.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer res.Body.Close()
	buf, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		return "", "", "", &UpstreamError{Status: res.StatusCode, Message: "chat-requirements/finalize failed", Body: string(buf)}
	}
	var out struct {
		Persona string `json:"persona"`
		Token   string `json:"token"`
		SOToken string `json:"so_token"`
	}
	if err := json.Unmarshal(buf, &out); err != nil {
		return "", "", "", fmt.Errorf("decode chat-requirements/finalize: %w", err)
	}
	return out.Token, out.Persona, out.SOToken, nil
}

// ChatRequirementsV2 是 sentinel 协议的新统一入口。
//
// 路由逻辑:
//  1. 先调 /prepare 拿 challenge;
//  2. 若返回 turnstile.required=false,直接把 prepare_token 走 finalize 拿最终 token;
//  3. 若 turnstile.required=true:
//     a. 优先用内置 SolveTurnstileToken(dx, prepare.p) 解算,然后走 finalize;
//     b. 内置解算失败且 opts.TurnstileSolver != nil 时,再尝试外部 solver;
//     c. 仍失败则直接返回错误,让调度器换号/冷却,不再回退旧单步协议拖成长轮询。
//
// 返回值与老的 ChatRequirements 保持一致,方便调用方无痛切换。
//
// ChatRequirementsV2 会先尝试 Bootstrap()。拿不到首页 cookie 不直接失败,继续让
// sentinel 返回明确错误,避免把临时首页错误误判成鉴权失败。
func (c *Client) ChatRequirementsV2(ctx context.Context) (*ChatRequirementsResp, error) {
	bootstrapErr := c.Bootstrap(ctx)
	if bootstrapErr != nil {
		// 和旧 ChatRequirements 保持一致:bootstrap 是浏览器状态预热,不是最终
		// 鉴权判定。真正不可用由 prepare/finalize 返回的状态决定。
		_ = bootstrapErr
	}

	prep, err := c.ChatRequirementsPrepare(ctx)
	c.checkpointTraffic("requirements.prepare")
	if err != nil {
		if logger := loggerL(); logger != nil {
			logger.Warn("chat-requirements/prepare failed",
				zap.Error(err))
		}
		return nil, err
	}

	// 组装最终响应;即使后面走 finalize,也要把 challenge 描述透传出去。
	resp := &ChatRequirementsResp{Persona: prep.Persona}
	resp.Turnstile.Required = prep.Turnstile.Required
	resp.Proofofwork.Required = prep.Proofofwork.Required
	resp.Proofofwork.Seed = prep.Proofofwork.Seed
	resp.Proofofwork.Difficulty = prep.Proofofwork.Difficulty

	// 本地解 PoW;finalize 与后续 f/conversation header 复用同一个 proof_token。
	var proof string
	if prep.Proofofwork.Required {
		proof = SolveProofToken(prep.Proofofwork.Seed, prep.Proofofwork.Difficulty, DefaultUserAgent)
		resp.ProofToken = proof
	}

	// Turnstile 路由
	var turnstileResp string
	if prep.Turnstile.Required {
		var solveDebug turnstileSolveDebug
		turnstileResp, solveDebug = solveTurnstileToken(prep.Turnstile.DX, prep.SourceP)
		if turnstileResp == "" && c.opts.TurnstileSolver != nil {
			sCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			out, solveErr := c.opts.TurnstileSolver.Solve(sCtx, prep.Turnstile.DX)
			if solveErr != nil || out == "" {
				if logger := loggerL(); logger != nil {
					logger.Warn("turnstile external solver failed",
						zap.Error(solveErr))
				}
			} else {
				turnstileResp = out
			}
		}
		if turnstileResp == "" {
			if logger := loggerL(); logger != nil {
				logger.Warn("turnstile solver returned empty",
					zap.String("stage", solveDebug.Stage),
					zap.String("encoding", solveDebug.Encoding),
					zap.Int("dx_len", solveDebug.DXLen),
					zap.Int("p_len", solveDebug.PLen),
					zap.Int("decoded_len", solveDebug.DecodedLen),
					zap.Int("instruction_count", solveDebug.InstructionCount),
					zap.Int("executed_count", solveDebug.ExecutedCount),
					zap.Int("recovered_count", solveDebug.RecoveredCount))
			}
			return nil, errors.New("turnstile solver returned empty")
		}
		if logger := loggerL(); logger != nil {
			logger.Info("turnstile solver ok",
				zap.String("encoding", solveDebug.Encoding),
				zap.Int("dx_len", solveDebug.DXLen),
				zap.Int("p_len", solveDebug.PLen),
				zap.Int("decoded_len", solveDebug.DecodedLen),
				zap.Int("instruction_count", solveDebug.InstructionCount),
				zap.Int("executed_count", solveDebug.ExecutedCount),
				zap.Int("recovered_count", solveDebug.RecoveredCount),
				zap.Int("token_len", len(turnstileResp)))
		}
		resp.TurnstileToken = turnstileResp
	}

	// finalize 拿真正 token
	token, persona, soToken, err := c.ChatRequirementsFinalize(ctx, prep.PrepareToken, proof, turnstileResp)
	c.checkpointTraffic("requirements.finalize")
	if err != nil {
		if logger := loggerL(); logger != nil {
			logger.Warn("chat-requirements/finalize failed",
				zap.Error(err))
		}
		return nil, err
	}
	resp.Token = token
	resp.SOToken = soToken
	if persona != "" {
		resp.Persona = persona
	}
	if logger := loggerL(); logger != nil {
		logger.Info("chat-requirements two-step ok",
			zap.String("persona", resp.Persona),
			zap.Bool("turnstile_required", prep.Turnstile.Required),
			zap.Bool("pow_required", prep.Proofofwork.Required),
			zap.Int("token_len", len(token)),
		)
	}
	return resp, nil
}

// SSEEvent 单条 SSE 数据。
type SSEEvent struct {
	Event       string
	Data        []byte
	Err         error
	ReadFailure SSEReadFailureClass
}
