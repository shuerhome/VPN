// Package store 是面板的 SQLite 存储。敏感字段在写入前加密。
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"luodi/internal/crypt"

	_ "modernc.org/sqlite"
)

type Airport struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"-"`
	Insecure  bool   `json:"insecure"`
	Upload    int64  `json:"upload"`
	Download  int64  `json:"download"`
	Total     int64  `json:"total"`
	Expire    int64  `json:"expire"`
	SyncedAt  int64  `json:"synced_at"`
	LastError string `json:"last_error"`
}

type Node struct {
	ID        int64          `json:"id"`
	AirportID int64          `json:"airport_id"`
	Name      string         `json:"name"`
	Type      string         `json:"type"`
	Server    string         `json:"server"`
	Port      int            `json:"port"`
	Country   string         `json:"country"`
	Raw       map[string]any `json:"-"`
	DelayMS   int            `json:"delay_ms"` // 0 未测，-1 超时
	TestedAt  int64          `json:"tested_at"`
}

type IP struct {
	ID          int64  `json:"id"`
	Protocol    string `json:"protocol"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	Password    string `json:"-"`
	Remark      string `json:"remark"`
	Expire      string `json:"expire"`
	BaselineIP  string `json:"baseline_ip"` // 第一次检测到的出口IP，之后以它为准
	LastExit    string `json:"last_exit"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	City        string `json:"city"`
	ISP         string `json:"isp"`
	ASN         string `json:"asn"`
	Hosting     bool   `json:"hosting"`
	ProxyFlag   bool   `json:"proxy_flag"`
	Mobile      bool   `json:"mobile"`
	Timezone    string `json:"timezone"`
	CheckedAt   int64  `json:"checked_at"`
	CheckMS     int    `json:"check_ms"`
	CheckError  string `json:"check_error"`
	CreatedAt   int64  `json:"created_at"`
}

// ExpectedExit 是判断「出口一致」用的IP：有基准用基准，否则用地址本身。
func (ip *IP) ExpectedExit() string {
	if ip.BaselineIP != "" {
		return ip.BaselineIP
	}
	return ip.Host
}

type Account struct {
	Platform string `json:"p"`
	Handle   string `json:"h"`
}

type Chain struct {
	ID          int64     `json:"id"`
	Device      string    `json:"device"`
	Accounts    []Account `json:"accounts"`
	IPID        int64     `json:"ip_id"`
	FrontMode   string    `json:"front_mode"` // auto | fixed
	FrontNodeID int64     `json:"front_node_id"`
	Route       string    `json:"route"` // global | split
	Token       string    `json:"token"`
	CreatedAt   int64     `json:"created_at"`
	CheckAt     int64     `json:"check_at"`
	CheckExit   string    `json:"check_exit"`
	CheckMS     int       `json:"check_ms"`
	CheckError  string    `json:"check_error"`
	CheckFront  string    `json:"check_front"`
	RelayUUID   string    `json:"-"`
	LastSeen    int64     `json:"last_seen"` // 最近一次通过中转上网的时间
	LastSrc     string    `json:"last_src"`  // 最近一次连接中转的来源IP
}

type Store struct {
	db  *sql.DB
	box *crypt.Box
}

const schema = `
CREATE TABLE IF NOT EXISTS airports (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL, url_enc TEXT NOT NULL, insecure INTEGER NOT NULL DEFAULT 0,
  upload INTEGER NOT NULL DEFAULT 0, download INTEGER NOT NULL DEFAULT 0, total INTEGER NOT NULL DEFAULT 0, expire INTEGER NOT NULL DEFAULT 0,
  synced_at INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS nodes (
  id INTEGER PRIMARY KEY, airport_id INTEGER NOT NULL, name TEXT NOT NULL, type TEXT NOT NULL, server TEXT NOT NULL, port INTEGER NOT NULL,
  country TEXT NOT NULL DEFAULT '', raw_enc TEXT NOT NULL, delay_ms INTEGER NOT NULL DEFAULT 0, tested_at INTEGER NOT NULL DEFAULT 0,
  UNIQUE(airport_id, name)
);
CREATE TABLE IF NOT EXISTS ips (
  id INTEGER PRIMARY KEY, protocol TEXT NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL, username TEXT NOT NULL DEFAULT '',
  password_enc TEXT NOT NULL DEFAULT '', remark TEXT NOT NULL DEFAULT '', expire TEXT NOT NULL DEFAULT '',
  baseline_ip TEXT NOT NULL DEFAULT '', last_exit TEXT NOT NULL DEFAULT '',
  country TEXT NOT NULL DEFAULT '', country_code TEXT NOT NULL DEFAULT '', city TEXT NOT NULL DEFAULT '', isp TEXT NOT NULL DEFAULT '', asn TEXT NOT NULL DEFAULT '',
  hosting INTEGER NOT NULL DEFAULT 0, proxy_flag INTEGER NOT NULL DEFAULT 0, mobile INTEGER NOT NULL DEFAULT 0, timezone TEXT NOT NULL DEFAULT '',
  checked_at INTEGER NOT NULL DEFAULT 0, check_ms INTEGER NOT NULL DEFAULT 0, check_error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
  UNIQUE(protocol, host, port, username)
);
CREATE TABLE IF NOT EXISTS chains (
  id INTEGER PRIMARY KEY, device TEXT NOT NULL, accounts TEXT NOT NULL DEFAULT '[]', ip_id INTEGER NOT NULL UNIQUE,
  front_mode TEXT NOT NULL DEFAULT 'auto', front_node_id INTEGER NOT NULL DEFAULT 0, route TEXT NOT NULL DEFAULT 'global',
  token TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL,
  check_at INTEGER NOT NULL DEFAULT 0, check_exit TEXT NOT NULL DEFAULT '', check_ms INTEGER NOT NULL DEFAULT 0,
  check_error TEXT NOT NULL DEFAULT '', check_front TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS traffic (
  chain_id INTEGER NOT NULL, day TEXT NOT NULL, up INTEGER NOT NULL DEFAULT 0, down INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(chain_id, day)
);
`

func Open(path string, box *crypt.Box) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("初始化数据库: %w", err)
	}
	// 后加的字段：已存在时 SQLite 会报 duplicate column，忽略即可
	for _, m := range []string{
		`ALTER TABLE chains ADD COLUMN relay_uuid TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE chains ADD COLUMN last_seen INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE chains ADD COLUMN last_src TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("升级数据库: %w", err)
		}
	}
	s := &Store{db: db, box: box}
	if err := s.fillRelayUUIDs(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

var ErrNotFound = errors.New("不存在")

func now() int64 { return time.Now().Unix() }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------- settings ----------------

func (s *Store) Setting(key string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ---------------- airports ----------------

func (s *Store) Airports() ([]Airport, error) {
	rows, err := s.db.Query(`SELECT id,name,url_enc,insecure,upload,download,total,expire,synced_at,last_error FROM airports ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Airport
	for rows.Next() {
		var a Airport
		var enc string
		var ins int
		if err := rows.Scan(&a.ID, &a.Name, &enc, &ins, &a.Upload, &a.Download, &a.Total, &a.Expire, &a.SyncedAt, &a.LastError); err != nil {
			return nil, err
		}
		a.Insecure = ins == 1
		a.URL, _ = s.box.Open(enc)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Airport(id int64) (*Airport, error) {
	list, err := s.Airports()
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == id {
			return &list[i], nil
		}
	}
	return nil, ErrNotFound
}

func (s *Store) AddAirport(name, url string, insecure bool) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO airports(name,url_enc,insecure) VALUES(?,?,?)`, name, s.box.Seal(url), b2i(insecure))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateAirportSync(id int64, up, down, total, expire int64, syncErr string) error {
	if syncErr != "" {
		_, err := s.db.Exec(`UPDATE airports SET last_error=?, synced_at=? WHERE id=?`, syncErr, now(), id)
		return err
	}
	_, err := s.db.Exec(`UPDATE airports SET upload=?,download=?,total=?,expire=?,synced_at=?,last_error='' WHERE id=?`, up, down, total, expire, now(), id)
	return err
}

func (s *Store) DeleteAirport(id int64) error {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM chains c JOIN nodes n ON c.front_node_id=n.id WHERE c.front_mode='fixed' AND n.airport_id=?`, id).Scan(&n)
	if n > 0 {
		return fmt.Errorf("有 %d 条链路固定使用这个机场的节点，先改掉再删", n)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM nodes WHERE airport_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM airports WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------- nodes ----------------

// ReplaceNodes 按 (机场, 节点名) 更新节点，保留已有节点的 ID，删除订阅里没有了的节点。
func (s *Store) ReplaceNodes(airportID int64, nodes []Node) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keep := map[string]bool{}
	for _, n := range nodes {
		raw, _ := json.Marshal(n.Raw)
		_, err := tx.Exec(`INSERT INTO nodes(airport_id,name,type,server,port,country,raw_enc) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(airport_id,name) DO UPDATE SET type=excluded.type, server=excluded.server, port=excluded.port, country=excluded.country, raw_enc=excluded.raw_enc`,
			airportID, n.Name, n.Type, n.Server, n.Port, n.Country, s.box.Seal(string(raw)))
		if err != nil {
			return err
		}
		keep[n.Name] = true
	}
	rows, err := tx.Query(`SELECT id,name FROM nodes WHERE airport_id=?`, airportID)
	if err != nil {
		return err
	}
	var drop []int64
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		if !keep[name] {
			drop = append(drop, id)
		}
	}
	rows.Close()
	for _, id := range drop {
		if _, err := tx.Exec(`DELETE FROM nodes WHERE id=?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Nodes() ([]Node, error) {
	rows, err := s.db.Query(`SELECT id,airport_id,name,type,server,port,country,raw_enc,delay_ms,tested_at FROM nodes ORDER BY airport_id,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		var enc string
		if err := rows.Scan(&n.ID, &n.AirportID, &n.Name, &n.Type, &n.Server, &n.Port, &n.Country, &enc, &n.DelayMS, &n.TestedAt); err != nil {
			return nil, err
		}
		if plain, err := s.box.Open(enc); err == nil {
			_ = json.Unmarshal([]byte(plain), &n.Raw)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) UpdateNodeDelay(id int64, ms int) error {
	_, err := s.db.Exec(`UPDATE nodes SET delay_ms=?, tested_at=? WHERE id=?`, ms, now(), id)
	return err
}

// ---------------- residential IPs ----------------

const ipCols = `id,protocol,host,port,username,password_enc,remark,expire,baseline_ip,last_exit,country,country_code,city,isp,asn,hosting,proxy_flag,mobile,timezone,checked_at,check_ms,check_error,created_at`

func (s *Store) scanIP(sc interface{ Scan(...any) error }) (IP, error) {
	var ip IP
	var enc string
	var h, p, m int
	err := sc.Scan(&ip.ID, &ip.Protocol, &ip.Host, &ip.Port, &ip.Username, &enc, &ip.Remark, &ip.Expire, &ip.BaselineIP, &ip.LastExit,
		&ip.Country, &ip.CountryCode, &ip.City, &ip.ISP, &ip.ASN, &h, &p, &m, &ip.Timezone, &ip.CheckedAt, &ip.CheckMS, &ip.CheckError, &ip.CreatedAt)
	if err != nil {
		return ip, err
	}
	ip.Hosting, ip.ProxyFlag, ip.Mobile = h == 1, p == 1, m == 1
	ip.Password, _ = s.box.Open(enc)
	return ip, nil
}

func (s *Store) IPs() ([]IP, error) {
	rows, err := s.db.Query(`SELECT ` + ipCols + ` FROM ips ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IP
	for rows.Next() {
		ip, err := s.scanIP(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ip)
	}
	return out, rows.Err()
}

func (s *Store) IP(id int64) (*IP, error) {
	ip, err := s.scanIP(s.db.QueryRow(`SELECT `+ipCols+` FROM ips WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ip, nil
}

// AddIP 返回新 ID；已存在（协议+地址+端口+账号相同）时返回 0。
func (s *Store) AddIP(ip IP) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO ips(protocol,host,port,username,password_enc,remark,expire,created_at) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(protocol,host,port,username) DO NOTHING`,
		ip.Protocol, ip.Host, ip.Port, ip.Username, s.box.Seal(ip.Password), ip.Remark, ip.Expire, now())
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, nil
	}
	return res.LastInsertId()
}

func (s *Store) UpdateIPMeta(id int64, remark, expire string) error {
	_, err := s.db.Exec(`UPDATE ips SET remark=?, expire=? WHERE id=?`, remark, expire, id)
	return err
}

func (s *Store) DeleteIP(id int64) error {
	var dev string
	if err := s.db.QueryRow(`SELECT device FROM chains WHERE ip_id=?`, id).Scan(&dev); err == nil {
		return fmt.Errorf("这个IP绑定在 %s 上，先删掉那条链路", dev)
	}
	_, err := s.db.Exec(`DELETE FROM ips WHERE id=?`, id)
	return err
}

// IPCheck 是一次检测的结果。Geo 为空时不覆盖已有地理信息。
type IPCheck struct {
	Exit  string
	MS    int
	Error string
	Geo   *Geo
}

type Geo struct {
	Country, CountryCode, City, ISP, ASN, Timezone string
	Hosting, Proxy, Mobile                         bool
}

func (s *Store) UpdateIPCheck(id int64, c IPCheck) error {
	if c.Error != "" {
		_, err := s.db.Exec(`UPDATE ips SET checked_at=?, check_error=? WHERE id=?`, now(), c.Error, id)
		return err
	}
	// 第一次成功检测时记下出口IP作为基准
	if _, err := s.db.Exec(`UPDATE ips SET baseline_ip=? WHERE id=? AND baseline_ip=''`, c.Exit, id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE ips SET last_exit=?, checked_at=?, check_ms=?, check_error='' WHERE id=?`, c.Exit, now(), c.MS, id); err != nil {
		return err
	}
	if g := c.Geo; g != nil {
		_, err := s.db.Exec(`UPDATE ips SET country=?,country_code=?,city=?,isp=?,asn=?,hosting=?,proxy_flag=?,mobile=?,timezone=? WHERE id=?`,
			g.Country, g.CountryCode, g.City, g.ISP, g.ASN, b2i(g.Hosting), b2i(g.Proxy), b2i(g.Mobile), g.Timezone, id)
		return err
	}
	return nil
}

// ResetBaseline 把基准改成最近一次检测到的出口IP（供应商换线后人工确认用）。
func (s *Store) ResetBaseline(id int64) error {
	_, err := s.db.Exec(`UPDATE ips SET baseline_ip=last_exit WHERE id=?`, id)
	return err
}

// ---------------- chains ----------------

const chainCols = `id,device,accounts,ip_id,front_mode,front_node_id,route,token,created_at,check_at,check_exit,check_ms,check_error,check_front,relay_uuid,last_seen,last_src`

func scanChain(sc interface{ Scan(...any) error }) (Chain, error) {
	var c Chain
	var acc string
	err := sc.Scan(&c.ID, &c.Device, &acc, &c.IPID, &c.FrontMode, &c.FrontNodeID, &c.Route, &c.Token, &c.CreatedAt,
		&c.CheckAt, &c.CheckExit, &c.CheckMS, &c.CheckError, &c.CheckFront, &c.RelayUUID, &c.LastSeen, &c.LastSrc)
	if err == nil {
		_ = json.Unmarshal([]byte(acc), &c.Accounts)
	}
	return c, err
}

func (s *Store) Chains() ([]Chain, error) {
	rows, err := s.db.Query(`SELECT ` + chainCols + ` FROM chains ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chain
	for rows.Next() {
		c, err := scanChain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Chain(id int64) (*Chain, error) {
	c, err := scanChain(s.db.QueryRow(`SELECT `+chainCols+` FROM chains WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) ChainByToken(token string) (*Chain, error) {
	c, err := scanChain(s.db.QueryRow(`SELECT `+chainCols+` FROM chains WHERE token=?`, token))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) AddChain(c Chain) (int64, error) {
	acc, _ := json.Marshal(c.Accounts)
	res, err := s.db.Exec(`INSERT INTO chains(device,accounts,ip_id,front_mode,front_node_id,route,token,created_at,relay_uuid) VALUES(?,?,?,?,?,?,?,?,?)`,
		c.Device, string(acc), c.IPID, c.FrontMode, c.FrontNodeID, c.Route, crypt.RandomToken(18), now(), crypt.UUID())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") && strings.Contains(err.Error(), "ip_id") {
			return 0, errors.New("这个住宅IP已经绑定到其他设备了，一个IP只给一台设备用")
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateChain(c Chain) error {
	acc, _ := json.Marshal(c.Accounts)
	_, err := s.db.Exec(`UPDATE chains SET device=?, accounts=?, front_mode=?, front_node_id=?, route=? WHERE id=?`,
		c.Device, string(acc), c.FrontMode, c.FrontNodeID, c.Route, c.ID)
	return err
}

func (s *Store) DeleteChain(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM traffic WHERE chain_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM chains WHERE id=?`, id)
	return err
}

// RotateToken 换掉订阅 token 和中转节点的 UUID，旧的二维码立即失效。
func (s *Store) RotateToken(id int64) error {
	_, err := s.db.Exec(`UPDATE chains SET token=?, relay_uuid=? WHERE id=?`, crypt.RandomToken(18), crypt.UUID(), id)
	return err
}

func (s *Store) fillRelayUUIDs() error {
	rows, err := s.db.Query(`SELECT id FROM chains WHERE relay_uuid=''`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.db.Exec(`UPDATE chains SET relay_uuid=? WHERE id=?`, crypt.UUID(), id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) UpdateChainCheck(id int64, exit string, ms int, errMsg, front string) error {
	_, err := s.db.Exec(`UPDATE chains SET check_at=?, check_exit=?, check_ms=?, check_error=?, check_front=? WHERE id=?`, now(), exit, ms, errMsg, front, id)
	return err
}

// ---------------- 使用情况 ----------------

func (s *Store) UpdateChainSeen(id int64, ts int64, src string) error {
	_, err := s.db.Exec(`UPDATE chains SET last_seen=?, last_src=? WHERE id=?`, ts, src, id)
	return err
}

// AddTraffic 累加某条链路某一天的流量（day 形如 2026-09-27）。
func (s *Store) AddTraffic(chainID int64, day string, up, down int64) error {
	_, err := s.db.Exec(`INSERT INTO traffic(chain_id,day,up,down) VALUES(?,?,?,?)
		ON CONFLICT(chain_id,day) DO UPDATE SET up=up+excluded.up, down=down+excluded.down`, chainID, day, up, down)
	return err
}

type Traffic struct {
	TodayUp   int64 `json:"today_up"`
	TodayDown int64 `json:"today_down"`
	MonthUp   int64 `json:"d30_up"`
	MonthDown int64 `json:"d30_down"`
}

// TrafficSummary 返回每条链路今天和近 30 天的流量。
func (s *Store) TrafficSummary(today, since string) (map[int64]Traffic, error) {
	rows, err := s.db.Query(`SELECT chain_id,
		SUM(CASE WHEN day=? THEN up ELSE 0 END), SUM(CASE WHEN day=? THEN down ELSE 0 END), SUM(up), SUM(down)
		FROM traffic WHERE day>=? GROUP BY chain_id`, today, today, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Traffic{}
	for rows.Next() {
		var id int64
		var t Traffic
		if err := rows.Scan(&id, &t.TodayUp, &t.TodayDown, &t.MonthUp, &t.MonthDown); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

// Backup 把数据库完整复制到 path（加密字段保持加密）。
func (s *Store) Backup(path string) error {
	_, err := s.db.Exec(`VACUUM INTO ?`, path)
	return err
}
