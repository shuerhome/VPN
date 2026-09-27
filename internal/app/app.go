// Package app 把订阅同步、链路检测、IP 信息查询串起来，并负责定时任务。
package app

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"luodi/internal/check"
	"luodi/internal/gen"
	"luodi/internal/geo"
	"luodi/internal/store"
	"luodi/internal/sub"
)

type App struct {
	Store  *store.Store
	Runner *check.Runner

	SyncEvery  time.Duration
	CheckEvery time.Duration

	busy sync.Mutex // 手动检测和定时检测不同时跑
}

// ---------------- 订阅 ----------------

func (a *App) SyncAirport(ctx context.Context, id int64) error {
	ap, err := a.Store.Airport(id)
	if err != nil {
		return err
	}
	res, err := sub.Fetch(ctx, ap.URL, ap.Insecure)
	if err != nil {
		_ = a.Store.UpdateAirportSync(id, 0, 0, 0, 0, err.Error())
		return err
	}
	nodes := make([]store.Node, 0, len(res.Nodes))
	for _, n := range res.Nodes {
		nodes = append(nodes, store.Node{Name: n.Name, Type: n.Type, Server: n.Server, Port: n.Port, Country: n.Country, Raw: n.Raw})
	}
	if err := a.Store.ReplaceNodes(id, nodes); err != nil {
		return err
	}
	return a.Store.UpdateAirportSync(id, res.Info.Upload, res.Info.Download, res.Info.Total, res.Info.Expire, "")
}

func (a *App) SyncAll(ctx context.Context) {
	aps, err := a.Store.Airports()
	if err != nil {
		log.Printf("读取机场失败: %v", err)
		return
	}
	for _, ap := range aps {
		if err := a.SyncAirport(ctx, ap.ID); err != nil {
			log.Printf("同步 %s 失败: %v", ap.Name, err)
		}
	}
}

// ---------------- 检测 ----------------

// TestNodes 只测节点延迟。
func (a *App) TestNodes(ctx context.Context) error {
	a.busy.Lock()
	defer a.busy.Unlock()
	nodes, err := a.Store.Nodes()
	if err != nil {
		return err
	}
	res, err := a.Runner.Run(ctx, nodes, nil)
	if err != nil {
		return err
	}
	for id, ms := range res.NodeDelay {
		_ = a.Store.UpdateNodeDelay(id, ms)
	}
	return nil
}

// CheckChains 检测链路；ids 为空时检测全部，并顺带测全部节点延迟。
func (a *App) CheckChains(ctx context.Context, ids ...int64) error {
	a.busy.Lock()
	defer a.busy.Unlock()

	chains, err := a.Store.Chains()
	if err != nil {
		return err
	}
	nodes, err := a.Store.Nodes()
	if err != nil {
		return err
	}
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var jobs []check.ChainJob
	byKey := map[string]store.Chain{}
	ipOf := map[string]store.IP{}
	for _, c := range chains {
		if len(ids) > 0 && !want[c.ID] {
			continue
		}
		ip, err := a.Store.IP(c.IPID)
		if err != nil {
			continue
		}
		key := fmt.Sprintf("c%d", c.ID)
		fronts := frontNodes(c, *ip, nodes)
		if len(fronts) == 0 {
			_ = a.Store.UpdateChainCheck(c.ID, "", 0, "没有可用的前置节点", "")
		} else {
			jobs = append(jobs, check.ChainJob{Key: key, IP: *ip, Fronts: fronts})
			byKey[key] = c
		}
		// 同时从服务器直连测一次住宅IP：这就是中转模式（小火箭扫码）实际走的路
		direct := fmt.Sprintf("ip%d", ip.ID)
		jobs = append(jobs, check.ChainJob{Key: direct, IP: *ip})
		ipOf[direct] = *ip
	}
	testNodes := nodes
	if len(ids) > 0 {
		testNodes = nil // 单条检测只测它自己的前置
	}
	res, err := a.Runner.Run(ctx, testNodes, jobs)
	if err != nil {
		return err
	}
	for id, ms := range res.NodeDelay {
		_ = a.Store.UpdateNodeDelay(id, ms)
	}
	for key, r := range res.Chains {
		if c, ok := byKey[key]; ok {
			_ = a.Store.UpdateChainCheck(c.ID, r.Exit, r.MS, r.Error, r.Front)
			continue
		}
		ip := ipOf[key]
		if r.Error != "" {
			_ = a.Store.UpdateIPCheck(ip.ID, store.IPCheck{Error: r.Error})
			continue
		}
		a.recordIP(ctx, ip, r)
	}
	return nil
}

// CheckIPs 从服务器直连住宅IP（不经机场），刚加进来的IP用它拿到出口IP和地区信息。
func (a *App) CheckIPs(ctx context.Context, ids ...int64) error {
	a.busy.Lock()
	defer a.busy.Unlock()
	var jobs []check.ChainJob
	ipOf := map[string]store.IP{}
	for _, id := range ids {
		ip, err := a.Store.IP(id)
		if err != nil {
			continue
		}
		key := fmt.Sprintf("ip%d", id)
		jobs = append(jobs, check.ChainJob{Key: key, IP: *ip})
		ipOf[key] = *ip
	}
	res, err := a.Runner.Run(ctx, nil, jobs)
	if err != nil {
		return err
	}
	for key, r := range res.Chains {
		ip := ipOf[key]
		if r.Error != "" {
			_ = a.Store.UpdateIPCheck(ip.ID, store.IPCheck{Error: r.Error})
			continue
		}
		a.recordIP(ctx, ip, r)
	}
	return nil
}

// recordIP 记下出口IP；出口和上次查询地区时不一样（或还没查过）才去查地区。
func (a *App) recordIP(ctx context.Context, ip store.IP, r check.ChainResult) {
	c := store.IPCheck{Exit: r.Exit, MS: r.MS}
	if ip.CountryCode == "" || ip.LastExit != r.Exit {
		if g, err := geo.Lookup(ctx, r.Exit); err == nil {
			c.Geo = g
		} else {
			log.Printf("查询 %s 地区失败: %v", r.Exit, err)
		}
	}
	_ = a.Store.UpdateIPCheck(ip.ID, c)
}

// ---------------- 定时任务 ----------------

func (a *App) Loop(ctx context.Context) {
	syncT := time.NewTicker(a.SyncEvery)
	checkT := time.NewTicker(a.CheckEvery)
	defer syncT.Stop()
	defer checkT.Stop()
	// 启动后先同步一次、检测一次
	go func() {
		a.SyncAll(ctx)
		if err := a.CheckChains(ctx); err != nil {
			log.Printf("检测失败: %v", err)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-syncT.C:
			a.SyncAll(ctx)
		case <-checkT.C:
			if err := a.CheckChains(ctx); err != nil {
				log.Printf("检测失败: %v", err)
			}
		}
	}
}

func frontNodes(c store.Chain, ip store.IP, nodes []store.Node) []store.Node {
	return gen.FrontNodes(c, ip, nodes)
}
