package sub

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseUserInfo(t *testing.T) {
	// 用户实际机场返回的值
	info, ok := ParseUserInfo("upload=19160570874; download=29679951854; total=912680550400; expire=1799920333")
	if !ok || info.Upload != 19160570874 || info.Download != 29679951854 || info.Total != 912680550400 || info.Expire != 1799920333 {
		t.Fatalf("%+v %v", info, ok)
	}
	if _, ok := ParseUserInfo("foo=bar"); ok {
		t.Fatal("无效内容不应识别")
	}
}

func TestParseClashYAML(t *testing.T) {
	body := []byte(`
proxies:
  - {name: "🇺🇸 美国 洛杉矶 01", type: vless, server: us1.example.com, port: 443, uuid: abc, tls: true}
  - {name: "剩余流量：804.5 GB", type: ss, server: x.example.com, port: 1, cipher: aes-128-gcm, password: p}
  - {name: "日本 东京 IEPL", type: trojan, server: jp.example.com, port: "443", password: p, dialer-proxy: other}
  - {name: "未知协议", type: foo, server: a.example.com, port: 1}
`)
	nodes, err := ParseBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3 {
		t.Fatalf("应解析出 3 个节点（含说明节点，Fetch 时再过滤），实际 %d", len(nodes))
	}
	if nodes[0].Country != "US" || nodes[2].Country != "JP" || nodes[2].Port != 443 {
		t.Fatalf("%+v", nodes)
	}
	if _, ok := nodes[2].Raw["dialer-proxy"]; ok {
		t.Fatal("机场节点自带的 dialer-proxy 应被去掉")
	}
	if !isInfoNode(nodes[1]) || isInfoNode(nodes[0]) {
		t.Fatal("说明节点识别不对")
	}
}

func TestParseBase64List(t *testing.T) {
	ss := "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:pa:ss")) + "@1.2.3.4:8388#%E9%A6%99%E6%B8%AF%2001"
	vmessJSON := `{"v":"2","ps":"新加坡 01","add":"sg.example.com","port":"443","id":"uuid-1","aid":"0","net":"ws","path":"/ray","host":"cdn.example.com","tls":"tls"}`
	vmess := "vmess://" + base64.StdEncoding.EncodeToString([]byte(vmessJSON))
	vless := "vless://uuid-2@us.example.com:443?encryption=none&flow=xtls-rprx-vision&security=reality&sni=www.apple.com&fp=chrome&pbk=PUB&sid=ab&type=tcp#US%20LA"
	trojan := "trojan://pw@jp.example.com:443?sni=jp.example.com&type=grpc&serviceName=g#JP"
	hy2 := "hysteria2://pw@hk.example.com:8443?sni=hk.example.com&insecure=1&obfs=salamander&obfs-password=o#HK%20hy2"
	list := base64.StdEncoding.EncodeToString([]byte(ss + "\n" + vmess + "\n" + vless + "\n" + trojan + "\n" + hy2 + "\n"))

	nodes, err := ParseBody([]byte(list))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 5 {
		t.Fatalf("应解析出 5 个节点，实际 %d", len(nodes))
	}
	byType := map[string]Node{}
	for _, n := range nodes {
		byType[n.Type] = n
	}
	if n := byType["ss"]; n.Raw["password"] != "pa:ss" || n.Raw["cipher"] != "aes-256-gcm" || n.Name != "香港 01" || n.Country != "HK" {
		t.Fatalf("ss: %+v", n)
	}
	if n := byType["vmess"]; n.Raw["network"] != "ws" || n.Raw["servername"] != "cdn.example.com" || n.Country != "SG" {
		t.Fatalf("vmess: %+v", n.Raw)
	}
	if n := byType["vless"]; n.Raw["reality-opts"] == nil || n.Raw["flow"] != "xtls-rprx-vision" || n.Country != "US" {
		t.Fatalf("vless: %+v", n.Raw)
	}
	if n := byType["trojan"]; n.Raw["network"] != "grpc" {
		t.Fatalf("trojan: %+v", n.Raw)
	}
	if n := byType["hysteria2"]; n.Raw["obfs"] != "salamander" || n.Raw["skip-cert-verify"] != true {
		t.Fatalf("hy2: %+v", n.Raw)
	}
}

func TestDetectCountry(t *testing.T) {
	cases := map[string]string{
		"🇯🇵 日本 01":         "JP",
		"日本 东京 香港中转":       "JP",
		"US-LosAngeles-01": "US",
		"BUS 专线":           "",
		"🇬🇧 伦敦":            "GB",
		"新加坡 IPLC x2":      "SG",
	}
	for name, want := range cases {
		if got := DetectCountry(name); got != want {
			t.Errorf("%q: 想要 %q，实际 %q", name, want, got)
		}
	}
}

func TestFillInfoFromName(t *testing.T) {
	var info Info
	fillInfoFromName(&info, "剩余流量：312.5 GB")
	fillInfoFromName(&info, "套餐到期：2027-01-14")
	if info.Total != int64(312.5*(1<<30)) || info.Expire == 0 {
		t.Fatalf("%+v", info)
	}
}

func TestNoticeNodesFiltered(t *testing.T) {
	body := []byte(`
proxies:
  - {name: "！！！请尽快使用官方 Ninja客户端，已支持iOS外部测试！！！", type: ss, server: hk.example.org, port: 443, cipher: aes-128-gcm, password: p}
  - {name: "占位", type: ss, server: 127.0.0.1, port: 443, cipher: aes-128-gcm, password: p}
  - {name: "香港 02（专线；智能）", type: ss, server: hk2.example.org, port: 443, cipher: aes-128-gcm, password: p}
`)
	res, err := Parse(body, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 1 || res.Nodes[0].Name != "香港 02（专线；智能）" || res.Nodes[0].Country != "HK" || len(res.Notices) != 2 {
		t.Fatalf("%+v", res)
	}
}

func TestUnsupportedAndProviders(t *testing.T) {
	_, err := Parse([]byte("proxies:\n  - {name: \"香港 01（公网；智能）\", type: ninja, server: a.example.org, port: 443}\n  - {name: \"香港 02（专线；智能）\", type: ninja, server: b.example.org, port: 443}\n"), "")
	if err == nil || !strings.Contains(err.Error(), "ninja（2 个）") {
		t.Fatalf("应说明是不支持的协议: %v", err)
	}
	_, err = Parse([]byte("proxy-providers:\n  airport:\n    type: http\n    url: https://sub.example.org/api?token=x\n    path: ./a.yaml\n"), "")
	if err == nil || !strings.Contains(err.Error(), "https://sub.example.org/api?token=x") {
		t.Fatalf("应提示 proxy-providers 的在线链接: %v", err)
	}
	res, err := Parse([]byte("proxies:\n  - {name: \"香港 01\", type: ss, server: a.example.org, port: 443, cipher: aes-128-gcm, password: p}\n  - {name: \"香港 02\", type: ninja, server: b.example.org, port: 443}\n"), "")
	if err != nil || len(res.Nodes) != 1 || !strings.Contains(res.Report.Describe(), "ninja（1 个）") {
		t.Fatalf("部分可用时应导入可用的并提示跳过的: %v %+v", err, res)
	}
}
