package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FrontsUnreachable 是服务器连不上任何前置节点时的检测错误前缀（入口在国内的专线常见）。
const FrontsUnreachable = "前置节点全部不可用"

// notify 异步发 Telegram，失败只记日志。
func (a *App) notify(text string) {
	if !a.Notify.Enabled() || a.Notify.ChatID() == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := a.Notify.Send(ctx, text); err != nil {
			log.Printf("发送 Telegram 失败: %v", err)
		}
	}()
}

// transition 只在状态变化时通知：变坏发 bad，恢复发 ok。state 为 "ok" 表示正常。
func (a *App) transition(key, state, bad, ok string) {
	k := "alert:" + key
	prev := a.Store.Setting(k)
	if prev == state {
		return
	}
	_ = a.Store.SetSetting(k, state)
	if state == "ok" {
		if prev != "" && ok != "" {
			a.notify(ok)
		}
		return
	}
	a.notify(bad)
}

func daysLeft(date string) (int, bool) {
	t, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return 0, false
	}
	y, m, d := time.Now().Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	return int(t.Sub(today).Hours() / 24), true
}

func expiryBucket(days int) string {
	switch {
	case days < 0:
		return "expired"
	case days <= 1:
		return "1"
	case days <= 7:
		return "7"
	}
	return "ok"
}

// EvaluateAlerts 检查所有该提醒的事情。每轮检测、同步之后调用。
func (a *App) EvaluateAlerts() {
	if !a.Notify.Enabled() {
		return
	}
	chains, err1 := a.Store.Chains()
	ips, err2 := a.Store.IPs()
	aps, err3 := a.Store.Airports()
	if err1 != nil || err2 != nil || err3 != nil {
		return
	}
	device := map[int64]string{}
	for _, c := range chains {
		device[c.IPID] = c.Device
	}

	for _, ip := range ips {
		name := device[ip.ID]
		label := ip.Host + fmt.Sprintf(":%d", ip.Port)
		// 中转路径（服务器直连住宅IP）只对已绑定的IP告警
		relayOK := true
		if name != "" && a.RelayEnabled && ip.CheckedAt > 0 {
			state, bad := "ok", ""
			switch {
			case ip.CheckError != "":
				state, bad = "down", fmt.Sprintf("🔴 %s 连不上住宅IP\n%s\n原因：%s", name, label, ip.CheckError)
			case ip.LastExit != ip.ExpectedExit():
				state = "changed:" + ip.LastExit
				bad = fmt.Sprintf("⚠️ %s 的出口IP变了\n原来 %s，现在 %s\n供应商可能换了线路。确认新IP可用后，在面板点「以当前出口为准」；否则先别让账号登录。", name, ip.ExpectedExit(), ip.LastExit)
			}
			relayOK = state == "ok"
			a.transition(fmt.Sprintf("relay:%d", ip.ID), state, bad, fmt.Sprintf("✅ %s 恢复正常，出口 %s", name, ip.LastExit))
		}
		// Stash 链路：中转已经报过的问题不重复报
		for _, c := range chains {
			if c.IPID != ip.ID || c.CheckAt == 0 || !relayOK {
				continue
			}
			if strings.HasPrefix(c.CheckError, FrontsUnreachable) {
				continue // 前置入口在国内、服务器连不上：服务器上验证不了，不算故障
			}
			state, bad := "ok", ""
			switch {
			case c.CheckError != "":
				state, bad = "down", fmt.Sprintf("🟠 %s 的 Stash 链路不通（机场 → 住宅IP）\n原因：%s\n小火箭扫码的中转不受影响。", c.Device, c.CheckError)
			case c.CheckExit != ip.ExpectedExit():
				state = "changed:" + c.CheckExit
				bad = fmt.Sprintf("⚠️ %s 的 Stash 链路出口是 %s，应为 %s", c.Device, c.CheckExit, ip.ExpectedExit())
			}
			a.transition(fmt.Sprintf("air:%d", c.ID), state, bad, fmt.Sprintf("✅ %s 的 Stash 链路恢复正常", c.Device))
		}
		// 到期提醒
		if d, ok := daysLeft(ip.Expire); ok {
			who := label
			if name != "" {
				who = name + "（" + label + "）"
			}
			msg := fmt.Sprintf("⏰ 住宅IP %s 还有 %d 天到期（%s），记得续费。续费后IP不变，账号不用动。", who, d, ip.Expire)
			if d < 0 {
				msg = fmt.Sprintf("⛔ 住宅IP %s 已于 %s 到期", who, ip.Expire)
			}
			a.transition(fmt.Sprintf("ipexp:%d:%s", ip.ID, ip.Expire), expiryBucket(d), msg, "")
		}
	}

	for _, ap := range aps {
		if ap.LastError != "" {
			a.transition(fmt.Sprintf("airsync:%d", ap.ID), "err", fmt.Sprintf("🟠 机场「%s」订阅同步失败：%s", ap.Name, ap.LastError), "")
		} else {
			a.transition(fmt.Sprintf("airsync:%d", ap.ID), "ok", "", fmt.Sprintf("✅ 机场「%s」订阅同步恢复", ap.Name))
		}
		if ap.Expire > 0 {
			d := int(time.Until(time.Unix(ap.Expire, 0)).Hours() / 24)
			date := time.Unix(ap.Expire, 0).Format("2006-01-02")
			a.transition(fmt.Sprintf("airexp:%d:%d", ap.ID, ap.Expire), expiryBucket(d),
				fmt.Sprintf("⏰ 机场「%s」还有 %d 天到期（%s）", ap.Name, d, date), "")
		}
		if ap.Total > 0 {
			used := float64(ap.Upload+ap.Download) / float64(ap.Total)
			state := "ok"
			if used >= 0.9 {
				state = "90"
			}
			a.transition(fmt.Sprintf("airtraffic:%d:%d", ap.ID, ap.Total), state,
				fmt.Sprintf("⚠️ 机场「%s」流量已用 %.0f%%（%.1f / %.0f GB）", ap.Name, used*100, gb(ap.Upload+ap.Download), gb(ap.Total)), "")
		}
	}
}

func gb(b int64) float64 { return float64(b) / (1 << 30) }

// MultiDeviceAlert 同一个二维码被两个以上的IP同时使用。6 小时内同一台设备只报一次。
func (a *App) MultiDeviceAlert(chainID int64, device string, ips []string) {
	a.multiMu.Lock()
	if a.multiLast == nil {
		a.multiLast = map[int64]time.Time{}
	}
	if time.Since(a.multiLast[chainID]) < 6*time.Hour {
		a.multiMu.Unlock()
		return
	}
	a.multiLast[chainID] = time.Now()
	a.multiMu.Unlock()
	log.Printf("%s 的二维码同时有多个来源IP: %v", device, ips)
	a.notify(fmt.Sprintf("🚨 %s 的二维码正被 %d 个IP同时使用：\n%s\n可能被转发给了别人或装在了多台设备上。如果不是你安排的，在面板里点「重置二维码」。",
		device, len(ips), strings.Join(ips, "\n")))
}

// SelfCheckAlert 处理手机上的安全自检结果：出口不是住宅IP、WebRTC 泄露时告警，恢复时再通知一次。
func (a *App) SelfCheckAlert(chainID int64, device, exit, want string, exitOK bool, leak string) {
	state, bad := "ok", ""
	switch {
	case !exitOK:
		state = "exit:" + exit
		bad = fmt.Sprintf("🚨 %s 手机自检：出口IP是 %s，不是住宅IP %s\n手机可能没开代理、没用全局模式，或开了 iCloud 专用代理。先别让这台手机登录账号。", device, exit, want)
	case leak != "":
		state = "webrtc"
		bad = fmt.Sprintf("⚠️ %s 手机自检：WebRTC 暴露了其他IP：%s", device, leak)
	}
	a.transition(fmt.Sprintf("selfcheck:%d", chainID), state, bad, fmt.Sprintf("✅ %s 手机自检通过，出口 %s", device, exit))
}

// ---------------- 备份 ----------------

// Backup 生成数据库备份文件，返回路径（调用方负责删除）。
func (a *App) Backup() (string, error) {
	if err := os.MkdirAll(a.TmpDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(a.TmpDir, "luodi-"+time.Now().Format("20060102-1504")+".db")
	_ = os.Remove(path)
	if err := a.Store.Backup(path); err != nil {
		return "", err
	}
	return path, nil
}

const backupCaption = "落地链路台数据库备份。住宅IP密码和订阅链接是加密的，恢复时需要服务器 .env 里的 SECRET_KEY（请另外保存，不要发到这里）。"

func (a *App) BackupToTelegram(ctx context.Context) error {
	path, err := a.Backup()
	if err != nil {
		return err
	}
	defer os.Remove(path)
	if err := a.Notify.SendDocument(ctx, path, backupCaption); err != nil {
		return err
	}
	_ = a.Store.SetSetting("backup:last", time.Now().Format("2006-01-02"))
	return nil
}

// dailyBackup 每天凌晨 4 点后备份一次到 Telegram。
func (a *App) dailyBackup(ctx context.Context) {
	if !a.Notify.Enabled() || a.Notify.ChatID() == 0 {
		return
	}
	now := time.Now()
	if now.Hour() < 4 || a.Store.Setting("backup:last") == now.Format("2006-01-02") {
		return
	}
	if err := a.BackupToTelegram(ctx); err != nil {
		log.Printf("每日备份失败: %v", err)
	}
}

// ---------------- Telegram 命令 ----------------

func (a *App) HandleCommand(ctx context.Context, cmd, arg string) string {
	switch cmd {
	case "/status":
		return a.statusText()
	case "/backup":
		if err := a.BackupToTelegram(ctx); err != nil {
			return "备份失败：" + err.Error()
		}
		return ""
	case "/help":
		return "/status 链路概况\n/backup 立即备份数据库"
	}
	return "看不懂这个命令。发送 /help 查看可用命令。"
}

func human(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", gb(b))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%d KB", b/1024)
}

func (a *App) statusText() string {
	chains, err := a.Store.Chains()
	if err != nil {
		return "读取失败：" + err.Error()
	}
	if len(chains) == 0 {
		return "还没有链路。"
	}
	today := time.Now().Format("2006-01-02")
	traffic, _ := a.Store.TrafficSummary(today, today)
	online := map[int64]bool{}
	if a.Stats != nil {
		for id, o := range a.Stats.Snapshot() {
			online[id] = o.Online
		}
	}
	var sb strings.Builder
	for _, c := range chains {
		ip, err := a.Store.IP(c.IPID)
		if err != nil {
			continue
		}
		mark := "✅"
		detail := "出口 " + ip.LastExit
		switch {
		case ip.CheckedAt == 0:
			mark, detail = "⏳", "待检测"
		case ip.CheckError != "":
			mark, detail = "🔴", ip.CheckError
		case ip.LastExit != ip.ExpectedExit():
			mark, detail = "⚠️", "出口变成 "+ip.LastExit
		}
		state := "离线"
		if online[c.ID] {
			state = "在线"
		}
		t := traffic[c.ID]
		fmt.Fprintf(&sb, "%s %s · %s · %s · 今日 %s\n", mark, c.Device, detail, state, human(t.TodayUp+t.TodayDown))
	}
	return strings.TrimSpace(sb.String())
}
