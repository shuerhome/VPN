package server

import (
	"encoding/base64"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"luodi/internal/store"

	qrcode "github.com/skip2/go-qrcode"
)

// clientKind 按 User-Agent 判断是哪种客户端在拉订阅。
func clientKind(ua string) string {
	u := strings.ToLower(ua)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(u, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("stash", "clash", "mihomo", "meta/", "verge", "flclash"):
		return "clash"
	case has("shadowrocket", "quantumult", "loon", "v2ray", "streisand", "hiddify", "v2box", "foxray", "karing", "sing-box", "singbox", "nekobox"):
		return "relay"
	case has("mozilla", "safari", "chrome"):
		return "page"
	}
	return ""
}

func qrDataURI(text string) template.URL {
	png, err := qrcode.Encode(text, qrcode.Medium, 360)
	if err != nil {
		return ""
	}
	return template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(png))
}

var langByCountry = map[string]string{
	"US": "English (US)", "GB": "English (UK)", "CA": "English (Canada)", "AU": "English (Australia)",
	"JP": "日本語", "KR": "한국어", "DE": "Deutsch", "FR": "Français", "SG": "English (Singapore)",
	"HK": "繁體中文（香港）", "TW": "繁體中文（台灣）", "BR": "Português (Brasil)", "MX": "Español (México)",
	"ES": "Español", "TH": "ไทย", "VN": "Tiếng Việt", "ID": "Bahasa Indonesia", "MY": "Bahasa Melayu", "PH": "English (Philippines)",
}

type sharePage struct {
	Device    string
	Accounts  []store.Account
	Relay     bool
	RelayLink string
	RelayQR   template.URL
	SubURL    string
	SubQR     template.URL
	StashURL  template.URL
	RelayOpen template.URL
	IP        string
	Country   string
	City      string
	Timezone  string
	Language  string
}

var shareTmpl = template.Must(template.New("share").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="robots" content="noindex, nofollow">
<title>{{.Device}} · 导入</title>
<style>
:root{--bg:#EDF0F3;--card:#fff;--ink:#141A21;--muted:#667180;--line:#D9DEE4;--accent:#3446D4;--land:#C9560F}
@media (prefers-color-scheme:dark){:root{color-scheme:dark;--bg:#0D1116;--card:#141920;--ink:#E3E8EE;--muted:#8B96A5;--line:#252D38;--accent:#8D99FF;--land:#F08C4E}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.6 "PingFang SC","Hiragino Sans GB","Noto Sans SC",system-ui,sans-serif;padding:20px 16px calc(32px + env(safe-area-inset-bottom))}
main{max-width:520px;margin:0 auto;display:grid;gap:14px}
h1{font-size:22px;margin:0}.muted{color:var(--muted);font-size:13px}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:16px;display:grid;gap:10px}
h2{font-size:16px;margin:0}.qr{width:220px;max-width:100%;aspect-ratio:1;border-radius:8px;background:#fff;justify-self:center}
.btn{display:block;text-align:center;padding:11px;border-radius:8px;background:var(--ink);color:var(--card);text-decoration:none;font-weight:600}
.btn.alt{background:none;color:var(--ink);border:1px solid var(--line)}
code{font:12px/1.5 ui-monospace,Menlo,monospace;word-break:break-all;background:var(--bg);padding:8px;border-radius:6px;display:block;user-select:all;-webkit-user-select:all}
ol{margin:0;padding-left:20px;display:grid;gap:4px}dl{margin:0;display:grid;grid-template-columns:auto 1fr;gap:6px 14px}dt{color:var(--muted)}dd{margin:0;font-weight:600}
.ip{font-family:ui-monospace,Menlo,monospace;color:var(--land)}
</style></head><body><main>
<div><h1>{{.Device}}</h1><div class="muted">{{range $i, $a := .Accounts}}{{if $i}} · {{end}}{{$a.Platform}} {{$a.Handle}}{{end}}</div></div>
{{if .Relay}}
<section class="card"><h2>小火箭扫码导入</h2>
<img class="qr" src="{{.RelayQR}}" alt="节点二维码">
<ol><li>用另一台设备打开本页，iPhone 小火箭点右上角扫码图标扫上面的码。</li>
<li>如果就在这台 iPhone 上打开本页，点下面的按钮，或长按复制链接后在小火箭里从剪贴板导入。</li>
<li>选中新节点，「全局路由」选「代理」，打开开关。</li></ol>
<a class="btn" href="{{.RelayOpen}}">在小火箭中打开</a>
<code>{{.RelayLink}}</code>
</section>
{{end}}
<section class="card"><h2>{{if .Relay}}或者用 Stash{{else}}Stash 导入{{end}}</h2>
<img class="qr" src="{{.SubQR}}" alt="订阅二维码">
<a class="btn alt" href="{{.StashURL}}">在 Stash 中导入</a>
<code>{{.SubURL}}</code>
</section>
<section class="card"><h2>手机设置对齐</h2>
<p class="muted" style="margin:0">平台看到的IP是 <span class="ip">{{.IP}}</span>{{if .Country}}（{{.Country}} {{.City}}）{{end}}。把手机设置改成和它一致：</p>
{{if .Timezone}}<dl><dt>时区</dt><dd>{{.Timezone}}</dd>
{{if .Language}}<dt>语言</dt><dd>{{.Language}}</dd>{{end}}
{{if .Country}}<dt>地区</dt><dd>{{.Country}}</dd>{{end}}</dl>
{{else}}<p class="muted" style="margin:0">这个IP的地区信息还没查到，稍后刷新本页，或问管理员要时区和语言。</p>{{end}}
<p class="muted" style="margin:0">设置 → 通用 → 日期与时间（关闭自动设置后选城市）；设置 → 通用 → 语言与地区。</p>
</section>
<p class="muted">这个页面只给这台手机用，不要转发。链接泄露时请联系管理员重置。</p>
</main></body></html>`))

// sharePage 渲染设备导入页（凭订阅 token 访问，无需登录）。
func (s *Server) share(w http.ResponseWriter, r *http.Request) {
	c, err := s.App.Store.ChainByToken(r.PathValue("token"))
	if err != nil || len(r.PathValue("token")) < 16 {
		http.Error(w, "链接不存在或已被重置", http.StatusNotFound)
		return
	}
	ip, err := s.App.Store.IP(c.IPID)
	if err != nil {
		http.Error(w, "住宅IP不存在", http.StatusNotFound)
		return
	}
	sub := s.baseURL(r) + "/sub/" + c.Token
	p := sharePage{
		Device: c.Device, Accounts: c.Accounts,
		SubURL: sub + "?target=clash", SubQR: qrDataURI(sub + "?target=clash"),
		StashURL: template.URL("stash://install-config?url=" + url.QueryEscape(sub+"?target=clash")),
		IP:       ip.ExpectedExit(), Country: ip.Country, City: ip.City, Timezone: ip.Timezone, Language: langByCountry[ip.CountryCode],
	}
	if s.Relay.Enabled() {
		if link, err := s.Relay.Link(*c, *ip); err == nil {
			p.Relay, p.RelayLink, p.RelayQR, p.RelayOpen = true, link, qrDataURI(link), template.URL(link)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = shareTmpl.Execute(w, p)
}
