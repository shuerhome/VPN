package relay

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
	"testing"
	"time"

	"luodi/internal/crypt"
	"luodi/internal/store"

	"gopkg.in/yaml.v3"
)

// 端到端：模拟手机的 mihomo 用生成的 vless 节点连中转，中转按用户把流量交给住宅 SOCKS5，
// 最后访问回显服务。再验证吊销后旧节点连不上。需要 MIHOMO_BIN。
func TestRelayEndToEnd(t *testing.T) {
	bin := os.Getenv("MIHOMO_BIN")
	if bin == "" {
		t.Skip("未设置 MIHOMO_BIN")
	}
	lan := localIPv4()
	if lan == "" {
		t.Skip("没有非回环的 IPv4 地址")
	}

	// 回显服务
	echo, _ := net.Listen("tcp", lan+":0")
	go http.Serve(echo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		fmt.Fprint(w, host)
	}))
	// Reality 回落目标：本地 TLS 1.3 站点
	destLn, _ := tls.Listen("tcp", lan+":0", &tls.Config{Certificates: []tls.Certificate{selfSigned(t, "test.local")}, MinVersion: tls.VersionTLS13})
	go http.Serve(destLn, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))

	// 住宅IP
	socksPort := freePort(t)
	startMihomo(t, bin, fmt.Sprintf(`log-level: warning
listeners:
  - {name: socks-in, type: socks, listen: 127.0.0.1, port: %d, users: [{username: res_user, password: res_pass}]}
rules: ["MATCH,DIRECT"]
`, socksPort))
	waitPort(t, socksPort)

	// 面板数据
	box, _ := crypt.Load("", t.TempDir())
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	ipID, _ := st.AddIP(store.IP{Protocol: "socks5", Host: "127.0.0.1", Port: socksPort, Username: "res_user", Password: "res_pass"})
	chainID, err := st.AddChain(store.Chain{Device: "iPhone 01", IPID: ipID, FrontMode: "auto", Route: "global"})
	if err != nil {
		t.Fatal(err)
	}

	relayPort := freePort(t)
	r := &Relay{Bin: bin, Dir: t.TempDir(), PublicHost: lan, PublicPort: relayPort, ListenPort: relayPort, SNI: "test.local", Dest: destAddr(destLn.Addr().String()), Store: st, LogLevel: os.Getenv("RELAY_LOG_LEVEL")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	waitPort(t, relayPort)

	c, _ := st.Chain(chainID)
	ip, _ := st.IP(ipID)
	link, err := r.Link(*c, *ip)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("小火箭扫码链接: %s", link)

	echoURL := "http://" + echo.Addr().String() + "/"
	if got := viaPhone(t, bin, r, *c, echoURL); got != lan {
		t.Fatalf("经中转访问的出口应是住宅IP那一跳（%s），实际 %q", lan, got)
	}

	// 吊销：换 UUID 后旧节点应当连不上
	old := *c
	if err := st.RotateToken(chainID); err != nil {
		t.Fatal(err)
	}
	r.Reload()
	time.Sleep(1500 * time.Millisecond)
	if got := viaPhone(t, bin, r, old, echoURL); got == lan {
		t.Fatal("吊销后旧节点仍然能用")
	}
	c2, _ := st.Chain(chainID)
	if got := viaPhone(t, bin, r, *c2, echoURL); got != lan {
		t.Fatalf("新节点应该能用，实际 %q", got)
	}
}

// viaPhone 起一个「手机端」mihomo，用中转节点访问 target，返回响应内容（失败返回空串）。
func viaPhone(t *testing.T, bin string, r *Relay, c store.Chain, target string) string {
	t.Helper()
	p, err := r.ClashProxy(c, "relay")
	if err != nil {
		t.Fatal(err)
	}
	mixed := freePort(t)
	doc := map[string]any{
		"log-level": "warning", "mixed-port": mixed, "mode": "rule",
		"proxies": []any{p}, "rules": []string{"MATCH,relay"},
	}
	// 测试用的自签站点：客户端需要跳过证书校验（真实环境 Reality 不需要）
	cfg, _ := yaml.Marshal(doc)
	stop := startMihomo(t, bin, string(cfg))
	defer stop()
	waitPort(t, mixed)
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", mixed))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 10 * time.Second}
	resp, err := client.Get(target)
	if err != nil {
		t.Logf("请求失败: %v", err)
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Logf("HTTP %d", resp.StatusCode)
		return ""
	}
	return strings.TrimSpace(string(b))
}

func startMihomo(t *testing.T, bin, cfg string) func() {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-d", dir, "-f", path)
	if os.Getenv("MIHOMO_LOG") != "" {
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(stop)
	return stop
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

// RELAY_DEST_HOST=test.local 时用域名作回落目标（需要 hosts 里把它指到本机网卡地址），覆盖正式环境的域名分支
func destAddr(addr string) string {
	if h := os.Getenv("RELAY_DEST_HOST"); h != "" {
		_, port, _ := net.SplitHostPort(addr)
		return net.JoinHostPort(h, port)
	}
	return addr
}
