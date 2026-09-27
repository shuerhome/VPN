package server

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"luodi/internal/store"
)

// langPrefix 是各国手机语言应有的前缀（navigator.language 的前两位）。
var langPrefix = map[string]string{
	"US": "en", "GB": "en", "CA": "en", "AU": "en", "SG": "en", "PH": "en", "IE": "en", "NZ": "en", "IN": "en",
	"JP": "ja", "KR": "ko", "DE": "de", "AT": "de", "FR": "fr", "BR": "pt", "PT": "pt", "MX": "es", "ES": "es", "AR": "es",
	"TH": "th", "VN": "vi", "ID": "id", "MY": "ms", "HK": "zh", "TW": "zh", "RU": "ru", "TR": "tr", "NL": "nl", "IT": "it",
}

// SelfCheckItem 是自检的一项。OK 为 nil 表示无法判断（比如还没查到地区）。
type SelfCheckItem struct {
	Key    string `json:"key"`
	OK     *bool  `json:"ok"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

func boolp(b bool) *bool { return &b }

// visitorIP 是访问者的公网IP。面板只能通过 Cloudflare 隧道从外网访问，
// cloudflared 会带上 CF-Connecting-IP；本机调试访问时退回连接地址。
func visitorIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
		return v
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// evaluateSelfCheck 对比手机上报的环境和住宅IP应有的环境。
func evaluateSelfCheck(ip store.IP, exit, tz, lang string, webrtc []string) []SelfCheckItem {
	want := ip.ExpectedExit()
	var items []SelfCheckItem

	if exit == want {
		items = append(items, SelfCheckItem{"ip", boolp(true), "出口IP 是住宅IP", exit})
	} else {
		items = append(items, SelfCheckItem{"ip", boolp(false), "出口IP 不是住宅IP",
			"现在是 " + exit + "，应为 " + want + "。检查小火箭是否打开、「全局路由」是否选了「代理」、是否选中了这台手机的节点。开着 iCloud 专用代理（Private Relay）也会这样，请关掉。"})
	}

	if ip.Timezone == "" {
		items = append(items, SelfCheckItem{"tz", nil, "时区", "住宅IP 的地区还没查到，无法对比。手机当前时区：" + tz})
	} else if tz == ip.Timezone {
		items = append(items, SelfCheckItem{"tz", boolp(true), "时区一致", tz})
	} else {
		items = append(items, SelfCheckItem{"tz", boolp(false), "时区不一致",
			"手机是 " + orDash(tz) + "，住宅IP 在 " + ip.Timezone + "。设置 → 通用 → 日期与时间：关闭「自动设置」，选对应城市。"})
	}

	if want := langPrefix[ip.CountryCode]; want == "" {
		items = append(items, SelfCheckItem{"lang", nil, "语言", "手机当前语言：" + orDash(lang)})
	} else if strings.HasPrefix(strings.ToLower(lang), want) {
		items = append(items, SelfCheckItem{"lang", boolp(true), "语言一致", lang})
	} else {
		items = append(items, SelfCheckItem{"lang", boolp(false), "语言和IP所在国家不一致",
			"手机是 " + orDash(lang) + "，" + ip.Country + " 一般用 " + want + " 开头的语言。设置 → 通用 → 语言与地区。"})
	}

	var leaked []string
	for _, w := range webrtc {
		if w != "" && w != want && net.ParseIP(w) != nil {
			leaked = append(leaked, w)
		}
	}
	switch {
	case len(leaked) > 0:
		items = append(items, SelfCheckItem{"webrtc", boolp(false), "WebRTC 暴露了其他IP",
			strings.Join(leaked, "、") + "。说明 UDP 没走代理：在小火箭里确认「UDP 转发」已开启，并使用全局代理。"})
	case len(webrtc) > 0:
		items = append(items, SelfCheckItem{"webrtc", boolp(true), "WebRTC 没有暴露其他IP", strings.Join(webrtc, "、")})
	default:
		items = append(items, SelfCheckItem{"webrtc", boolp(true), "WebRTC 没有暴露IP", "没有拿到任何公网IP"})
	}
	return items
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// selfCheck 是手机在导入页点「开始自检」时调用的接口（凭订阅 token，无需登录）。
func (s *Server) selfCheck(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	c, err := s.App.Store.ChainByToken(token)
	if err != nil || len(token) < 16 {
		writeErr(w, http.StatusNotFound, "链接不存在或已被重置")
		return
	}
	ip, err := s.App.Store.IP(c.IPID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "住宅IP不存在")
		return
	}
	var body struct {
		TZ     string   `json:"tz"`
		Lang   string   `json:"lang"`
		WebRTC []string `json:"webrtc"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	_ = json.NewDecoder(r.Body).Decode(&body)
	if len(body.WebRTC) > 10 {
		body.WebRTC = body.WebRTC[:10]
	}
	exit := visitorIP(r)
	items := evaluateSelfCheck(*ip, exit, trim(body.TZ, 64), trim(body.Lang, 32), body.WebRTC)
	ok := true
	for _, it := range items {
		if it.OK != nil && !*it.OK {
			ok = false
		}
	}
	raw, _ := json.Marshal(items)
	_ = s.App.Store.UpdateSelfCheck(c.ID, ok, string(raw))
	s.App.SelfCheckAlert(c.ID, c.Device, exit, ip.ExpectedExit(), items[0].OK != nil && *items[0].OK, leakedIPs(items))
	writeJSON(w, map[string]any{"ok": ok, "items": items, "exit": exit})
}

func leakedIPs(items []SelfCheckItem) string {
	for _, it := range items {
		if it.Key == "webrtc" && it.OK != nil && !*it.OK {
			return it.Detail
		}
	}
	return ""
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
