// 运行：node --test prototype/parse-proxy.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

const { parse } = createRequire(import.meta.url)('./parse-proxy.js');

const one = (text, opts) => {
  const { records, errors } = parse(text, opts);
  assert.equal(errors.length, 0, JSON.stringify(errors));
  assert.equal(records.length, 1);
  return records[0];
};

const pick = r => ({ protocol: r.protocol, host: r.host, port: r.port, username: r.username, password: r.password });

test('ip:port:user:pass', () => {
  const r = one('203.0.113.24:1080:la_user01:Xk9#pQ2');
  assert.deepEqual(pick(r), { protocol: 'socks5', host: '203.0.113.24', port: 1080, username: 'la_user01', password: 'Xk9#pQ2' });
  assert.equal(r.format, 'IP:端口:账号:密码');
  assert.equal(r.protocolGuessed, true);
});

test('密码里带冒号', () => {
  const r = one('203.0.113.24:1080:user:pa:ss');
  assert.equal(r.username, 'user');
  assert.equal(r.password, 'pa:ss');
});

test('user:pass:ip:port', () => {
  const r = one('ny_home:77aa21:198.51.100.7:8000');
  assert.deepEqual(pick(r), { protocol: 'socks5', host: '198.51.100.7', port: 8000, username: 'ny_home', password: '77aa21' });
  assert.equal(r.format, '账号:密码:IP:端口');
});

test('账号里带点，不被误认成域名', () => {
  const r = one('cust.res-us:secret:198.51.100.7:8000');
  assert.equal(r.host, '198.51.100.7');
  assert.equal(r.username, 'cust.res-us');
});

test('user:pass@ip:port', () => {
  const r = one('tk_uk:Zz8812@192.0.2.55:45001');
  assert.deepEqual(pick(r), { protocol: 'socks5', host: '192.0.2.55', port: 45001, username: 'tk_uk', password: 'Zz8812' });
});

test('ip:port@user:pass', () => {
  const r = one('192.0.2.55:45001@tk_uk:Zz8812');
  assert.equal(r.host, '192.0.2.55');
  assert.equal(r.username, 'tk_uk');
  assert.equal(r.format, 'IP:端口@账号:密码');
});

test('密码里带 @', () => {
  const r = one('user:p@ss@192.0.2.55:45001');
  assert.equal(r.password, 'p@ss');
});

test('socks5:// 链接与备注', () => {
  const r = one('socks5://jp_res:q1w2e3@203.0.113.90:1080#JP-Osaka');
  assert.deepEqual(pick(r), { protocol: 'socks5', host: '203.0.113.90', port: 1080, username: 'jp_res', password: 'q1w2e3' });
  assert.equal(r.remark, 'JP-Osaka');
  assert.equal(r.protocolGuessed, false);
});

test('http:// 域名 + URL 编码的密码', () => {
  const r = one('http://user%40x:p%23w@us-res.example.net:3128/');
  assert.deepEqual(pick(r), { protocol: 'http', host: 'us-res.example.net', port: 3128, username: 'user@x', password: 'p#w' });
});

test('小火箭 Base64 分享链接', () => {
  const b64 = Buffer.from('sr_user:sr_pass@203.0.113.12:2080').toString('base64');
  const r = one(`socks://${b64}?remarks=x#US-Dallas`);
  assert.deepEqual(pick(r), { protocol: 'socks5', host: '203.0.113.12', port: 2080, username: 'sr_user', password: 'sr_pass' });
  assert.equal(r.format, 'Base64 分享链接');
  assert.equal(r.remark, 'US-Dallas');
});

test('Base64 只编码账号密码部分', () => {
  const b64 = Buffer.from('sr_user:sr_pass').toString('base64');
  const r = one(`socks://${b64}@203.0.113.12:2080`);
  assert.equal(r.username, 'sr_user');
  assert.equal(r.password, 'sr_pass');
});

test('空格 / Tab / 竖线分隔', () => {
  assert.deepEqual(pick(one('198.51.100.20 6000 u1 p1')), { protocol: 'socks5', host: '198.51.100.20', port: 6000, username: 'u1', password: 'p1' });
  assert.equal(one('198.51.100.20\t6000\tu1\tp1').password, 'p1');
  assert.equal(one('198.51.100.20|6000|u1|p1').username, 'u1');
});

test('行里带协议名', () => {
  const r = one('HTTP 198.51.100.20 6000 u1 p1');
  assert.equal(r.protocol, 'http');
  assert.equal(r.username, 'u1');
});

test('全角冒号', () => {
  const r = one('203.0.113.24：1080：user：pass');
  assert.equal(r.port, 1080);
  assert.equal(r.password, 'pass');
});

test('供应商发来的带标签文本', () => {
  const r = one(`
    订单号：880213
    IP地址：203.0.113.77
    端口：9595
    账号：cxr_0927
    密码：Hh7!kL2
    到期时间：2026-10-27
  `);
  assert.deepEqual(pick(r), { protocol: 'socks5', host: '203.0.113.77', port: 9595, username: 'cxr_0927', password: 'Hh7!kL2' });
  assert.equal(r.expire, '2026-10-27');
  assert.equal(r.format, '标签文本');
});

test('英文标签，host 里带端口', () => {
  const r = one('Host: 203.0.113.77:9595  Username: abc  Password: def  Protocol: HTTP');
  assert.equal(r.port, 9595);
  assert.equal(r.protocol, 'http');
});

test('无认证的 ip:port 给出提示', () => {
  const r = one('203.0.113.24:1080');
  assert.equal(r.username, '');
  assert.match(r.warnings[0], /白名单/);
});

test('内网地址给出提示', () => {
  const r = one('192.168.1.10:1080:a:b');
  assert.ok(r.warnings.some(w => w.includes('内网')));
});

test('多行批量 + 去重 + 注释 + 错误行', () => {
  const { records, errors } = parse(`
// 美国洛杉矶
203.0.113.24:1080:la_user01:Xk9pQ2
203.0.113.24:1080:la_user01:Xk9pQ2
tk_uk:Zz8812@192.0.2.55:45001
这一行不是代理
vmess://abc
  `);
  assert.equal(records.length, 2);
  assert.equal(errors.length, 2);
});

test('默认协议可切换', () => {
  assert.equal(one('203.0.113.24:8080:a:b', { defaultProtocol: 'http' }).protocol, 'http');
});

test('备注：空格 + #', () => {
  const r = one('203.0.113.24:1080:user:pass #洛杉矶 Comcast');
  assert.equal(r.password, 'pass');
  assert.equal(r.remark, '洛杉矶 Comcast');
});
