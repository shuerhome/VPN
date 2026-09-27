// Package notify 通过 Telegram 机器人发告警、收命令、发备份。
//
// 绑定方式：面板「设置」里显示一个 6 位绑定码，在 Telegram 里给机器人发 /bind 绑定码，
// 这个聊天就成为唯一接收通知、能下命令的对象。
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"luodi/internal/store"
)

type Telegram struct {
	Token  string
	Store  *store.Store
	APIURL string // 默认 https://api.telegram.org，测试时可替换

	client     *http.Client
	clientOnce sync.Once
}

func (t *Telegram) Enabled() bool { return t != nil && t.Token != "" }

func (t *Telegram) base() string {
	if t.APIURL != "" {
		return strings.TrimRight(t.APIURL, "/")
	}
	return "https://api.telegram.org"
}

func (t *Telegram) http() *http.Client {
	t.clientOnce.Do(func() { t.client = &http.Client{Timeout: 70 * time.Second} })
	return t.client
}

// ChatID 返回已绑定的聊天，没绑定返回 0。
func (t *Telegram) ChatID() int64 {
	id, _ := strconv.ParseInt(t.Store.Setting("tg_chat"), 10, 64)
	return id
}

// BindCode 返回当前绑定码，没有就生成一个。
func (t *Telegram) BindCode() string {
	code := t.Store.Setting("tg_bind_code")
	if code == "" {
		n, _ := rand.Int(rand.Reader, big.NewInt(900000))
		code = strconv.FormatInt(n.Int64()+100000, 10)
		_ = t.Store.SetSetting("tg_bind_code", code)
	}
	return code
}

func (t *Telegram) call(ctx context.Context, method string, params any, out any) error {
	body, _ := json.Marshal(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base()+"/bot"+t.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.http().Do(req)
	if err != nil {
		return errors.New("连不上 Telegram")
	}
	defer resp.Body.Close()
	return decode(resp.Body, out)
}

func decode(r io.Reader, out any) error {
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(r).Decode(&env); err != nil {
		return err
	}
	if !env.OK {
		return errors.New("Telegram：" + env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// SendTo 给指定聊天发纯文本。
func (t *Telegram) SendTo(ctx context.Context, chat int64, text string) error {
	return t.call(ctx, "sendMessage", map[string]any{"chat_id": chat, "text": text, "disable_web_page_preview": true}, nil)
}

// Send 给已绑定的聊天发消息；没绑定时直接返回。
func (t *Telegram) Send(ctx context.Context, text string) error {
	if !t.Enabled() {
		return nil
	}
	chat := t.ChatID()
	if chat == 0 {
		return errors.New("还没绑定 Telegram")
	}
	return t.SendTo(ctx, chat, text)
}

// SendDocument 发送文件（数据库备份）。
func (t *Telegram) SendDocument(ctx context.Context, path, caption string) error {
	chat := t.ChatID()
	if !t.Enabled() || chat == 0 {
		return errors.New("还没绑定 Telegram")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", strconv.FormatInt(chat, 10))
	_ = w.WriteField("caption", caption)
	part, _ := w.CreateFormFile("document", filepath.Base(path))
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	w.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base()+"/bot"+t.Token+"/sendDocument", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := t.http().Do(req)
	if err != nil {
		return errors.New("连不上 Telegram")
	}
	defer resp.Body.Close()
	return decode(resp.Body, nil)
}

// Handler 处理已绑定聊天发来的命令，返回要回复的文字（空串表示不回复）。
type Handler func(ctx context.Context, cmd, arg string) string

// Poll 长轮询收消息。/bind 在这里处理；其他命令只接受已绑定的聊天。
func (t *Telegram) Poll(ctx context.Context, handle Handler) {
	if !t.Enabled() {
		return
	}
	var me struct {
		Username string `json:"username"`
	}
	if err := t.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		log.Printf("Telegram 机器人不可用：%v", err)
	} else {
		_ = t.Store.SetSetting("tg_bot_name", me.Username)
		_ = t.call(ctx, "setMyCommands", map[string]any{"commands": []map[string]string{
			{"command": "status", "description": "链路概况"},
			{"command": "backup", "description": "立即备份数据库"},
			{"command": "bind", "description": "绑定面板：/bind 绑定码"},
		}}, nil)
	}
	offset := int64(0)
	for ctx.Err() == nil {
		var updates []struct {
			UpdateID int64 `json:"update_id"`
			Message  *struct {
				Text string `json:"text"`
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
			} `json:"message"`
		}
		err := t.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 50, "allowed_updates": []string{"message"}}, &updates)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("Telegram 收消息失败：%v", err)
				select {
				case <-ctx.Done():
				case <-time.After(15 * time.Second):
				}
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if u.Message == nil {
				continue
			}
			chat := u.Message.Chat.ID
			cmd, arg, _ := strings.Cut(strings.TrimSpace(u.Message.Text), " ")
			cmd = strings.ToLower(strings.SplitN(cmd, "@", 2)[0])
			arg = strings.TrimSpace(arg)

			if cmd == "/bind" || cmd == "/start" {
				if arg != "" && arg == t.BindCode() {
					_ = t.Store.SetSetting("tg_chat", strconv.FormatInt(chat, 10))
					_ = t.Store.SetSetting("tg_bind_code", "") // 用过即作废
					_ = t.SendTo(ctx, chat, "✅ 已绑定落地链路台。之后的告警和每日备份会发到这里。\n发送 /status 查看链路概况。")
				} else if chat != t.ChatID() {
					_ = t.SendTo(ctx, chat, "请在面板「设置」页找到绑定码，然后发送：/bind 绑定码")
				}
				continue
			}
			if chat != t.ChatID() {
				continue // 陌生人发的消息一律忽略
			}
			if reply := handle(ctx, cmd, arg); reply != "" {
				_ = t.SendTo(ctx, chat, reply)
			}
		}
	}
}

// Unbind 解除绑定。
func (t *Telegram) Unbind() error {
	return t.Store.SetSetting("tg_chat", "")
}

func (t *Telegram) BotName() string { return t.Store.Setting("tg_bot_name") }

// String 用于日志，避免打印 Token。
func (t *Telegram) String() string { return fmt.Sprintf("telegram(bound=%v)", t.ChatID() != 0) }
