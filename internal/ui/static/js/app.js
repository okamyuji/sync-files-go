// app.js  HTMX を補助する最小スクリプト。
//
// 目的:
//   1. CSRF Cookie を読み、HTMX のリクエストヘッダ X-CSRF-Token に積む
//   2. アップロード進捗バーを更新する
//   3. サーバが HX-Trigger で発火する openConflictModal イベントを <dialog> で表示する
//
// CSP nonce 配下で動くため、すべての DOM 操作は addEventListener 経由で行う。
//   <script src="/static/js/app.js" nonce="{{.CSPNonce}}" defer></script>
(function () {
  'use strict';

  // === CSRF: Cookie を読み htmx の各リクエストにヘッダを積む ===
  function readCookie(name) {
    var prefix = name + '=';
    var parts = document.cookie ? document.cookie.split('; ') : [];
    for (var i = 0; i < parts.length; i++) {
      if (parts[i].indexOf(prefix) === 0) {
        return decodeURIComponent(parts[i].substring(prefix.length));
      }
    }
    return '';
  }

  document.body.addEventListener('htmx:configRequest', function (e) {
    var token = readCookie('__Host-sync_csrf');
    if (token && /^(POST|PUT|PATCH|DELETE)$/i.test(e.detail.verb || '')) {
      e.detail.headers['X-CSRF-Token'] = token;
    }
  });

  // === アップロード進捗バー ===
  function findProgressFor(form) {
    var id = form.dataset.progress;
    return id ? document.getElementById(id) : null;
  }
  document.body.addEventListener('htmx:xhr:progress', function (e) {
    var p = findProgressFor(e.target);
    if (!p) return;
    if (e.detail.lengthComputable && e.detail.total > 0) {
      p.value = (e.detail.loaded / e.detail.total) * 100;
    }
  });
  document.body.addEventListener('htmx:afterRequest', function (e) {
    var p = findProgressFor(e.target);
    if (p) p.value = 0;
  });

  // === ドラッグ&ドロップで <input type=file> に流し込む ===
  document.querySelectorAll('[data-upload-zone]').forEach(function (zone) {
    var input = zone.querySelector('input[type=file]');
    if (!input) return;
    ['dragenter', 'dragover'].forEach(function (ev) {
      zone.addEventListener(ev, function (e) {
        e.preventDefault();
        zone.dataset.dragover = 'true';
      });
    });
    ['dragleave', 'drop'].forEach(function (ev) {
      zone.addEventListener(ev, function (e) {
        e.preventDefault();
        zone.dataset.dragover = 'false';
      });
    });
    zone.addEventListener('drop', function (e) {
      if (!e.dataTransfer || !e.dataTransfer.files || !e.dataTransfer.files.length) return;
      input.files = e.dataTransfer.files;
      input.dispatchEvent(new Event('change', { bubbles: true }));
    });
  });

  // === 競合モーダル: サーバが HX-Trigger: openConflictModal で発火、本文は HX-Trigger ヘッダの JSON ===
  // すべての動的テキストは textContent / createElement 経由で挿入し、
  // innerHTML は使わない（CSP + 二重防御）。
  document.body.addEventListener('openConflictModal', function (e) {
    var modal = document.getElementById('conflict-modal');
    if (!modal || typeof modal.showModal !== 'function') return;
    var body = modal.querySelector('[data-conflict-body]');
    var detail = (e && e.detail) || {};
    if (body) {
      // 既存の子ノードを安全に空にする
      while (body.firstChild) body.removeChild(body.firstChild);

      var p = document.createElement('p');
      p.textContent = 'サーバ側のバージョンが先に進んでいます。';
      body.appendChild(p);

      var dl = document.createElement('dl');
      dl.className = 'dl';
      appendRow(dl, 'サーバ最終更新', String(detail.current_modified_at || '不明'));
      var versionDt = document.createElement('dt');
      versionDt.textContent = 'サーババージョン';
      var versionDd = document.createElement('dd');
      var code = document.createElement('code');
      code.textContent = String(detail.current_version_id || '?');
      versionDd.appendChild(code);
      dl.appendChild(versionDt);
      dl.appendChild(versionDd);
      body.appendChild(dl);
    }
    if (!modal.open) modal.showModal();
  });

  function appendRow(dl, label, value) {
    var dt = document.createElement('dt');
    dt.textContent = label;
    var dd = document.createElement('dd');
    dd.textContent = value;
    dl.appendChild(dt);
    dl.appendChild(dd);
  }

  // === data-action="reload" で window.location.reload() を呼ぶ（CSP のため inline onclick は使わない） ===
  document.body.addEventListener('click', function (e) {
    var t = e.target;
    if (!t) return;
    if (t.dataset && t.dataset.action === 'reload') {
      e.preventDefault();
      window.location.reload();
    }
  });

  // === ログアウトリンクは form POST に変える（GET だと CSRF を避けられない問題はないが意味的に POST） ===
  document.querySelectorAll('[data-logout]').forEach(function (a) {
    a.addEventListener('click', function (e) {
      e.preventDefault();
      var f = document.createElement('form');
      f.method = 'POST';
      f.action = a.getAttribute('href') || '/logout';
      var token = readCookie('__Host-sync_csrf');
      if (token) {
        var h = document.createElement('input');
        h.type = 'hidden';
        h.name = '_csrf';
        h.value = token;
        f.appendChild(h);
      }
      document.body.appendChild(f);
      f.submit();
    });
  });
})();
