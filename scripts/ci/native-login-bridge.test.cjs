const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const test = require('node:test');
const vm = require('node:vm');
const script = readFileSync('web/public/native_login_bridge.js', 'utf8');

function harness() {
  const navigations = [], requests = [], listeners = {};
  let nodes = [], observer;
  class XHR {
    open(...args) { this.args = args; }
    send(body) { requests.push([this.args, body]); }
    abort() { this.aborted = true; }
  }
  const window = {
    location: { href: 'https://panel.test/', origin: 'https://panel.test', assign: path => navigations.push(path) },
    fetch(...args) { requests.push(args); return Promise.resolve({ ok: true }); },
  };
  const context = vm.createContext({ window, URL, Request, DOMException, WeakSet, XMLHttpRequest: XHR,
    HTMLFormElement: class {},
    MutationObserver: class { constructor(callback) { observer = callback; } observe() {} },
    document: { documentElement: {}, querySelectorAll: () => nodes, addEventListener: (name, fn) => { listeners[name] = fn; } },
  });
  vm.runInContext(script, context);
  return { window, navigations, requests, XHR,
    nodes(value) { nodes = value; observer(); },
    repeat() { vm.runInContext(script, context); },
  };
}

test('opening a public login dialog navigates once, closed dialogs do not', () => {
  const h = harness();
  assert.deepEqual(h.navigations, []);
  h.nodes([{ closest: () => ({}) }]);
  assert.deepEqual(h.navigations, []);
  h.nodes([{ closest: () => null }]);
  h.nodes([{ closest: () => null }]);
  h.repeat();
  assert.deepEqual(h.navigations, ['/login?returnTo=%2Fadmin%2Fdashboard']);
});

test('legacy fetch redirects before sending credentials; unrelated requests still work', async () => {
  const h = harness();
  await h.window.fetch('/api/version');
  await h.window.fetch('https://provider.test/api/login');
  assert.equal(h.requests.length, 2);
  await assert.rejects(h.window.fetch(new Request('https://panel.test/api/login'), { method: 'POST', body: 'synthetic' }), { name: 'AbortError' });
  assert.equal(h.requests.length, 2);
  assert.equal(h.navigations.length, 1);
});

test('legacy XHR redirects without sending and reused XHRs retain normal behavior', () => {
  const h = harness(), xhr = new h.XHR();
  xhr.open('POST', '/api/login');
  xhr.send('synthetic');
  assert.equal(xhr.aborted, true);
  assert.equal(h.requests.length, 0);
  xhr.open('GET', '/api/version');
  xhr.send();
  assert.equal(h.requests.length, 1);
});
