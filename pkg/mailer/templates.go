package mailer

import (
	"fmt"
	"strings"
	"time"
)

// 下面是预置模板。为了不引入 text/template 的运行时开销,直接用 fmt.Sprintf。
// 如果后续要可运营化,再切到 template + DB 配置。

const baseCSS = `<style>
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;background:#f5f7fa;margin:0;padding:24px;color:#333}
.card{max-width:560px;margin:0 auto;background:#fff;border-radius:10px;padding:28px;box-shadow:0 2px 10px rgba(0,0,0,.05)}
h1{font-size:20px;margin:0 0 12px;color:#222}
.hi{color:#666;font-size:14px;margin-bottom:18px}
.box{background:#f2f8ff;border-left:4px solid #409eff;padding:14px 16px;border-radius:4px;margin:14px 0}
.muted{color:#999;font-size:12px;margin-top:18px}
table{width:100%;border-collapse:collapse;margin:8px 0}
td{padding:6px 0;font-size:14px}
td.l{color:#666;width:120px}
td.v{color:#222;font-weight:500}
</style>`

// RenderWelcome 注册欢迎邮件。
func RenderWelcome(nickname, email, baseURL string) (subject, html string) {
	subject = "欢迎加入积云"
	if nickname == "" {
		nickname = email
	}
	if baseURL == "" {
		baseURL = "/"
	}
	html = fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8">%s</head><body>
<div class="card">
  <h1>欢迎,%s</h1>
  <div class="hi">你的积云账号已开通,账号 <b>%s</b>。</div>
  <div class="box">
    <b>新手指引</b><br/>
    · 到 <b>个人中心 → API Key</b> 创建一把 sk- 开头的 key<br/>
    · 把 base_url 指向 <code>%s/v1</code> 即可用 OpenAI SDK 调用<br/>
    · 首次注册会赠送试用积分,可在 <b>账单</b> 里看到明细
  </div>
  <div class="muted">如果这不是你本人的操作,请直接忽略本邮件。</div>
</div>
</body></html>`, baseCSS, htmlEscape(nickname), htmlEscape(email), htmlEscape(baseURL))
	return
}

// RenderPaid 充值成功邮件。
func RenderPaid(nickname, outTradeNo string, priceCNYFen int, credits, bonus int64, paidAt time.Time) (subject, html string) {
	subject = "充值成功通知"
	if nickname == "" {
		nickname = "用户"
	}
	price := fmt.Sprintf("¥ %.2f", float64(priceCNYFen)/100.0)
	ts := paidAt.Format("2006-01-02 15:04:05")
	html = fmt.Sprintf(`<!doctype html><html><head><meta charset="utf-8">%s</head><body>
<div class="card">
  <h1>充值成功 ✔</h1>
  <div class="hi">Hi %s,你本次的充值已到账。</div>
  <table>
    <tr><td class="l">订单号</td><td class="v"><code>%s</code></td></tr>
    <tr><td class="l">实付金额</td><td class="v">%s</td></tr>
    <tr><td class="l">到账积分</td><td class="v">%d 厘 (基础) + %d 厘 (赠送)</td></tr>
    <tr><td class="l">支付时间</td><td class="v">%s</td></tr>
  </table>
  <div class="muted">如有疑问请联系客服并提供订单号。本邮件由系统自动发送。</div>
</div>
</body></html>`, baseCSS, htmlEscape(nickname), htmlEscape(outTradeNo), price, credits, bonus, ts)
	return
}

// htmlEscape 防注入。
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}
