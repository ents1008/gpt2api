package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// defaultProbeTargets 是用户 `proxy.probe_target_url` 留空时后台轮询的**候选链**。
//
// 设计目标:
//   - 三个**不同厂商**的轻量接口,被同一条代理同时 Block 的概率极低
//   - 响应体都 < 1 KB,走 2xx/3xx(非 204 也接受,见 probe 里 status 判断)
//   - HTTPS / HTTP 混合,避免遇到 TLS MITM / 明文被劫持都能暴露其中一类问题
//   - 任意 1 个通过即判代理可用,全部失败才标失败 → 根治"gstatic 被 ISP/代理屏蔽就误判代理挂掉"
//
// 顺序有讲究:响应小且稳定的放前面,拿到 200 就短路。
var defaultProbeTargets = []string{
	"https://api.ipify.org/?format=json",
	"https://www.cloudflare.com/cdn-cgi/trace",
	"http://httpbin.org/ip",
}

// ProbeSettings 探测器配置提供者(从 settings.Service 注入)。
// 所有字段都支持热更新:循环每轮结束都会重新读取。
type ProbeSettings interface {
	ProbeEnabled() bool
	ProbeIntervalSec() int // 两轮探测之间的间隔(秒);<= 0 视为关闭
	ProbeTimeoutSec() int  // 单次探测超时(秒)
	ProbeTargetURL() string
	ProbeConcurrency() int // 并发 worker 数;<=0 默认 8
}

// ProbeResult 单次探测结果。
type ProbeResult struct {
	ProxyID   uint64        `json:"proxy_id"`
	OK        bool          `json:"ok"`
	LatencyMs int           `json:"latency_ms"`
	Error     string        `json:"error,omitempty"`
	TriedAt   time.Time     `json:"tried_at"`
	Duration  time.Duration `json:"-"`
}

// Prober 周期性对启用的代理发起连通性探测,刷新 health_score/last_probe_at/last_error。
//
// 评分策略:
//   - 成功  → score = min(100, score + 10),清空 last_error
//   - 失败  → score = max(0,   score - 20),记录简短 error
type Prober struct {
	svc      *Service
	settings ProbeSettings
	log      *zap.Logger

	// 手动触发通道:发送 <id> 探测单个(0 表示全部)
	kickCh chan uint64
}

func NewProber(svc *Service, settings ProbeSettings, log *zap.Logger) *Prober {
	return &Prober{
		svc:      svc,
		settings: settings,
		log:      log,
		kickCh:   make(chan uint64, 32),
	}
}

// Run 后台循环探测,受 ctx 控制。建议作为独立 goroutine 启动。
func (p *Prober) Run(ctx context.Context) {
	// 启动后先睡 5 秒,避开启动峰值。
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
	}

	for {
		interval := time.Duration(p.settings.ProbeIntervalSec()) * time.Second
		if !p.settings.ProbeEnabled() || interval <= 0 {
			// 关闭状态下也要相应 kick 和 ctx,用小节拍轮询配置。
			select {
			case <-ctx.Done():
				return
			case id := <-p.kickCh:
				p.runOnce(ctx, id)
			case <-time.After(30 * time.Second):
			}
			continue
		}

		p.runOnce(ctx, 0)

		select {
		case <-ctx.Done():
			return
		case id := <-p.kickCh:
			p.runOnce(ctx, id)
		case <-time.After(interval):
		}
	}
}

// Kick 触发一次立即探测。id=0 表示全部启用的代理;否则只探一条。
// 非阻塞(通道满时直接丢弃,避免调用者卡住)。
func (p *Prober) Kick(id uint64) {
	select {
	case p.kickCh <- id:
	default:
	}
}

// ProbeOne 对单条代理做一次同步探测(不写库)。对外暴露用于手动测试。
func (p *Prober) ProbeOne(ctx context.Context, pr *Proxy) ProbeResult {
	return p.probe(ctx, pr)
}

// ProbeByID 手动触发单条探测并写库,返回结果。
func (p *Prober) ProbeByID(ctx context.Context, id uint64) (ProbeResult, error) {
	pr, err := p.svc.Get(ctx, id)
	if err != nil {
		return ProbeResult{}, err
	}
	res := p.probe(ctx, pr)
	p.applyResult(ctx, pr, res)
	return res, nil
}

// ProbeAll 手动触发对所有启用代理的并发探测并写库,返回结果列表。
func (p *Prober) ProbeAll(ctx context.Context) ([]ProbeResult, error) {
	list, err := p.svc.dao.ListAllEnabled(ctx)
	if err != nil {
		return nil, err
	}
	return p.probeBatch(ctx, list), nil
}

// ---------- 内部实现 ----------

func (p *Prober) runOnce(ctx context.Context, only uint64) {
	var list []*Proxy
	var err error
	if only == 0 {
		list, err = p.svc.dao.ListAllEnabled(ctx)
	} else {
		pr, gerr := p.svc.Get(ctx, only)
		if gerr == nil {
			list = []*Proxy{pr}
		}
		err = gerr
	}
	if err != nil {
		p.log.Warn("prober: list failed", zap.Error(err))
		return
	}
	if len(list) == 0 {
		return
	}
	results := p.probeBatch(ctx, list)
	ok, bad := 0, 0
	for _, r := range results {
		if r.OK {
			ok++
		} else {
			bad++
		}
	}
	p.log.Info("prober: round finished",
		zap.Int("total", len(results)), zap.Int("ok", ok), zap.Int("bad", bad))
}

func (p *Prober) probeBatch(ctx context.Context, list []*Proxy) []ProbeResult {
	conc := p.settings.ProbeConcurrency()
	if conc <= 0 {
		conc = 8
	}
	if conc > len(list) {
		conc = len(list)
	}

	results := make([]ProbeResult, len(list))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup

	for i, pr := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, pr *Proxy) {
			defer wg.Done()
			defer func() { <-sem }()
			r := p.probe(ctx, pr)
			p.applyResult(ctx, pr, r)
			results[i] = r
		}(i, pr)
	}
	wg.Wait()
	return results
}

// probe 做一次真实 HTTP(S) 请求。只组装结果,不写库。
//
// 目标选择:
//   - 管理员在「系统设置 → 代理探测目标 URL」配置了非空值 → 只试那一个(遵从运维意图)
//   - 留空 → 按 defaultProbeTargets 顺序轮询,任意 1 个 2xx/3xx 即判成功,全部失败才算失败
func (p *Prober) probe(ctx context.Context, pr *Proxy) ProbeResult {
	out := ProbeResult{ProxyID: pr.ID, TriedAt: time.Now()}

	proxyURL, err := p.svc.BuildURL(pr)
	if err != nil {
		out.Error = "密码解密失败:" + err.Error()
		return out
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		out.Error = "代理 URL 格式错误:" + err.Error()
		return out
	}

	timeout := time.Duration(p.settings.ProbeTimeoutSec()) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	targets := p.candidateTargets()
	var triedErrs []string
	for _, target := range targets {
		ok, lat, perr := p.probeOneTarget(ctx, u, target, timeout)
		if ok {
			out.OK = true
			out.LatencyMs = lat
			out.Duration = time.Duration(lat) * time.Millisecond
			return out
		}
		triedErrs = append(triedErrs,
			fmt.Sprintf("%s → %s", shortTargetHost(target), shortenErr(perr)))
	}

	// 所有候选都失败
	switch len(triedErrs) {
	case 0:
		out.Error = "未配置任何探测目标"
	case 1:
		// 单目标(用户显式配置)直接透出友好文案,不要加 "1 个候选全部失败" 的啰嗦前缀
		out.Error = firstErrMsg(triedErrs[0])
	default:
		out.Error = fmt.Sprintf("%d 个探测目标均不通(任一通过即判可用);%s",
			len(triedErrs), strings.Join(triedErrs, "; "))
	}
	return out
}

// candidateTargets 返回本次需要轮询的目标 URL 列表。
//   - 用户配置了非空 probe_target_url → 尊重配置,只用那一个
//   - 配置为空 → 走内置候选链 defaultProbeTargets
func (p *Prober) candidateTargets() []string {
	if t := strings.TrimSpace(p.settings.ProbeTargetURL()); t != "" {
		return []string{t}
	}
	return defaultProbeTargets
}

// probeOneTarget 对单个目标 URL 做一次 HTTP GET。
// 返回:(是否 2xx/3xx, 延迟毫秒, 若失败的底层 error)。
func (p *Prober) probeOneTarget(ctx context.Context, proxyU *url.URL, target string, timeout time.Duration) (bool, int, error) {
	transport := &http.Transport{
		Proxy:                 http.ProxyURL(proxyU),
		DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return false, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "gpt2api-proxy-prober/1.0")

	start := time.Now()
	resp, err := client.Do(req)
	lat := int(time.Since(start) / time.Millisecond)
	if err != nil {
		return false, lat, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return true, lat, nil
	}
	return false, lat, fmt.Errorf("目标站返回异常状态码 %d", resp.StatusCode)
}

// shortTargetHost 给错误摘要用,把 URL 压成 "host" 短字符串。
func shortTargetHost(target string) string {
	if u, err := url.Parse(target); err == nil && u.Host != "" {
		return u.Host
	}
	return target
}

// firstErrMsg 去掉 `host → ` 前缀,把单目标错误回到纯文案形态。
func firstErrMsg(line string) string {
	if i := strings.Index(line, " → "); i >= 0 && i+len(" → ") < len(line) {
		return line[i+len(" → "):]
	}
	return line
}

func (p *Prober) applyResult(ctx context.Context, pr *Proxy, r ProbeResult) {
	score := pr.HealthScore
	lastErr := ""
	if r.OK {
		score += 10
		if score > 100 {
			score = 100
		}
	} else {
		score -= 20
		if score < 0 {
			score = 0
		}
		lastErr = r.Error
		lastErr = truncateProbeError(lastErr, 200)
	}
	if err := p.svc.dao.UpdateHealth(ctx, pr.ID, score, lastErr); err != nil {
		p.log.Warn("prober: update health failed",
			zap.Uint64("proxy_id", pr.ID), zap.Error(err))
	}
}

func truncateProbeError(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	end := 0
	for i := range s {
		if i > maxBytes {
			break
		}
		end = i
	}
	if end == 0 {
		return ""
	}
	return s[:end]
}

// shortenErr 把网络错误压成一行、对前端友好的中文字符串。
// 兜底会带上简短的英文原文,便于排障。
func shortenErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	low := strings.ToLower(s)

	switch {
	// 超时 / 主动取消
	case errors.Is(err, context.DeadlineExceeded),
		strings.Contains(low, "deadline exceeded"),
		strings.Contains(low, "i/o timeout"),
		strings.Contains(low, "timeout awaiting"),
		strings.Contains(low, "request canceled") && strings.Contains(low, "timeout"):
		return "连接超时(探测超时)"

	// 407 代理鉴权失败 —— 用户名/密码错误最常见
	case strings.Contains(s, "Proxy Authentication Required"),
		strings.Contains(low, "407"):
		return "代理鉴权失败(407,请核对用户名/密码)"

	// DNS 解析失败 —— 细分代理自身域名 vs 目标站域名
	case strings.Contains(low, "proxyconnect") && strings.Contains(low, "no such host"):
		return "DNS 解析失败:代理域名无法解析(宿主梯子/DNS 污染,可在 docker-compose 里给 server 指定公共 DNS 如 8.8.8.8)"
	case strings.Contains(low, "no such host"),
		strings.Contains(low, "lookup ") && strings.Contains(low, "no such"):
		return "DNS 解析失败(域名不存在或 DNS 被污染)"

	// 各类拒绝 / 不可达
	case strings.Contains(low, "connection refused"):
		return "目标拒绝连接(connection refused)"
	case strings.Contains(low, "network is unreachable"):
		return "网络不可达"
	case strings.Contains(low, "no route to host"):
		return "无法路由到目标主机"
	case strings.Contains(low, "host is down"):
		return "目标主机不可达"

	// 连接在握手/发送中被对端断开 —— 原因有多种,不要把结论钉死在"鉴权错误"
	case strings.Contains(low, "connection reset by peer"):
		return "对端重置连接(目标站限流、代理线路抖动或鉴权都可能,换个探测目标再看)"
	case strings.Contains(low, "broken pipe"):
		return "连接已断开(broken pipe;代理线路抖动可能性大)"
	case strings.Contains(low, "unexpected eof"),
		low == "eof",
		strings.HasSuffix(low, ": eof"),
		strings.Contains(low, "remotedisconnected"),
		strings.Contains(low, "connection closed"):
		// 注意:HTTPS 握手前被对端 RST/FIN 很多情形都会报 EOF,不要单一归因为"鉴权失败"。
		// 常见原因按概率排序:
		//   1) 目标站对此出口 IP / UA / ALPN 做了限制(比如 gstatic 在部分区域直接丢包)
		//   2) 代理自身到目标站的链路抖动 / 节点降级
		//   3) 代理鉴权失败(较少见,多数情况 407 就返回了)
		// 所以文案里把"鉴权"降到最后,给运维留个排障方向。
		return "连接被对端提前关闭(EOF);可能原因:① 目标站限制此出口 IP ② 代理线路抖动 ③ 鉴权失败。建议在「系统设置」把探测目标留空以走内置候选链"

	// 代理协议问题
	case strings.Contains(low, "proxyconnect tcp"):
		return "代理握手失败(请检查 host:port/scheme)"
	case strings.Contains(low, "malformed http response"):
		return "代理响应非 HTTP(scheme 可能写错)"
	case strings.Contains(low, "socks"):
		return "SOCKS 代理握手失败"

	// TLS
	case strings.Contains(low, "tls:"),
		strings.Contains(low, "x509:"),
		strings.Contains(low, "certificate"):
		return "TLS/证书错误"

	// 其它 —— 给中文前缀 + 简短原文,方便排障
	default:
		// 截断 "Get \"...\": " 等 net/http 前缀
		if i := strings.Index(s, "\": "); i > 0 && i < len(s)-3 {
			s = s[i+3:]
		} else if i := strings.Index(s, ": "); i > 0 && i < len(s)-2 {
			s = s[i+2:]
		}
		if len(s) > 140 {
			s = s[:140] + "…"
		}
		return "探测失败:" + s
	}
}
