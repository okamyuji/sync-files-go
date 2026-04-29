// app.js  HTMX を補助する最小スクリプト。
//
// 目的:
//   1. CSRF Cookie を読み、HTMX のリクエストヘッダ X-CSRF-Token に積む
//   2. アップロードフォームを乗っ取り、進捗バー + 409 競合モーダル + 3 択再送を扱う
//   3. 競合モーダル本文をサーバ JSON から組み立てる
//   4. data-action="reload" / data-logout など小ヘルパ
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

  function csrfToken() { return readCookie('__Host-sync_csrf'); }

  document.body.addEventListener('htmx:configRequest', function (e) {
    var token = csrfToken();
    if (token && /^(POST|PUT|PATCH|DELETE)$/i.test(e.detail.verb || '')) {
      e.detail.headers['X-CSRF-Token'] = token;
    }
  });

  // === アップロードフォーム: data-upload-form があれば JS が submit を乗っ取る ===
  // 競合 409 を扱うために fetch ベースで実装。進捗は XMLHttpRequest で取る。
  var uploadState = {
    file: null,            // 直近の File
    originalPath: '',      // 直近の X-File-Path
    conflictFileID: '',    // サーバが返した既存ファイル ID
  };

  document.querySelectorAll('[data-upload-form]').forEach(function (form) {
    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var input = form.querySelector('input[type=file]');
      if (!input || !input.files || !input.files[0]) return;
      var file = input.files[0];
      var path = '/' + file.name;
      uploadState.file = file;
      uploadState.originalPath = path;
      uploadState.conflictFileID = '';
      sendUpload({
        url: '/api/files',
        headers: { 'X-File-Path': path, 'If-None-Match': '*' },
        progressEl: form.querySelector('progress'),
        onConflict: function (data) {
          uploadState.conflictFileID = (data && data.file && data.file.id) || '';
          openConflictDialog(data);
        },
        onDone: function () { window.location.reload(); },
      });
    });
  });

  function sendUpload(opts) {
    var xhr = new XMLHttpRequest();
    xhr.open('POST', opts.url, true);
    xhr.setRequestHeader('X-CSRF-Token', csrfToken());
    Object.keys(opts.headers || {}).forEach(function (k) {
      xhr.setRequestHeader(k, opts.headers[k]);
    });
    xhr.upload.onprogress = function (ev) {
      if (opts.progressEl && ev.lengthComputable) {
        opts.progressEl.value = (ev.loaded / ev.total) * 100;
      }
    };
    xhr.onload = function () {
      if (opts.progressEl) opts.progressEl.value = 0;
      if (xhr.status === 409) {
        try {
          var data = JSON.parse(xhr.responseText);
          opts.onConflict && opts.onConflict(data);
        } catch (_) {
          alert('アップロードが失敗しました（競合）。');
        }
        return;
      }
      if (xhr.status >= 200 && xhr.status < 300) {
        opts.onDone && opts.onDone();
        return;
      }
      alert('アップロードに失敗しました: ' + xhr.status + ' ' + xhr.responseText);
    };
    xhr.onerror = function () {
      if (opts.progressEl) opts.progressEl.value = 0;
      alert('ネットワークエラーが発生しました。');
    };
    // body は file blob 単独（multipart/form-data ではなく、サーバ側がそのまま暗号化に流す前提）
    xhr.send(uploadState.file);
  }

  // === 競合モーダル ===
  function openConflictDialog(data) {
    var modal = document.getElementById('conflict-modal');
    if (!modal || typeof modal.showModal !== 'function') return;
    var body = modal.querySelector('[data-conflict-body]');
    var info = (data && data.file) || {};
    if (body) {
      while (body.firstChild) body.removeChild(body.firstChild);

      var p = document.createElement('p');
      p.textContent = 'サーバ側のバージョンが先に進んでいます。どうしますか？';
      body.appendChild(p);

      var dl = document.createElement('dl');
      dl.className = 'dl';
      appendRow(dl, 'パス', String(info.path || ''));
      appendRow(dl, 'サーバ最終更新', formatTime(info.current_modified_at));
      var dt = document.createElement('dt');
      dt.textContent = 'サーババージョン';
      var dd = document.createElement('dd');
      var code = document.createElement('code');
      code.textContent = String(info.current_version_id || '?');
      dd.appendChild(code);
      dl.appendChild(dt);
      dl.appendChild(dd);
      body.appendChild(dl);
    }
    if (!modal.open) modal.showModal();
  }

  function appendRow(dl, label, value) {
    var dt = document.createElement('dt');
    dt.textContent = label;
    var dd = document.createElement('dd');
    dd.textContent = value;
    dl.appendChild(dt);
    dl.appendChild(dd);
  }

  function formatTime(iso) {
    if (!iso) return '';
    var d = new Date(iso);
    if (isNaN(d.getTime())) return String(iso);
    var pad = function (n) { return n < 10 ? '0' + n : '' + n; };
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) +
      ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
  }

  // モーダル内 3 ボタン
  document.body.addEventListener('click', function (e) {
    var btn = e.target && e.target.closest && e.target.closest('[data-conflict-action]');
    if (!btn) return;
    var action = btn.getAttribute('data-conflict-action');
    var modal = document.getElementById('conflict-modal');
    if (!uploadState.file) {
      // ファイル参照を失った（例: ページ再読込後）。閉じるだけ。
      modal && modal.close && modal.close();
      return;
    }
    var fid = uploadState.conflictFileID;
    if (action === 'cancel') {
      modal && modal.close && modal.close();
      return;
    }
    if (action === 'view_server') {
      if (fid) window.open('/api/files/' + fid, '_blank', 'noopener');
      return;
    }
    if (action === 'force_overwrite') {
      modal && modal.close && modal.close();
      sendUpload({
        url: '/api/files',
        headers: { 'X-File-Path': uploadState.originalPath, 'If-Match': '*' },
        onDone: function () { window.location.reload(); },
        onConflict: function () { /* 強制上書きで 409 はあり得ないが念のため */ },
      });
      return;
    }
    if (action === 'save_as_copy') {
      if (!fid) return;
      modal && modal.close && modal.close();
      sendUpload({
        url: '/api/files/' + fid + '/save-as-copy',
        headers: {},
        onDone: function () { window.location.reload(); },
        onConflict: function () { /* save-as-copy は新規扱いなので 409 はサーバ側で別名連番にする想定 */ },
      });
      return;
    }
  });

  // === 汎用ヘルパ ===
  document.body.addEventListener('click', function (e) {
    var t = e.target;
    if (!t) return;
    if (t.dataset && t.dataset.action === 'reload') {
      e.preventDefault();
      window.location.reload();
    }
  });

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

  document.querySelectorAll('[data-logout]').forEach(function (a) {
    a.addEventListener('click', function (e) {
      e.preventDefault();
      var f = document.createElement('form');
      f.method = 'POST';
      f.action = a.getAttribute('href') || '/logout';
      var token = csrfToken();
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
