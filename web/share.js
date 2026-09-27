// 设备导入页的「安全自检」：在手机上打开，检查出口IP、时区、语言和 WebRTC 是否会暴露真实IP。
(function () {
  'use strict';
  var btn = document.getElementById('selfcheck');
  var out = document.getElementById('selfcheck-result');

  // 「复制」按钮：复制不了（非 HTTPS 或被拒绝）时选中文字，方便长按复制
  document.addEventListener('click', function (ev) {
    var b = ev.target.closest && ev.target.closest('[data-copy]');
    if (!b) return;
    var text = b.getAttribute('data-copy');
    var select = function () {
      var code = b.parentNode.querySelector('code');
      if (!code) return;
      var r = document.createRange(); r.selectNodeContents(code);
      var s = window.getSelection(); s.removeAllRanges(); s.addRange(r);
    };
    var done = function () {
      b.textContent = '已复制';
      clearTimeout(b._t);
      b._t = setTimeout(function () { b.textContent = '复制'; }, 1600);
    };
    if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(done, select); else select();
  });

  if (!btn || !out) return;

  // 通过 STUN 拿到本机对外的公网IP（srflx 候选）。UDP 没走代理时这里会露出真实IP。
  function stunIPs() {
    return new Promise(function (resolve) {
      var RTC = window.RTCPeerConnection || window.webkitRTCPeerConnection;
      if (!RTC) { resolve([]); return; }
      var ips = {}, finished = false, pc;
      function done() {
        if (finished) return;
        finished = true;
        try { pc.close(); } catch (e) { /* 已关闭 */ }
        resolve(Object.keys(ips));
      }
      try {
        pc = new RTC({ iceServers: [{ urls: ['stun:stun.cloudflare.com:3478', 'stun:stun.l.google.com:19302'] }] });
      } catch (e) { resolve([]); return; }
      pc.createDataChannel('probe');
      pc.onicecandidate = function (ev) {
        if (!ev.candidate) { done(); return; }
        var m = /\s(\S+)\s\d+\styp\ssrflx/.exec(ev.candidate.candidate || '');
        if (m) ips[m[1]] = true;
      };
      pc.createOffer().then(function (o) { return pc.setLocalDescription(o); }).catch(done);
      setTimeout(done, 5000);
    });
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function render(d) {
    var head = d.ok
      ? '<div class="verdict ok">全部通过，可以正常使用</div>'
      : '<div class="verdict bad">有问题，先处理好再登录账号</div>';
    out.innerHTML = head + '<ul class="checks">' + d.items.map(function (it) {
      var cls = it.ok === true ? 'ok' : it.ok === false ? 'bad' : 'info';
      var mark = it.ok === true ? '✓' : it.ok === false ? '×' : 'i';
      return '<li class="' + cls + '"><span class="ic">' + mark + '</span><div><b>' + esc(it.title) + '</b><span>' + esc(it.detail) + '</span></div></li>';
    }).join('') + '</ul>';
  }

  btn.addEventListener('click', function () {
    btn.disabled = true;
    btn.textContent = '检测中…（约 5 秒）';
    out.innerHTML = '';
    var tz = '';
    try { tz = Intl.DateTimeFormat().resolvedOptions().timeZone || ''; } catch (e) { /* 旧浏览器 */ }
    stunIPs().then(function (webrtc) {
      return fetch(location.pathname.replace(/\/$/, '') + '/check', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ tz: tz, lang: navigator.language || '', webrtc: webrtc })
      });
    }).then(function (r) { return r.json(); }).then(function (d) {
      if (d.error) throw new Error(d.error);
      render(d);
      out.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    }).catch(function (e) {
      out.innerHTML = '<div class="verdict bad">检测失败：' + esc(e.message) + '</div>';
    }).then(function () {
      btn.disabled = false;
      btn.textContent = '重新检测';
      btn.classList.add('alt');
    });
  });
})();
