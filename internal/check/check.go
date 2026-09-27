// Package check 在服务器上启动一个临时的 mihomo 进程，复现手机上的链路，测节点延迟和出口IP。
//
// 每条链路在本机开一个 mixed 监听端口，入站流量直接交给「落地」节点，落地经前置组拨号，
// 和手机上的走法完全一样。检测完进程就退出。
package check

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"luodi/internal/crypt"
	"luodi/internal/gen"
	"luodi/internal/store"

	"gopkg.in/yaml.v3"
)

const delayURL = "https://www.gstatic.com/generate_204"

type Runner struct {
	Bin       string   // mihomo 可执行文件
	WorkDir   string   // 临时配置目录的父目录
	CheckURLs []string // 返回纯文本出口IP的接口，依次尝试

	mu sync.Mutex // 同一时间只跑一轮
}

// ChainJob 是一条要检测的链路。Fronts 为空表示从服务器直连住宅IP（单测IP本身）。
type ChainJob struct {
	Key    string
	IP     store.IP
	Fronts []store.Node
}

type ChainResult struct {
	Exit  string
	MS    int
	Error string
	Front string // 实际用到的前置节点名
}

type Results struct {
	NodeDelay map[int64]int // -1 表示超时
	Chains    map[string]ChainResult
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func nodeKey(id int64) string { return fmt.Sprintf("n%d", id) }

// Run 跑一轮：先测 nodes 的延迟，再逐条测 chains 的出口IP。
func (r *Runner) Run(ctx context.Context, nodes []store.Node, chains []ChainJob) (*Results, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	res := &Results{NodeDelay: map[int64]int{}, Chains: map[string]ChainResult{}}
	if len(nodes) == 0 && len(chains) == 0 {
		return res, nil
	}

	// 所有用到的节点都要写进配置
	all := map[int64]store.Node{}
	for _, n := range nodes {
		all[n.ID] = n
	}
	for _, c := range chains {
		for _, n := range c.Fronts {
			all[n.ID] = n
		}
	}

	ctlPort, err := freePort()
	if err != nil {
		return nil, err
	}
	secret := crypt.RandomToken(16)
	var proxies, groups, listeners []any
	for _, n := range all {
		raw := map[string]any{}
		for k, v := range n.Raw {
			raw[k] = v
		}
		proxies = append(proxies, orderedWithName(raw, nodeKey(n.ID)))
	}
	ports := map[string]int{}
	for _, c := range chains {
		dialer := ""
		if len(c.Fronts) > 0 {
			dialer = "F-" + c.Key
			var names []string
			for _, n := range c.Fronts {
				names = append(names, nodeKey(n.ID))
			}
			groups = append(groups, gen.O("name", dialer, "type", "fallback", "url", delayURL, "interval", 600, "proxies", names))
		}
		proxies = append(proxies, gen.LandProxy(c.IP, "L-"+c.Key, dialer))
		p, err := freePort()
		if err != nil {
			return nil, err
		}
		ports[c.Key] = p
		listeners = append(listeners, gen.O("name", "in-"+c.Key, "type", "mixed", "listen", "127.0.0.1", "port", p, "proxy", "L-"+c.Key))
	}
	doc := gen.O(
		"log-level", "warning",
		"ipv6", false,
		"allow-lan", false,
		"mode", "rule",
		"external-controller", fmt.Sprintf("127.0.0.1:%d", ctlPort),
		"secret", secret,
		"profile", gen.O("store-selected", false, "store-fake-ip", false),
		"proxies", proxies,
	)
	if len(groups) > 0 {
		doc = append(doc, gen.KV{K: "proxy-groups", V: groups})
	}
	if len(listeners) > 0 {
		doc = append(doc, gen.KV{K: "listeners", V: listeners})
	}
	doc = append(doc, gen.KV{K: "rules", V: []string{"MATCH,DIRECT"}})
	cfg, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp(r.WorkDir, "check-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var logBuf bytes.Buffer
	cmd := exec.CommandContext(runCtx, r.Bin, "-d", dir, "-f", cfgPath)
	cmd.Stdout = &logBuf
	cmd.Stderr = &logBuf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 mihomo 失败：%w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	defer func() {
		cancel()
		<-exited
	}()

	api := &controller{base: fmt.Sprintf("http://127.0.0.1:%d", ctlPort), secret: secret}
	if err := api.waitReady(runCtx, exited); err != nil {
		return nil, fmt.Errorf("%v：%s", err, lastLines(logBuf.String(), 5))
	}

	// 1) 节点延迟（顺带让 fallback 组知道哪些节点是活的）
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, 16)
	for id := range all {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d := api.delay(runCtx, nodeKey(id))
			mu.Lock()
			res.NodeDelay[id] = d
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	// 2) 链路出口
	names := map[string]string{}
	for id, n := range all {
		names[nodeKey(id)] = n.Name
	}
	sem = make(chan struct{}, 8)
	for _, c := range chains {
		wg.Add(1)
		go func(c ChainJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cr := r.exitIP(runCtx, ports[c.Key])
			if len(c.Fronts) > 0 {
				if now := api.groupNow(runCtx, "F-"+c.Key); now != "" {
					cr.Front = names[now]
				}
				alive := false
				for _, n := range c.Fronts {
					if res.NodeDelay[n.ID] > 0 {
						alive = true
					}
				}
				if cr.Error != "" && !alive {
					cr.Error = "前置节点全部不可用（" + cr.Error + "）"
				}
			}
			mu.Lock()
			res.Chains[c.Key] = cr
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	return res, nil
}

func orderedWithName(raw map[string]any, name string) gen.OM {
	delete(raw, "dialer-proxy")
	o := gen.O("name", name)
	for k, v := range raw {
		if k != "name" {
			o = append(o, gen.KV{K: k, V: v})
		}
	}
	return o
}

func (r *Runner) exitIP(ctx context.Context, port int) ChainResult {
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}
	client := &http.Client{Transport: tr, Timeout: 20 * time.Second}
	var lastErr string
	for _, u := range r.CheckURLs {
		start := time.Now()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		req.Header.Set("User-Agent", "curl/8.5.0")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = simplify(err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		ip := strings.TrimSpace(string(body))
		if resp.StatusCode == http.StatusOK && net.ParseIP(ip) != nil {
			return ChainResult{Exit: ip, MS: int(time.Since(start).Milliseconds())}
		}
		if resp.StatusCode == http.StatusBadGateway {
			lastErr = "链路不通：住宅IP账号密码错误，或住宅IP / 前置节点连不上"
		} else {
			lastErr = fmt.Sprintf("检测接口返回 HTTP %d", resp.StatusCode)
		}
	}
	return ChainResult{Error: orDefault(lastErr, "没有可用的检测接口")}
}

func simplify(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "Client.Timeout"), strings.Contains(s, "deadline exceeded"):
		return "连接超时"
	case strings.Contains(s, "authentication"), strings.Contains(s, "auth"):
		return "住宅IP 账号或密码错误"
	case strings.Contains(s, "connection refused"):
		return "连接被拒绝"
	}
	if i := strings.LastIndex(s, ": "); i >= 0 && len(s)-i < 120 {
		return s[i+2:]
	}
	return s
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// ---------------- mihomo 控制接口 ----------------

type controller struct{ base, secret string }

func (c *controller) get(ctx context.Context, path string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *controller) waitReady(ctx context.Context, exited <-chan struct{}) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return errors.New("mihomo 启动后立即退出，配置可能有误")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		if c.get(ctx, "/version", nil) == nil {
			return nil
		}
	}
	return errors.New("mihomo 启动超时")
}

func (c *controller) delay(ctx context.Context, name string) int {
	var out struct {
		Delay int `json:"delay"`
	}
	q := url.Values{"url": {delayURL}, "timeout": {"6000"}}
	if err := c.get(ctx, "/proxies/"+url.PathEscape(name)+"/delay?"+q.Encode(), &out); err != nil || out.Delay <= 0 {
		return -1
	}
	return out.Delay
}

func (c *controller) groupNow(ctx context.Context, name string) string {
	var out struct {
		Now string `json:"now"`
	}
	if err := c.get(ctx, "/proxies/"+url.PathEscape(name), &out); err != nil {
		return ""
	}
	return out.Now
}
