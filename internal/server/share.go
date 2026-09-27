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
	// 负数尺寸表示每个模块 8 像素：模块大小一致，四周保留标准的 4 格空白
	png, err := qrcode.Encode(text, qrcode.Medium, -8)
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
	ScriptURL string
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
:root{--bg:#EDF0F3;--card:#fff;--card-2:#F5F7F9;--ink:#141A21;--ink-2:#3A434F;--muted:#667180;--line:#D9DEE4;--line-strong:#C2C9D2;--accent:#3446D4;--land:#C9560F;--land-soft:#FBEADF;--ok:#1B8452;--ok-soft:#E0F2E9;--bad:#C2332B;--bad-soft:#FBE3E1}
@media (prefers-color-scheme:dark){:root{color-scheme:dark;--bg:#0A0D11;--card:#151A21;--card-2:#1B222B;--ink:#E3E8EE;--ink-2:#C3CBD5;--muted:#8B96A5;--line:#29313D;--line-strong:#38434F;--accent:#8D99FF;--land:#F08C4E;--land-soft:#3A2416;--ok:#45C388;--ok-soft:#12301F;--bad:#F2766C;--bad-soft:#3A1614}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.6 "PingFang SC","Hiragino Sans GB","Noto Sans SC",system-ui,sans-serif;-webkit-text-size-adjust:100%;padding:20px max(16px,env(safe-area-inset-right)) calc(32px + env(safe-area-inset-bottom)) max(16px,env(safe-area-inset-left))}
main{max-width:520px;margin:0 auto;display:grid;gap:12px}
h1{font-size:24px;line-height:1.3;margin:0}h2{margin:0}p{margin:0}
.head{display:grid;gap:4px;padding:4px 2px 8px}.head .eyebrow{font-size:12px;color:var(--muted)}.head .accts{font-size:13px;color:var(--muted)}
.exit{margin-top:8px;display:flex;flex-wrap:wrap;align-items:baseline;gap:2px 10px;padding:10px 12px;border-radius:6px;background:var(--land-soft)}
.exit .k{font-size:12px;font-weight:600;color:var(--land)}.exit .ip{font:600 17px/1.3 ui-monospace,Menlo,monospace}.exit .loc{font-size:13px;color:var(--ink-2)}
.card{background:var(--card);border:1px solid var(--line);border-radius:8px;padding:16px;display:grid;gap:12px}
.card h2{display:flex;align-items:center;gap:10px;font-size:17px;line-height:1.35}
.n{flex:none;width:22px;height:22px;border-radius:50%;display:grid;place-items:center;font:600 12px/1 ui-monospace,Menlo,monospace;background:var(--ink);color:var(--card)}
.lead{font-size:14px;color:var(--ink-2)}
.btn{display:flex;align-items:center;justify-content:center;width:100%;min-height:48px;padding:0 14px;border:1px solid transparent;border-radius:6px;background:var(--ink);color:var(--card);font:inherit;font-weight:600;text-decoration:none;cursor:pointer;-webkit-tap-highlight-color:transparent}
.btn.alt{background:none;color:var(--ink);border-color:var(--line-strong)}
.btn:active{opacity:.82}.btn:disabled{opacity:.6}
.btn:focus-visible,.copy:focus-visible,summary:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.qrbox{margin:0;display:grid;justify-items:center;gap:10px;padding:16px 12px 12px;border-radius:6px;background:var(--card-2)}
.qr{display:block;width:min(100%,264px);height:auto;aspect-ratio:1;border-radius:4px;background:#fff}
.qrbox figcaption{font-size:13px;color:var(--muted);text-align:center;text-wrap:balance}
.link{display:flex;gap:8px;align-items:center;padding:6px 6px 6px 10px;border-radius:6px;background:var(--card-2);border:1px solid var(--line)}
.link code{flex:1;min-width:0;font:12px/1.5 ui-monospace,Menlo,monospace;color:var(--muted);word-break:break-all;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden;user-select:all;-webkit-user-select:all}
.copy{flex:none;min-height:40px;padding:0 14px;border-radius:6px;border:1px solid var(--line-strong);background:var(--card);color:var(--ink);font:inherit;font-size:13px;font-weight:600;cursor:pointer}
.steps{margin:0;padding-left:20px;display:grid;gap:6px;font-size:14px;color:var(--ink-2)}
dl{margin:0;display:grid;grid-template-columns:auto minmax(0,1fr);border-top:1px solid var(--line)}
dt,dd{margin:0;padding:10px 0;border-bottom:1px solid var(--line)}dt{color:var(--muted);padding-right:18px}dd{font-weight:600}
dd.mono{font-family:ui-monospace,Menlo,monospace}
.how{font-size:13px;color:var(--muted);line-height:1.7}.how b{color:var(--ink-2);font-weight:600}
#selfcheck-result{display:grid;gap:4px}#selfcheck-result:empty{display:none}
.verdict{display:flex;align-items:center;gap:8px;font-weight:700;padding:10px 12px;border-radius:6px}.verdict.ok{background:var(--ok-soft);color:var(--ok)}.verdict.bad{background:var(--bad-soft);color:var(--bad)}
.checks{list-style:none;margin:0;padding:0;display:grid}
.checks li{display:grid;grid-template-columns:20px minmax(0,1fr);gap:10px;padding:10px 0;border-top:1px solid var(--line)}.checks li:first-child{border-top:0}
.checks .ic{width:20px;height:20px;border-radius:50%;display:grid;place-items:center;margin-top:1px;font-size:12px;font-weight:700;color:var(--card);background:var(--muted)}
.checks .ok .ic{background:var(--ok)}.checks .bad .ic{background:var(--bad)}
.checks b{display:block;font-weight:600;line-height:1.45}.checks .bad b{color:var(--bad)}
.checks li div>span{display:block;font-size:13px;line-height:1.5;color:var(--muted);overflow-wrap:anywhere}
details.card{padding:0;gap:0}
details.card>summary{list-style:none;display:flex;align-items:center;justify-content:space-between;gap:12px;min-height:52px;padding:0 16px;font-weight:600;cursor:pointer}
details.card>summary::-webkit-details-marker{display:none}
details.card>summary small{display:block;font-size:12px;font-weight:400;color:var(--muted)}
details.card>summary::after{content:"";flex:none;width:7px;height:7px;border-right:2px solid var(--muted);border-bottom:2px solid var(--muted);transform:translateY(-2px) rotate(45deg)}
details.card[open]>summary::after{transform:translateY(2px) rotate(-135deg)}
details.card .body{display:grid;gap:12px;padding:0 16px 16px}
.foot{font-size:13px;color:var(--muted);text-align:center;padding:4px 8px}
</style></head><body><main>
<header class="head"><p class="eyebrow">导入页 · 只给这台手机用</p><h1>{{.Device}}</h1>
{{with .Accounts}}<p class="accts">{{range $i, $a := .}}{{if $i}} · {{end}}{{$a.Platform}} {{$a.Handle}}{{end}}</p>{{end}}
<p class="exit"><span class="k">平台看到的出口</span><span class="ip">{{.IP}}</span>{{if .Country}}<span class="loc">{{.Country}} {{.City}}</span>{{end}}</p></header>
{{if .Relay}}
<section class="card"><h2><span class="n">1</span>导入小火箭</h2>
<a class="btn" href="{{.RelayOpen}}">在小火箭中打开</a>
<figure class="qrbox"><img class="qr" src="{{.RelayQR}}" alt="小火箭节点二维码"><figcaption>在别的设备上打开本页时：用这台 iPhone 的小火箭点首页右上角扫码</figcaption></figure>
<div class="link"><code>{{.RelayLink}}</code><button class="copy" type="button" data-copy="{{.RelayLink}}">复制</button></div>
<ol class="steps"><li>选中新节点，「全局路由」选「代理」，打开开关。</li><li>「设置」里打开「按需连接」，确认「UDP 转发」已开启：VPN 意外断开会自动重连，不会用手机自己的IP上网。</li></ol>
</section>
{{else}}<section class="card"><h2><span class="n">1</span>导入 Stash</h2>{{template "stash" .}}</section>{{end}}
<section class="card"><h2><span class="n">2</span>把手机设置改成当地</h2>
{{if .Timezone}}<dl><dt>时区</dt><dd class="mono">{{.Timezone}}</dd>{{if .Language}}<dt>语言</dt><dd>{{.Language}}</dd>{{end}}{{if .Country}}<dt>地区</dt><dd>{{.Country}}</dd>{{end}}</dl>
{{else}}<p class="lead">这个IP的地区信息还没查到，稍后刷新本页，或问管理员要时区和语言。</p>{{end}}
<p class="how"><b>设置 → 通用 → 日期与时间</b>：关闭「自动设置」后选城市<br><b>设置 → 通用 → 语言与地区</b></p></section>
<section class="card" id="check"><h2><span class="n">3</span>安全自检</h2>
<p class="lead">做完上面两步、开着代理，在这台 iPhone 上点下面的按钮。会检查出口IP、时区、语言和 WebRTC，结果会同步给管理员。</p>
<button class="btn" type="button" id="selfcheck">开始自检</button><div id="selfcheck-result"></div></section>
{{if .Relay}}<details class="card"><summary><span>改用 Stash 导入<small>不用小火箭时</small></span></summary><div class="body">{{template "stash" .}}</div></details>{{end}}
<p class="foot">这个页面只给这台手机用，不要转发。链接泄露时请联系管理员重置，旧链接会立即失效。</p>
</main><script src="{{.ScriptURL}}"></script></body></html>
{{define "stash"}}<figure class="qrbox"><img class="qr" src="{{.SubQR}}" alt="Stash 订阅二维码"><figcaption>用 Stash 扫码，或在这台手机上点下面的按钮</figcaption></figure>
<a class="btn{{if .Relay}} alt{{end}}" href="{{.StashURL}}">在 Stash 中导入</a>
<div class="link"><code>{{.SubURL}}</code><button class="copy" type="button" data-copy="{{.SubURL}}">复制</button></div>{{end}}`))

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
		ScriptURL: s.assetSet().URL("share.js"),
		Device:    c.Device, Accounts: c.Accounts,
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
