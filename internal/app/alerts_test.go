package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"luodi/internal/crypt"
	"luodi/internal/notify"
	"luodi/internal/store"
)

type inbox struct {
	mu   sync.Mutex
	msgs []string
}

func (b *inbox) all() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.msgs...)
}

func setup(t *testing.T) (*App, *inbox) {
	box, _ := crypt.Load("", t.TempDir())
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	got := &inbox{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		got.mu.Lock()
		got.msgs = append(got.msgs, m["text"].(string))
		got.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	t.Cleanup(srv.Close)
	_ = st.SetSetting("tg_chat", "1001")
	return &App{Store: st, Notify: &notify.Telegram{Token: "t", Store: st, APIURL: srv.URL}, RelayEnabled: true, TmpDir: t.TempDir()}, got
}

// settle 等异步发送完成，返回新增的消息
func settle(b *inbox, before int) []string {
	time.Sleep(150 * time.Millisecond)
	return b.all()[before:]
}

func TestAlertTransitions(t *testing.T) {
	a, box := setup(t)
	id, _ := a.Store.AddIP(store.IP{Protocol: "socks5", Host: "203.0.113.24", Port: 1080})
	_, _ = a.Store.AddChain(store.Chain{Device: "iPhone 01", IPID: id, FrontMode: "auto"})

	// 第一次检测正常：不发消息
	_ = a.Store.UpdateIPCheck(id, store.IPCheck{Exit: "203.0.113.24", MS: 80})
	a.EvaluateAlerts()
	if msgs := settle(box, 0); len(msgs) != 0 {
		t.Fatalf("正常时不应发消息: %v", msgs)
	}
	// 出口变了：发一次，重复检测不重复发
	_ = a.Store.UpdateIPCheck(id, store.IPCheck{Exit: "198.51.100.9", MS: 80})
	a.EvaluateAlerts()
	a.EvaluateAlerts()
	msgs := settle(box, 0)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "出口IP变了") || !strings.Contains(msgs[0], "198.51.100.9") {
		t.Fatalf("出口变化应只通知一次: %v", msgs)
	}
	// 连不上：状态变了，再发一次
	_ = a.Store.UpdateIPCheck(id, store.IPCheck{Error: "连接超时"})
	a.EvaluateAlerts()
	if msgs := settle(box, 1); len(msgs) != 1 || !strings.Contains(msgs[0], "连不上住宅IP") {
		t.Fatalf("断线应通知: %v", msgs)
	}
	// 恢复：发恢复消息
	_ = a.Store.UpdateIPCheck(id, store.IPCheck{Exit: "203.0.113.24", MS: 80})
	a.EvaluateAlerts()
	if msgs := settle(box, 2); len(msgs) != 1 || !strings.Contains(msgs[0], "恢复正常") {
		t.Fatalf("恢复应通知: %v", msgs)
	}
}

func TestExpiryAlerts(t *testing.T) {
	a, box := setup(t)
	soon := time.Now().AddDate(0, 0, 5).Format("2006-01-02")
	id, _ := a.Store.AddIP(store.IP{Protocol: "socks5", Host: "203.0.113.24", Port: 1080, Expire: soon})
	_ = id
	apID, _ := a.Store.AddAirport("机场 A", "https://example.com/sub", false)
	_ = a.Store.UpdateAirportSync(apID, 460<<30, 0, 500<<30, time.Now().Add(3*24*time.Hour).Unix(), "")

	a.EvaluateAlerts()
	a.EvaluateAlerts()
	msgs := strings.Join(settle(box, 0), "\n---\n")
	for _, want := range []string{"还有 5 天到期", "机场「机场 A」还有", "流量已用 92%"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("缺少提醒 %q:\n%s", want, msgs)
		}
	}
	if n := len(box.all()); n != 3 {
		t.Fatalf("应只发 3 条，实际 %d:\n%s", n, msgs)
	}
}

func TestMultiDeviceCooldown(t *testing.T) {
	a, box := setup(t)
	a.MultiDeviceAlert(9, "iPhone 09", []string{"1.1.1.1", "2.2.2.2"})
	a.MultiDeviceAlert(9, "iPhone 09", []string{"1.1.1.1", "3.3.3.3"})
	if msgs := settle(box, 0); len(msgs) != 1 || !strings.Contains(msgs[0], "2 个IP同时使用") {
		t.Fatalf("6 小时内只报一次: %v", msgs)
	}
}

func TestStatusText(t *testing.T) {
	a, _ := setup(t)
	id, _ := a.Store.AddIP(store.IP{Protocol: "socks5", Host: "203.0.113.24", Port: 1080})
	cid, _ := a.Store.AddChain(store.Chain{Device: "iPhone 01", IPID: id, FrontMode: "auto"})
	_ = a.Store.UpdateIPCheck(id, store.IPCheck{Exit: "203.0.113.24", MS: 80})
	_ = a.Store.AddTraffic(cid, time.Now().Format("2006-01-02"), 1<<20, 5<<20)
	if got := a.statusText(); !strings.Contains(got, "✅ iPhone 01 · 出口 203.0.113.24 · 离线 · 今日 6.0 MB") {
		t.Fatalf("%q", got)
	}
}

func TestSelfCheckAlert(t *testing.T) {
	a, box := setup(t)
	a.SelfCheckAlert(3, "iPhone 03", "203.0.113.24", "203.0.113.24", true, "")
	a.SelfCheckAlert(3, "iPhone 03", "117.136.0.8", "203.0.113.24", false, "")
	a.SelfCheckAlert(3, "iPhone 03", "117.136.0.8", "203.0.113.24", false, "")
	a.SelfCheckAlert(3, "iPhone 03", "203.0.113.24", "203.0.113.24", true, "")
	msgs := settle(box, 0)
	all := strings.Join(msgs, "\n")
	if len(msgs) != 2 || !strings.Contains(all, "不是住宅IP") || !strings.Contains(all, "自检通过") {
		t.Fatalf("应先告警一次，恢复时再通知一次: %v", msgs)
	}
}
