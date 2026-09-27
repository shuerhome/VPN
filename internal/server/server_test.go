package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"luodi/internal/app"
	"luodi/internal/crypt"
	"luodi/internal/notify"
	"luodi/internal/relay"
	"luodi/internal/stats"
	"luodi/internal/store"
)

func newServer(t *testing.T) (*httptest.Server, *store.Store, string) {
	box, _ := crypt.Load("", t.TempDir())
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	ipID, _ := st.AddIP(store.IP{Protocol: "socks5", Host: "203.0.113.24", Port: 1080, Username: "u", Password: "p"})
	_ = st.UpdateIPCheck(ipID, store.IPCheck{Exit: "203.0.113.24", Geo: &store.Geo{Country: "美国", CountryCode: "US", City: "洛杉矶", Timezone: "America/Los_Angeles"}})
	apID, _ := st.AddAirport("A", "https://example.com/sub", false)
	_ = st.ReplaceNodes(apID, []store.Node{{Name: "美国 01", Type: "ss", Server: "a.example.com", Port: 443, Country: "US",
		Raw: map[string]any{"name": "美国 01", "type": "ss", "server": "a.example.com", "port": 443, "cipher": "aes-128-gcm", "password": "x"}}})
	cid, _ := st.AddChain(store.Chain{Device: "iPhone 01", IPID: ipID, FrontMode: "auto", Route: "global", Accounts: []store.Account{{Platform: "TikTok", Handle: "@shop"}}})
	c, _ := st.Chain(cid)

	rl := &relay.Relay{PublicHost: "72.61.12.131", PublicPort: 443, SNI: "www.microsoft.com", Store: st}
	a := &app.App{Store: st, Notify: &notify.Telegram{Store: st}, Stats: &stats.Collector{Relay: rl, Store: st}, RelayEnabled: true}
	assets := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>panel")}}
	srv := httptest.NewServer(New(a, rl, "password123", "", assets).Handler())
	t.Cleanup(srv.Close)
	return srv, st, c.Token
}

func get(t *testing.T, url, ua string) (*http.Response, string) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("User-Agent", ua)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestSubscriptionByClient(t *testing.T) {
	srv, _, token := newServer(t)
	sub := srv.URL + "/sub/" + token

	// 小火箭：Base64 包着 vless 中转节点
	resp, body := get(t, sub, "Shadowrocket/2070 CFNetwork/1498.700.2 Darwin/23.6.0")
	dec, _ := base64.StdEncoding.DecodeString(body)
	if resp.StatusCode != 200 || !strings.HasPrefix(string(dec), "vless://") || !strings.Contains(string(dec), "@72.61.12.131:443") {
		t.Fatalf("小火箭应拿到中转节点: %d %q", resp.StatusCode, dec)
	}
	// Stash：机场链路 YAML
	resp, body = get(t, sub, "Stash/2.7.1 Clash/1.9.0")
	if resp.StatusCode != 200 || !strings.Contains(body, "dialer-proxy: 前置-US") {
		t.Fatalf("Stash 应拿到链路配置: %d %s", resp.StatusCode, body)
	}
	// 浏览器：跳到导入页
	resp, _ = get(t, sub, "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Safari/604.1")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/d/"+token {
		t.Fatalf("浏览器应跳转导入页: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// 明确指定 target 时以它为准
	if _, body = get(t, sub+"?target=clash", "Shadowrocket/2070"); !strings.Contains(body, "proxies:") {
		t.Fatal("target=clash 应返回 YAML")
	}
	// 错误 token
	if resp, _ = get(t, srv.URL+"/sub/AAAAAAAAAAAAAAAAAAAAAAAA", "Stash"); resp.StatusCode != 404 {
		t.Fatalf("错误 token 应 404: %d", resp.StatusCode)
	}
}

func TestSharePage(t *testing.T) {
	srv, _, token := newServer(t)
	resp, body := get(t, srv.URL+"/d/"+token, "Mozilla/5.0")
	if resp.StatusCode != 200 {
		t.Fatalf("导入页: %d", resp.StatusCode)
	}
	for _, want := range []string{"iPhone 01", "TikTok @shop", "data:image/png;base64,", "vless://", "stash://install-config?url=", "America/Los_Angeles", "English (US)", "203.0.113.24"} {
		if !strings.Contains(body, want) {
			t.Errorf("导入页缺少 %q", want)
		}
	}
	if strings.Contains(body, `"p"`) || strings.Contains(body, "password") {
		t.Error("导入页不应出现住宅IP密码")
	}
}

func TestStateRequiresLogin(t *testing.T) {
	srv, _, _ := newServer(t)
	if resp, _ := get(t, srv.URL+"/api/state", "x"); resp.StatusCode != 401 {
		t.Fatalf("未登录应 401: %d", resp.StatusCode)
	}
	resp, err := http.Post(srv.URL+"/api/login", "application/json", strings.NewReader(`{"password":"password123"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("登录失败: %v %v", err, resp.StatusCode)
	}
	cookie := resp.Cookies()[0]
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/state", nil)
	req.AddCookie(cookie)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("state: %v %v", err, resp.StatusCode)
	}
	var st struct {
		Chains []struct {
			PageURL string          `json:"page_url"`
			Online  json.RawMessage `json:"online"`
			Traffic json.RawMessage `json:"traffic"`
		} `json:"chains"`
		Telegram map[string]any `json:"telegram"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	if len(st.Chains) != 1 || !strings.Contains(st.Chains[0].PageURL, "/d/") || st.Chains[0].Traffic == nil || st.Telegram == nil {
		t.Fatalf("state 缺字段: %+v", st)
	}
	// 写操作缺少 X-Panel 头要被拒
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/check-all", nil)
	req.AddCookie(cookie)
	if resp, _ = http.DefaultClient.Do(req); resp.StatusCode != 403 {
		t.Fatalf("缺少 X-Panel 头应 403: %d", resp.StatusCode)
	}
}

func TestClientKind(t *testing.T) {
	cases := map[string]string{
		"Shadowrocket/2070 CFNetwork": "relay",
		"Quantumult%20X/1.4":          "relay",
		"Loon/3.2":                    "relay",
		"Stash/2.7.1 Clash/1.9.0":     "clash",
		"clash.meta":                  "clash",
		"ClashX Pro/1.9":              "clash",
		"Mozilla/5.0 (iPhone) Safari": "page",
		"curl/8.0":                    "",
	}
	for ua, want := range cases {
		if got := clientKind(ua); got != want {
			t.Errorf("%q: 想要 %q，实际 %q", ua, want, got)
		}
	}
}

func TestSelfCheck(t *testing.T) {
	srv, st, token := newServer(t)
	post := func(cfIP, body string) map[string]any {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/d/"+token+"/check", strings.NewReader(body))
		req.Header.Set("CF-Connecting-IP", cfIP)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	// 一切正确
	got := post("203.0.113.24", `{"tz":"America/Los_Angeles","lang":"en-US","webrtc":["203.0.113.24"]}`)
	if got["ok"] != true {
		t.Fatalf("应全部通过: %v", got)
	}
	c, _ := st.ChainByToken(token)
	if !c.SelfCheckOK || c.SelfCheckAt == 0 || !strings.Contains(c.SelfCheck, "出口IP 是住宅IP") {
		t.Fatalf("自检结果没记下来: %+v", c)
	}
	// 没开代理：出口是国内IP；时区是上海；语言中文；WebRTC 暴露真实IP
	got = post("117.136.0.8", `{"tz":"Asia/Shanghai","lang":"zh-CN","webrtc":["117.136.0.8"]}`)
	if got["ok"] != false {
		t.Fatalf("应不通过: %v", got)
	}
	bad := 0
	for _, it := range got["items"].([]any) {
		if m := it.(map[string]any); m["ok"] == false {
			bad++
		}
	}
	if bad != 4 {
		t.Fatalf("出口、时区、语言、WebRTC 四项都应不通过，实际 %d: %v", bad, got)
	}
	c, _ = st.ChainByToken(token)
	if c.SelfCheckOK {
		t.Fatal("应记为不通过")
	}
	// 错误 token
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/d/AAAAAAAAAAAAAAAAAAAAAAAA/check", strings.NewReader(`{}`))
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 404 {
		t.Fatalf("错误 token 应 404: %d", resp.StatusCode)
	}
}

func TestAssetVersioning(t *testing.T) {
	box, _ := crypt.Load("", t.TempDir())
	st, _ := store.Open(filepath.Join(t.TempDir(), "p.db"), box)
	rl := &relay.Relay{Store: st}
	a := &app.App{Store: st, Notify: &notify.Telegram{Store: st}}
	fsys := fstest.MapFS{
		"index.html": {Data: []byte(`<link rel="stylesheet" href="style.css"><script src="app.js"></script><script src="https://cdn.example.com/x.js"></script>`)},
		"app.js":     {Data: []byte("console.log(1)")},
		"style.css":  {Data: []byte("body{}")},
	}
	srv := httptest.NewServer(New(a, rl, "password123", "", fsys).Handler())
	defer srv.Close()

	resp, body := get(t, srv.URL+"/", "x")
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("页面不应缓存: %q", resp.Header.Get("Cache-Control"))
	}
	m := regexp.MustCompile(`src="app\.js\?v=([0-9a-f]{10})"`).FindStringSubmatch(body)
	if m == nil || !strings.Contains(body, `href="style.css?v=`) || !strings.Contains(body, `src="https://cdn.example.com/x.js"`) {
		t.Fatalf("引用没有加上版本号（或误改了外部地址）: %s", body)
	}
	resp, js := get(t, srv.URL+"/app.js?v="+m[1], "x")
	if js != "console.log(1)" || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("带当前版本号的文件应长期缓存: %q %q", js, resp.Header.Get("Cache-Control"))
	}
	if resp, _ = get(t, srv.URL+"/app.js?v=old", "x"); resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("旧版本号不应长期缓存: %q", resp.Header.Get("Cache-Control"))
	}
}
