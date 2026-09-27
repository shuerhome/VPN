// Package gen 生成下发到手机的配置：Stash / Clash Meta 订阅，以及小火箭用的落地节点链接。
package gen

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"luodi/internal/store"

	"gopkg.in/yaml.v3"
)

// MaxAutoFronts 自动模式下前置组最多放几个节点。
const MaxAutoFronts = 8

// FastestInterval 是「最快」模式下手机端测速的间隔（秒）。
const FastestInterval = 10

// FastestTolerance：新节点要比当前节点快这么多毫秒才切换，避免在差不多快的节点之间来回跳。
const FastestTolerance = 50

// FrontNodes 选出这条链路的前置节点。
// 固定模式：就是那一个节点。自动 / 最快模式：与落地IP同国家的节点，按服务器测速排序，最多 MaxAutoFronts 个；
// 同国家没有节点时退回全部节点。
//
// 服务器（VPS 在海外）测不通的节点也保留：入口在国内的专线从海外常常连不上，但手机上能用。
// 手机上的客户端每 10 秒自己测速，连不上的节点会被自动跳过。
func FrontNodes(c store.Chain, ip store.IP, nodes []store.Node) []store.Node {
	if c.FrontMode == "fixed" {
		for _, n := range nodes {
			if n.ID == c.FrontNodeID {
				return []store.Node{n}
			}
		}
		return nil
	}
	var pick []store.Node
	if ip.CountryCode != "" {
		for _, n := range nodes {
			if n.Country == ip.CountryCode {
				pick = append(pick, n)
			}
		}
	}
	if len(pick) == 0 {
		pick = append(pick, nodes...)
	}
	rank := func(n store.Node) int {
		switch {
		case n.DelayMS > 0:
			return 0 // 服务器能连上
		case n.DelayMS == 0:
			return 1 // 还没测
		}
		return 2 // 服务器连不上（手机上可能可以）
	}
	sort.SliceStable(pick, func(i, j int) bool {
		ri, rj := rank(pick[i]), rank(pick[j])
		if ri != rj {
			return ri < rj
		}
		return pick[i].DelayMS < pick[j].DelayMS
	})
	if len(pick) > MaxAutoFronts {
		pick = pick[:MaxAutoFronts]
	}
	return pick
}

func LandName(ip store.IP) string {
	loc := strings.TrimSpace(ip.CountryCode + " " + ip.City)
	if loc == "" {
		loc = ip.Host
	}
	return "落地·" + loc
}

func frontName(n store.Node) string { return "前置·" + n.Name }

// ---------------- 有序 YAML ----------------

type KV struct {
	K string
	V any
}

// OM 是保持字段顺序的 YAML 映射。
type OM []KV

// O 用 "键", 值, "键", 值… 的形式构造 OM，方便其他包使用。
func O(kv ...any) OM {
	o := make(OM, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		o = append(o, KV{kv[i].(string), kv[i+1]})
	}
	return o
}

func (o OM) MarshalYAML() (any, error) {
	node := &yaml.Node{Kind: yaml.MappingNode}
	for _, kv := range o {
		var v yaml.Node
		if err := v.Encode(kv.V); err != nil {
			return nil, err
		}
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: kv.K}, &v)
	}
	return node, nil
}

// orderedProxy 把 name/type/server/port 放在最前面，其余按字母序。
func orderedProxy(raw map[string]any, name string) OM {
	o := OM{{"name", name}}
	for _, k := range []string{"type", "server", "port"} {
		if v, ok := raw[k]; ok {
			o = append(o, KV{k, v})
		}
	}
	var rest []string
	for k := range raw {
		switch k {
		case "name", "type", "server", "port", "dialer-proxy":
			continue
		}
		rest = append(rest, k)
	}
	sort.Strings(rest)
	for _, k := range rest {
		o = append(o, KV{k, raw[k]})
	}
	return o
}

// LandProxy 生成住宅IP的节点定义；dialer 为空表示直连（服务器上单测住宅IP时用）。
func LandProxy(ip store.IP, name, dialer string) OM {
	typ := "socks5"
	if ip.Protocol == "http" || ip.Protocol == "https" {
		typ = "http"
	}
	o := OM{{"name", name}, {"type", typ}, {"server", ip.Host}, {"port", ip.Port}}
	if ip.Username != "" {
		o = append(o, KV{"username", ip.Username})
	}
	if ip.Password != "" {
		o = append(o, KV{"password", ip.Password})
	}
	if ip.Protocol == "https" {
		o = append(o, KV{"tls", true})
	}
	if typ == "socks5" {
		o = append(o, KV{"udp", true})
	}
	if dialer != "" {
		o = append(o, KV{"dialer-proxy", dialer})
	}
	return o
}

// ClientConfig 生成手机用的 Stash / Clash Meta 配置。
func ClientConfig(c store.Chain, ip store.IP, nodes []store.Node) (string, error) {
	fronts := FrontNodes(c, ip, nodes)
	if len(fronts) == 0 {
		return "", fmt.Errorf("没有可用的前置节点：先添加机场订阅，或把固定前置换成还在的节点")
	}
	land := LandName(ip)

	proxies := make([]any, 0, len(fronts)+1)
	used := map[string]bool{}
	var frontNames []string
	for _, n := range fronts {
		name := frontName(n)
		for i := 2; used[name]; i++ {
			name = fmt.Sprintf("%s #%d", frontName(n), i)
		}
		used[name] = true
		frontNames = append(frontNames, name)
		proxies = append(proxies, orderedProxy(n.Raw, name))
	}

	dialer := frontNames[0]
	var groups []any
	switch c.FrontMode {
	case "fixed":
	case "auto":
		// 故障切换：一直用第一个，它挂了才换下一个
		dialer = "前置-" + orDefault(ip.CountryCode, "自动")
		groups = append(groups, OM{
			{"name", dialer}, {"type", "fallback"},
			{"url", "https://www.gstatic.com/generate_204"}, {"interval", 60},
			{"proxies", frontNames},
		})
	default: // fastest
		// 最快：手机每 10 秒测一次延迟，自动用最快的前置。换前置不影响落地IP。
		dialer = "前置-" + orDefault(ip.CountryCode, "自动")
		groups = append(groups, OM{
			{"name", dialer}, {"type", "url-test"},
			{"url", "https://www.gstatic.com/generate_204"}, {"interval", FastestInterval},
			{"tolerance", FastestTolerance}, {"lazy", false},
			{"proxies", frontNames},
		})
	}
	proxies = append(proxies, LandProxy(ip, land, dialer))
	groups = append(groups, OM{{"name", "社媒出口"}, {"type", "select"}, {"proxies", []string{land}}})

	// 全局：手机的所有流量都从住宅IP出去，没有任何直连或走机场出口的规则
	rules := []string{"MATCH,社媒出口"}

	doc := OM{
		{"mixed-port", 7890},
		{"allow-lan", false},
		{"mode", "rule"},
		{"log-level", "warning"},
		{"ipv6", false},
		{"dns", OM{
			{"enable", true},
			{"ipv6", false},
			{"enhanced-mode", "fake-ip"},
			{"fake-ip-range", "198.18.0.1/16"},
			{"nameserver", []string{"https://1.1.1.1/dns-query", "https://8.8.8.8/dns-query"}},
		}},
		{"proxies", proxies},
		{"proxy-groups", groups},
		{"rules", rules},
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s · 落地链路台 · %s\n", c.Device, time.Now().Format("2006-01-02 15:04"))
	sb.WriteString("# 手机 → 前置(机场) → 落地(住宅IP) → 平台。「社媒出口」只有落地一个节点，断线不回落。\n")
	enc := yaml.NewEncoder(&sb)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return "", err
	}
	_ = enc.Close()
	return sb.String(), nil
}

// RocketLink 生成小火箭可扫码导入的住宅IP节点链接。
func RocketLink(ip store.IP) string {
	auth := ""
	if ip.Username != "" {
		auth = ip.Username + ":" + ip.Password + "@"
	}
	remark := url.PathEscape(strings.TrimPrefix(LandName(ip), "落地·"))
	if ip.Protocol == "socks5" {
		return "socks://" + base64.StdEncoding.EncodeToString([]byte(auth+ip.Host+fmt.Sprintf(":%d", ip.Port))) + "#" + remark
	}
	u := url.URL{Scheme: ip.Protocol, Host: fmt.Sprintf("%s:%d", ip.Host, ip.Port)}
	if ip.Username != "" {
		u.User = url.UserPassword(ip.Username, ip.Password)
	}
	return u.String() + "#" + remark
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
