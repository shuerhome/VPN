package stats

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"luodi/internal/crypt"
	"luodi/internal/relay"
	"luodi/internal/store"

	"gopkg.in/yaml.v3"
)

// 端到端：两部「手机」从不同来源IP同时用同一个二维码下载，
// 采集器应当统计到流量、显示在线，并触发一码多机告警。需要 MIHOMO_BIN。
func TestCollectorEndToEnd(t *testing.T) {
	bin := os.Getenv("MIHOMO_BIN")
	if bin == "" {
		t.Skip("未设置 MIHOMO_BIN")
	}
	lan := localIPv4()
	if lan == "" {
		t.Skip("没有非回环的 IPv4 地址")
	}

	// 慢速下载：3 秒内分批发 300KB，保证采样时连接还在
	slow, _ := net.Listen("tcp", lan+":0")
	go http.Serve(slow, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("x", 10*1024)
		for i := 0; i < 30; i++ {
			_, _ = io.WriteString(w, chunk)
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))

	socksPort := freePort(t)
	startMihomo(t, bin, fmt.Sprintf(`log-level: warning
listeners:
  - {name: socks-in, type: socks, listen: 127.0.0.1, port: %d, users: [{username: u, password: p}]}
rules: ["MATCH,DIRECT"]
`, socksPort))
	waitPort(t, socksPort)

	box, _ := crypt.Load("", t.TempDir())
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	ipID, _ := st.AddIP(store.IP{Protocol: "socks5", Host: "127.0.0.1", Port: socksPort, Username: "u", Password: "p"})
	chainID, _ := st.AddChain(store.Chain{Device: "iPhone 07", IPID: ipID, FrontMode: "auto", Route: "global"})

	// Reality 每次握手都要连伪装站点，测试里用本地 TLS 1.3 站点代替
	dest, _ := tls.Listen("tcp", lan+":0", &tls.Config{Certificates: []tls.Certificate{selfSigned(t, "test.local")}, MinVersion: tls.VersionTLS13})
	go http.Serve(dest, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	port := freePort(t)
	r := &relay.Relay{Bin: bin, Dir: t.TempDir(), PublicHost: lan, PublicPort: port, ListenPort: port, SNI: "test.local", Dest: dest.Addr().String(), Store: st}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	waitPort(t, port)
	c, _ := st.Chain(chainID)

	var mu sync.Mutex
	var alerted []string
	col := &Collector{Relay: r, Store: st, Alert: func(id int64, device string, ips []string) {
		mu.Lock()
		alerted = append(alerted, device+" "+strings.Join(ips, ","))
		mu.Unlock()
	}}

	// 两部手机：一部从网卡地址连，一部从 127.0.0.1 连，来源IP不同
	phoneA := phone(t, bin, r, *c, lan)
	phoneB := phone(t, bin, r, *c, "127.0.0.1")
	target := "http://" + slow.Addr().String() + "/"
	var wg sync.WaitGroup
	got := make([]int, 2)
	for i, p := range []string{phoneA, phoneB} {
		wg.Add(1)
		go func(i int, proxy string) {
			defer wg.Done()
			got[i] = download(t, proxy, target)
		}(i, p)
	}
	time.Sleep(1200 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if err := col.Poll(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)
	}
	snap := col.Snapshot()[chainID]
	wg.Wait()
	time.Sleep(time.Second) // 等连接关闭
	_ = col.Poll(ctx)

	if !snap.Online || len(snap.IPs) != 2 {
		t.Fatalf("应显示在线且有 2 个来源IP：%+v", snap)
	}
	mu.Lock()
	if len(alerted) != 1 {
		t.Fatalf("应触发一次一码多机告警，实际 %v", alerted)
	}
	mu.Unlock()
	if got[0] != 300*1024 || got[1] != 300*1024 {
		t.Fatalf("下载不完整: %v", got)
	}
	day := time.Now().Format("2006-01-02")
	sum, _ := st.TrafficSummary(day, day)
	// 两部手机各下载 300KB，加上 HTTP 头，统计误差应在 5% 以内
	want := int64(600 * 1024)
	if tr := sum[chainID]; tr.TodayDown < want || tr.TodayDown > want*105/100 {
		t.Fatalf("流量统计不准: 统计 %d，实际约 %d", tr.TodayDown, want)
	} else {
		t.Logf("统计到下行 %d 字节（实际 %d + HTTP 头）", tr.TodayDown, want)
	}
	if after := col.Snapshot()[chainID]; after.Conns != 0 || !after.Online {
		t.Fatalf("下载刚结束：连接数应为 0，但一分钟内仍算在线: %+v", after)
	}

	// 短连接：在两次采样之间开始又结束，连接列表里根本看不到，靠全局流量补上
	fast, _ := net.Listen("tcp", lan+":0")
	go http.Serve(fast, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("y", 200*1024))
	}))
	before := sum[chainID].TodayDown
	for i := 0; i < 3; i++ {
		if n := download(t, phoneA, "http://"+fast.Addr().String()+"/"); n != 200*1024 {
			t.Fatalf("短下载失败: %d", n)
		}
	}
	time.Sleep(300 * time.Millisecond)
	_ = col.Poll(ctx)
	sum, _ = st.TrafficSummary(day, day)
	if got := sum[chainID].TodayDown - before; got < 600*1024 || got > 600*1024*110/100 {
		t.Fatalf("短连接流量统计不准: %d，实际约 %d", got, 600*1024)
	} else {
		t.Logf("短连接：统计到 %d 字节（实际 %d + HTTP 头）", got, 600*1024)
	}
}

// phone 起一个「手机端」mihomo，连中转时用 host 作为服务器地址，返回它的 HTTP 代理地址。
func phone(t *testing.T, bin string, r *relay.Relay, c store.Chain, host string) string {
	t.Helper()
	p, err := r.ClashProxy(c, "relay")
	if err != nil {
		t.Fatal(err)
	}
	for i := range p {
		if p[i].K == "server" {
			p[i].V = host
		}
	}
	mixed := freePort(t)
	cfg, _ := yaml.Marshal(map[string]any{"log-level": "warning", "mixed-port": mixed, "mode": "rule", "proxies": []any{p}, "rules": []string{"MATCH,relay"}})
	startMihomo(t, bin, string(cfg))
	waitPort(t, mixed)
	return fmt.Sprintf("http://127.0.0.1:%d", mixed)
}

func download(t *testing.T, proxy, target string) int {
	u, _ := url.Parse(proxy)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u), DisableKeepAlives: true}, Timeout: 20 * time.Second}
	resp, err := client.Get(target)
	if err != nil {
		t.Logf("下载失败: %v", err)
		return 0
	}
	defer resp.Body.Close()
	n, _ := io.Copy(io.Discard, resp.Body)
	return int(n)
}

func startMihomo(t *testing.T, bin, cfg string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-d", dir, "-f", path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitPort(t *testing.T, port int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			c.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("端口 %d 没有起来", port)
}

func localIPv4() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
}

func selfSigned(t *testing.T, host string) tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
