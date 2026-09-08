package public

import "strings"

// httpErrorNoticeMarker identifies the injected script tag so a document is
// never given two copies of it.
const httpErrorNoticeMarker = "data-komari-http-notice"

// httpErrorNoticeHostMarker identifies the notice's own DOM container. It is
// deliberately not a superstring of httpErrorNoticeMarker, so that counting the
// script marker in a document stays an exact count of injected copies.
const httpErrorNoticeHostMarker = "data-komari-http-host"

// httpErrorNoticeScript surfaces HTTP rejections - 429, 401, 403 and 5xx - to
// whoever is looking at the panel.
//
// Why this is injected by the server rather than built into the frontend: a
// theme is a complete replacement SPA, shipping its own bundle, its own React
// root and its own entry point. Anything mounted inside this repository's
// application tree therefore only exists while the bundled theme is active, and
// a third-party theme would keep showing a blank chart with no indication that
// the request was refused. The server-rendered document is the one part every
// theme has in common, so the notice lives here and works regardless of which
// bundle boots afterwards.
//
// Constraints this code is written to:
//   - No framework, no imports, no build step. It runs before any bundle.
//   - A shadow root with inline styles, so a theme's CSS cannot restyle or hide
//     it and it cannot leak styles into the theme.
//   - A fixed container with its own stacking context at the top of the layer
//     order, so a theme's own overlays do not cover it.
//   - Both light and dark are painted explicitly rather than inheriting, since
//     a theme's palette is unknown.
//   - Failures are swallowed: a broken notice must never break the panel.
//
// The transport hooks patch fetch and XMLHttpRequest, which between them carry
// every network path in the application - Connect RPC, plain REST calls and
// file uploads - so no individual call site has to opt in, in this theme or in
// any other. Themes can also report their own failures through
// window.__komari.notifyHttpError(status).
const httpErrorNoticeScript = `<script ` + httpErrorNoticeMarker + `>
(function () {
  "use strict";
  if (window.__komariHttpNotice) { return; }

  // Only same-origin traffic is the panel's own. A theme fetching a third
  // party font or a plugin calling an external API must not raise a notice
  // that blames the panel.
  function sameOrigin(url) {
    try {
      if (url === undefined || url === null) { return true; }
      var text = String(url);
      if (text === "" || text.charAt(0) === "/") { return true; }
      if (/^[a-zA-Z][a-zA-Z0-9+.-]*:/.test(text) === false) { return true; }
      return new URL(text, window.location.href).origin === window.location.origin;
    } catch (e) { return false; }
  }

  var LANG = (function () {
    var raw = "";
    try {
      raw = (document.documentElement && document.documentElement.lang) || navigator.language || "";
    } catch (e) { raw = ""; }
    return String(raw).toLowerCase().indexOf("zh") === 0 ? "zh" : "en";
  })();

  var TEXT = {
    en: {
      429: ["Too many requests", "The panel is rate limiting this browser. Charts and lists may be incomplete until it recovers."],
      401: ["Not signed in", "The session was rejected or has expired. Sign in again to continue."],
      403: ["Access denied", "This account is not permitted to read that data."],
      500: ["Panel error", "The panel could not complete the request."],
      retry: "Retry in about ",
      seconds: " s",
      dismiss: "Dismiss",
      repeat: " more"
    },
    zh: {
      429: ["请求过于频繁", "面板正在限制本浏览器的请求频率，图表与列表可能显示不全，稍后会自动恢复。"],
      401: ["未登录", "会话已失效或被拒绝，请重新登录后继续。"],
      403: ["无访问权限", "当前账号没有读取该数据的权限。"],
      500: ["面板错误", "面板未能完成该请求。"],
      retry: "约 ",
      seconds: " 秒后重试",
      dismiss: "关闭",
      repeat: " 次"
    }
  }[LANG];

  function describe(status) {
    if (status === 429) { return TEXT[429]; }
    if (status === 401) { return TEXT[401]; }
    if (status === 403) { return TEXT[403]; }
    return TEXT[500];
  }

  var host = null;
  var root = null;
  // One live notice per status: a rate limit typically refuses many requests at
  // once, and a notice per refusal would bury the panel rather than inform.
  var active = Object.create(null);

  function ensureRoot() {
    if (root) { return root; }
    try {
      host = document.createElement("div");
      host.setAttribute(` + "\"" + httpErrorNoticeHostMarker + "\"" + `, "");
      // isolation and a maximal z-index keep the notice above theme overlays
      // without depending on where in the document it was attached.
      host.style.cssText = "position:fixed;z-index:2147483647;top:0;right:0;left:0;pointer-events:none;isolation:isolate;";
      root = host.attachShadow ? host.attachShadow({ mode: "open" }) : host;
      var style = document.createElement("style");
      style.textContent = [
        ":host{all:initial;}",
        ".stack{position:fixed;top:12px;right:12px;display:flex;flex-direction:column;gap:8px;",
        "max-width:min(380px,calc(100vw - 24px));font:14px/1.45 system-ui,-apple-system,'Segoe UI',Roboto,'Helvetica Neue',Arial,'PingFang SC','Microsoft YaHei',sans-serif;}",
        ".card{pointer-events:auto;box-sizing:border-box;padding:12px 14px;border-radius:10px;",
        "background:#ffffff;color:#1a1a1a;border:1px solid rgba(0,0,0,.14);",
        "box-shadow:0 6px 24px rgba(0,0,0,.16);}",
        ".row{display:flex;align-items:flex-start;gap:10px;}",
        ".dot{flex:none;width:8px;height:8px;margin-top:6px;border-radius:50%;background:#b45309;}",
        ".card[data-kind='auth'] .dot{background:#b91c1c;}",
        ".body{flex:1 1 auto;min-width:0;}",
        ".title{margin:0 0 2px;font-weight:600;}",
        ".detail{margin:0;opacity:.82;overflow-wrap:break-word;}",
        ".meta{margin:6px 0 0;font-variant-numeric:tabular-nums;opacity:.66;}",
        ".close{flex:none;padding:2px 6px;border:0;border-radius:6px;background:transparent;",
        "color:inherit;opacity:.55;cursor:pointer;font:inherit;}",
        ".close:hover{opacity:1;}",
        "@media (prefers-color-scheme:dark){",
        ".card{background:#1d1f23;color:#f2f3f5;border-color:rgba(255,255,255,.16);",
        "box-shadow:0 6px 24px rgba(0,0,0,.5);}",
        ".dot{background:#f59e0b;}",
        ".card[data-kind='auth'] .dot{background:#f87171;}",
        "}"
      ].join("");
      root.appendChild(style);
      var stack = document.createElement("div");
      stack.className = "stack";
      root.appendChild(stack);
      root.__stack = stack;
      (document.body || document.documentElement).appendChild(host);
      return root;
    } catch (e) { return null; }
  }

  function render(status, retryAfter) {
    var mounted = ensureRoot();
    if (!mounted || !mounted.__stack) { return; }
    var existing = active[status];
    if (existing) {
      existing.count += 1;
      if (existing.countEl) {
        existing.countEl.textContent = "x" + existing.count + TEXT.repeat;
      }
      // Restart the dwell time so a continuing condition stays on screen.
      schedule(existing, retryAfter);
      return;
    }

    var words = describe(status);
    var card = document.createElement("div");
    card.className = "card";
    card.setAttribute("data-kind", status === 401 || status === 403 ? "auth" : "load");
    card.setAttribute("role", "alert");
    card.setAttribute("aria-live", "polite");

    var row = document.createElement("div");
    row.className = "row";
    var dot = document.createElement("span");
    dot.className = "dot";
    row.appendChild(dot);

    var body = document.createElement("div");
    body.className = "body";
    var title = document.createElement("p");
    title.className = "title";
    title.textContent = words[0] + " (" + status + ")";
    body.appendChild(title);
    var detail = document.createElement("p");
    detail.className = "detail";
    detail.textContent = words[1];
    body.appendChild(detail);
    var meta = document.createElement("p");
    meta.className = "meta";
    body.appendChild(meta);
    row.appendChild(body);

    var close = document.createElement("button");
    close.className = "close";
    close.type = "button";
    close.setAttribute("aria-label", TEXT.dismiss);
    close.textContent = "x";
    row.appendChild(close);

    card.appendChild(row);
    mounted.__stack.appendChild(card);

    var entry = { card: card, count: 1, countEl: null, timer: 0, tick: 0 };
    active[status] = entry;
    close.addEventListener("click", function () { remove(status); });

    // A server that said when to come back is repeated verbatim, so the
    // reading matches what the panel actually enforces.
    if (retryAfter > 0) {
      var remaining = Math.ceil(retryAfter);
      var countdown = document.createElement("span");
      meta.appendChild(countdown);
      var paint = function () {
        countdown.textContent = TEXT.retry + remaining + TEXT.seconds;
      };
      paint();
      entry.tick = window.setInterval(function () {
        remaining -= 1;
        if (remaining <= 0) { window.clearInterval(entry.tick); entry.tick = 0; countdown.textContent = ""; return; }
        paint();
      }, 1000);
    }
    entry.countEl = document.createElement("span");
    meta.appendChild(entry.countEl);
    schedule(entry, retryAfter);
  }

  function schedule(entry, retryAfter) {
    if (entry.timer) { window.clearTimeout(entry.timer); }
    var dwell = 8000;
    if (retryAfter > 0) { dwell = Math.min(60000, Math.max(dwell, (retryAfter + 1) * 1000)); }
    entry.timer = window.setTimeout(function () {
      for (var key in active) {
        if (active[key] === entry) { remove(key); return; }
      }
    }, dwell);
  }

  function remove(status) {
    var entry = active[status];
    if (!entry) { return; }
    if (entry.timer) { window.clearTimeout(entry.timer); }
    if (entry.tick) { window.clearInterval(entry.tick); }
    delete active[status];
    try {
      if (entry.card && entry.card.parentNode) { entry.card.parentNode.removeChild(entry.card); }
    } catch (e) {}
  }

  function parseRetryAfter(value) {
    if (!value) { return 0; }
    var seconds = parseInt(value, 10);
    if (isNaN(seconds) === false && String(seconds) === String(value).trim()) {
      return seconds > 0 ? seconds : 0;
    }
    var at = Date.parse(value);
    if (isNaN(at)) { return 0; }
    var delta = Math.round((at - Date.now()) / 1000);
    return delta > 0 ? delta : 0;
  }

  function notify(status, retryAfter) {
    var code = parseInt(status, 10);
    // 404 and 400 are ordinary application outcomes and are left to the
    // application to present; only refusals the user can act on are shown.
    if (isNaN(code)) { return; }
    if (code !== 429 && code !== 401 && code !== 403 && code < 500) { return; }
    if (code > 599) { return; }
    try { render(code, retryAfter > 0 ? retryAfter : 0); } catch (e) {}
  }

  window.__komariHttpNotice = true;
  var api = window.__komari || (window.__komari = {});
  api.notifyHttpError = function (status, retryAfter) {
    notify(status, parseRetryAfter(retryAfter));
  };

  var nativeFetch = window.fetch;
  if (typeof nativeFetch === "function") {
    window.fetch = function (input, init) {
      var target = input;
      try { if (input && typeof input === "object" && "url" in input) { target = input.url; } } catch (e) {}
      var watched = sameOrigin(target);
      return nativeFetch.apply(this, arguments).then(function (response) {
        try {
          if (watched && response && response.ok === false) {
            notify(response.status, parseRetryAfter(response.headers && response.headers.get("Retry-After")));
          }
        } catch (e) {}
        return response;
      });
      // A rejected fetch is a transport failure or an abort. Aborts happen on
      // every route change and unmount, so raising a notice for them would
      // mean a steady stream of false alarms; those are left alone.
    };
  }

  var XHR = window.XMLHttpRequest;
  if (XHR && XHR.prototype && typeof XHR.prototype.open === "function") {
    var nativeOpen = XHR.prototype.open;
    XHR.prototype.open = function (method, url) {
      try { this.__komariWatched = sameOrigin(url); } catch (e) { this.__komariWatched = false; }
      return nativeOpen.apply(this, arguments);
    };
    var nativeSend = XHR.prototype.send;
    XHR.prototype.send = function () {
      try {
        var request = this;
        request.addEventListener("load", function () {
          try {
            if (request.__komariWatched && request.status >= 400) {
              notify(request.status, parseRetryAfter(request.getResponseHeader("Retry-After")));
            }
          } catch (e) {}
        });
      } catch (e) {}
      return nativeSend.apply(this, arguments);
    };
  }
})();
</script>`

// injectHTTPErrorNotice places the notice in the document head so it installs
// its transport hooks before any theme bundle issues a request.
func injectHTTPErrorNotice(html string) string {
	if strings.Contains(html, httpErrorNoticeMarker) {
		return html
	}
	if location := headClosePattern.FindStringIndex(html); location != nil {
		return html[:location[0]] + httpErrorNoticeScript + html[location[0]:]
	}
	if location := bodyClosePattern.FindStringIndex(html); location != nil {
		return html[:location[0]] + httpErrorNoticeScript + html[location[0]:]
	}
	return httpErrorNoticeScript + html
}
