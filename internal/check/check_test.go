package check

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"luodi/internal/store"
)

// 集成测试：本机起一个 mihomo 充当「机场 ss 节点」和「住宅 SOCKS5」，
// 再用 Runner 走 前置 → 落地 → 本地回显服务，确认链路和账号校验都生效。
// 需要设置 MIHOMO_BIN 才会运行。
func TestRunnerChain(t *testing.T) {
	bin := os.Getenv("MIHOMO_BIN")
	if bin == "" {
		t.Skip("未设置 MIHOMO_BIN")
	}

	// mihomo 会拒绝连回环地址（防止代理绕回自己），所以回显服务绑在本机网卡地址上
	lanIP := localIPv4()
	if lanIP == "" {
		t.Skip("没有非回环的 IPv4 地址")
	}
	echo, err := net.Listen("tcp", lanIP+":0")
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(echo, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		fmt.Fprint(w, host)
	}))

	ssPort, _ := freePort()
	socksPort, _ := freePort()
	// 「机场」和「住宅IP」必须是两个进程，同一个 mihomo 连自己的监听会被当成回环拒绝
	startHelper(t, bin, fmt.Sprintf(`log-level: warning
listeners:
  - {name: ss-in, type: shadowsocks, listen: 127.0.0.1, port: %d, cipher: aes-128-gcm, password: airport-pass}
rules: ["MATCH,DIRECT"]
`, ssPort))
	startHelper(t, bin, fmt.Sprintf(`log-level: warning
listeners:
  - {name: socks-in, type: socks, listen: 127.0.0.1, port: %d, users: [{username: res_user, password: res_pass}]}
rules: ["MATCH,DIRECT"]
`, socksPort))
	waitPort(t, socksPort)
	waitPort(t, ssPort)

	front := store.Node{ID: 1, Name: "美国 测试 01", Raw: map[string]any{
		"name": "美国 测试 01", "type": "ss", "server": "127.0.0.1", "port": ssPort, "cipher": "aes-128-gcm", "password": "airport-pass", "udp": true,
	}}
	good := store.IP{ID: 1, Protocol: "socks5", Host: "127.0.0.1", Port: socksPort, Username: "res_user", Password: "res_pass"}
	bad := good
	bad.Password = "wrong"

	r := &Runner{Bin: bin, WorkDir: t.TempDir(), CheckURLs: []string{"http://" + echo.Addr().String() + "/"}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := r.Run(ctx, nil, []ChainJob{
		{Key: "chain-ok", IP: good, Fronts: []store.Node{front}},
		{Key: "chain-badpass", IP: bad, Fronts: []store.Node{front}},
		{Key: "direct", IP: good},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Chains["chain-ok"]; got.Error != "" || got.Exit != lanIP || got.Front != "美国 测试 01" {
		t.Fatalf("链路检测结果不对: %+v", got)
	}
	if got := res.Chains["direct"]; got.Error != "" || got.Exit != lanIP {
		t.Fatalf("直连检测结果不对: %+v", got)
	}
	if got := res.Chains["chain-badpass"]; got.Error == "" {
		t.Fatalf("密码错误应该检测失败: %+v", got)
	} else {
		t.Logf("密码错误时的提示: %s", got.Error)
	}
	if _, ok := res.NodeDelay[1]; !ok {
		t.Fatal("应该测过前置节点延迟")
	}
}

func waitPort(t *testing.T, port int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
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

func startHelper(t *testing.T, bin, cfg string) {
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
