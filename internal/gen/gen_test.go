package gen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"luodi/internal/store"
)

func sample() (store.Chain, store.IP, []store.Node) {
	ip := store.IP{ID: 1, Protocol: "socks5", Host: "203.0.113.24", Port: 1080, Username: "u", Password: "p", CountryCode: "US", City: "Los Angeles"}
	nodes := []store.Node{
		{ID: 1, Name: "美国 01", Country: "US", DelayMS: 150, Raw: map[string]any{"name": "美国 01", "type": "ss", "server": "a.example.com", "port": 443, "cipher": "aes-128-gcm", "password": "x"}},
		{ID: 2, Name: "美国 02", Country: "US", DelayMS: -1, Raw: map[string]any{"name": "美国 02", "type": "ss", "server": "b.example.com", "port": 443, "cipher": "aes-128-gcm", "password": "x"}},
		{ID: 3, Name: "日本 01", Country: "JP", DelayMS: 60, Raw: map[string]any{"name": "日本 01", "type": "trojan", "server": "c.example.com", "port": 443, "password": "x"}},
		{ID: 4, Name: "美国 03", Country: "US", DelayMS: 0, Raw: map[string]any{"name": "美国 03", "type": "trojan", "server": "d.example.com", "port": 443, "password": "x"}},
	}
	return store.Chain{ID: 1, Device: "iPhone 01", IPID: 1, FrontMode: "fastest", Route: "global"}, ip, nodes
}

func TestFrontNodes(t *testing.T) {
	c, ip, nodes := sample()
	got := FrontNodes(c, ip, nodes)
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 4 {
		t.Fatalf("自动模式应取同国家、未超时、测过的优先：%+v", got)
	}
	c.FrontMode, c.FrontNodeID = "fixed", 3
	if got := FrontNodes(c, ip, nodes); len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("固定模式：%+v", got)
	}
}

// 生成的订阅要能被 mihomo 解析（Stash 与它格式兼容），三种前置模式都要验证
func TestClientConfigValid(t *testing.T) {
	c, ip, nodes := sample()
	want := map[string]string{"fastest": "type: url-test", "auto": "type: fallback", "fixed": "dialer-proxy: 前置·日本 01"}
	for _, mode := range []string{"fastest", "auto", "fixed"} {
		c.FrontMode, c.FrontNodeID = mode, 3
		cfg, err := ClientConfig(c, ip, nodes)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(cfg, want[mode]) || !strings.Contains(cfg, "- MATCH,社媒出口") || strings.Contains(cfg, "DIRECT") {
			t.Fatalf("%s 模式配置不对:\n%s", mode, cfg)
		}
		if mode == "fastest" && (!strings.Contains(cfg, "interval: 10") || !strings.Contains(cfg, "tolerance: 50")) {
			t.Fatalf("最快模式应每 10 秒测速:\n%s", cfg)
		}
		bin := os.Getenv("MIHOMO_BIN")
		if bin == "" {
			continue
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		_ = os.WriteFile(path, []byte(cfg), 0o600)
		if out, err := exec.Command(bin, "-t", "-d", dir, "-f", path).CombinedOutput(); err != nil {
			t.Fatalf("mihomo 校验失败（%s）: %v\n%s\n%s", mode, err, out, cfg)
		}
	}
}

func TestRocketLink(t *testing.T) {
	_, ip, _ := sample()
	if got := RocketLink(ip); !strings.HasPrefix(got, "socks://dTpwQDIwMy4wLjExMy4yNDoxMDgw#") {
		t.Fatalf("%s", got)
	}
}
