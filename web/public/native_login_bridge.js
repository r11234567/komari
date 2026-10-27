// Public themes may still ship the retired single-request password dialog.
// Send them to the native page before collecting credentials; authentication
// and second-factor verification remain entirely owned by that page.
(() => {
  "use strict";
  if (window.__komariNativeLoginBridge) return;
  window.__komariNativeLoginBridge = true;
  const loginPath = "/login?returnTo=%2Fadmin%2Fdashboard";
  let navigating = false;
  const navigate = () => {
    if (!navigating) {
      navigating = true;
      window.location.assign(loginPath);
    }
  };
  const legacyEndpoint = (value) => {
    try {
      const url = new URL(value instanceof Request ? value.url : value, window.location.href);
      return url.origin === window.location.origin && url.pathname === "/api/login";
    } catch { return false; }
  };
  const checkDialog = () => {
    for (const form of document.querySelectorAll(".km-login-dialog, .km-login-form")) {
      if (form.closest('[data-state="closed"], [hidden]')) continue;
      navigate();
      break;
    }
  };
  new MutationObserver(checkDialog).observe(document.documentElement, { childList: true, subtree: true, attributes: true, attributeFilter: ["data-state", "hidden"] });
  checkDialog();
  document.addEventListener("submit", (event) => {
    if (event.target instanceof HTMLFormElement &&
        (event.target.matches(".km-login-form") || legacyEndpoint(event.target.action))) {
      event.preventDefault();
      event.stopImmediatePropagation();
      navigate();
    }
  }, true);
  // Compatibility for third-party themes whose dialog has different markup.
  // Never send the password to the retired endpoint or carry it across pages.
  const fetch = window.fetch;
  window.fetch = function (input, init) {
    if (legacyEndpoint(input)) {
      navigate();
      return Promise.reject(new DOMException("Continue on the native login page", "AbortError"));
    }
    return fetch.call(this, input, init);
  };
  const open = XMLHttpRequest.prototype.open;
  const send = XMLHttpRequest.prototype.send;
  const legacyRequests = new WeakSet();
  XMLHttpRequest.prototype.open = function (method, url, ...rest) {
    legacyRequests.delete(this);
    if (legacyEndpoint(url)) legacyRequests.add(this);
    return open.call(this, method, url, ...rest);
  };
  XMLHttpRequest.prototype.send = function (body) {
    if (legacyRequests.has(this)) { navigate(); this.abort(); return; }
    return send.call(this, body);
  };
})();
