(function () {
  'use strict';

  const $ = (s, el) => (el || document).querySelector(s);
  const $$ = (s, el) => Array.from((el || document).querySelectorAll(s));
  const esc = s => String(s == null ? '' : s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

  if (window.qrcode && qrcode.stringToBytesFuncs && qrcode.stringToBytesFuncs['UTF-8']) {
    qrcode.stringToBytes = qrcode.stringToBytesFuncs['UTF-8'];
  }

  // ---------------- 接口 ----------------
  async function api(method, path, body) {
    const opts = { method, headers: { 'X-Panel': '1' }, credentials: 'same-origin' };
    if (body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(path, opts);
    let data = null;
    try { data = await res.json(); } catch (e) { /* 空响应 */ }
    if (res.status === 401 && path !== '/api/login') { showLogin(); throw new Error('请先登录'); }
    if (!res.ok) throw new Error((data && data.error) || ('HTTP ' + res.status));
    return data;
  }

  // ---------------- 状态 ----------------
  const S = {
    data: null,
    view: 'chains',
    selected: null,
    tab: null,
    showPass: false,
    region: 'all',
    defaultProto: 'socks5',
    paste: { records: [], errors: [], sel: 0 },
    configCache: {},
    editing: null,
  };

  const DAY = 864e5;
  function daysUntil(d) {
    if (!d) return null;
    const t = new Date(); t.setHours(0, 0, 0, 0);
    return Math.round((new Date(d + 'T00:00:00') - t) / DAY);
  }
  function ago(ts) {
    if (!ts) return '从未';
    const s = Math.max(0, Math.round(Date.now() / 1000 - ts));
    if (s < 60) return '刚刚';
    if (s < 3600) return Math.round(s / 60) + ' 分钟前';
    if (s < 86400) return Math.round(s / 3600) + ' 小时前';
    return Math.round(s / 86400) + ' 天前';
  }
  function human(b) {
    b = b || 0;
    if (b >= 1073741824) return (b / 1073741824).toFixed(2) + ' GB';
    if (b >= 1048576) return (b / 1048576).toFixed(1) + ' MB';
    return Math.round(b / 1024) + ' KB';
  }
  function onlineBadge(c) {
    const o = c.online || {};
    if (o.online && (o.ips || []).length > 1) return '<span class="online multi">' + o.ips.length + ' 个IP同时在用</span>';
    if (o.online) return '<span class="online on">在线</span>';
    return '<span class="online">' + (c.last_seen ? ago(c.last_seen) + '在线' : '未连接过') + '</span>';
  }
  function gb(bytes) { return (bytes / 1073741824).toFixed(bytes >= 1073741824 * 100 ? 0 : 2); }
  function dateOf(ts) {
    const d = new Date(ts * 1000);
    return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0');
  }

  const LANG = { US: 'English (US)', GB: 'English (UK)', CA: 'English (CA)', AU: 'English (AU)', JP: '日本語', KR: '한국어', DE: 'Deutsch', FR: 'Français', SG: 'English (SG)', HK: '繁體中文（香港）', TW: '繁體中文（台灣）', BR: 'Português (BR)', ES: 'Español', MX: 'Español (MX)', TH: 'ไทย', VN: 'Tiếng Việt', ID: 'Bahasa Indonesia', MY: 'Bahasa Melayu', PH: 'English (PH)' };

  const ipById = id => S.data.ips.find(r => r.id === id);
  const nodeById = id => S.data.nodes.find(n => n.id === id);
  const airById = id => S.data.airports.find(a => a.id === id);
  const chainOfIp = id => S.data.chains.find(c => c.ip_id === id);
  const expected = ip => ip.baseline_ip || ip.host;
  const ipKind = ip => !ip.country_code ? '' : ip.hosting ? '机房' : ip.mobile ? '移动网络' : '住宅 / ISP';

  // 两条路径各自的状态
  function relayPath(ip) {
    if (!S.data.relay.enabled) return { s: 'pending', t: '中转未开启', d: '在 .env 里设置 RELAY_HOST 后重启面板' };
    if (!ip.checked_at) return { s: 'pending', t: '等待检测', d: '服务器直连住宅IP，测出口' };
    if (ip.check_error) return { s: 'bad', t: '住宅IP 连不上', d: ip.check_error + ' · ' + ago(ip.checked_at) };
    if (ip.last_exit !== expected(ip)) return { s: 'bad', t: '出口IP变了', d: '检测到 ' + ip.last_exit + '，原来是 ' + expected(ip) };
    return { s: 'ok', t: '出口 ' + ip.last_exit, d: ip.check_ms + 'ms · ' + ago(ip.checked_at) };
  }
  function airPath(c, ip) {
    if (!c.check_at) return { s: 'pending', t: '等待检测', d: '手机 → 机场节点 → 住宅IP' };
    if (c.check_error) return { s: 'bad', t: '链路不通', d: c.check_error + ' · ' + ago(c.check_at) };
    if (c.check_exit !== expected(ip)) return { s: 'bad', t: '出口IP不一致', d: '检测到 ' + c.check_exit + '，应为 ' + expected(ip) };
    return { s: 'ok', t: '出口 ' + c.check_exit, d: (c.check_front ? '经 ' + c.check_front + ' · ' : '') + c.check_ms + 'ms · ' + ago(c.check_at) };
  }

  function evaluate(c) {
    const ip = ipById(c.ip_id);
    const fronts = (c.fronts || []).map(nodeById).filter(Boolean);
    const relay = relayPath(ip);
    const air = airPath(c, ip);
    const issues = [];
    [relay, air].forEach((p, i) => { if (p.s === 'bad') issues.push({ s: 'bad', t: (i ? 'Stash 链路：' : '中转：') + p.t }); });
    if (ip.hosting) issues.push({ s: 'warn', t: '落地IP 是机房IP，不建议用于社媒' });
    const d = daysUntil(ip.expire);
    if (d != null && d <= 7) issues.push({ s: 'warn', t: d < 0 ? '住宅IP 已过期' : '住宅IP ' + d + ' 天后到期' });
    let status = 'ok';
    // 主要看中转（小火箭用）；中转没开时看 Stash 链路
    const main = S.data.relay.enabled ? relay : air;
    if (main.s === 'pending') status = 'pending';
    if (issues.some(i => i.s === 'warn') && status === 'ok') status = 'warn';
    if (main.s === 'bad') status = 'bad';
    else if (relay.s === 'bad' || air.s === 'bad') status = status === 'pending' ? 'pending' : 'warn';
    return { ip, fronts, relay, air, issues, status };
  }

  const STATUS_TEXT = { ok: '正常', warn: '需注意', bad: '故障', pending: '待检测' };
  const pill = s => '<span class="pill ' + s + '">' + STATUS_TEXT[s] + '</span>';

  // ---------------- 渲染：链路 ----------------
  function renderNav() {
    $('#navChains').textContent = S.data.chains.length;
    $('#navIps').textContent = S.data.ips.length;
    $('#navNodes').textContent = S.data.nodes.length;
    $('#navSettings').textContent = S.data.telegram.bound ? '' : (S.data.telegram.enabled ? '待绑定' : '');
    const r = S.data.relay;
    $('#relayStatus').textContent = r.enabled ? '中转 ' + r.host + ':' + r.port + ' · ' + (r.status || '启动中') : '中转未开启';
    $('#busyLine').hidden = !S.data.running;
  }

  function renderSummary() {
    const ev = S.data.chains.map(evaluate);
    const n = s => ev.filter(e => e.status === s).length;
    const expiring = S.data.ips.filter(r => { const d = daysUntil(r.expire); return d != null && d <= 7; }).length;
    $('#summary').innerHTML =
      '<div class="stat"><span class="num">' + S.data.chains.length + '</span><span class="lbl">条链路</span></div>' +
      '<div class="stat ok"><span class="num">' + n('ok') + '</span><span class="lbl">正常</span></div>' +
      '<div class="stat warn"><span class="num">' + n('warn') + '</span><span class="lbl">需注意</span></div>' +
      '<div class="stat bad"><span class="num">' + n('bad') + '</span><span class="lbl">故障</span></div>' +
      '<div class="stat warn"><span class="num">' + expiring + '</span><span class="lbl">个住宅IP 7 天内到期</span></div>';
  }

  function renderChains() {
    renderSummary();
    const list = $('#chainList');
    if (!S.data.chains.length) {
      const hasIp = S.data.ips.length > 0;
      list.innerHTML = '<li class="empty"><b>还没有链路</b><span>' + (hasIp ? '点「新建链路」，给一台 iPhone 绑定一个住宅IP。' : '先去「住宅IP」粘贴你的静态住宅IP，再回来新建链路。') + '</span>' +
        (hasIp ? '<button class="btn primary" type="button" data-act="new-chain">新建链路</button>' : '<a class="btn primary" href="#ips">添加住宅IP</a>') + '</li>';
      $('#detail').innerHTML = '<section><span class="eyebrow">怎么用</span><ol class="export-note" style="margin:0;padding-left:18px;display:grid;gap:4px">' +
        '<li>「机场订阅」里添加机场订阅链接（Stash 链路需要）。</li><li>「住宅IP」里粘贴静态住宅IP。</li><li>新建链路，绑定设备和IP。</li><li>用 iPhone 扫链路详情里的二维码。</li></ol></section>';
      return;
    }
    if (!S.data.chains.some(c => c.id === S.selected)) S.selected = S.data.chains[0].id;
    list.innerHTML = S.data.chains.map(c => {
      const e = evaluate(c);
      const top = e.issues[0];
      const foot = top ? '<div class="row-foot ' + top.s + '">' + esc(top.t) + '</div>'
        : '<div class="row-foot">' + esc(e.status === 'pending' ? '检测中…' : (S.data.relay.enabled ? e.relay : e.air).t + ' · 与绑定一致') + '</div>';
      const f = e.fronts[0];
      const front = '<div class="hop front' + (f ? '' : ' down') + '"><span class="k">前置 · ' + (c.front_mode === 'fixed' ? '固定' : '自动 ' + e.fronts.filter(n => n.delay_ms > 0).length + '/' + e.fronts.length) + '</span><span class="v">' + esc(f ? f.name : '无可用节点') + '</span></div>';
      const land = '<div class="hop land"><span class="k">落地 · ' + esc(ipKind(e.ip) || '住宅IP') + '</span><span class="v"><span class="ip">' + esc(e.ip.host) + '</span> ' + esc(e.ip.city || '') + '</span></div>';
      const ms = c.check_ms ? c.check_ms + 'ms' : '';
      return '<li class="chain-row" role="option" tabindex="0" data-id="' + c.id + '" aria-selected="' + (c.id === S.selected) + '">' +
        '<div class="row-top"><span class="dev">' + esc(c.device) + '</span>' + (S.data.relay.enabled ? onlineBadge(c) : '') + '<span class="accts">' + (c.accounts || []).map(a => esc(a.p + ' ' + a.h)).join(' · ') + '</span>' + pill(e.status) + '</div>' +
        '<div class="hops">' + front + '<div class="wire' + (e.air.s === 'bad' ? ' broken' : '') + '"><span>' + ms + '</span></div>' + land + '</div>' +
        foot + '</li>';
    }).join('');
    renderDetail();
  }

  function renderDetail() {
    const c = S.data.chains.find(x => x.id === S.selected);
    if (!c) return;
    const e = evaluate(c);
    const ip = e.ip;
    const icon = { ok: '✓', warn: '!', bad: '×', info: 'i', pending: '…' };
    const pathRow = (label, p) => '<div class="path"><span class="pill ' + p.s + '">' + label + '</span><div><div class="t">' + esc(p.t) + '</div><div class="d">' + esc(p.d) + '</div></div></div>';
    const checks = [];
    if (ip.country_code) checks.push(ip.hosting
      ? { s: 'bad', t: 'IP 类型：机房', d: ip.asn + ' ' + ip.isp + ' 属于数据中心网段，社媒容易识别' }
      : { s: 'ok', t: 'IP 类型：' + ipKind(ip), d: [ip.asn, ip.isp, ip.proxy_flag ? '被标记为代理' : ''].filter(Boolean).join(' · ') });
    checks.push({ s: 'ok', t: '断线不回落', d: '链路断了就断网，不会漏出机场IP或 VPS 的IP' });
    const d = daysUntil(ip.expire);
    if (d != null) checks.push(d <= 7 ? { s: 'warn', t: d < 0 ? '已过期' : d + ' 天后到期', d: '到期日 ' + ip.expire } : { s: 'ok', t: '有效期还有 ' + d + ' 天', d: '到期日 ' + ip.expire });
    else checks.push({ s: 'info', t: '没填到期日', d: '在「住宅IP」里填上，快到期时会提醒' });
    if (ip.timezone) checks.push({ s: 'info', t: '手机设置请对齐', d: '时区 ' + ip.timezone + ' · 语言 ' + (LANG[ip.country_code] || '当地语言') + ' · 地区 ' + ip.country + '。这几项在 iPhone 设置里手动改。' });

    const tabs = [];
    if (S.data.relay.enabled) tabs.push(['relay', '小火箭扫码（推荐）']);
    tabs.push(['stash', 'Stash']);
    tabs.push(['manual', '手动代理链']);
    if (!tabs.some(t => t[0] === S.tab)) S.tab = tabs[0][0];

    $('#detail').innerHTML =
      '<section>' +
        '<div class="detail-head"><h2>' + esc(c.device) + '</h2>' + pill(e.status) +
          '<div class="actions">' +
            '<button class="btn small" type="button" data-act="check" data-id="' + c.id + '">立即检测</button>' +
            '<button class="btn small" type="button" data-act="edit-chain" data-id="' + c.id + '">编辑</button>' +
          '</div></div>' +
        '<div class="acct-list">' + ((c.accounts || []).map(a => '<span class="acct"><b>' + esc(a.p) + '</b>' + esc(a.h) + '</span>').join('') || '<span class="hint">没有填账号</span>') + '</div>' +
      '</section>' +
      usageSection(c) +
      '<section aria-label="出口检测"><span class="eyebrow">出口检测 · 平台看到的IP应当始终是 <span class="ip">' + esc(expected(ip)) + '</span></span><div class="paths">' +
        (S.data.relay.enabled ? pathRow('中转', e.relay) : '') + pathRow('Stash', e.air) +
      '</div>' +
      ((e.relay.t === '出口IP变了' || e.air.t === '出口IP不一致') ? '<div class="code-tools"><span class="hint warn">确认是供应商换了线路、新IP可以用，再点右边。</span><button class="btn small" type="button" data-act="baseline" data-id="' + ip.id + '">以当前出口为准</button></div>' : '') +
      '</section>' +
      '<section aria-label="环境检查"><span class="eyebrow">环境检查</span><ul class="checks">' +
        checks.map(k => '<li class="' + k.s + '"><span class="ic" aria-hidden="true">' + icon[k.s] + '</span><div><div class="t">' + esc(k.t) + '</div><div class="d">' + esc(k.d) + '</div></div></li>').join('') +
      '</ul></section>' +
      '<section aria-label="导入到手机"><span class="eyebrow">导入到手机</span>' +
        '<div class="path"><span class="pill ok">导入页</span><div><div class="t">发给操作这台手机的同事</div><div class="d">打开就是二维码和手机设置说明，不需要面板密码。只发给这台手机的使用者。</div></div>' +
          '<div class="actions"><button class="btn small" type="button" data-copy="' + esc(c.page_url) + '">复制链接</button><a class="btn small" href="' + esc(c.page_url) + '" target="_blank" rel="noopener">打开</a></div></div>' +
        '<div class="tabs" role="tablist">' + tabs.map(t => '<button type="button" role="tab" data-tab="' + t[0] + '" aria-selected="' + (S.tab === t[0]) + '">' + t[1] + '</button>').join('') + '</div>' +
        '<div id="exportPane"></div>' +
        '<div class="code-tools"><span class="hint">二维码泄露了？重置后旧二维码和订阅立即失效。</span><div class="actions">' +
          '<button class="btn small" type="button" data-act="rotate" data-id="' + c.id + '">重置二维码</button>' +
          '<button class="btn small danger" type="button" data-act="delete-chain" data-id="' + c.id + '">删除链路</button></div></div>' +
      '</section>';
    renderExport(c, e);
  }

  function usageSection(c) {
    if (!S.data.relay.enabled) return '';
    const o = c.online || {};
    const t = c.traffic || {};
    const ips = o.ips || [];
    return '<section aria-label="使用情况"><span class="eyebrow">使用情况（中转）</span>' +
      (ips.length > 1 ? '<div class="alert-box">这个二维码正被 ' + ips.length + ' 个IP同时使用：' + ips.map(esc).join('、') + '。如果不是你安排的，点下方「重置二维码」。</div>' : '') +
      '<div class="usage">' +
        '<div><b>' + (o.online ? '在线' : '离线') + '</b><span>' + usageLine(c, o, ips) + '</span></div>' +
        '<div><b>' + human((t.today_up || 0) + (t.today_down || 0)) + '</b><span>今天 · ↑' + human(t.today_up) + ' ↓' + human(t.today_down) + '</span></div>' +
        '<div><b>' + human((t.d30_up || 0) + (t.d30_down || 0)) + '</b><span>近 30 天</span></div>' +
      '</div></section>';
  }

  function usageLine(c, o, ips) {
    const src = ips.length ? ips : (c.last_src ? [c.last_src] : []);
    const parts = [];
    if (o.online) parts.push(o.conns ? o.conns + ' 个连接' : '最近一分钟在用');
    else if (c.last_seen) parts.push('最近 ' + ago(c.last_seen));
    else return '还没连过';
    if (src.length) parts.push('来自 ' + src.map(esc).join('、'));
    return parts.join(' · ');
  }

  function renderSettings() {
    const tg = S.data.telegram;
    const r = S.data.relay;
    let tgBody;
    if (!tg.enabled) {
      tgBody = '<p class="hint">还没配置。步骤：</p><ol class="export-note" style="margin:0;padding-left:18px;display:grid;gap:4px">' +
        '<li>在 Telegram 里找 <b>@BotFather</b>，发送 /newbot，按提示起名，拿到一串 Token。</li>' +
        '<li>在服务器上编辑 <span class="mono">/opt/luodi/.env</span>，加一行 <span class="mono">TG_BOT_TOKEN=你的Token</span>。</li>' +
        '<li>执行 <span class="mono">cd /opt/luodi && docker compose up -d</span> 重启面板，回到这里绑定。</li></ol>';
    } else if (!tg.bound) {
      tgBody = '<p>在 Telegram 里打开机器人' + (tg.bot ? ' <b>@' + esc(tg.bot) + '</b>' : '') + '，发送：</p>' +
        '<div class="sub-url"><code class="bind-code">/bind ' + esc(tg.bind_code) + '</code><button class="btn small" type="button" data-copy="/bind ' + esc(tg.bind_code) + '">复制</button></div>' +
        '<p class="hint">绑定码只能用一次。绑定后这个页面会自动刷新。</p>';
    } else {
      tgBody = '<p>已绑定' + (tg.bot ? ' <b>@' + esc(tg.bot) + '</b>' : '') + '。会通知这些事：</p>' +
        '<ul class="export-note" style="margin:0;padding-left:18px;display:grid;gap:2px"><li>住宅IP连不上、出口IP变了，以及恢复</li><li>二维码被多个IP同时使用</li><li>住宅IP、机场 7 天内到期；机场流量用到 90%</li><li>机场订阅同步失败</li><li>每天凌晨 4 点自动备份数据库</li></ul>' +
        '<p class="hint">在 Telegram 里发 /status 看链路概况，/backup 立即备份。</p>' +
        '<div class="actions"><button class="btn small" type="button" data-act="tg-test">发测试消息</button><button class="btn small danger" type="button" data-act="tg-unbind">解除绑定</button></div>';
    }
    $('#settingsGrid').innerHTML =
      '<div class="panel"><h2>Telegram 通知 ' + (tg.bound ? '<span class="pill ok">已绑定</span>' : tg.enabled ? '<span class="pill warn">待绑定</span>' : '<span class="pill pending">未配置</span>') + '</h2>' + tgBody + '</div>' +
      '<div class="panel"><h2>备份</h2>' +
        '<p class="hint">备份包含全部住宅IP、链路和机场订阅。密码和订阅链接是加密的，恢复时需要服务器 <span class="mono">.env</span> 里的 <span class="mono">SECRET_KEY</span>，请把 .env 另外保存好。</p>' +
        '<dl class="kv"><dt>上次自动备份</dt><dd>' + esc(tg.last_backup || '还没有') + '</dd></dl>' +
        '<div class="actions"><a class="btn small" href="/api/backup">下载备份</a>' + (tg.bound ? '<button class="btn small" type="button" data-act="backup-tg">发送到 Telegram</button>' : '') + '</div></div>' +
      '<div class="panel"><h2>中转（小火箭扫码） ' + (r.enabled ? '<span class="pill ' + (r.status === '运行中' ? 'ok' : 'warn') + '">' + esc(r.status || '启动中') + '</span>' : '<span class="pill pending">未开启</span>') + '</h2>' +
        (r.enabled ? '<dl class="kv"><dt>地址</dt><dd class="mono">' + esc(r.host) + ':' + r.port + '</dd><dt>协议</dt><dd>VLESS Reality（伪装 ' + esc(r.sni) + '）</dd><dt>在线手机</dt><dd>' + S.data.chains.filter(c => c.online && c.online.online).length + ' / ' + S.data.chains.length + '</dd></dl>' +
          '<p class="hint">手机连不上时，先确认 VPS 防火墙和 Hostinger 控制台放行了 ' + r.port + '/tcp。</p>'
          : '<p class="hint">在 .env 里设置 RELAY_HOST（VPS 公网 IP）后重启面板即可开启。</p>') + '</div>';
  }

  function qrSvg(text) {
    if (typeof qrcode !== 'function') return '二维码';
    try {
      const q = qrcode(0, 'M');
      q.addData(text);
      q.make();
      return q.createSvgTag({ cellSize: 3, margin: 2, scalable: true });
    } catch (err) { return '内容太长，无法生成二维码'; }
  }

  function copyRow(text) {
    return '<div class="sub-url"><code>' + esc(text) + '</code><button class="btn small" type="button" data-copy="' + esc(text) + '">复制</button></div>';
  }

  async function renderExport(c, e) {
    const pane = $('#exportPane');
    if (!pane) return;
    if (S.tab === 'relay') {
      pane.innerHTML = '<div class="export-grid"><div style="display:grid;gap:10px;min-width:0">' + copyRow(c.relay_link) +
        '<div class="export-note">用小火箭扫右侧二维码（首页右上角的扫码图标），导入后选中这个节点、打开开关即可。Loon、Stash、Quantumult X 也能扫。' +
        '<ol><li>手机 → 你的 VPS（' + esc(S.data.relay.host) + '）→ 住宅IP <b class="ip">' + esc(e.ip.host) + '</b> → 平台。</li>' +
        '<li>小火箭「全局路由」选「代理」，这台手机所有流量都走住宅IP。</li>' +
        '<li>这一路不经过机场。二维码只给这台手机用，不要转发。</li></ol></div></div>' +
        '<div class="qr" aria-label="中转节点二维码">' + qrSvg(c.relay_link) + '</div></div>';
      return;
    }
    let cfg = S.configCache[c.id];
    if (!cfg) {
      pane.innerHTML = '<p class="hint">正在生成配置…</p>';
      try { cfg = S.configCache[c.id] = await api('GET', '/api/chains/' + c.id + '/config'); } catch (err) { pane.innerHTML = '<p class="hint warn">' + esc(err.message) + '</p>'; return; }
      if (S.selected !== c.id) return;
    }
    if (S.tab === 'manual') {
      const f = e.fronts.find(n => n.delay_ms > 0) || e.fronts[0];
      pane.innerHTML = '<div class="export-grid"><div style="display:grid;gap:10px;min-width:0">' + copyRow(cfg.rocket) +
        '<div class="export-note">不用中转、想在小火箭或 Loon 里自己建代理链时用：' +
        '<ol><li>扫右侧二维码导入住宅IP节点，识别不了就手动填 IP、端口、账号、密码。</li>' +
        '<li>在「代理链 / 链式代理」里依次加入：先机场节点 <b>' + esc(f ? f.name : '（先添加机场订阅）') + '</b>，再住宅IP <b class="ip">' + esc(e.ip.host) + '</b>。</li>' +
        '<li>全局路由选「代理」，选中这条代理链。</li></ol></div></div>' +
        '<div class="qr" aria-label="住宅IP节点二维码">' + qrSvg(cfg.rocket) + '</div></div>';
      return;
    }
    // stash
    const shown = S.showPass ? cfg.clash : maskPass(cfg.clash, e.ip);
    pane.innerHTML = (cfg.error ? '<p class="hint warn">' + esc(cfg.error) + '</p>' : '') +
      '<div class="export-grid"><div style="display:grid;gap:8px;min-width:0">' + copyRow(c.sub_url) +
      '<p class="export-note">Stash 扫码或粘贴订阅链接导入。走「手机 → 机场节点 → 住宅IP」，前置节点坏了自动切换，落地IP不变。面板改了配置，Stash 更新订阅就会同步。</p></div>' +
      '<div class="qr" aria-label="订阅二维码">' + qrSvg(c.sub_url) + '</div></div>' +
      (cfg.clash ? '<div class="code-tools"><span class="hint">配置预览</span><div class="actions">' +
        '<div class="seg" role="group" aria-label="密码显示"><button type="button" data-act="pass" aria-pressed="' + !S.showPass + '">隐藏密码</button><button type="button" data-act="pass" aria-pressed="' + S.showPass + '">显示</button></div>' +
        '<button class="btn small" type="button" data-act="copy-config" data-id="' + c.id + '">复制配置</button></div></div>' +
        '<pre class="config" id="configPre">' + highlight(shown) + '</pre>' : '');
  }

  function maskPass(text, ip) {
    return text.split('\n').map(l => /^\s*(password|uuid):/.test(l) ? l.replace(/:\s.*$/, ': ••••••••') : l).join('\n');
  }

  function highlight(text) {
    return text.split('\n').map(line => {
      const h = esc(line);
      if (/^\s*#/.test(line)) return '<span class="c">' + h + '</span>';
      if (/dialer-proxy/.test(line)) return h.replace(/(dialer-proxy)/, '<span class="l">$1</span>');
      return h.replace(/^(\s*-?\s*)([a-z-]+)(:)/i, '$1<span class="k">$2</span>$3');
    }).join('\n');
  }

  // ---------------- 渲染：住宅IP ----------------
  function renderIps() {
    const rows = S.data.ips.map(r => {
      const d = daysUntil(r.expire);
      const c = chainOfIp(r.id);
      const exitCell = !r.checked_at ? '<span class="hint">检测中…</span>'
        : r.check_error ? '<span class="mismatch">连不上</span><span class="sub">' + esc(r.check_error) + '</span>'
        : '<span class="ip' + (r.last_exit !== expected(r) ? ' mismatch' : '') + '">' + esc(r.last_exit) + '</span><span class="sub">' + (r.last_exit !== expected(r) ? '原为 ' + esc(expected(r)) : r.check_ms + 'ms · ' + ago(r.checked_at)) + '</span>';
      const where = r.country_code ? '<span class="tag cc">' + esc(r.country_code) + '</span> ' + esc(r.city || r.country) + '<span class="sub">' + esc([r.isp, r.asn].filter(Boolean).join(' ')) + '</span>' : '<span class="hint">—</span>';
      const kind = r.country_code ? (r.hosting ? '<span class="pill bad">机房</span>' : r.mobile ? '<span class="pill ok">移动网络</span>' : '<span class="pill ok">住宅 / ISP</span>') + (r.proxy_flag ? '<span class="sub">被标记为代理</span>' : '') : '<span class="hint">—</span>';
      return '<tr>' +
        '<td><span class="ip">' + esc(r.host) + ':' + r.port + '</span><span class="sub">' + r.protocol.toUpperCase() + (r.username ? ' · ' + esc(r.username) : ' · 无认证') + (r.remark ? ' · ' + esc(r.remark) : '') + '</span></td>' +
        '<td>' + exitCell + '</td><td>' + where + '</td><td>' + kind + '</td>' +
        '<td><input type="date" value="' + esc(r.expire) + '" data-expire="' + r.id + '" aria-label="到期日"><span class="sub" style="color:' + (d != null && d <= 7 ? 'var(--warn)' : 'var(--muted)') + '">' + (d == null ? '未填写' : d < 0 ? '已过期' : d + ' 天后') + '</span></td>' +
        '<td>' + (c ? esc(c.device) : '<span class="hint">空闲</span>') + '</td>' +
        '<td class="mono">' + (c && c.traffic ? human((c.traffic.d30_up || 0) + (c.traffic.d30_down || 0)) : '<span class="hint">—</span>') + '</td>' +
        '<td><div class="row-actions"><button class="btn small" type="button" data-act="ip-check" data-id="' + r.id + '">检测</button>' +
          (c ? '' : '<button class="btn small danger" type="button" data-act="ip-delete" data-id="' + r.id + '">删除</button>') + '</div></td>' +
      '</tr>';
    }).join('');
    $('#ipRows').innerHTML = rows || '<tr><td colspan="8" class="hint" style="text-align:center;padding:24px">还没有住宅IP，在上面粘贴添加。</td></tr>';
  }

  const F = { host: '#fHost', port: '#fPort', username: '#fUser', password: '#fPass', remark: '#fRemark', expire: '#fExpire' };
  function setProto(v) { $$('#fProto button').forEach(b => b.setAttribute('aria-pressed', String(b.dataset.v === v))); }
  function getProto() { const b = $('#fProto button[aria-pressed="true"]'); return b ? b.dataset.v : 'socks5'; }

  function fillFields(r, flash) {
    Object.keys(F).forEach(k => {
      const el = $(F[k]);
      const v = r ? (r[k] == null ? '' : String(r[k])) : '';
      if (el.value === v) return;
      el.value = v;
      if (flash && v) { el.classList.remove('flash'); void el.offsetWidth; el.classList.add('flash'); }
    });
    setProto(r ? r.protocol : S.defaultProto);
  }

  function runParse(flash) {
    const res = ProxyParser.parse($('#paste').value, { defaultProtocol: S.defaultProto });
    const prev = S.paste.records[S.paste.sel];
    S.paste = { records: res.records, errors: res.errors, sel: Math.min(S.paste.sel, Math.max(0, res.records.length - 1)) };
    const cur = S.paste.records[S.paste.sel];
    renderParsed();
    const changed = !prev || !cur || prev.host !== cur.host || prev.port !== cur.port || prev.username !== cur.username || prev.password !== cur.password;
    if (changed) fillFields(cur, flash);
  }

  function renderParsed() {
    const { records, errors, sel } = S.paste;
    $('#parseCount').textContent = records.length ? '识别出 ' + records.length + ' 个' + (errors.length ? '，' + errors.length + ' 行没认出' : '') : (errors.length ? errors.length + ' 行没认出' : '');
    const html = records.map((r, i) =>
      '<li role="option" tabindex="0" data-i="' + i + '" aria-selected="' + (i === sel) + '">' +
        '<div class="fmt"><span class="n">#' + (i + 1) + '</span><span>' + esc(r.format) + '</span><span class="tag">' + r.protocol.toUpperCase() + (r.protocolGuessed ? ' · 默认' : '') + '</span></div>' +
        '<div class="toks"><span class="tok"><i>IP</i>' + esc(r.host) + '</span><span class="tok"><i>端口</i>' + esc(r.port) + '</span>' +
          (r.username ? '<span class="tok"><i>账户</i>' + esc(r.username) + '</span>' : '') +
          (r.password ? '<span class="tok"><i>密码</i>' + esc(r.password) + '</span>' : '') +
          (r.expire ? '<span class="tok"><i>到期</i>' + esc(r.expire) + '</span>' : '') +
          (r.remark ? '<span class="tok"><i>备注</i>' + esc(r.remark) + '</span>' : '') + '</div>' +
        r.warnings.map(w => '<div class="warn-line">' + esc(w) + '</div>').join('') +
      '</li>').join('') +
      errors.map(er => '<li class="err"><div class="fmt">没认出：' + esc(er.error) + '</div><div class="toks"><span class="tok">' + esc(er.raw.trim().slice(0, 60)) + '</span></div></li>').join('');
    $('#parsed').innerHTML = html || '<li class="err"><div class="fmt" style="color:var(--muted)">粘贴后这里会显示识别结果，也可以直接在下面手动填写。</div></li>';
    $('#addIps').textContent = records.length > 1 ? '全部加入IP库（' + records.length + ' 个）' : '加入IP库';
  }

  function recordFromFields() {
    const port = parseInt($('#fPort').value, 10);
    return { protocol: getProto(), host: $('#fHost').value.trim(), port: isNaN(port) ? 0 : port, username: $('#fUser').value.trim(), password: $('#fPass').value, remark: $('#fRemark').value.trim(), expire: $('#fExpire').value };
  }

  function syncFieldsToRecord() {
    const r = S.paste.records[S.paste.sel];
    if (!r) return;
    Object.assign(r, recordFromFields());
    renderParsed();
  }

  async function addIps() {
    let list = S.paste.records.map(r => ({ protocol: r.protocol, host: r.host, port: +r.port, username: r.username, password: r.password, remark: r.remark || '', expire: r.expire || '' }));
    if (!list.length) {
      const r = recordFromFields();
      if (!r.host || !(r.port >= 1 && r.port <= 65535)) { toast('先填好 IP 和端口'); $('#fHost').focus(); return; }
      list = [r];
    }
    const btn = $('#addIps');
    btn.disabled = true;
    try {
      const res = await api('POST', '/api/ips', { records: list });
      $('#paste').value = '';
      S.paste = { records: [], errors: [], sel: 0 };
      renderParsed();
      fillFields(null, false);
      toast(res.added.length ? '已加入 ' + res.added.length + ' 个，正在检测出口IP和地区' + (res.duplicates ? '；' + res.duplicates + ' 个已存在' : '') : '都已经在IP库里了');
      await refresh();
    } catch (err) { toast(err.message); } finally { btn.disabled = false; }
  }

  // ---------------- 渲染：机场 ----------------
  function renderAirports() {
    const cards = S.data.airports.map(a => {
      const used = a.upload + a.download;
      const pct = a.total ? Math.min(100, used / a.total * 100) : 0;
      const d = a.expire ? Math.round((a.expire * 1000 - Date.now()) / DAY) : null;
      return '<div class="panel air">' +
        '<div class="air-top"><h3>' + esc(a.name) + '</h3><button class="btn small" type="button" data-act="air-sync" data-id="' + a.id + '">更新</button><button class="btn small danger" type="button" data-act="air-delete" data-id="' + a.id + '">删除</button></div>' +
        '<code>' + esc(a.url_masked) + '</code>' +
        (a.last_error ? '<span class="hint warn">' + esc(a.last_error) + '</span>' : '') +
        '<div class="meter" title="已用流量"><i style="width:' + pct.toFixed(1) + '%"></i></div>' +
        '<div class="air-stats">' +
          '<div><b>' + (a.total ? gb(used) + ' / ' + gb(a.total) : '—') + '</b>GB 已用' + (a.total ? '，剩 ' + gb(a.total - used) : '') + '</div>' +
          '<div class="' + (d != null && d <= 10 ? 'warn' : '') + '"><b>' + (d == null ? '—' : dateOf(a.expire)) + '</b>' + (d == null ? '到期未知' : d + ' 天后到期') + '</div>' +
          '<div><b>' + a.alive + ' / ' + a.nodes + '</b>节点在线</div>' +
        '</div>' +
        '<span class="hint">' + ago(a.synced_at) + '同步' + (a.insecure ? ' · 已忽略证书错误' : '') + '</span>' +
      '</div>';
    }).join('');
    $('#airGrid').innerHTML = cards +
      '<div class="panel air add"><span class="eyebrow">添加机场订阅</span>' +
      '<form id="airForm"><input id="airName" placeholder="名称（可选）" aria-label="机场名称" style="flex:0 1 120px;font-family:inherit"><input id="airUrl" placeholder="粘贴订阅链接" aria-label="订阅链接" spellcheck="false" required>' +
      '<label><input type="checkbox" id="airInsecure">忽略证书错误</label><button class="btn primary" type="submit" id="airSubmit">添加</button></form>' +
      '<span class="hint">订阅地址是纯 IP（例如 https://45.x.x.x/…）时会自动勾选忽略证书错误。</span></div>';

    const codes = Array.from(new Set(S.data.nodes.map(n => n.country || '??'))).sort();
    if (S.region !== 'all' && !codes.includes(S.region)) S.region = 'all';
    $('#regionChips').innerHTML = ['all'].concat(codes).map(r => '<button type="button" data-region="' + esc(r) + '" aria-pressed="' + (S.region === r) + '">' + (r === 'all' ? '全部 ' + S.data.nodes.length : (r === '??' ? '未识别' : r) + ' ' + S.data.nodes.filter(n => (n.country || '??') === r).length) + '</button>').join('');

    const useCount = {};
    S.data.chains.forEach(c => (c.fronts || []).forEach(id => { useCount[id] = (useCount[id] || 0) + 1; }));
    const rows = S.data.nodes.filter(n => S.region === 'all' || (n.country || '??') === S.region).map(n => {
      const lat = n.delay_ms > 0 ? '<span class="lat"><span class="bar"><i style="width:' + Math.min(100, n.delay_ms / 3) + '%"></i></span>' + n.delay_ms + ' ms</span>'
        : n.delay_ms < 0 ? '<span class="lat dead">超时</span>' : '<span class="hint">未测</span>';
      const air = airById(n.airport_id);
      return '<tr><td>' + (n.country ? '<span class="tag cc">' + esc(n.country) + '</span> ' : '') + esc(n.name) + '</td><td>' + esc(air ? air.name : '') + '</td><td class="mono">' + esc(n.type) + '</td><td>' + lat + '</td><td>' + (useCount[n.id] ? useCount[n.id] + ' 条链路' : '<span class="hint">—</span>') + '</td></tr>';
    }).join('');
    $('#nodeRows').innerHTML = rows || '<tr><td colspan="5" class="hint" style="text-align:center;padding:24px">添加机场订阅后，节点会显示在这里。</td></tr>';
  }

  // ---------------- 新建 / 编辑链路 ----------------
  function openSheet(chain) {
    S.editing = chain || null;
    $('#h-sheet').textContent = chain ? '编辑 ' + chain.device : '新建链路';
    $('#sheetSubmit').textContent = chain ? '保存' : '创建并检测';
    $('#sheetErr').textContent = '';
    $('#nDevice').value = chain ? chain.device : 'iPhone ' + String(S.data.chains.length + 1).padStart(2, '0');
    const acc = chain ? chain.accounts || [] : [];
    $('#nPlatform').value = acc[0] ? acc[0].p : 'TikTok';
    $('#nHandle').value = acc.map(a => a.h).join(' ');
    $('#nIpField').hidden = !!chain;
    $('#nIp').innerHTML = S.data.ips.map(r => {
      const c = chainOfIp(r.id);
      const label = r.host + ':' + r.port + '  ' + (r.country_code ? r.country_code + ' ' + (r.city || '') : '待检测') + (r.hosting ? '  （机房IP，不推荐）' : '') + (c ? '  — 已绑定 ' + c.device : '');
      return '<option value="' + r.id + '"' + (c ? ' disabled' : '') + '>' + esc(label) + '</option>';
    }).join('') || '<option value="">先去「住宅IP」添加</option>';
    const free = S.data.ips.find(r => !chainOfIp(r.id) && !r.hosting) || S.data.ips.find(r => !chainOfIp(r.id));
    if (free) $('#nIp').value = String(free.id);
    const alive = S.data.nodes.filter(n => n.delay_ms >= 0).sort((a, b) => (a.country || '').localeCompare(b.country || '') || a.delay_ms - b.delay_ms);
    $('#nNode').innerHTML = alive.map(n => '<option value="' + n.id + '">' + esc((n.country ? n.country + ' · ' : '') + n.name + (n.delay_ms > 0 ? ' · ' + n.delay_ms + 'ms' : '')) + '</option>').join('') || '<option value="">没有可用节点</option>';
    const mode = chain ? chain.front_mode : 'auto';
    $('input[name="front"][value="' + mode + '"]').checked = true;
    if (chain && chain.front_node_id) $('#nNode').value = String(chain.front_node_id);
    $('input[name="route"][value="' + (chain ? chain.route : 'global') + '"]').checked = true;
    $('#sheetBg').hidden = false;
    $('#nDevice').focus();
  }
  function closeSheet() { $('#sheetBg').hidden = true; }

  async function submitSheet(ev) {
    ev.preventDefault();
    const platform = $('#nPlatform').value;
    const body = {
      device: $('#nDevice').value.trim(),
      accounts: $('#nHandle').value.split(/[\s,，]+/).filter(Boolean).map(h => ({ p: platform, h })),
      front_mode: $('input[name="front"]:checked').value,
      front_node_id: parseInt($('#nNode').value, 10) || 0,
      route: $('input[name="route"]:checked').value,
    };
    const btn = $('#sheetSubmit');
    btn.disabled = true;
    try {
      if (S.editing) {
        await api('PUT', '/api/chains/' + S.editing.id, body);
        delete S.configCache[S.editing.id];
        toast('已保存');
      } else {
        body.ip_id = parseInt($('#nIp').value, 10) || 0;
        const res = await api('POST', '/api/chains', body);
        S.selected = res.id;
        toast('已创建，正在检测。扫右侧二维码导入手机。');
      }
      closeSheet();
      await refresh();
    } catch (err) { $('#sheetErr').textContent = err.message; } finally { btn.disabled = false; }
  }

  // ---------------- 通用 ----------------
  let toastTimer;
  function toast(msg) {
    const t = $('#toast');
    t.textContent = msg;
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { t.hidden = true; }, 3200);
  }

  function copy(text) {
    const fallback = () => {
      const ta = document.createElement('textarea');
      ta.value = text; document.body.appendChild(ta); ta.select();
      try { document.execCommand('copy'); toast('已复制'); } catch (e) { toast('复制失败，请手动选择'); }
      ta.remove();
    };
    if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(() => toast('已复制'), fallback);
    else fallback();
  }

  function render() {
    if (!S.data) return;
    renderNav();
    if (S.view === 'chains') renderChains();
    if (S.view === 'ips') renderIps();
    if (S.view === 'airports') renderAirports();
    if (S.view === 'settings') renderSettings();
  }

  function show(view) {
    if (!['chains', 'ips', 'airports', 'settings'].includes(view)) view = 'chains';
    S.view = view;
    $$('.view').forEach(v => { v.hidden = v.id !== 'view-' + view; });
    $$('.nav a').forEach(a => { if (a.dataset.view === view) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current'); });
    render();
  }

  let refreshTimer;
  async function refresh() {
    clearTimeout(refreshTimer);
    try {
      const fresh = await api('GET', '/api/state');
      const cfgKey = d => JSON.stringify([d.chains.map(c => [c.id, c.token, c.front_mode, c.front_node_id, c.route, c.fronts]), d.ips.map(r => [r.id, r.country_code, r.city]), d.nodes.map(n => [n.id, n.delay_ms >= 0])]);
      const changed = !S.data || cfgKey(fresh) !== cfgKey(S.data);
      if (changed) S.configCache = {};
      const focused = document.activeElement;
      const typing = focused && (focused.matches('#airUrl, #airName, td input') );
      S.data = fresh;
      if (!typing) render(); else renderNav();
    } catch (err) {
      if (err.message !== '请先登录') toast('刷新失败：' + err.message);
      if ($('#app').hidden) return;
    }
    const fast = S.data && (S.data.running || (S.view === 'settings' && S.data.telegram.enabled && !S.data.telegram.bound));
    refreshTimer = setTimeout(refresh, fast ? 4000 : S.view === 'chains' ? 10000 : 30000);
  }

  function showLogin() {
    clearTimeout(refreshTimer);
    $('#app').hidden = true;
    $('#login').hidden = false;
    $('#loginPass').focus();
  }

  async function start() {
    $('#login').hidden = true;
    $('#app').hidden = false;
    await refresh();
    show(location.hash.slice(1) || 'chains');
  }

  async function busyButton(btn, fn) {
    btn.disabled = true;
    const old = btn.textContent;
    btn.textContent = '处理中…';
    try { await fn(); } catch (err) { toast(err.message); } finally { btn.disabled = false; btn.textContent = old; }
  }

  // ---------------- 事件 ----------------
  document.addEventListener('click', ev => {
    const t = ev.target.closest('[data-act],[data-tab],[data-copy],[data-region],[data-proto],.chain-row,#parsed li[data-i],#fProto button');
    if (!t) return;
    if (t.matches('.chain-row')) { S.selected = +t.dataset.id; renderChains(); if (innerWidth <= 1180) $('#detail').scrollIntoView({ behavior: 'smooth', block: 'start' }); return; }
    if (t.matches('#parsed li[data-i]')) { S.paste.sel = +t.dataset.i; renderParsed(); fillFields(S.paste.records[S.paste.sel], true); return; }
    if (t.matches('#fProto button')) { setProto(t.dataset.v); syncFieldsToRecord(); return; }
    if (t.dataset.tab) { S.tab = t.dataset.tab; renderDetail(); return; }
    if (t.dataset.copy) { copy(t.dataset.copy); return; }
    if (t.dataset.region) { S.region = t.dataset.region; renderAirports(); return; }
    if (t.dataset.proto) {
      S.defaultProto = t.dataset.proto;
      $$('[data-proto]').forEach(b => b.setAttribute('aria-pressed', String(b === t)));
      S.paste.records = [];
      runParse(true);
      return;
    }
    const id = +t.dataset.id;
    switch (t.dataset.act) {
      case 'new-chain':
        if (!S.data.ips.length) { toast('先去「住宅IP」添加一个静态住宅IP'); location.hash = 'ips'; break; }
        openSheet(null); break;
      case 'edit-chain': openSheet(S.data.chains.find(c => c.id === id)); break;
      case 'close-sheet': closeSheet(); break;
      case 'logout': api('POST', '/api/logout').finally(showLogin); break;
      case 'check': busyButton(t, async () => {
        const c = await api('POST', '/api/chains/' + id + '/check');
        await refresh();
        toast(c.check_error ? c.device + '：' + c.check_error : c.device + '：出口 ' + c.check_exit);
      }); break;
      case 'check-all': busyButton(t, async () => { await api('POST', '/api/check-all'); toast('已开始检测全部链路，结果稍后刷新'); await refresh(); }); break;
      case 'test-nodes': busyButton(t, async () => { await api('POST', '/api/nodes/test'); toast('已开始测速，结果稍后刷新'); await refresh(); }); break;
      case 'baseline': busyButton(t, async () => { await api('POST', '/api/ips/' + id + '/baseline'); toast('已把当前出口设为基准'); await refresh(); }); break;
      case 'rotate':
        if (!confirm('重置后旧二维码和订阅链接立即失效，手机需要重新扫码。继续吗？')) break;
        busyButton(t, async () => { await api('POST', '/api/chains/' + id + '/token'); toast('已重置，请用手机重新扫码'); await refresh(); }); break;
      case 'delete-chain':
        if (!confirm('删除这条链路？住宅IP会变回空闲，手机上的节点会失效。')) break;
        busyButton(t, async () => { await api('DELETE', '/api/chains/' + id); toast('已删除'); await refresh(); }); break;
      case 'pass': S.showPass = !S.showPass; renderDetail(); break;
      case 'copy-config': {
        const cfg = S.configCache[id];
        if (cfg) copy(cfg.clash);
        break;
      }
      case 'ip-check': busyButton(t, async () => {
        const r = await api('POST', '/api/ips/' + id + '/check');
        await refresh();
        toast(r.check_error ? r.host + '：' + r.check_error : r.host + '：出口 ' + r.last_exit + (r.country ? ' · ' + r.country + ' ' + r.city : ''));
      }); break;
      case 'ip-delete':
        if (!confirm('删除这个住宅IP？')) break;
        busyButton(t, async () => { await api('DELETE', '/api/ips/' + id); toast('已删除'); await refresh(); }); break;
      case 'tg-test': busyButton(t, async () => { await api('POST', '/api/telegram/test'); toast('已发送，去 Telegram 看看'); }); break;
      case 'tg-unbind':
        if (!confirm('解除后不再收到通知。继续吗？')) break;
        busyButton(t, async () => { await api('POST', '/api/telegram/unbind'); toast('已解除绑定'); await refresh(); }); break;
      case 'backup-tg': busyButton(t, async () => { await api('POST', '/api/backup/telegram'); toast('备份已发送到 Telegram'); await refresh(); }); break;
      case 'air-sync': busyButton(t, async () => { await api('POST', '/api/airports/' + id + '/sync'); toast('已同步，正在测速'); await refresh(); }); break;
      case 'air-delete':
        if (!confirm('删除这个机场订阅和它的全部节点？')) break;
        busyButton(t, async () => { await api('DELETE', '/api/airports/' + id); toast('已删除'); await refresh(); }); break;
    }
  });

  document.addEventListener('change', ev => {
    const el = ev.target;
    if (el.dataset && el.dataset.expire) {
      const r = ipById(+el.dataset.expire);
      api('PATCH', '/api/ips/' + r.id, { remark: r.remark, expire: el.value }).then(() => { toast('到期日已保存'); refresh(); }, err => toast(err.message));
    }
    if (el.id === 'nNode') $('input[name="front"][value="fixed"]').checked = true;
  });

  document.addEventListener('input', ev => {
    if (ev.target.id === 'airUrl') {
      try { const u = new URL(ev.target.value.trim()); $('#airInsecure').checked = /^[\d.]+$|^\[.*\]$/.test(u.hostname); } catch (e) { /* 还没输完 */ }
    }
  });

  document.addEventListener('submit', ev => {
    if (ev.target.id !== 'airForm') return;
    ev.preventDefault();
    const btn = $('#airSubmit');
    busyButton(btn, async () => {
      const res = await api('POST', '/api/airports', { name: $('#airName').value.trim(), url: $('#airUrl').value.trim(), insecure: $('#airInsecure').checked });
      toast(res.warning || '已添加，正在测速');
      $('#airUrl').value = ''; $('#airName').value = '';
      await refresh();
      renderAirports();
    });
  });

  document.addEventListener('keydown', ev => {
    if (ev.key === 'Escape' && !$('#sheetBg').hidden) closeSheet();
    if ((ev.key === 'Enter' || ev.key === ' ') && ev.target.matches('.chain-row, #parsed li[data-i]')) { ev.preventDefault(); ev.target.click(); }
  });
  $('#sheetBg').addEventListener('click', ev => { if (ev.target.id === 'sheetBg') closeSheet(); });
  $('#chainForm').addEventListener('submit', submitSheet);

  $('#loginForm').addEventListener('submit', async ev => {
    ev.preventDefault();
    $('#loginErr').textContent = '';
    try {
      await api('POST', '/api/login', { password: $('#loginPass').value });
      $('#loginPass').value = '';
      start();
    } catch (err) { $('#loginErr').textContent = err.message; }
  });

  let parseTimer;
  $('#paste').addEventListener('input', () => { clearTimeout(parseTimer); parseTimer = setTimeout(() => runParse(true), 120); });
  $('#addIps').addEventListener('click', addIps);
  ['#fPort', '#fUser', '#fPass', '#fRemark', '#fExpire', '#fHost'].forEach(s => $(s).addEventListener('input', syncFieldsToRecord));
  Object.values(F).slice(0, 4).forEach(sel => {
    $(sel).addEventListener('paste', ev => {
      const text = (ev.clipboardData || window.clipboardData).getData('text');
      const res = ProxyParser.parse(text, { defaultProtocol: S.defaultProto });
      const r = res.records[0];
      if (!(r && (r.username || /[:@\s|]/.test(text.trim())))) return;
      ev.preventDefault();
      $('#paste').value = text.trim();
      S.paste = { records: [], errors: [], sel: 0 };
      runParse(true);
      toast('已拆分为 IP / 端口 / 账户 / 密码');
    });
  });

  window.addEventListener('hashchange', () => show(location.hash.slice(1)));

  renderParsed();
  start();
})();
