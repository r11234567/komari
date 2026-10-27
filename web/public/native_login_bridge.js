// Compatibility for public themes that still POST /api/login. Opening a dialog
// does nothing. Only a submitted password starts the same native cookie-bound
// ceremony used by the system UI. No password, OTP or token crosses documents.
(() => {
  "use strict";
  if (window.__komariNativeLoginBridge) return;
  window.__komariNativeLoginBridge = true;
  const endpoint = "/komari.credential.v1.PasswordLoginService/BeginPasswordLogin";
  const originalFetch = window.fetch;
  const legacyEndpoint = (value) => {
    try {
      const url = new URL(value instanceof Request ? value.url : value, window.location.href);
      return url.origin === window.location.origin && url.pathname === "/api/login";
    } catch { return false; }
  };
  const credentials = (body) => {
    const value = JSON.parse(body);
    if (typeof value?.username !== "string" || typeof value.password !== "string") throw new Error("Username and password are required");
    return JSON.stringify({ username: value.username, password: value.password });
  };
  const finish = (result) => {
    if (result.authenticated === true) {
      window.location.assign("/admin/dashboard");
      return;
    }
    if (!Array.isArray(result.providers) || !result.providers.length) throw new Error("No available second factor");
    // The HttpOnly service cookie is the only authority across documents.
    // No browser storage, client clock, URL credential or password replay.
    window.location.assign("/login/verify");
  };
  const failure = (message, status = 400) => new Response(JSON.stringify({ status: "error", message }), { status, headers: { "Content-Type": "application/json" } });
  window.fetch = async function (input, init) {
    const method = (init?.method ?? (input instanceof Request ? input.method : "GET")).toUpperCase();
    if (!legacyEndpoint(input) || method !== "POST") return originalFetch.call(this, input, init);
    try {
      const body = init?.body ?? (input instanceof Request ? await input.clone().text() : "");
      const response = await originalFetch.call(window, endpoint, {
        method: "POST", credentials: "same-origin", redirect: "error",
        headers: { "Content-Type": "application/json", "Connect-Protocol-Version": "1" },
        signal: init?.signal ?? (input instanceof Request ? input.signal : undefined), body: credentials(body),
      });
      const result = await response.json();
      if (!response.ok) return failure(result.message || "Login failed", response.status);
      finish(result);
      // Do not let an old theme mistake preauthentication for a full session or
      // reload/cancel the handoff before navigation completes.
      return new Promise(() => {});
    } catch (error) {
      if (error?.name === "AbortError") throw error;
      return failure("Unable to continue login: " + (error instanceof Error ? error.message : "request failed"));
    }
  };

  // XHR themes use the same endpoint and body adapter. Register before their
  // load handlers and suppress success events during the document handoff.
  const open = XMLHttpRequest.prototype.open;
  const send = XMLHttpRequest.prototype.send;
  const setRequestHeader = XMLHttpRequest.prototype.setRequestHeader;
  const requests = new WeakMap();
  XMLHttpRequest.prototype.open = function (method, url, ...rest) {
    if (requests.get(this)?.overridden) {
      delete this.status; delete this.responseText; delete this.response;
    }
    requests.delete(this);
    if (method.toUpperCase() === "POST" && legacyEndpoint(url)) requests.set(this, { handled: false, navigating: false });
    const state = requests.get(this);
    if (state) {
      for (const name of ["readystatechange", "load", "loadend"]) this.addEventListener(name, (event) => {
        if (requests.get(this) !== state || this.readyState !== 4) return;
        if (!state.handled) {
          state.handled = true;
          try {
            const result = this.responseType === "json" ? this.response : JSON.parse(this.responseText);
            if (this.status >= 200 && this.status < 300) { finish(result); state.navigating = true; }
          } catch {
            // Invalid response metadata must never look like a full login.
            state.overridden = true;
            Object.defineProperty(this, "status", { configurable: true, value: 409 });
            const failed = { status: "error", message: "Unable to continue login; please retry" };
            Object.defineProperty(this, "responseText", { configurable: true, value: JSON.stringify(failed) });
            Object.defineProperty(this, "response", { configurable: true, value: this.responseType === "json" ? failed : JSON.stringify(failed) });
          }
        }
        if (state.navigating) event.stopImmediatePropagation();
      });
    }
    const result = open.call(this, method, state ? endpoint : url, ...rest);
    if (state) {
      setRequestHeader.call(this, "Content-Type", "application/json");
      setRequestHeader.call(this, "Connect-Protocol-Version", "1");
    }
    return result;
  };
  XMLHttpRequest.prototype.setRequestHeader = function (name, value) {
    if (requests.has(this) && ["content-type", "connect-protocol-version"].includes(name.toLowerCase())) return;
    return setRequestHeader.call(this, name, value);
  };
  XMLHttpRequest.prototype.send = function (body) {
    if (requests.has(this)) {
      body = credentials(body);
    }
    return send.call(this, body);
  };
})();
