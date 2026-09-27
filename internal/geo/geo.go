// Package geo 查询出口IP的地区、运营商、ASN，以及是不是机房/代理IP。
// 用 ip-api.com 免费接口（每分钟 45 次，只支持 http），从服务器直接查，不经过链路。
package geo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"luodi/internal/store"
)

var (
	mu   sync.Mutex
	last time.Time
)

func Lookup(ctx context.Context, ip string) (*store.Geo, error) {
	// 简单限速：两次查询至少间隔 1.5 秒
	mu.Lock()
	if wait := time.Until(last.Add(1500 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	last = time.Now()
	mu.Unlock()

	url := "http://ip-api.com/json/" + ip + "?lang=zh-CN&fields=status,message,country,countryCode,city,isp,org,as,timezone,hosting,proxy,mobile"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		Status, Message, Country, CountryCode, City, ISP, Org, AS, Timezone string
		Hosting, Proxy, Mobile                                              bool
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	if r.Status != "success" {
		return nil, errors.New("IP 信息查询失败：" + r.Message)
	}
	asn, _, _ := strings.Cut(r.AS, " ")
	isp := r.ISP
	if isp == "" {
		isp = r.Org
	}
	return &store.Geo{
		Country: r.Country, CountryCode: r.CountryCode, City: r.City, ISP: isp, ASN: asn, Timezone: r.Timezone,
		Hosting: r.Hosting, Proxy: r.Proxy, Mobile: r.Mobile,
	}, nil
}
