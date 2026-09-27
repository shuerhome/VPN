package sub

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ParseURI 把 ss:// vmess:// vless:// trojan:// hysteria2:// 链接转成 Clash Meta 的节点字段。
func ParseURI(s string) (map[string]any, error) {
	scheme, _, ok := strings.Cut(s, "://")
	if !ok {
		return nil, errors.New("不是链接")
	}
	switch strings.ToLower(scheme) {
	case "vmess":
		return parseVmess(s)
	case "vless":
		return parseVless(s)
	case "trojan":
		return parseTrojan(s)
	case "ss":
		return parseSS(s)
	case "hysteria2", "hy2":
		return parseHy2(s)
	}
	return nil, errors.New("暂不支持的协议 " + scheme)
}

func name(u *url.URL, fallback string) string {
	if u.Fragment != "" {
		return strings.TrimSpace(u.Fragment)
	}
	return fallback
}

func port(u *url.URL) (int, error) {
	p, err := strconv.Atoi(u.Port())
	if err != nil || p <= 0 || p > 65535 {
		return 0, errors.New("端口无效")
	}
	return p, nil
}

func truthy(v string) bool { return v == "1" || strings.EqualFold(v, "true") }

// 传输层参数（ws / grpc / h2），vless 和 trojan 共用
func applyTransport(m map[string]any, q url.Values) {
	network := q.Get("type")
	if network == "" || network == "tcp" {
		if q.Get("headerType") == "http" {
			m["network"] = "http"
		}
		return
	}
	m["network"] = network
	switch network {
	case "ws", "httpupgrade":
		opts := map[string]any{"path": orDefault(q.Get("path"), "/")}
		if h := q.Get("host"); h != "" {
			opts["headers"] = map[string]any{"Host": h}
		}
		if network == "httpupgrade" {
			m["network"] = "ws"
			opts["v2ray-http-upgrade"] = true
		}
		m["ws-opts"] = opts
	case "grpc":
		m["grpc-opts"] = map[string]any{"grpc-service-name": q.Get("serviceName")}
	case "h2":
		opts := map[string]any{"path": orDefault(q.Get("path"), "/")}
		if h := q.Get("host"); h != "" {
			opts["host"] = []string{h}
		}
		m["h2-opts"] = opts
	}
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func parseVless(s string) (map[string]any, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	p, err := port(u)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	m := map[string]any{
		"name": name(u, u.Hostname()), "type": "vless", "server": u.Hostname(), "port": p,
		"uuid": u.User.Username(), "udp": true,
	}
	if f := q.Get("flow"); f != "" {
		m["flow"] = f
	}
	switch q.Get("security") {
	case "tls":
		m["tls"] = true
	case "reality":
		m["tls"] = true
		m["reality-opts"] = map[string]any{"public-key": q.Get("pbk"), "short-id": q.Get("sid")}
	}
	if sni := q.Get("sni"); sni != "" {
		m["servername"] = sni
	}
	if fp := q.Get("fp"); fp != "" {
		m["client-fingerprint"] = fp
	}
	if truthy(q.Get("allowInsecure")) || truthy(q.Get("insecure")) {
		m["skip-cert-verify"] = true
	}
	applyTransport(m, q)
	return m, nil
}

func parseTrojan(s string) (map[string]any, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	p, err := port(u)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	m := map[string]any{
		"name": name(u, u.Hostname()), "type": "trojan", "server": u.Hostname(), "port": p,
		"password": u.User.Username(), "udp": true,
	}
	if sni := orDefault(q.Get("sni"), q.Get("peer")); sni != "" {
		m["sni"] = sni
	}
	if fp := q.Get("fp"); fp != "" {
		m["client-fingerprint"] = fp
	}
	if truthy(q.Get("allowInsecure")) || truthy(q.Get("insecure")) {
		m["skip-cert-verify"] = true
	}
	applyTransport(m, q)
	return m, nil
}

func parseHy2(s string) (map[string]any, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	p, err := port(u)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	pass := u.User.Username()
	if pw, ok := u.User.Password(); ok {
		pass += ":" + pw
	}
	m := map[string]any{
		"name": name(u, u.Hostname()), "type": "hysteria2", "server": u.Hostname(), "port": p, "password": pass,
	}
	if sni := q.Get("sni"); sni != "" {
		m["sni"] = sni
	}
	if truthy(q.Get("insecure")) {
		m["skip-cert-verify"] = true
	}
	if o := q.Get("obfs"); o != "" {
		m["obfs"] = o
		m["obfs-password"] = q.Get("obfs-password")
	}
	if ports := q.Get("mport"); ports != "" {
		m["ports"] = ports
	}
	return m, nil
}

func parseSS(s string) (map[string]any, error) {
	body := strings.TrimPrefix(s, s[:strings.Index(s, "://")+3])
	frag := ""
	if i := strings.Index(body, "#"); i >= 0 {
		frag, _ = url.PathUnescape(body[i+1:])
		body = body[:i]
	}
	query := ""
	if i := strings.Index(body, "?"); i >= 0 {
		query = body[i+1:]
		body = body[:i]
	}
	body = strings.TrimSuffix(body, "/")

	var userinfo, hostport string
	if at := strings.LastIndex(body, "@"); at >= 0 {
		userinfo, hostport = body[:at], body[at+1:]
		if dec, ok := decodeB64(userinfo); ok && strings.Contains(dec, ":") {
			userinfo = dec
		} else if un, err := url.PathUnescape(userinfo); err == nil {
			userinfo = un
		}
	} else {
		dec, ok := decodeB64(body)
		if !ok {
			return nil, errors.New("ss 链接无法解码")
		}
		at := strings.LastIndex(dec, "@")
		if at < 0 {
			return nil, errors.New("ss 链接格式不对")
		}
		userinfo, hostport = dec[:at], dec[at+1:]
	}
	method, pass, ok := strings.Cut(userinfo, ":")
	if !ok {
		return nil, errors.New("ss 链接缺少加密方式或密码")
	}
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, errors.New("端口无效")
	}
	m := map[string]any{
		"name": orDefault(strings.TrimSpace(frag), host), "type": "ss", "server": host, "port": p,
		"cipher": method, "password": pass, "udp": true,
	}
	if query != "" {
		q, _ := url.ParseQuery(query)
		if plugin := q.Get("plugin"); plugin != "" {
			parts := strings.Split(plugin, ";")
			opts := map[string]any{}
			for _, kv := range parts[1:] {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "obfs":
					opts["mode"] = v
				case "obfs-host":
					opts["host"] = v
				default:
					opts[k] = v
				}
			}
			switch {
			case strings.Contains(parts[0], "obfs"):
				m["plugin"] = "obfs"
			case strings.Contains(parts[0], "v2ray"):
				m["plugin"] = "v2ray-plugin"
			default:
				m["plugin"] = parts[0]
			}
			m["plugin-opts"] = opts
		}
	}
	return m, nil
}

func parseVmess(s string) (map[string]any, error) {
	raw := strings.TrimPrefix(s, s[:strings.Index(s, "://")+3])
	dec, ok := decodeB64(raw)
	if !ok {
		return nil, errors.New("vmess 链接无法解码")
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(dec), &v); err != nil {
		return nil, errors.New("vmess 链接内容不是 JSON")
	}
	str := func(k string) string {
		switch x := v[k].(type) {
		case string:
			return x
		case float64:
			return strconv.Itoa(int(x))
		}
		return ""
	}
	p, _ := strconv.Atoi(str("port"))
	if p <= 0 {
		return nil, errors.New("端口无效")
	}
	aid, _ := strconv.Atoi(str("aid"))
	m := map[string]any{
		"name": orDefault(str("ps"), str("add")), "type": "vmess", "server": str("add"), "port": p,
		"uuid": str("id"), "alterId": aid, "cipher": orDefault(str("scy"), "auto"), "udp": true,
	}
	if str("tls") == "tls" {
		m["tls"] = true
		if sni := orDefault(str("sni"), str("host")); sni != "" {
			m["servername"] = sni
		}
		if fp := str("fp"); fp != "" {
			m["client-fingerprint"] = fp
		}
	}
	q := url.Values{}
	q.Set("type", str("net"))
	q.Set("path", str("path"))
	q.Set("host", str("host"))
	q.Set("serviceName", str("path"))
	q.Set("headerType", str("type"))
	applyTransport(m, q)
	return m, nil
}
