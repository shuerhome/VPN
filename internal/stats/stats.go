// Package stats 定时读取中转的计量数据和连接列表，统计每台手机的在线状态、来源IP和流量，
// 并在同一个二维码被两个不同的IP同时使用时告警。
package stats

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"luodi/internal/relay"
	"luodi/internal/store"
)

const (
	// OnlineWindow 内有过连接就算在线。
	OnlineWindow = 60 * time.Second
	// multiWindow 内出现过的来源IP算「同时在用」；需要连续 multiPolls 次采样都成立才告警，
	// 避免手机在 Wi-Fi 和蜂窝之间切换时误报。
	multiWindow = 10 * time.Second
	multiPolls  = 3
)

type Online struct {
	Online   bool     `json:"online"`
	IPs      []string `json:"ips"`   // 最近一分钟连过中转的来源IP
	Conns    int      `json:"conns"` // 当前连接数
	LastSeen int64    `json:"last_seen"`
}

type Collector struct {
	Relay *relay.Relay
	Store *store.Store
	Every time.Duration // 采样间隔，默认 2 秒
	// Alert 在发现一码多机时调用。
	Alert func(chainID int64, device string, ips []string)

	mu       sync.Mutex
	prev     map[int64]relay.Counter        // 上次采样时各链路的累计流量
	ipSeen   map[int64]map[string]time.Time // 链路 → 来源IP → 最近看到的时间
	lastSeen map[int64]time.Time
	conns    map[int64]int
	multi    map[int64]int
	savedAt  map[int64]time.Time // 上次把 last_seen 写进数据库的时间
}

func (c *Collector) init() {
	if c.prev == nil {
		c.prev = map[int64]relay.Counter{}
		c.ipSeen = map[int64]map[string]time.Time{}
		c.lastSeen = map[int64]time.Time{}
		c.conns = map[int64]int{}
		c.multi = map[int64]int{}
		c.savedAt = map[int64]time.Time{}
	}
}

// Snapshot 返回各链路当前在线情况（给页面用）。
func (c *Collector) Snapshot() map[int64]Online {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.init()
	now := time.Now()
	out := map[int64]Online{}
	for id, seen := range c.lastSeen {
		o := Online{LastSeen: seen.Unix(), Online: now.Sub(seen) <= OnlineWindow, Conns: c.conns[id]}
		if o.Online {
			o.IPs = ipsWithin(c.ipSeen[id], now, OnlineWindow)
		}
		out[id] = o
	}
	return out
}

func ipsWithin(m map[string]time.Time, now time.Time, window time.Duration) []string {
	var out []string
	for ip, t := range m {
		if now.Sub(t) <= window {
			out = append(out, ip)
		}
	}
	sort.Strings(out)
	return out
}

func (c *Collector) every() time.Duration {
	if c.Every == 0 {
		return 2 * time.Second
	}
	return c.Every
}

func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.every())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Poll(ctx); err != nil {
				log.Printf("读取中转连接失败: %v", err)
			}
		}
	}
}

// Poll 采样一次。
//
// 流量和在线状态来自中转的计量转发（每个字节都算得到，短连接也不会漏）；
// 来源IP来自中转的连接列表，采样得到，用来发现一码多机。
func (c *Collector) Poll(ctx context.Context) error {
	s, err := c.Relay.Connections(ctx)
	if err != nil {
		return err
	}
	counters := c.Relay.Meter.Counters()
	now := time.Now()
	day := now.Format("2006-01-02")

	c.mu.Lock()
	c.init()
	delta := map[int64][2]int64{}
	for id, cur := range counters {
		prev := c.prev[id]
		if cur.Up < prev.Up || cur.Down < prev.Down { // 转发重建过，计数归零
			prev = relay.Counter{}
		}
		if du, dd := cur.Up-prev.Up, cur.Down-prev.Down; du > 0 || dd > 0 {
			delta[id] = [2]int64{du, dd}
		}
		c.prev[id] = cur
		if cur.Active > 0 {
			c.lastSeen[id] = now
		} else if !cur.Last.IsZero() && cur.Last.After(c.lastSeen[id]) {
			c.lastSeen[id] = cur.Last
		}
	}
	for id := range c.prev {
		if _, ok := counters[id]; !ok {
			delete(c.prev, id)
		}
	}

	count := map[int64]int{}
	if s != nil {
		for _, cn := range s.Conns {
			if cn.Chain == 0 {
				continue // 不属于设备（如伪装站点握手）
			}
			count[cn.Chain]++
			c.lastSeen[cn.Chain] = now
			if cn.SourceIP != "" {
				if c.ipSeen[cn.Chain] == nil {
					c.ipSeen[cn.Chain] = map[string]time.Time{}
				}
				c.ipSeen[cn.Chain][cn.SourceIP] = now
			}
		}
	}
	c.conns = count

	// 一码多机：短时间内出现两个以上来源IP，连续几次采样都这样才算
	type alert struct {
		id  int64
		ips []string
	}
	var alerts []alert
	for id, m := range c.ipSeen {
		ips := ipsWithin(m, now, multiWindow)
		if len(ips) > 1 {
			c.multi[id]++
			if c.multi[id] == multiPolls {
				alerts = append(alerts, alert{id, ips})
			}
		} else {
			c.multi[id] = 0
		}
		for ip, t := range m {
			if now.Sub(t) > OnlineWindow {
				delete(m, ip)
			}
		}
	}

	// 在线时间最多 30 秒写一次数据库
	type seenRow struct {
		id  int64
		at  time.Time
		src string
	}
	var seen []seenRow
	for id, t := range c.lastSeen {
		if t.Sub(c.savedAt[id]) >= 30*time.Second {
			c.savedAt[id] = t
			src := ""
			if ips := ipsWithin(c.ipSeen[id], now, OnlineWindow); len(ips) > 0 {
				src = ips[0]
			}
			seen = append(seen, seenRow{id, t, src})
		}
	}
	c.mu.Unlock()

	for id, d := range delta {
		if id > 0 && (d[0] > 0 || d[1] > 0) {
			if err := c.Store.AddTraffic(id, day, d[0], d[1]); err != nil {
				return err
			}
		}
	}
	for _, r := range seen {
		_ = c.Store.UpdateChainSeen(r.id, r.at.Unix(), r.src)
	}
	if c.Alert != nil {
		for _, a := range alerts {
			device := fmt.Sprintf("链路 %d", a.id)
			if ch, err := c.Store.Chain(a.id); err == nil {
				device = ch.Device
			}
			c.Alert(a.id, device, a.ips)
		}
	}
	return nil
}
