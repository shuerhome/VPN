// 落地链路台：把机场节点和静态住宅IP绑成固定出口的链路，下发给 iPhone。
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"luodi/internal/app"
	"luodi/internal/check"
	"luodi/internal/crypt"
	"luodi/internal/relay"
	"luodi/internal/server"
	"luodi/internal/store"
	"luodi/web"
)

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= time.Minute {
			return d
		}
		log.Printf("%s=%q 无效，使用默认 %s", key, v, def)
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
		log.Printf("%s=%q 无效，使用默认 %d", key, v, def)
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[panel] ")

	password := os.Getenv("PANEL_PASSWORD")
	if len(password) < 8 {
		log.Fatal("PANEL_PASSWORD 未设置或少于 8 位")
	}
	dataDir := env("DATA_DIR", "/data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	box, err := crypt.Load(os.Getenv("SECRET_KEY"), dataDir)
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dataDir, "panel.db"), box)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	workDir := filepath.Join(dataDir, "tmp")
	_ = os.RemoveAll(workDir)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		log.Fatal(err)
	}
	runner := &check.Runner{
		Bin:       env("MIHOMO_BIN", "/usr/local/bin/mihomo"),
		WorkDir:   workDir,
		CheckURLs: strings.Split(env("CHECK_URLS", "https://api.ipify.org,https://ifconfig.me/ip,https://icanhazip.com"), ","),
	}
	a := &app.App{
		Store: st, Runner: runner,
		SyncEvery:  envDuration("SYNC_EVERY", 30*time.Minute),
		CheckEvery: envDuration("CHECK_EVERY", 15*time.Minute),
	}

	rl := &relay.Relay{
		Bin:        runner.Bin,
		Dir:        filepath.Join(dataDir, "relay"),
		PublicHost: os.Getenv("RELAY_HOST"),
		PublicPort: envInt("RELAY_PORT", 443),
		ListenPort: envInt("RELAY_LISTEN", 8443),
		SNI:        env("REALITY_SNI", "www.microsoft.com"),
		Store:      st,
	}
	if !rl.Enabled() {
		log.Printf("未设置 RELAY_HOST，中转模式（小火箭扫码直连）不开启")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.Loop(ctx)
	go rl.Run(ctx)

	srv := &http.Server{
		Addr:              env("LISTEN", ":8080"),
		Handler:           server.New(a, rl, password, os.Getenv("PUBLIC_URL"), web.Files).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("监听 %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
