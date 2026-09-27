// Package crypt 用 AES-256-GCM 加密存进数据库的敏感字段（住宅IP密码、订阅链接、节点参数）。
package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const prefix = "v1:"

type Box struct{ aead cipher.AEAD }

// Load 优先用环境变量里的 base64 密钥；没有就在 dataDir 里生成并保存一个。
func Load(envKey, dataDir string) (*Box, error) {
	var key []byte
	if envKey != "" {
		k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(envKey))
		if err != nil || len(k) != 32 {
			return nil, errors.New("SECRET_KEY 必须是 32 字节的 base64（openssl rand -base64 32）")
		}
		key = k
	} else {
		p := filepath.Join(dataDir, "secret.key")
		if b, err := os.ReadFile(p); err == nil {
			k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
			if err != nil || len(k) != 32 {
				return nil, errors.New(p + " 内容无效")
			}
			key = k
		} else {
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return nil, err
			}
			if err := os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
				return nil, err
			}
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

func (b *Box) Seal(plain string) string {
	if plain == "" {
		return ""
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	out := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.StdEncoding.EncodeToString(out)
}

func (b *Box) Open(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if !strings.HasPrefix(s, prefix) {
		return "", errors.New("未知的密文格式")
	}
	raw, err := base64.StdEncoding.DecodeString(s[len(prefix):])
	if err != nil {
		return "", err
	}
	n := b.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("密文太短")
	}
	out, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("解密失败：SECRET_KEY 可能被换过")
	}
	return string(out), nil
}

// UUID 生成随机的 v4 UUID。
func UUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// RandomToken 生成 URL 安全的随机串，用于订阅 token 和会话密钥。
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
