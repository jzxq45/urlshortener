// app.js — minimal progressive enhancement: no framework required.
// htmx (if loaded) handles the shorten form; the rest is vanilla JS.
(function () {
  'use strict';

  function postJSON(url, body) {
    return fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }).then(function (r) {
      return r.json().then(function (j) { return { ok: r.ok, status: r.status, body: j }; });
    });
  }

  var form = document.getElementById('shorten-form');
  var msg = document.getElementById('form-msg');
  if (form && !window.htmx) {
    // fallback when htmx.min.js is unavailable (fully static deployment)
    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var f = e.target;
      var body = { url: f.url.value };
      if (f.custom_code.value) body.custom_code = f.custom_code.value;
      if (f.ttl_seconds.value) body.ttl_seconds = parseInt(f.ttl_seconds.value, 10);
      var btn = f.querySelector('button[type=submit]');
      btn.disabled = true;
      postJSON('/api/shorten', body).then(function (res) {
        btn.disabled = false;
        if (!res.ok) {
          msg.textContent = res.body.error || 'error';
          msg.className = 'msg err';
          return;
        }
        msg.textContent = 'created: ' + location.origin + '/r/' + res.body.code;
        msg.className = 'msg ok';
        window.location.href = '/' + res.body.code;
      });
    });
  }

  document.addEventListener('click', function (e) {
    var t = e.target.closest('button');
    if (!t) return;

    if (t.classList.contains('copy')) {
      var v = t.getAttribute('data-copy');
      navigator.clipboard.writeText(location.origin + v).then(function () {
        var old = t.textContent;
        t.textContent = 'copied!';
        setTimeout(function () { t.textContent = old; }, 900);
      });
    }

    if (t.classList.contains('del')) {
      e.preventDefault();
      var code = t.getAttribute('data-code');
      fetch('/api/links/' + code, { method: 'DELETE' }).then(function (r) {
        if (r.status === 204) {
          var row = t.closest('tr');
          if (row) row.remove();
          else window.location.href = '/links';
        } else if (r.status === 403) {
          alert('only the link creator can delete it');
        } else {
          alert('delete failed (' + r.status + ')');
        }
      });
    }
  });
})();
