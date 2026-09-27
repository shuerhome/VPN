// Package relay 是「中转模式」：VPS 上常驻一个 mihomo，对外开一个 VLESS Reality 入站。
// 每条链路是其中一个用户，按用户把流量送到它绑定的住宅IP。
// 手机只需扫码导入一个普通 VLESS 节点，小火箭、Loon 等任何客户端都能用。
package relay

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"luodi/internal/crypt"
	"luodi/internal/gen"
	"luodi/internal/store"

	"gopkg.in/yaml.v3"
)

type Relay struct {
	Bin        string
	Dir        string
	PublicHost string // 手机连接的地址（VPS 公网 IP 或解析到它的域名，不能走 Cloudflare 代理）
	PublicPort int    // 手机连接的端口
	ListenPort int    // 容器内监听端口
	SNI        string // Reality 伪装的网站
	Dest       string // Reality 回落目标，默认 SNI:443
	LogLevel   string // 默认 warning
	Store      *store.Store
	Meter      Meter // 每台手机的流量计量转发

	reload   chan struct{}
	initOnce sync.Once
	mu       sync.Mutex
	status   string
	ctlPort  int    // 运行中的中转进程的控制端口
	secret   string // 控制接口密钥
}

func (r *Relay) Enabled() bool { return r.PublicHost != "" && r.PublicPort > 0 }

func (r *Relay) Status() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *Relay) setStatus(s string) {
	r.mu.Lock()
	r.status = s
	r.mu.Unlock()
}

// Keys 返回 Reality 密钥，第一次调用时生成并保存。
func (r *Relay) Keys() (priv, pub, shortID string, err error) {
	priv = r.Store.Setting("reality_private")
	shortID = r.Store.Setting("reality_short_id")
	if priv == "" {
		k, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return "", "", "", err
		}
		priv = base64.RawURLEncoding.EncodeToString(k.Bytes())
		sid := make([]byte, 8)
		_, _ = rand.Read(sid)
		shortID = hex.EncodeToString(sid)
		if err := r.Store.SetSetting("reality_private", priv); err != nil {
			return "", "", "", err
		}
		if err := r.Store.SetSetting("reality_short_id", shortID); err != nil {
			return "", "", "", err
		}
	}
	raw, err := base64.RawURLEncoding.DecodeString(priv)
	if err != nil {
		return "", "", "", err
	}
	k, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", "", "", err
	}
	pub = base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes())
	return priv, pub, shortID, nil
}

func (r *Relay) dest() string {
	if r.Dest != "" {
		return r.Dest
	}
	return r.SNI + ":443"
}

func userName(c store.Chain) string { return fmt.Sprintf("c%d", c.ID) }

// Link 生成手机扫码用的 vless:// 链接。
func (r *Relay) Link(c store.Chain, ip store.IP) (string, error) {
	if !r.Enabled() {
		return "", errors.New("中转没有开启：在 .env 里设置 RELAY_HOST")
	}
	_, pub, sid, err := r.Keys()
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("encryption", "none")
	q.Set("flow", "xtls-rprx-vision")
	q.Set("security", "reality")
	q.Set("sni", r.SNI)
	q.Set("fp", "chrome")
	q.Set("pbk", pub)
	q.Set("sid", sid)
	q.Set("type", "tcp")
	host := net.JoinHostPort(r.PublicHost, strconv.Itoa(r.PublicPort))
	name := c.Device + " " + strings.TrimPrefix(gen.LandName(ip), "落地·")
	return "vless://" + c.RelayUUID + "@" + host + "?" + q.Encode() + "#" + url.PathEscape(name), nil
}

// ClashProxy 是同一个中转节点的 Clash Meta 写法（Stash 也能用中转）。
func (r *Relay) ClashProxy(c store.Chain, name string) (gen.OM, error) {
	_, pub, sid, err := r.Keys()
	if err != nil {
		return nil, err
	}
	return gen.O(
		"name", name, "type", "vless", "server", r.PublicHost, "port", r.PublicPort,
		"uuid", c.RelayUUID, "flow", "xtls-rprx-vision", "network", "tcp", "tls", true, "udp", true,
		"servername", r.SNI, "client-fingerprint", "chrome",
		"reality-opts", gen.O("public-key", pub, "short-id", sid),
	), nil
}

// buildConfig 生成中转进程的配置。没有链路时返回 nil。
func (r *Relay) buildConfig(ctlPort int, secret string) ([]byte, error) {
	chains, err := r.Store.Chains()
	if err != nil {
		return nil, err
	}
	priv, _, sid, err := r.Keys()
	if err != nil {
		return nil, err
	}
	var users, proxies []any
	var rules []string
	keep := map[int64]bool{}
	defer r.Meter.Retain(keep)
	for _, c := range chains {
		ip, err := r.Store.IP(c.IPID)
		if err != nil || c.RelayUUID == "" {
			continue
		}
		land := "L-" + userName(c)
		users = append(users, gen.O("username", userName(c), "uuid", c.RelayUUID, "flow", "xtls-rprx-vision"))
		// 中转服务器本身在境外，直接连住宅IP；域名交给住宅IP那边解析，不在 VPS 上解析。
		// SOCKS5 / HTTP 住宅IP 经本机计量端口转发，用来精确统计每台手机的流量；
		// HTTPS 住宅IP 要校验证书里的域名，不能改地址，直接连。
		target := *ip
		if ip.Protocol != "https" {
			port, err := r.Meter.Ensure(c.ID, net.JoinHostPort(ip.Host, strconv.Itoa(ip.Port)))
			if err != nil {
				return nil, err
			}
			target.Host, target.Port = "127.0.0.1", port
		}
		keep[c.ID] = true
		proxies = append(proxies, gen.LandProxy(target, land, ""))
		rules = append(rules, "IN-USER,"+userName(c)+","+land)
	}
	if len(users) == 0 {
		return nil, nil
	}
	// Reality 握手时中转自己要连伪装站点，这条内部连接也会走规则，必须放行
	if host, _, err := net.SplitHostPort(r.dest()); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			rules = append(rules, fmt.Sprintf("IP-CIDR,%s/%d,DIRECT,no-resolve", host, bits))
		} else {
			rules = append(rules, "DOMAIN,"+host+",DIRECT")
		}
	}
	rules = append(rules, "MATCH,REJECT") // 其余一律拒绝，绝不从 VPS 自己的 IP 出去
	level := r.LogLevel
	if level == "" {
		level = "warning"
	}
	doc := gen.O(
		"log-level", level,
		"ipv6", false,
		"mode", "rule",
		"allow-lan", false,
		"external-controller", fmt.Sprintf("127.0.0.1:%d", ctlPort),
		"secret", secret,
		"profile", gen.O("store-selected", false, "store-fake-ip", false),
		"listeners", []any{gen.O(
			"name", "relay", "type", "vless", "listen", "0.0.0.0", "port", r.ListenPort,
			"users", users,
			"reality-config", gen.O("dest", r.dest(), "private-key", priv, "short-id", []string{sid}, "server-names", []string{r.SNI}),
		)},
		"proxies", proxies,
		"rules", rules,
	)
	return yaml.Marshal(doc)
}

func (r *Relay) reloadCh() chan struct{} {
	r.initOnce.Do(func() { r.reload = make(chan struct{}, 1) })
	return r.reload
}

// Reload 在链路或住宅IP变化后调用，重新生成配置。
func (r *Relay) Reload() {
	select {
	case r.reloadCh() <- struct{}{}:
	default:
	}
}

// Run 常驻运行中转进程：配置变了就热加载，进程意外退出就重启。
func (r *Relay) Run(ctx context.Context) {
	if !r.Enabled() {
		r.setStatus("未开启")
		return
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		r.setStatus("出错：" + err.Error())
		return
	}
	cfgPath := filepath.Join(r.Dir, "config.yaml")
	secret := crypt.RandomToken(16)
	ctlPort := 0
	setCtl := func(port int) {
		r.mu.Lock()
		r.ctlPort, r.secret = port, secret
		r.mu.Unlock()
	}

	var cmd *exec.Cmd
	var exited chan struct{}
	var logBuf bytes.Buffer
	var current []byte
	stop := func() {
		if cmd != nil {
			_ = cmd.Process.Kill()
			<-exited
			cmd = nil
		}
	}
	defer stop()

	apply := func() {
		if ctlPort == 0 {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err == nil {
				ctlPort = l.Addr().(*net.TCPAddr).Port
				l.Close()
			}
		}
		cfg, err := r.buildConfig(ctlPort, secret)
		if err != nil {
			r.setStatus("出错：" + err.Error())
			return
		}
		if cfg == nil {
			stop()
			setCtl(0)
			current = nil
			r.setStatus("等待第一条链路")
			return
		}
		if cmd != nil && bytes.Equal(cfg, current) {
			return
		}
		if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
			r.setStatus("出错：" + err.Error())
			return
		}
		if cmd != nil && hotReload(ctlPort, secret, cfgPath) == nil {
			current = cfg
			r.setStatus("运行中")
			return
		}
		stop()
		logBuf.Reset()
		cmd = exec.Command(r.Bin, "-d", r.Dir, "-f", cfgPath)
		out := io.MultiWriter(&logBuf, os.Stderr) // 警告级别日志也进容器日志
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			cmd = nil
			r.setStatus("启动失败：" + err.Error())
			return
		}
		exited = make(chan struct{})
		go func(c *exec.Cmd, done chan struct{}) { _ = c.Wait(); close(done) }(cmd, exited)
		current = cfg
		setCtl(ctlPort)
		r.setStatus("运行中")
		log.Printf("中转已启动，端口 %d", r.ListenPort)
	}

	apply()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		var done chan struct{}
		if cmd != nil {
			done = exited
		}
		select {
		case <-ctx.Done():
			return
		case <-r.reloadCh():
			apply()
		case <-tick.C:
			apply() // 兜底：住宅IP改了密码等情况
		case <-done:
			msg := strings.TrimSpace(logBuf.String())
			if i := strings.LastIndex(msg, "\n"); i >= 0 {
				msg = msg[i+1:]
			}
			setCtl(0)
			log.Printf("中转进程退出，3 秒后重启：%s", msg)
			r.setStatus("重启中：" + msg)
			cmd = nil
			current = nil
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			apply()
		}
	}
}

func hotReload(port int, secret, path string) error {
	body := strings.NewReader(fmt.Sprintf(`{"path":%q}`, path))
	req, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/configs?force=true", port), body)
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("热加载返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

// Conn 是中转上的一条活动连接。
type Conn struct {
	ID       string
	Chain    int64 // 属于哪条链路（入站用户 cN），0 表示不属于设备
	SourceIP string
	Host     string
	Upload   int64
	Download int64
	Start    time.Time
}

// Snapshot 是中转当前的连接和进程启动以来的累计流量。
type Snapshot struct {
	UploadTotal   int64
	DownloadTotal int64
	Conns         []Conn
}

// Connections 读取中转当前的全部连接；中转没运行时返回 nil。
func (r *Relay) Connections(ctx context.Context) (*Snapshot, error) {
	r.mu.Lock()
	port, secret := r.ctlPort, r.secret
	r.mu.Unlock()
	if port == 0 {
		return nil, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/connections", port), nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("连接列表返回 HTTP %d", resp.StatusCode)
	}
	var body struct {
		UploadTotal   int64 `json:"uploadTotal"`
		DownloadTotal int64 `json:"downloadTotal"`
		Connections   []struct {
			ID       string    `json:"id"`
			Upload   int64     `json:"upload"`
			Download int64     `json:"download"`
			Start    time.Time `json:"start"`
			Metadata struct {
				SourceIP    string `json:"sourceIP"`
				Host        string `json:"host"`
				DestIP      string `json:"destinationIP"`
				InboundUser string `json:"inboundUser"`
			} `json:"metadata"`
		} `json:"connections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := &Snapshot{UploadTotal: body.UploadTotal, DownloadTotal: body.DownloadTotal, Conns: make([]Conn, 0, len(body.Connections))}
	for _, c := range body.Connections {
		id, _ := chainIDFromUser(c.Metadata.InboundUser) // 0 表示不属于任何设备（如伪装站点握手）
		host := c.Metadata.Host
		if host == "" {
			host = c.Metadata.DestIP
		}
		out.Conns = append(out.Conns, Conn{ID: c.ID, Chain: id, SourceIP: c.Metadata.SourceIP, Host: host, Upload: c.Upload, Download: c.Download, Start: c.Start})
	}
	return out, nil
}

func chainIDFromUser(u string) (int64, bool) {
	if !strings.HasPrefix(u, "c") {
		return 0, false
	}
	id, err := strconv.ParseInt(u[1:], 10, 64)
	return id, err == nil && id > 0
}
