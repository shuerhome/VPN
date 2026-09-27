/*
 * 住宅IP 智能识别
 * 把供应商发来的各种格式拆成 { protocol, host, port, username, password }。
 *
 * 支持：
 *   ip:port:user:pass            user:pass:ip:port
 *   user:pass@ip:port            ip:port@user:pass
 *   socks5://user:pass@ip:port   http(s)://user:pass@host:port
 *   socks://BASE64#备注（小火箭分享格式）
 *   ip port user pass（空格 / Tab / | / , / ; 分隔）
 *   带标签的多行文本：「IP: … 端口: … 账号: … 密码: …」
 *   ip:port（无认证，白名单授权）
 *
 * 浏览器里挂到 window.ProxyParser，Node 里用 module.exports。
 */
(function (root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.ProxyParser = api;
})(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  const SCHEME_MAP = {
    socks: 'socks5', socks5: 'socks5', socks5h: 'socks5', socks4: 'socks5', socks4a: 'socks5',
    http: 'http', https: 'https',
  };

  const IPV4 = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/;
  const IPV6_BRACKET = /^\[([0-9a-fA-F:.]+)\]$/;
  const HOSTNAME = /^(?=.{1,253}$)(?!-)[a-zA-Z0-9-]{1,63}(\.[a-zA-Z0-9-]{1,63})*\.[a-zA-Z]{2,63}$/;

  const LABELS = {
    host: '(?:ip地址|ip|服务器地址|服务器|代理地址|地址|主机|hostname|host|server|address|proxy)',
    port: '(?:端口号|端口|port)',
    username: '(?:用户名|账号|帐号|账户|帐户|用户|username|user|login|account)',
    password: '(?:密码|口令|password|passwd|pass|pwd)',
    protocol: '(?:协议|类型|protocol|type|scheme)',
    expire: '(?:到期时间|到期日期|到期|过期时间|有效期至|有效期|expires?|expiry|expiration)',
  };
  const SEP = '\\s*[:：=＝]\\s*';

  function normalize(text) {
    return String(text || '')
      .replace(/\r\n?/g, '\n')
      .replace(/[：]/g, ':')
      .replace(/[＠]/g, '@')
      .replace(/[｜]/g, '|')
      .replace(/[，]/g, ',')
      .replace(/[；]/g, ';')
      .replace(/ |　/g, ' ');
  }

  function isIPv4(s) { return IPV4.test(s); }
  function isHost(s) { return isIPv4(s) || IPV6_BRACKET.test(s) || HOSTNAME.test(s); }
  function isPort(s) { return /^\d{1,5}$/.test(s) && +s >= 1 && +s <= 65535; }

  function stripHost(h) {
    const m = IPV6_BRACKET.exec(h);
    return m ? m[1] : h;
  }

  function isReservedIPv4(ip) {
    if (!isIPv4(ip)) return false;
    const [a, b] = ip.split('.').map(Number);
    return a === 10 || a === 127 || a === 0 || (a === 172 && b >= 16 && b <= 31) ||
      (a === 192 && b === 168) || (a === 169 && b === 254) || (a === 100 && b >= 64 && b <= 127) || a >= 224;
  }

  function safeDecodeURI(s) {
    try { return decodeURIComponent(s); } catch (e) { return s; }
  }

  function b64decode(s) {
    const t = s.replace(/-/g, '+').replace(/_/g, '/').replace(/\s+/g, '');
    if (!/^[A-Za-z0-9+/]+={0,2}$/.test(t) || t.length < 4) return null;
    const padded = t + '==='.slice((t.length + 3) % 4);
    try {
      let out;
      if (typeof atob === 'function') {
        const bin = atob(padded);
        out = decodeURIComponent(Array.prototype.map.call(bin, c => '%' + c.charCodeAt(0).toString(16).padStart(2, '0')).join(''));
      } else {
        out = Buffer.from(padded, 'base64').toString('utf8');
      }
      return /^[\x20-\x7e -￿]+$/.test(out) ? out : null;
    } catch (e) {
      return null;
    }
  }

  // "host:port" / "[v6]:port" → { host, port }
  function splitHostPort(s) {
    s = s.trim().replace(/\/+$/, '');
    let m = /^(\[[0-9a-fA-F:.]+\]):(\d{1,5})$/.exec(s);
    if (m && isPort(m[2])) return { host: stripHost(m[1]), port: +m[2] };
    const i = s.lastIndexOf(':');
    if (i <= 0) return null;
    const host = s.slice(0, i), port = s.slice(i + 1);
    if (!isHost(host) || !isPort(port)) return null;
    return { host, port: +port };
  }

  // "user:pass" → { username, password }（密码里可以带冒号）
  function splitAuth(s) {
    const i = s.indexOf(':');
    if (i < 0) return { username: safeDecodeURI(s), password: '' };
    return { username: safeDecodeURI(s.slice(0, i)), password: safeDecodeURI(s.slice(i + 1)) };
  }

  function finish(rec, opts) {
    const out = {
      protocol: rec.protocol || opts.defaultProtocol,
      protocolGuessed: !rec.protocol,
      host: rec.host,
      port: rec.port,
      username: rec.username || '',
      password: rec.password || '',
      remark: rec.remark || '',
      expire: rec.expire || '',
      format: rec.format,
      warnings: [],
    };
    if (!out.username && !out.password) out.warnings.push('没有账号密码，可能是 IP 白名单授权');
    else if (!out.password) out.warnings.push('只识别到账号，缺少密码');
    if (isReservedIPv4(out.host)) out.warnings.push('这是内网或保留地址，不能当作落地IP');
    return out;
  }

  // ---- 带标签的块：「IP: 1.2.3.4 端口: 1080 账号: a 密码: b」 ----
  function labelRegex(key) {
    // 标签前不能紧贴英文字母，避免把 "zip:" 当成 "ip:"
    return new RegExp('(?<![A-Za-z])' + LABELS[key] + SEP + '([^\\s,;|]+)', 'i');
  }

  function parseLabeled(block, opts) {
    const found = {};
    for (const key of Object.keys(LABELS)) {
      const m = labelRegex(key).exec(block);
      if (m) found[key] = m[1];
    }
    const hits = ['host', 'port', 'username', 'password'].filter(k => found[k]).length;
    if (hits < 2 || !found.host) return null;

    let host = found.host.replace(/^[a-z0-9]+:\/\//i, '');
    let port = found.port;
    const hp = splitHostPort(host);
    if (hp) { host = hp.host; if (!port) port = String(hp.port); }
    host = stripHost(host);
    if (!isHost(host) && !isHost('[' + host + ']')) return null;
    if (!port || !isPort(port)) return { error: '识别到地址 ' + host + '，但没有有效端口', raw: block };

    const proto = found.protocol && SCHEME_MAP[found.protocol.toLowerCase().replace(/[^a-z0-9]/g, '')];
    return finish({
      protocol: proto, host, port: +port,
      username: found.username, password: found.password,
      expire: found.expire, format: '标签文本',
    }, opts);
  }

  // ---- 单行 ----
  function parseLine(line, opts) {
    let s = line.trim().replace(/^["'`]+|["'`]+$/g, '');
    if (!s) return null;

    let remark = '';
    const sm = /^([a-z][a-z0-9]*):\/\/(.+)$/i.exec(s);
    // 链接里 # 后面是备注；普通格式只认「空格 + #」，因为密码里可能有 #
    const hash = sm ? s.indexOf('#') : s.search(/\s#/);
    if (hash > 0) {
      remark = safeDecodeURI(s.slice(hash).replace(/^\s*#/, '')).trim();
      s = s.slice(0, hash).trim();
    }

    // 1) 带协议头
    if (sm) {
      const protocol = SCHEME_MAP[sm[1].toLowerCase()];
      if (!protocol) return { error: '不支持的协议 ' + sm[1] + '://（落地IP只需要 SOCKS5 / HTTP）', raw: line };
      let rest = s.slice(sm[1].length + 3).split('?')[0].replace(/\/+$/, '');
      let format = sm[1].toLowerCase() + ':// 链接';

      if (!rest.includes('@') && !splitHostPort(rest)) {
        const dec = b64decode(rest);
        if (dec) { rest = dec; format = 'Base64 分享链接'; }
      } else if (rest.includes('@')) {
        const at = rest.lastIndexOf('@');
        const userinfo = rest.slice(0, at);
        if (!userinfo.includes(':')) {
          const dec = b64decode(userinfo);
          if (dec && dec.includes(':')) { rest = dec + rest.slice(at); format = 'Base64 分享链接'; }
        }
      }

      if (rest.includes('@')) {
        const at = rest.lastIndexOf('@');
        const hp = splitHostPort(rest.slice(at + 1));
        if (!hp) return { error: '链接里的地址或端口无效', raw: line };
        return finish({ protocol, ...hp, ...splitAuth(rest.slice(0, at)), remark, format }, opts);
      }
      const hp = splitHostPort(rest);
      if (!hp) return { error: '链接里的地址或端口无效', raw: line };
      return finish({ protocol, ...hp, remark, format }, opts);
    }

    // 2) 含 @：user:pass@ip:port 或 ip:port@user:pass
    if (s.includes('@')) {
      const at = s.lastIndexOf('@');
      const left = s.slice(0, at).trim(), right = s.slice(at + 1).trim();
      const r = splitHostPort(right);
      if (r) return finish({ ...r, ...splitAuth(left), remark, format: '账号:密码@IP:端口' }, opts);
      const firstAt = s.indexOf('@');
      const l = splitHostPort(s.slice(0, firstAt).trim());
      if (l) return finish({ ...l, ...splitAuth(s.slice(firstAt + 1).trim()), remark, format: 'IP:端口@账号:密码' }, opts);
    }

    // 3) 分隔符：空白 / | / , / ;，否则按冒号
    let tokens = s.split(/[\s|,;]+/).filter(Boolean);
    let protocol;
    // 「socks5 1.2.3.4 1080 user pass」这类行首/行尾带协议名的
    tokens = tokens.filter(t => {
      const p = SCHEME_MAP[t.toLowerCase()];
      if (p && !protocol) { protocol = p; return false; }
      return true;
    });
    const bySpace = tokens.length > 1;
    if (!bySpace) tokens = tokens.join('').split(':');

    // 找「地址 + 相邻端口」：优先 IPv4，其次域名（账号里带点时不会被误认成域名）
    const portNear = i => (i + 1 < tokens.length && isPort(tokens[i + 1])) ? i + 1 : (i > 0 && isPort(tokens[i - 1])) ? i - 1 : -1;
    const cands = tokens.map((t, i) => i).filter(i => isHost(tokens[i]));
    if (!cands.length) return { error: '没有找到 IP 或域名', raw: line };
    const withPort = cands.filter(i => portNear(i) >= 0);
    if (!withPort.length) return { error: '找到地址 ' + stripHost(tokens[cands[0]]) + '，但旁边没有端口', raw: line };
    const hi = withPort.find(i => isIPv4(tokens[i])) ?? withPort[0];
    const pi = portNear(hi);
    const host = stripHost(tokens[hi]);

    const rest = tokens.filter((_, i) => i !== hi && i !== pi);
    const glue = bySpace ? ' ' : ':';
    const username = rest[0] || '';
    const password = rest.slice(1).join(glue);

    let format;
    if (!rest.length) format = 'IP:端口（无认证）';
    else if (bySpace) format = '空格 / 分隔符';
    else format = hi === 0 ? 'IP:端口:账号:密码' : '账号:密码:IP:端口';

    return finish({ protocol, host, port: +tokens[pi], username, password, remark, format }, opts);
  }

  function hasLabels(block) {
    return ['host', 'port', 'username', 'password'].filter(k => labelRegex(k).test(block)).length >= 2;
  }

  /**
   * 解析整段粘贴内容，返回 { records, errors }。
   * 带标签的文本按空行分块；其余按行解析。结果按 协议+地址+端口+账号 去重。
   */
  function parse(text, options) {
    const opts = Object.assign({ defaultProtocol: 'socks5' }, options);
    const records = [], errors = [];
    const seen = new Set();

    const push = (r, raw) => {
      if (!r) return;
      if (r.error) { errors.push({ raw: r.raw || raw, error: r.error }); return; }
      const key = [r.protocol, r.host, r.port, r.username].join('|');
      if (seen.has(key)) return;
      seen.add(key);
      r.raw = raw.trim();
      records.push(r);
    };

    const blocks = normalize(text).split(/\n\s*\n/);
    for (const block of blocks) {
      if (!block.trim()) continue;
      if (hasLabels(block)) {
        const r = parseLabeled(block, opts);
        if (r) { push(r, block); continue; }
      }
      for (const line of block.split('\n')) {
        if (!line.trim() || /^\s*(\/\/|;|--)/.test(line)) continue;
        push(parseLine(normalize(line), opts), line);
      }
    }
    return { records, errors };
  }

  return { parse, parseLine: (l, o) => parseLine(normalize(l), Object.assign({ defaultProtocol: 'socks5' }, o)) };
});
