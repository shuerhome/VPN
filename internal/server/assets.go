package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

var zeroTime time.Time

// 前端文件按内容加版本号：页面里引用的是 app.js?v=内容哈希，内容变了地址就变，
// Cloudflare 和浏览器缓存的旧文件不会再和新页面混用。
type assetSet struct {
	fsys    fs.FS
	version map[string]string // 文件名 → 内容哈希前 10 位
	index   []byte            // 引用都已加上版本号的 index.html
}

var assetRef = regexp.MustCompile(`(src|href)="/?([A-Za-z0-9_.-]+\.(?:js|css))"`)

func loadAssets(fsys fs.FS) *assetSet {
	a := &assetSet{fsys: fsys, version: map[string]string{}}
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		a.version[p] = hex.EncodeToString(sum[:])[:10]
		return nil
	})
	if b, err := fs.ReadFile(fsys, "index.html"); err == nil {
		a.index = []byte(a.rewrite(string(b)))
	}
	return a
}

// rewrite 给 HTML 里引用的本站 js / css 加上 ?v=版本号。
func (a *assetSet) rewrite(html string) string {
	return assetRef.ReplaceAllStringFunc(html, func(m string) string {
		sub := assetRef.FindStringSubmatch(m)
		v, ok := a.version[sub[2]]
		if !ok {
			return m
		}
		lead := ""
		if strings.Contains(m, `="/`) {
			lead = "/"
		}
		return sub[1] + `="` + lead + sub[2] + "?v=" + v + `"`
	})
}

// URL 返回带版本号的地址，例如 /share.js?v=1a2b3c4d5e。
func (a *assetSet) URL(name string) string {
	if v, ok := a.version[name]; ok {
		return "/" + name + "?v=" + v
	}
	return "/" + name
}

func (s *Server) assetSet() *assetSet {
	s.assetsOnce.Do(func() { s.assets = loadAssets(s.Assets) })
	return s.assets
}

func (s *Server) static() http.Handler {
	a := s.assetSet()
	files := http.FileServer(http.FS(a.fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" || name == "index.html" {
			// 页面本身永远不缓存，这样总能拿到最新的版本号
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.ServeContent(w, r, "index.html", zeroTime, bytes.NewReader(a.index))
			return
		}
		if v, ok := a.version[name]; ok && r.URL.Query().Get("v") == v {
			// 地址里带着当前内容的版本号：内容不会再变，可以长期缓存
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
