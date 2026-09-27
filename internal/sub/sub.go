// Package sub 拉取机场订阅：读流量/到期，解析 Clash YAML 或 Base64 节点列表。
package sub

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// DefaultUA 是默认的订阅客户端身份。很多机场会按 User-Agent 返回不同的节点列表，
// 可以在面板里给每个机场单独设置，或用「探测」挑出节点最全的那个。
const DefaultUA = "clash.meta"

// ProbeUAs 是探测时依次尝试的常见客户端身份。
var ProbeUAs = []string{
	"clash.meta",
	"mihomo/1.19.13",
	"clash-verge/v2.2.3",
	"clash-verge/v1.7.7",
	"ClashforWindows/0.20.39",
	"Clash Nyanpasu/v1.6.1",
	"FlClash/v0.8.80 clash-verge",
	"ClashX Pro/1.118.0",
	"Stash/2.7.0 Clash/1.9.0",
	"Shadowrocket/2070 CFNetwork/1498.700.2 Darwin/23.6.0",
	"Quantumult%20X/1.4.1",
	"sing-box 1.11.4",
	"v2rayN/7.10.0",
	"Hiddify/2.5.7",
}

type Info struct {
	Upload, Download, Total, Expire int64
}

type Node struct {
	Name    string
	Type    string
	Server  string
	Port    int
	Country string
	Raw     map[string]any
}

type Result struct {
	Info    Info
	HasInfo bool
	Nodes   []Node
	Skipped int      // 被过滤掉的「剩余流量 / 到期」之类的说明节点
	Notices []string // 这些说明节点的名字
	Report  Report   // 被跳过的协议、在线链接
}

// IsIPHost 判断订阅地址是不是纯 IP，这类地址通常是自签证书。
func IsIPHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return net.ParseIP(u.Hostname()) != nil
}

// Fetch 用指定的客户端身份拉取订阅并解析。ua 为空时用 DefaultUA。
func Fetch(ctx context.Context, rawURL string, insecure bool, ua string) (*Result, error) {
	body, userinfo, err := Download(ctx, rawURL, insecure, ua)
	if err != nil {
		return nil, err
	}
	return Parse(body, userinfo)
}

// Download 只下载订阅内容，返回正文和 subscription-userinfo 头。
func Download(ctx context.Context, rawURL string, insecure bool, ua string) ([]byte, string, error) {
	body, h, err := download(ctx, rawURL, insecure, ua)
	if err != nil {
		return nil, "", err
	}
	return body, h.Get("subscription-userinfo"), nil
}

func download(ctx context.Context, rawURL string, insecure bool, ua string) ([]byte, http.Header, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, nil, errors.New("订阅链接格式不对，应以 http:// 或 https:// 开头")
	}
	if ua == "" {
		ua = DefaultUA
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: insecure}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		var cerr *tls.CertificateVerificationError
		if errors.As(err, &cerr) {
			return nil, nil, errors.New("证书校验失败。订阅地址是纯 IP 或自签证书时，勾选「忽略证书错误」")
		}
		return nil, nil, fmt.Errorf("请求订阅失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("订阅返回 HTTP %d，检查链接是否过期", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, nil, err
	}
	return body, resp.Header, nil
}

// Parse 解析订阅正文（Clash YAML 或节点链接列表），过滤掉「剩余流量」「请使用官方客户端」这类说明节点。
func Parse(body []byte, userinfo string) (*Result, error) {
	res := &Result{}
	if userinfo != "" {
		res.Info, res.HasInfo = ParseUserInfo(userinfo)
	}
	nodes, rep, err := parseBodyReport(body)
	if err != nil {
		return nil, err
	}
	res.Report = rep
	for _, n := range nodes {
		if isInfoNode(n) {
			if !res.HasInfo {
				fillInfoFromName(&res.Info, n.Name)
			}
			res.Notices = append(res.Notices, n.Name)
			res.Skipped++
			continue
		}
		res.Nodes = append(res.Nodes, n)
	}
	if len(res.Nodes) == 0 {
		if msg := rep.Describe(); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, errors.New("订阅里没有解析出可用节点")
	}
	return res, nil
}

// ProbeResult 是用某个客户端身份拉订阅的结果。
type ProbeResult struct {
	UA      string         `json:"ua"`
	Nodes   int            `json:"nodes"`
	Types   map[string]int `json:"types"`
	Sample  []string       `json:"sample"`  // 前几个节点名
	Notices []string       `json:"notices"` // 说明节点（如「请使用官方客户端」）
	HasInfo bool           `json:"has_info"`
	Error   string         `json:"error"`
	Format  string         `json:"format"` // 拿到的内容是什么：Clash 配置、Base64 节点链接、加密或二进制数据……
	Bytes   int            `json:"bytes"`
	Digest  string         `json:"digest"` // 内容指纹：指纹相同说明机场给的是同一份内容
}

// Probe 用一组客户端身份分别拉取订阅，找出机场对哪个身份给的节点最全。
func Probe(ctx context.Context, rawURL string, insecure bool, uas []string) []ProbeResult {
	out := make([]ProbeResult, len(uas))
	sem := make(chan struct{}, 4)
	done := make(chan struct{})
	for i, ua := range uas {
		go func(i int, ua string) {
			sem <- struct{}{}
			defer func() { <-sem; done <- struct{}{} }()
			r := ProbeResult{UA: ua, Types: map[string]int{}}
			body, h, err := download(ctx, rawURL, insecure, ua)
			if err != nil {
				r.Error = err.Error()
				out[i] = r
				return
			}
			sum := sha256.Sum256(body)
			r.Format, r.Bytes, r.Digest = DetectFormat(body, h.Get("Content-Type")), len(body), hex.EncodeToString(sum[:4])
			res, err := Parse(body, h.Get("subscription-userinfo"))
			if err != nil {
				r.Error = err.Error()
				out[i] = r
				return
			}
			r.Nodes, r.HasInfo, r.Notices = len(res.Nodes), res.HasInfo, res.Notices
			for _, n := range res.Nodes {
				r.Types[n.Type]++
				if len(r.Sample) < 6 {
					r.Sample = append(r.Sample, n.Name)
				}
			}
			out[i] = r
		}(i, ua)
	}
	for range uas {
		<-done
	}
	return out
}

// DetectFormat 说明订阅返回的是什么内容。官方客户端专用的订阅常常是加密的，
// 这时面板解不出节点，但能告诉用户「拿到的是加密数据」，而不是笼统地报错。
func DetectFormat(body []byte, contentType string) string {
	text := strings.TrimSpace(strings.TrimPrefix(string(body), "\uFEFF"))
	lower := strings.ToLower(text)
	switch {
	case text == "":
		return "空内容"
	case !utf8.ValidString(text) || binaryRatio(text) > 0.1:
		return "二进制数据（很可能是加密的）"
	case strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html"):
		return "网页（HTML）"
	case (text[0] == '{' || text[0] == '[') && json.Valid([]byte(text)):
		return "JSON 数据"
	case strings.Contains(text, "proxies:") || strings.Contains(text, "proxy-providers:"):
		return "Clash 配置"
	case strings.Contains(text, "://"):
		return "节点链接列表"
	}
	if dec, ok := decodeB64(text); ok {
		switch {
		case strings.Contains(dec, "://"):
			return "Base64 节点链接"
		case strings.Contains(dec, "proxies:"):
			return "Base64 编码的 Clash 配置"
		case !utf8.ValidString(dec) || binaryRatio(dec) > 0.1:
			return "Base64 编码的二进制数据（很可能是加密的）"
		}
		return "Base64 编码的文本"
	}
	if strings.Contains(strings.ToLower(contentType), "octet-stream") {
		return "二进制数据（很可能是加密的）"
	}
	return "看不懂的文本"
}

// binaryRatio 是控制字符（换行、回车、制表符除外）和无效字符所占的比例。
func binaryRatio(s string) float64 {
	n, bad := 0, 0
	for _, r := range s {
		n++
		if r == utf8.RuneError || (r < 0x20 && r != '\n' && r != '\r' && r != '\t') || r == 0x7f {
			bad++
		}
	}
	if n == 0 {
		return 0
	}
	return float64(bad) / float64(n)
}

// ParseUserInfo 解析 "upload=1; download=2; total=3; expire=4"。
func ParseUserInfo(h string) (Info, bool) {
	var info Info
	ok := false
	for _, part := range strings.Split(h, ";") {
		k, v, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "upload":
			info.Upload, ok = int64(n), true
		case "download":
			info.Download, ok = int64(n), true
		case "total":
			info.Total, ok = int64(n), true
		case "expire":
			info.Expire = int64(n)
		}
	}
	return info, ok
}

// ParseBody 依次尝试 Clash YAML、Base64 节点列表、明文节点列表。
func ParseBody(body []byte) ([]Node, error) {
	nodes, _, err := parseBodyReport(body)
	return nodes, err
}

// Report 记录解析时被跳过的内容，用来给出明白的提示。
type Report struct {
	Unsupported map[string]int // 不支持的协议 → 节点数（如官方客户端自有协议）
	Providers   []string       // 配置里的 proxy-providers 在线链接
}

func (r Report) Describe() string {
	var parts []string
	if len(r.Unsupported) > 0 {
		var types []string
		total := 0
		for t, n := range r.Unsupported {
			types = append(types, fmt.Sprintf("%s（%d 个）", t, n))
			total += n
		}
		sort.Strings(types)
		parts = append(parts, fmt.Sprintf("有 %d 个节点的协议是 %s，Stash 和小火箭都不支持（可能是官方客户端自己的协议），已跳过", total, strings.Join(types, "、")))
	}
	if len(r.Providers) > 0 {
		parts = append(parts, "这份配置的节点来自在线链接（proxy-providers）："+strings.Join(r.Providers, "、")+"。可以把这个链接当作订阅链接添加")
	}
	return strings.Join(parts, "；")
}

func parseBodyReport(body []byte) ([]Node, Report, error) {
	rep := Report{Unsupported: map[string]int{}}
	text := strings.TrimSpace(strings.TrimPrefix(string(body), "\uFEFF"))
	if strings.Contains(text, "proxies:") || strings.Contains(text, "proxy-providers:") {
		var doc struct {
			Proxies   []map[string]any          `yaml:"proxies"`
			Providers map[string]map[string]any `yaml:"proxy-providers"`
		}
		if err := yaml.Unmarshal([]byte(text), &doc); err == nil && (len(doc.Proxies) > 0 || len(doc.Providers) > 0) {
			var out []Node
			for _, p := range doc.Proxies {
				if n, ok := fromClash(p); ok {
					out = append(out, n)
				} else if typ, _ := p["type"].(string); typ != "" && !supported[typ] {
					rep.Unsupported[typ]++
				}
			}
			for _, pv := range doc.Providers {
				if u, _ := pv["url"].(string); strings.HasPrefix(u, "http") {
					rep.Providers = append(rep.Providers, u)
				}
			}
			sort.Strings(rep.Providers)
			return dedupe(out), rep, nil
		}
	}
	if !strings.Contains(text, "://") {
		if dec, ok := decodeB64(text); ok {
			text = dec
		}
	}
	var out []Node
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m, err := ParseURI(line); err == nil {
			if n, ok := fromClash(m); ok {
				out = append(out, n)
			}
		} else if scheme, _, found := strings.Cut(line, "://"); found && len(scheme) < 20 {
			rep.Unsupported[strings.ToLower(scheme)]++
		}
	}
	if len(out) == 0 {
		if msg := rep.Describe(); msg != "" {
			return nil, rep, errors.New(msg)
		}
		return nil, rep, errors.New("看不懂订阅内容：既不是 Clash 配置，也不是节点链接列表")
	}
	return dedupe(out), rep, nil
}

var supported = map[string]bool{
	"ss": true, "ssr": true, "vmess": true, "vless": true, "trojan": true, "hysteria": true, "hysteria2": true,
	"tuic": true, "wireguard": true, "snell": true, "anytls": true, "socks5": true, "http": true,
}

func fromClash(p map[string]any) (Node, bool) {
	name, _ := p["name"].(string)
	typ, _ := p["type"].(string)
	server, _ := p["server"].(string)
	port := toInt(p["port"])
	if name == "" || !supported[typ] || server == "" || port <= 0 {
		return Node{}, false
	}
	// 机场节点不能带 dialer-proxy，否则会和我们的链路冲突
	delete(p, "dialer-proxy")
	return Node{Name: strings.TrimSpace(name), Type: typ, Server: server, Port: port, Country: DetectCountry(name), Raw: p}, true
}

func dedupe(in []Node) []Node {
	seen := map[string]int{}
	for i := range in {
		base := in[i].Name
		if seen[base] > 0 {
			in[i].Name = fmt.Sprintf("%s #%d", base, seen[base]+1)
			in[i].Raw["name"] = in[i].Name
		}
		seen[base]++
	}
	return in
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	}
	return 0
}

func decodeB64(s string) (string, bool) {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' {
			return -1
		}
		return r
	}, s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return string(b), true
		}
	}
	return "", false
}

// ---------------- 说明节点 ----------------

var infoNodeRe = regexp.MustCompile(`(?i)剩余|到期|过期|有效期|官网|网址|重置|订阅|群组|频道|客服|公告|更新|客户端|官方|请尽快|！！|!!!|traffic|expire|remaining|telegram`)

// 说明节点常用的占位服务器地址
var placeholderServers = map[string]bool{"127.0.0.1": true, "0.0.0.0": true, "1.1.1.1": true, "8.8.8.8": true, "localhost": true, "example.com": true}

func isInfoNode(n Node) bool {
	return infoNodeRe.MatchString(n.Name) || placeholderServers[strings.ToLower(n.Server)] || n.Port <= 1
}

var (
	remainRe = regexp.MustCompile(`剩余流量[:：\s]*([\d.]+)\s*([KMGT]i?B|[KMGT])`)
	expireRe = regexp.MustCompile(`(?:到期|过期)[^\d]*(\d{4})[-/.年](\d{1,2})[-/.月](\d{1,2})`)
)

// 没有 subscription-userinfo 时，从「剩余流量：312.5 GB」「套餐到期：2026-12-31」这类节点名里读。
func fillInfoFromName(info *Info, name string) {
	if m := remainRe.FindStringSubmatch(name); m != nil && info.Total == 0 {
		v, _ := strconv.ParseFloat(m[1], 64)
		mult := map[byte]float64{'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30, 'T': 1 << 40}[strings.ToUpper(m[2])[0]]
		info.Total = int64(v * mult) // 只知道剩余量：记成总量，已用为 0
	}
	if m := expireRe.FindStringSubmatch(name); m != nil && info.Expire == 0 {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		info.Expire = time.Date(y, time.Month(mo), d, 23, 59, 59, 0, time.FixedZone("CST", 8*3600)).Unix()
	}
}

// ---------------- 地区识别 ----------------

var countryWords = []struct {
	code  string
	words []string
}{
	{"HK", []string{"香港", "HK", "Hong Kong", "HongKong"}},
	{"TW", []string{"台湾", "台灣", "台北", "TW", "Taiwan"}},
	{"JP", []string{"日本", "东京", "東京", "大阪", "JP", "Japan", "Tokyo", "Osaka"}},
	{"SG", []string{"新加坡", "狮城", "SG", "Singapore"}},
	{"KR", []string{"韩国", "韓國", "首尔", "KR", "Korea", "Seoul"}},
	{"US", []string{"美国", "美國", "洛杉矶", "圣何塞", "西雅图", "纽约", "硅谷", "达拉斯", "芝加哥", "凤凰城", "波特兰", "迈阿密", "US", "USA", "United States", "America"}},
	{"GB", []string{"英国", "英國", "伦敦", "UK", "GB", "Britain", "United Kingdom", "London"}},
	{"DE", []string{"德国", "法兰克福", "DE", "Germany", "Frankfurt"}},
	{"FR", []string{"法国", "巴黎", "FR", "France", "Paris"}},
	{"NL", []string{"荷兰", "阿姆斯特丹", "NL", "Netherlands"}},
	{"CA", []string{"加拿大", "CA", "Canada"}},
	{"AU", []string{"澳大利亚", "澳洲", "悉尼", "AU", "Australia"}},
	{"RU", []string{"俄罗斯", "RU", "Russia"}},
	{"IN", []string{"印度", "IN", "India"}},
	{"TR", []string{"土耳其", "TR", "Turkey"}},
	{"BR", []string{"巴西", "BR", "Brazil"}},
	{"AR", []string{"阿根廷", "AR", "Argentina"}},
	{"MY", []string{"马来西亚", "MY", "Malaysia"}},
	{"TH", []string{"泰国", "TH", "Thailand"}},
	{"VN", []string{"越南", "VN", "Vietnam"}},
	{"PH", []string{"菲律宾", "PH", "Philippines"}},
	{"ID", []string{"印尼", "印度尼西亚", "Indonesia"}},
}

// DetectCountry 从节点名里认国家：先看国旗，再看关键词。认不出返回空串。
func DetectCountry(name string) string {
	rs := []rune(name)
	for i := 0; i+1 < len(rs); i++ {
		a, b := rs[i], rs[i+1]
		if a >= 0x1F1E6 && a <= 0x1F1FF && b >= 0x1F1E6 && b <= 0x1F1FF {
			cc := string([]rune{rune('A' + a - 0x1F1E6), rune('A' + b - 0x1F1E6)})
			if cc == "UK" {
				cc = "GB"
			}
			return cc
		}
	}
	// 取在名字里最先出现的那个，避免「日本 东京 香港中转」被认成香港
	best, bestAt := "", len(name)+1
	for _, c := range countryWords {
		for _, w := range c.words {
			var at int
			if isASCII(w) {
				at = wordIndex(name, w)
			} else {
				at = strings.Index(name, w)
			}
			if at >= 0 && at < bestAt {
				best, bestAt = c.code, at
			}
		}
	}
	return best
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// 英文缩写要求前后不是字母，避免 "BUS" 里认出 "US"
func wordIndex(name, w string) int {
	ln, lw := strings.ToLower(name), strings.ToLower(w)
	for i := 0; ; {
		j := strings.Index(ln[i:], lw)
		if j < 0 {
			return -1
		}
		j += i
		before := j == 0 || !isLetter(ln[j-1])
		after := j+len(lw) == len(ln) || !isLetter(ln[j+len(lw)])
		if before && after {
			return j
		}
		i = j + 1
	}
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' }
