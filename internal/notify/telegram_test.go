package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"luodi/internal/crypt"
	"luodi/internal/store"
)

// fakeTG 模拟 Telegram Bot API：按顺序吐出预设的消息，记录机器人发出的内容。
type fakeTG struct {
	mu      sync.Mutex
	inbox   []map[string]any
	sent    []map[string]any
	docs    []string
	served  bool
	updates chan struct{}
}

func (f *fakeTG) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		reply := func(result any) { _ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result}) }
		switch method {
		case "getMe":
			reply(map[string]any{"username": "luodi_test_bot"})
		case "setMyCommands":
			reply(true)
		case "getUpdates":
			f.mu.Lock()
			batch := f.inbox
			f.inbox = nil
			f.mu.Unlock()
			if len(batch) == 0 {
				time.Sleep(50 * time.Millisecond)
			}
			reply(batch)
		case "sendMessage":
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			f.mu.Lock()
			f.sent = append(f.sent, m)
			f.mu.Unlock()
			reply(map[string]any{})
		case "sendDocument":
			_ = r.ParseMultipartForm(10 << 20)
			file, hdr, err := r.FormFile("document")
			if err == nil {
				b, _ := io.ReadAll(file)
				f.mu.Lock()
				f.docs = append(f.docs, hdr.Filename+":"+string(b[:min(len(b), 16)]))
				f.mu.Unlock()
			}
			reply(map[string]any{})
		default:
			t.Errorf("未预期的调用 %s", method)
		}
	})
}

func (f *fakeTG) push(id int64, chat int64, text string) {
	f.mu.Lock()
	f.inbox = append(f.inbox, map[string]any{"update_id": id, "message": map[string]any{"text": text, "chat": map[string]any{"id": chat}}})
	f.mu.Unlock()
}

func (f *fakeTG) texts(chat int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.sent {
		if int64(m["chat_id"].(float64)) == chat {
			out = append(out, m["text"].(string))
		}
	}
	return out
}

func newStore(t *testing.T) *store.Store {
	box, _ := crypt.Load("", t.TempDir())
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("等待超时")
}

func TestBindAndCommands(t *testing.T) {
	fake := &fakeTG{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	st := newStore(t)
	tg := &Telegram{Token: "123:abc", Store: st, APIURL: srv.URL}
	code := tg.BindCode()

	var handled []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tg.Poll(ctx, func(ctx context.Context, cmd, arg string) string {
		handled = append(handled, cmd)
		return "reply:" + cmd
	})

	const owner, stranger = int64(1001), int64(2002)
	fake.push(1, stranger, "/status")      // 没绑定前陌生人发命令：忽略
	fake.push(2, stranger, "/bind 000000") // 错误绑定码：提示
	fake.push(3, owner, "/bind "+code)     // 正确绑定码
	waitFor(t, func() bool { return tg.ChatID() == owner })
	fake.push(4, stranger, "/bind "+code) // 绑定码用过即作废
	fake.push(5, owner, "/status@luodi_test_bot")
	fake.push(6, stranger, "/status") // 绑定后陌生人仍被忽略
	waitFor(t, func() bool { return len(fake.texts(owner)) >= 2 })

	if got := fake.texts(owner); !strings.Contains(got[0], "已绑定") || got[1] != "reply:/status" {
		t.Fatalf("主人收到的消息不对: %v", got)
	}
	if len(handled) != 1 {
		t.Fatalf("只有主人的命令应被处理，实际 %v", handled)
	}
	if got := fake.texts(stranger); len(got) != 2 || !strings.Contains(got[0], "绑定码") {
		t.Fatalf("陌生人只应收到绑定提示: %v", got)
	}
	if tg.ChatID() != owner {
		t.Fatal("绑定被陌生人抢走了")
	}
	if tg.BotName() != "luodi_test_bot" {
		t.Fatalf("机器人名: %q", tg.BotName())
	}
}

func TestSendDocument(t *testing.T) {
	fake := &fakeTG{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	st := newStore(t)
	tg := &Telegram{Token: "123:abc", Store: st, APIURL: srv.URL}
	if err := tg.SendDocument(context.Background(), "x", "c"); err == nil {
		t.Fatal("未绑定时应报错")
	}
	_ = st.SetSetting("tg_chat", "1001")
	path := filepath.Join(t.TempDir(), "b.db")
	if err := st.Backup(path); err != nil {
		t.Fatal(err)
	}
	if err := tg.SendDocument(context.Background(), path, "备份"); err != nil {
		t.Fatal(err)
	}
	if len(fake.docs) != 1 || !strings.HasPrefix(fake.docs[0], "b.db:SQLite format 3") {
		t.Fatalf("备份文件不对: %v", fake.docs)
	}
}
