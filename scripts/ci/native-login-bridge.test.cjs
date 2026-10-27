const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const test = require('node:test');
const vm = require('node:vm');
const script = readFileSync('web/public/native_login_bridge.js', 'utf8');
const endpoint = '/komari.credential.v1.PasswordLoginService/BeginPasswordLogin';
const password = JSON.stringify({ username: 'synthetic', password: 'synthetic-password', '2fa_code': 'unused' });
const pending = () => ({ expiresAt: new Date(Date.now() + 170_000).toISOString(), providers: [{ id: 'native:email_otp', displayName: 'Email OTP', token: 'must-not-persist' }] });

function harness(result = pending(), status = 200, denyStorage = false) {
  const navigations = [], requests = [], values = new Map();
  class XHR {
    constructor() { this.listeners = {}; this.headers = {}; this.readyState = 0; this.responseType = ''; }
    addEventListener(name, fn) { (this.listeners[name] ??= []).push(fn); }
    open(...args) { this.args = args; this.headers = {}; this.readyState = 1; }
    setRequestHeader(name, value) { this.headers[name] = value; }
    send(body) { requests.push({ args: this.args, body, headers: this.headers }); }
    finish(value, code = status) {
      this.status = code; this.readyState = 4; this.responseText = JSON.stringify(value); this.response = this.responseType === 'json' ? value : this.responseText;
      for (const name of ['readystatechange', 'load', 'loadend']) {
        let stopped = false;
        for (const fn of this.listeners[name] ?? []) { fn({ stopImmediatePropagation() { stopped = true; } }); if (stopped) break; }
      }
    }
  }
  const window = {
    location: { href: 'https://panel.test/', origin: 'https://panel.test', assign: path => navigations.push(path) },
    sessionStorage: { setItem(k, v) { if (denyStorage) throw new Error('storage disabled'); values.set(k, v); }, removeItem(k) { values.delete(k); } },
    async fetch(...args) { requests.push(args); return new Response(JSON.stringify(result), { status }); },
  };
  const context = vm.createContext({ window, URL, Request, Response, Error, XMLHttpRequest: XHR, console });
  vm.runInContext(script, context);
  return { window, navigations, requests, XHR, values, repeat() { vm.runInContext(script, context); } };
}
async function submit(h, input = '/api/login', init = { method: 'POST', body: password }) {
  // Success promises deliberately stay pending until the new document loads.
  const request = h.window.fetch(input, init);
  await new Promise(resolve => setImmediate(resolve));
  return { request };
}

test('installing the bridge or opening a dialog never redirects or submits credentials', () => {
  // No MutationObserver or document is supplied: the adapter cannot inspect or
  // react to dialog appearance, field focus, typing, or a login button click.
  const h = harness(); h.repeat();
  assert.deepEqual(h.navigations, []); assert.deepEqual(h.requests, []);
});
test('Continue verifies password before handing off to dedicated /login/verify without secrets', async () => {
  const h = harness();
  await submit(h);
  assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0][0], endpoint);
  assert.deepEqual(JSON.parse(h.requests[0][1].body), { username: 'synthetic', password: 'synthetic-password' });
  assert.equal(h.requests[0][1].credentials, 'same-origin');
  assert.deepEqual(h.navigations, ['/login/verify']);
  assert.equal(h.values.size, 0);
});
test('incorrect password stays in original dialog and displays the actual server error', async () => {
  const h = harness({ code: 'unauthenticated', message: 'Invalid credentials' }, 401);
  const response = await h.window.fetch('/api/login', { method: 'POST', body: password });
  assert.equal(response.status, 401); assert.equal((await response.json()).message, 'Invalid credentials');
  assert.deepEqual(h.navigations, []); assert.equal(h.values.size, 0);
});
test('native endpoints, OAuth, passkey and other origins are untouched', async () => {
  const h = harness();
  for (const url of ['/api/version', '/api/oauth', endpoint, '/api/plugins/passkey/login', 'https://provider.test/api/login']) await h.window.fetch(url);
  assert.equal(h.requests.length, 5); assert.deepEqual(h.navigations, []);
});
test('Request bodies are supported and full no-factor sessions go directly to dashboard', async () => {
  const h = harness({ authenticated: true });
  await submit(h, new Request('https://panel.test/api/login', { method: 'POST', body: password }), {});
  assert.deepEqual(h.navigations, ['/admin/dashboard']); assert.equal(h.values.size, 0);
});
test('disabled browser storage does not break the cookie-bound handoff', async () => {
  const h = harness(pending(), 200, true);
  await submit(h);
  assert.deepEqual(h.navigations, ['/login/verify']);
  assert.equal(h.values.size, 0);
});
test('malformed input never submits or claims login', async () => {
  const invalid = harness();
  const response = await invalid.window.fetch('/api/login', { method: 'POST', body: '{}' });
  assert.equal(response.status, 400); assert.equal(invalid.requests.length, 0);
});
test('XHR submits the native flow without duplicate Content-Type or early redirect', () => {
  const h = harness(), xhr = new h.XHR(); let themeLoads = 0;
  xhr.open('POST', '/api/login', true);
  xhr.setRequestHeader('Content-Type', 'application/json');
  xhr.addEventListener('load', () => { themeLoads++; });
  xhr.send(password);
  assert.equal(h.requests[0].args[1], endpoint);
  assert.equal(h.requests[0].headers['Content-Type'], 'application/json');
  assert.deepEqual(h.navigations, []);
  xhr.finish(pending());
  assert.deepEqual(h.navigations, ['/login/verify']); assert.equal(themeLoads, 0);
});
test('XHR bad password stays put, and reuse for non-login requests is normal', () => {
  const h = harness(), xhr = new h.XHR(); let loads = 0;
  xhr.open('POST', '/api/login'); xhr.addEventListener('load', () => { loads++; }); xhr.send(password);
  xhr.finish({ message: 'Invalid credentials' }, 401);
  assert.equal(loads, 1); assert.deepEqual(h.navigations, []);
  xhr.open('GET', '/api/version'); xhr.send(); xhr.finish({ version: 'synthetic' });
  assert.equal(loads, 2); assert.equal(h.requests[1].args[1], '/api/version');
});
test('XHR also hands off without browser storage', () => {
  const h = harness(pending(), 200, true), xhr = new h.XHR();
  xhr.open('POST', '/api/login'); xhr.send(password); xhr.finish(pending());
  assert.deepEqual(h.navigations, ['/login/verify']);
  assert.equal(h.values.size, 0);
});
