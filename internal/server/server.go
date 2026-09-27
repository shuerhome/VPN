// Package server 提供面板的网页、JSON 接口和手机订阅地址。
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"luodi/internal/app"
	"luodi/internal/crypt"
	"luodi/internal/gen"
	"luodi/internal/relay"
	"luodi/internal/stats"
	"luodi/internal/store"
	"luodi/internal/sub"
)

const cookieName = "ld_session"

type Server struct {
	App       *app.App
	Password  string
	PublicURL string // 例如 https://panel.example.com，为空时按请求推断
	Assets    fs.FS
	Relay     *relay.Relay

	sessionKey []byte
	assets     *assetSet
	assetsOnce sync.Once
	running    atomic.Int32
	limiter    loginLimiter
}

func New(a *app.App, rl *relay.Relay, password, publicURL string, assets fs.FS) *Server {
	key := a.Store.Setting("session_key")
	if key == "" {
		key = crypt.RandomToken(32)
		_ = a.Store.SetSetting("session_key", key)
	}
	return &Server{App: a, Relay: rl, Password: password, PublicURL: strings.TrimRight(publicURL, "/"), Assets: assets, sessionKey: []byte(key)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sub/{token}", s.subscription)
	mux.HandleFunc("GET /d/{token}", s.share)
	mux.HandleFunc("POST /d/{token}/check", s.selfCheck)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)

	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.state)
	api.HandleFunc("POST /api/airports", s.addAirport)
	api.HandleFunc("POST /api/airports/{id}/sync", s.syncAirport)
	api.HandleFunc("POST /api/airports/{id}/probe", s.probeAirport)
	api.HandleFunc("PATCH /api/airports/{id}", s.updateAirport)
	api.HandleFunc("DELETE /api/airports/{id}", s.deleteAirport)
	api.HandleFunc("POST /api/nodes/test", s.testNodes)
	api.HandleFunc("POST /api/ips", s.addIPs)
	api.HandleFunc("PATCH /api/ips/{id}", s.updateIP)
	api.HandleFunc("DELETE /api/ips/{id}", s.deleteIP)
	api.HandleFunc("POST /api/ips/{id}/check", s.checkIP)
	api.HandleFunc("POST /api/ips/{id}/baseline", s.resetBaseline)
	api.HandleFunc("POST /api/chains", s.addChain)
	api.HandleFunc("PUT /api/chains/{id}", s.updateChain)
	api.HandleFunc("DELETE /api/chains/{id}", s.deleteChain)
	api.HandleFunc("POST /api/chains/{id}/check", s.checkChain)
	api.HandleFunc("POST /api/chains/{id}/token", s.rotateToken)
	api.HandleFunc("GET /api/chains/{id}/config", s.chainConfig)
	api.HandleFunc("POST /api/check-all", s.checkAll)
	api.HandleFunc("POST /api/telegram/test", s.telegramTest)
	api.HandleFunc("POST /api/telegram/unbind", s.telegramUnbind)
	api.HandleFunc("GET /api/backup", s.backupDownload)
	api.HandleFunc("POST /api/backup/telegram", s.backupTelegram)
	mux.Handle("/api/", s.requireAuth(api))

	mux.Handle("/", s.static())
	return securityHeaders(mux)
}

// ---------------- 认证 ----------------

func (s *Server) sign(exp int64) string {
	m := hmac.New(sha256.New, s.sessionKey)
	m.Write([]byte(strconv.FormatInt(exp, 10)))
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(m.Sum(nil))
}

func (s *Server) validSession(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	expStr, _, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(s.sign(exp)))
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.validSession(r) {
			writeErr(w, http.StatusUnauthorized, "请先登录")
			return
		}
		// 写操作要求自定义请求头，挡掉跨站表单提交
		if r.Method != http.MethodGet && r.Header.Get("X-Panel") != "1" {
			writeErr(w, http.StatusForbidden, "缺少请求头")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" || strings.HasPrefix(s.PublicURL, "https://")
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.limiter.allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "尝试次数太多，10 分钟后再试")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(s.Password)) != 1 {
		s.limiter.fail(ip)
		writeErr(w, http.StatusUnauthorized, "密码不对")
		return
	}
	s.limiter.reset(ip)
	exp := time.Now().Add(14 * 24 * time.Hour).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: s.sign(exp), Path: "/", HttpOnly: true, Secure: s.isHTTPS(r),
		SameSite: http.SameSiteStrictMode, Expires: time.Unix(exp, 0),
	})
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, map[string]bool{"ok": true})
}

type loginLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (l *loginLimiter) recent(ip string) []time.Time {
	cut := time.Now().Add(-10 * time.Minute)
	var keep []time.Time
	for _, t := range l.hits[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	return keep
}

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(ip)) < 8
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	l.hits[ip] = append(l.recent(ip), time.Now())
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, ip)
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("CF-Connecting-IP"); v != "" {
		return v
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// ---------------- 工具 ----------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return false
	}
	return true
}

func notFoundOr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "不存在")
		return
	}
	writeErr(w, http.StatusBadRequest, err.Error())
}

// background 在后台跑耗时任务，state 里的 running 用来让页面显示「检测中」。
func (s *Server) background(name string, fn func(ctx context.Context) error) {
	s.running.Add(1)
	go func() {
		defer s.running.Add(-1)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := fn(ctx); err != nil {
			log.Printf("%s: %v", name, err)
		}
	}()
}

func (s *Server) baseURL(r *http.Request) string {
	if s.PublicURL != "" {
		return s.PublicURL
	}
	scheme := "http"
	if s.isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ---------------- 状态 ----------------

type airportView struct {
	store.Airport
	Manual    bool   `json:"manual"` // 手动粘贴的配置
	URLMasked string `json:"url_masked"`
	Nodes     int    `json:"nodes"`
	Alive     int    `json:"alive"`
}

func maskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "••••"
	}
	out := u.Scheme + "://" + u.Host + u.Path
	if len(out) > 60 {
		out = out[:60] + "…"
	}
	if u.RawQuery != "" {
		out += "?••••"
	}
	return out
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	st := s.App.Store
	aps, err1 := st.Airports()
	nodes, err2 := st.Nodes()
	ips, err3 := st.IPs()
	chains, err4 := st.Chains()
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := make([]airportView, 0, len(aps))
	for _, a := range aps {
		v := airportView{Airport: a, URLMasked: maskURL(a.URL), Manual: a.Content != ""}
		for _, n := range nodes {
			if n.AirportID == a.ID {
				v.Nodes++
				if n.DelayMS > 0 {
					v.Alive++
				}
			}
		}
		views = append(views, v)
	}
	ipByID := map[int64]store.IP{}
	for _, ip := range ips {
		ipByID[ip.ID] = ip
	}
	type chainView struct {
		store.Chain
		Fronts    []int64       `json:"fronts"`
		SubURL    string        `json:"sub_url"`
		PageURL   string        `json:"page_url"`
		RelayLink string        `json:"relay_link"`
		Online    stats.Online  `json:"online"`
		Traffic   store.Traffic `json:"traffic"`
	}
	now := time.Now()
	traffic, _ := s.App.Store.TrafficSummary(now.Format("2006-01-02"), now.AddDate(0, 0, -29).Format("2006-01-02"))
	var online map[int64]stats.Online
	if s.App.Stats != nil {
		online = s.App.Stats.Snapshot()
	}
	cviews := make([]chainView, 0, len(chains))
	for _, c := range chains {
		v := chainView{Chain: c, SubURL: s.baseURL(r) + "/sub/" + c.Token, PageURL: s.baseURL(r) + "/d/" + c.Token,
			Online: online[c.ID], Traffic: traffic[c.ID]}
		if s.Relay.Enabled() {
			v.RelayLink, _ = s.Relay.Link(c, ipByID[c.IPID])
		}
		for _, n := range gen.FrontNodes(c, ipByID[c.IPID], nodes) {
			v.Fronts = append(v.Fronts, n.ID)
		}
		cviews = append(cviews, v)
	}
	if nodes == nil {
		nodes = []store.Node{}
	}
	if ips == nil {
		ips = []store.IP{}
	}
	writeJSON(w, map[string]any{
		"airports": views, "nodes": nodes, "ips": ips, "chains": cviews,
		"running": s.running.Load() > 0, "now": time.Now().Unix(),
		"telegram": map[string]any{
			"enabled": s.App.Notify.Enabled(), "bound": s.App.Notify.Enabled() && s.App.Notify.ChatID() != 0,
			"bind_code": s.telegramBindCode(), "bot": s.telegramBotName(),
			"last_backup": s.App.Store.Setting("backup:last"),
		},
		"relay": map[string]any{
			"enabled": s.Relay.Enabled(), "status": s.Relay.Status(),
			"host": s.Relay.PublicHost, "port": s.Relay.PublicPort, "sni": s.Relay.SNI,
		},
	})
}

// ---------------- 机场 ----------------

func (s *Server) addAirport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		Insecure *bool  `json:"insecure"`
		UA       string `json:"ua"`
		Content  string `json:"content"` // 手动粘贴的 Clash 配置或节点链接
	}
	if !decode(w, r, &body) {
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	body.Content = strings.TrimSpace(body.Content)
	name := strings.TrimSpace(body.Name)
	insecure := false
	if body.Content != "" {
		if _, err := sub.Parse([]byte(body.Content), ""); err != nil {
			writeErr(w, http.StatusBadRequest, "粘贴的内容里没有解析出节点：请粘贴 Clash 配置（含 proxies:）或节点链接")
			return
		}
		body.URL = ""
		if name == "" {
			name = "手动导入"
		}
	} else {
		u, err := url.Parse(body.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			writeErr(w, http.StatusBadRequest, "订阅链接应以 http:// 或 https:// 开头")
			return
		}
		insecure = sub.IsIPHost(body.URL)
		if body.Insecure != nil {
			insecure = *body.Insecure
		}
		if name == "" {
			name = u.Hostname()
		}
	}
	id, err := s.App.Store.AddAirportFull(name, body.URL, insecure, strings.TrimSpace(body.UA), body.Content)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := s.App.SyncAirport(ctx, id); err != nil {
		writeJSON(w, map[string]any{"id": id, "warning": "已保存，但同步失败：" + err.Error()})
		return
	}
	s.background("测速", s.App.TestNodes)
	writeJSON(w, map[string]any{"id": id})
}

// updateAirport 修改机场的客户端身份，或替换手动粘贴的配置，然后立即重新同步。
func (s *Server) updateAirport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "ID 不对")
		return
	}
	var body struct {
		UA      *string `json:"ua"`
		Content *string `json:"content"`
	}
	if !decode(w, r, &body) {
		return
	}
	if _, err := s.App.Store.Airport(id); err != nil {
		notFoundOr(w, err)
		return
	}
	if body.UA != nil {
		if err := s.App.Store.UpdateAirportUA(id, strings.TrimSpace(*body.UA)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.Content != nil {
		c := strings.TrimSpace(*body.Content)
		if _, err := sub.Parse([]byte(c), ""); err != nil {
			writeErr(w, http.StatusBadRequest, "粘贴的内容里没有解析出节点")
			return
		}
		if err := s.App.Store.UpdateAirportContent(id, c); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := s.App.SyncAirport(ctx, id); err != nil {
		writeErr(w, http.StatusBadRequest, "已保存，但同步失败："+err.Error())
		return
	}
	s.Relay.Reload()
	s.background("测速", s.App.TestNodes)
	writeJSON(w, map[string]bool{"ok": true})
}

// probeAirport 用多种客户端身份分别拉订阅，看机场对哪个身份给的节点最全。
func (s *Server) probeAirport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "ID 不对")
		return
	}
	ap, err := s.App.Store.Airport(id)
	if err != nil {
		notFoundOr(w, err)
		return
	}
	if ap.URL == "" {
		writeErr(w, http.StatusBadRequest, "这是手动粘贴的配置，没有订阅链接可以探测")
		return
	}
	var body struct {
		Extra string `json:"extra"` // 额外要试的身份（比如从官方客户端里看到的）
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
	uas := append([]string{}, sub.ProbeUAs...)
	if e := strings.TrimSpace(body.Extra); e != "" {
		uas = append([]string{e}, uas...)
	}
	if ap.UA != "" && !contains(uas, ap.UA) {
		uas = append([]string{ap.UA}, uas...)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	writeJSON(w, map[string]any{"current": ap.UA, "results": sub.Probe(ctx, ap.URL, ap.Insecure, uas)})
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Server) syncAirport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "ID 不对")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := s.App.SyncAirport(ctx, id); err != nil {
		notFoundOr(w, err)
		return
	}
	s.background("测速", s.App.TestNodes)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) deleteAirport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "ID 不对")
		return
	}
	if err := s.App.Store.DeleteAirport(id); err != nil {
		notFoundOr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) testNodes(w http.ResponseWriter, r *http.Request) {
	s.background("测速", s.App.TestNodes)
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------------- 住宅IP ----------------

func validProto(p string) bool { return p == "socks5" || p == "http" || p == "https" }

func (s *Server) addIPs(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Records []struct {
			Protocol string `json:"protocol"`
			Host     string `json:"host"`
			Port     int    `json:"port"`
			Username string `json:"username"`
			Password string `json:"password"`
			Remark   string `json:"remark"`
			Expire   string `json:"expire"`
		} `json:"records"`
	}
	if !decode(w, r, &body) {
		return
	}
	var added []int64
	dup := 0
	for _, rec := range body.Records {
		host := strings.TrimSpace(rec.Host)
		if !validProto(rec.Protocol) || host == "" || rec.Port < 1 || rec.Port > 65535 {
			writeErr(w, http.StatusBadRequest, "有记录的协议、地址或端口不对："+host)
			return
		}
		if rec.Expire != "" {
			if _, err := time.Parse("2006-01-02", rec.Expire); err != nil {
				rec.Expire = ""
			}
		}
		id, err := s.App.Store.AddIP(store.IP{Protocol: rec.Protocol, Host: host, Port: rec.Port, Username: rec.Username, Password: rec.Password, Remark: rec.Remark, Expire: rec.Expire})
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if id == 0 {
			dup++
			continue
		}
		added = append(added, id)
	}
	if len(added) > 0 {
		ids := added
		s.background("检测住宅IP", func(ctx context.Context) error { return s.App.CheckIPs(ctx, ids...) })
	}
	writeJSON(w, map[string]any{"added": added, "duplicates": dup})
}

func (s *Server) updateIP(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "ID 不对")
		return
	}
	var body struct {
		Remark string `json:"remark"`
		Expire string `json:"expire"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Expire != "" {
		if _, err := time.Parse("2006-01-02", body.Expire); err != nil {
			writeErr(w, http.StatusBadRequest, "到期日格式应为 2026-10-31")
			return
		}
	}
	if err := s.App.Store.UpdateIPMeta(id, body.Remark, body.Expire); err != nil {
		notFoundOr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) deleteIP(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "ID 不对")
		return
	}
	if err := s.App.Store.DeleteIP(id); err != nil {
		notFoundOr(w, err)
		return
	}
	s.Relay.Reload()
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) checkIP(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "ID 不对")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if err := s.App.CheckIPs(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ip, err := s.App.Store.IP(id)
	if err != nil {
		notFoundOr(w, err)
		return
	}
	writeJSON(w, ip)
}

func (s *Server) resetBaseline(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "ID 不对")
		return
	}
	if err := s.App.Store.ResetBaseline(id); err != nil {
		notFoundOr(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------------- 链路 ----------------

type chainBody struct {
	Device      string          `json:"device"`
	Accounts    []store.Account `json:"accounts"`
	IPID        int64           `json:"ip_id"`
	FrontMode   string          `json:"front_mode"`
	FrontNodeID int64           `json:"front_node_id"`
	Route       string          `json:"route"`
}

func (b *chainBody) validate() string {
	b.Device = strings.TrimSpace(b.Device)
	if b.Device == "" {
		return "设备名不能为空"
	}
	switch b.FrontMode {
	case "fixed":
		if b.FrontNodeID == 0 {
			return "固定前置需要选一个节点"
		}
	case "auto":
		b.FrontNodeID = 0
	default:
		b.FrontMode, b.FrontNodeID = "fastest", 0
	}
	// 只有全局模式：住宅IP不限流量，所有流量都从住宅IP出去最安全
	b.Route = "global"
	var acc []store.Account
	for _, a := range b.Accounts {
		if strings.TrimSpace(a.Handle) != "" {
			acc = append(acc, store.Account{Platform: strings.TrimSpace(a.Platform), Handle: strings.TrimSpace(a.Handle)})
		}
	}
	b.Accounts = acc
	return ""
}

func (s *Server) addChain(w http.ResponseWriter, r *http.Request) {
	var body chainBody
	if !decode(w, r, &body) {
		return
	}
	if msg := body.validate(); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if _, err := s.App.Store.IP(body.IPID); err != nil {
		writeErr(w, http.StatusBadRequest, "住宅IP不存在")
		return
	}
	id, err := s.App.Store.AddChain(store.Chain{Device: body.Device, Accounts: body.Accounts, IPID: body.IPID, FrontMode: body.FrontMode, FrontNodeID: body.FrontNodeID, Route: body.Route})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Relay.Reload()
	s.background("检测新链路", func(ctx context.Context) error { return s.App.CheckChains(ctx, id) })
	writeJSON(w, map[string]int64{"id": id})
}

func (s *Server) updateChain(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "ID 不对")
		return
	}
	var body chainBody
	if !decode(w, r, &body) {
		return
	}
	if msg := body.validate(); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	c, err := s.App.Store.Chain(id)
	if err != nil {
		notFoundOr(w, err)
		return
	}
	c.Device, c.Accounts, c.FrontMode, c.FrontNodeID, c.Route = body.Device, body.Accounts, body.FrontMode, body.FrontNodeID, body.Route
	if err := s.App.Store.UpdateChain(*c); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Relay.Reload()
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) deleteChain(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "ID 不对")
		return
	}
	if err := s.App.Store.DeleteChain(id); err != nil {
		notFoundOr(w, err)
		return
	}
	s.Relay.Reload()
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) checkChain(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "ID 不对")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if err := s.App.CheckChains(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	c, err := s.App.Store.Chain(id)
	if err != nil {
		notFoundOr(w, err)
		return
	}
	writeJSON(w, c)
}

func (s *Server) rotateToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "ID 不对")
		return
	}
	if err := s.App.Store.RotateToken(id); err != nil {
		notFoundOr(w, err)
		return
	}
	s.Relay.Reload()
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) checkAll(w http.ResponseWriter, r *http.Request) {
	s.background("检测全部链路", func(ctx context.Context) error { return s.App.CheckChains(ctx) })
	writeJSON(w, map[string]bool{"ok": true})
}

// chainConfig 返回面板里预览用的配置（含明文密码，只给已登录的管理员）。
func (s *Server) chainConfig(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "ID 不对")
		return
	}
	c, err := s.App.Store.Chain(id)
	if err != nil {
		notFoundOr(w, err)
		return
	}
	ip, err := s.App.Store.IP(c.IPID)
	if err != nil {
		notFoundOr(w, err)
		return
	}
	nodes, err := s.App.Store.Nodes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg, cfgErr := gen.ClientConfig(*c, *ip, nodes)
	out := map[string]string{"clash": cfg, "rocket": gen.RocketLink(*ip)}
	if s.Relay.Enabled() {
		out["relay"], _ = s.Relay.Link(*c, *ip)
	}
	if cfgErr != nil {
		out["error"] = cfgErr.Error()
	}
	writeJSON(w, out)
}

// ---------------- 手机订阅 ----------------

func (s *Server) subscription(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	c, err := s.App.Store.ChainByToken(token)
	if err != nil || len(token) < 16 {
		http.Error(w, "订阅不存在或已被吊销", http.StatusNotFound)
		return
	}
	ip, err := s.App.Store.IP(c.IPID)
	if err != nil {
		http.Error(w, "住宅IP不存在", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	// 没指定 target 时按客户端自动选：小火箭等拿中转节点，Stash / Clash 拿机场链路配置，浏览器打开导入页
	target := r.URL.Query().Get("target")
	if target == "" {
		switch clientKind(r.UserAgent()) {
		case "page":
			http.Redirect(w, r, "/d/"+token, http.StatusFound)
			return
		case "clash":
			target = "clash"
		default:
			target = "relay"
		}
	}
	if target == "relay" && !s.Relay.Enabled() {
		target = "clash"
	}
	if target == "relay" {
		link, err := s.Relay.Link(*c, *ip)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		// 小火箭订阅格式：Base64 编码的节点链接列表
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString([]byte(link + "\n"))))
		return
	}
	if target == "rocket" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(gen.RocketLink(*ip) + "\n"))
		return
	}
	nodes, err := s.App.Store.Nodes()
	if err != nil {
		http.Error(w, "读取节点失败", http.StatusInternalServerError)
		return
	}
	cfg, err := gen.ClientConfig(*c, *ip, nodes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(c.Device)+".yaml")
	w.Header().Set("profile-update-interval", "6")
	_, _ = w.Write([]byte(cfg))
}

// ---------------- 静态文件 ----------------

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; img-src 'self' data:; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// ---------------- Telegram 与备份 ----------------

func (s *Server) telegramBindCode() string {
	if !s.App.Notify.Enabled() || s.App.Notify.ChatID() != 0 {
		return ""
	}
	return s.App.Notify.BindCode()
}

func (s *Server) telegramBotName() string {
	if !s.App.Notify.Enabled() {
		return ""
	}
	return s.App.Notify.BotName()
}

func (s *Server) telegramTest(w http.ResponseWriter, r *http.Request) {
	if !s.App.Notify.Enabled() {
		writeErr(w, http.StatusBadRequest, "还没配置机器人：在 .env 里填 TG_BOT_TOKEN 后重启面板")
		return
	}
	if err := s.App.Notify.Send(r.Context(), "👋 测试消息：落地链路台通知正常。"); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) telegramUnbind(w http.ResponseWriter, r *http.Request) {
	if err := s.App.Notify.Unbind(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) backupDownload(w http.ResponseWriter, r *http.Request) {
	path, err := s.App.Backup()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer os.Remove(path)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(path))
	http.ServeFile(w, r, path)
}

func (s *Server) backupTelegram(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.App.BackupToTelegram(ctx); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
