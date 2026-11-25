// Focused acceptance: built Web, actual server, HTTPS/Nginx and installed Agent.
// No route mocks, direct database writes, borrowed sessions or unrelated E2Es.
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { spawn } = require('node:child_process');
const { chromium, request } = require('@playwright/test');
require('./guard.cjs');

const base = process.env.E2E_URL || 'https://127.0.0.1:29443';
const root = process.env.E2E_ROOT;
const artifacts = process.env.E2E_ARTIFACTS;
const checks = [];
let admin, guest, browser, context, login, daemon;
const credentialsPath = path.join(root, 'entry-agent-credentials.json');
const denyRefresh = path.join(root, 'entry-deny-refresh');

async function rpc(client, service, method, data = {}, headers = {}) {
  const response = await client.post(`/komari.${service}/${method}`, {
    data,
    headers: { Origin: base, 'Connect-Protocol-Version': '1', ...headers },
    timeout: 30000,
  });
  const status = response.status();
  const body = await response.json();
  await response.dispose();
  return { status, body };
}

async function post(client, url, data) {
  const response = await client.post(url, { data, headers: { Origin: base } });
  assert(response.ok(), `${url}: HTTP ${response.status()} ${await response.text()}`);
  const result = await response.json();
  await response.dispose();
  return result;
}

async function until(label, read, ready, timeout = 60000) {
  const deadline = Date.now() + timeout;
  for (;;) {
    const value = await read();
    if (ready(value)) return value;
    assert(Date.now() < deadline, `${label} timed out`);
    await new Promise(resolve => setTimeout(resolve, 1000));
  }
}

async function run() {
  admin = await request.newContext({ baseURL: base });
  const password = `Ci9-${crypto.randomBytes(24).toString('hex')}`;
  console.log(`::add-mask::${password}`);
  await post(admin, '/api/install/complete', {
    username: 'entry-user',
    password,
    sitename: 'Komari entry acceptance',
    description: '',
    metric_dsn: './data/metrics.db',
  });
  await until('installed panel', async () => {
    const response = await admin.get('/api/version');
    const status = response.status();
    await response.dispose();
    return status;
  }, status => status === 200);

  const signedIn = await rpc(admin, 'credential.v1.PasswordLoginService', 'BeginPasswordLogin', {
    username: 'entry-user',
    password,
  });
  assert.equal(signedIn.status, 200);
  assert.equal(signedIn.body.authenticated, true);
  await post(admin, '/api/admin/settings', { private_site: true });
  checks.push('private-site-enabled-through-authenticated-settings');

  guest = await request.newContext({ baseURL: base });
  for (const [service, method] of [
    ['enrollment.v1.EnrollmentService', 'RevokeCredentials'],
    ['enrollment.v1.EnrollmentAdminService', 'ListPendingEnrollments'],
    ['enrollment.v1.EnrollmentAdminService', 'GetPendingEnrollment'],
    ['enrollment.v1.EnrollmentAdminService', 'ApproveEnrollment'],
    ['enrollment.v1.EnrollmentAdminService', 'DenyEnrollment'],
    ['browser.v1.BrowserService', 'ListAgents'],
    ...['ListTrustedCredentialProviders', 'BeginVerification', 'CompleteVerification', 'CancelVerification', 'GetCredentialContacts', 'BeginContactBinding', 'SaveCredentialContact', 'DeleteCredentialContact'].map(m => ['credential.v1.TrustedCredentialService', m]),
  ]) {
    const denied = await rpc(guest, service, method);
    assert.equal(denied.status, 401, `${service}/${method}`);
    assert.equal(denied.body.code, 'unauthenticated');
  }
  const invalidBearer = await rpc(guest, 'enrollment.v1.EnrollmentService', 'GetTrustBundle', {}, {
    Authorization: 'Bearer invalid-entry-fixture',
  });
  assert.equal(invalidBearer.status, 401);
  assert.equal(invalidBearer.body.code, 'unauthenticated');
  const refresh = await rpc(guest, 'enrollment.v1.EnrollmentService', 'RefreshCredentials');
  assert.equal(refresh.status, 401);
  assert.equal(refresh.body.code, 'unauthenticated');
  assert.match(refresh.body.message, /refresh/);
  assert.equal((await rpc(guest, 'enrollment.v1.EnrollmentService', 'GetTrustBundle')).status, 200);
  checks.push('guest-cannot-approve-revoke-or-use-trusted-credential-service', 'invalid-bearer-does-not-fall-back', 'refresh-and-key-discovery-reach-their-own-validators');

  // Verify PrivateSiteMiddleware blocks guest /api/rpc2, and RPC2 enforces body/batch caps for authenticated callers.
  const guestRpcBlocked = await guest.post('/api/rpc2', {
    data: { jsonrpc: '2.0', id: 1, method: 'rpc.ping' },
    headers: { Origin: base },
  });
  assert.equal(guestRpcBlocked.status(), 401, 'guest /api/rpc2 was not blocked under private_site');
  await guestRpcBlocked.dispose();

  const hugeBody = JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'rpc.ping', params: { pad: 'x'.repeat(270 * 1024) } });
  const rpcTooLarge = await admin.fetch('/api/rpc2', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Origin: base },
    data: hugeBody,
  });
  assert.equal(rpcTooLarge.status(), 413, 'oversized /api/rpc2 body was not rejected with 413');
  await rpcTooLarge.dispose();

  const hugeBatch = Array.from({ length: 257 }, (_, i) => ({ jsonrpc: '2.0', id: i + 1, method: 'rpc.ping' }));
  const rpcBatchRejected = await admin.post('/api/rpc2', {
    data: hugeBatch,
    headers: { Origin: base },
  });
  assert.equal(rpcBatchRejected.status(), 400, 'oversized /api/rpc2 batch was not rejected with 400');
  await rpcBatchRejected.dispose();
  checks.push('rpc2-private-site-blocks-guest-and-enforces-256kib-body-and-256-batch-limits');

  const authorization = (await post(admin, '/api/bind/ticket', {})).data;
  assert(authorization.expires_in > 0);
  assert.equal(authorization.ticket, undefined);
  const state = await admin.storageState();
  assert(state.cookies.some(cookie => cookie.name === '__Host-komari_bind' && cookie.secure && cookie.httpOnly && cookie.sameSite === 'Strict'));
  const log = fs.openSync(path.join(root, 'entry-agent-private.log'), 'w', 0o600);
  try {
    login = spawn('/usr/lib/komari/komari-e2e/komari-agent', ['login', '--endpoint', base, '--credentials-file', credentialsPath], {
      stdio: ['ignore', log, log],
    });
  } finally {
    fs.closeSync(log);
  }
  let spawnError;
  login.on('error', error => { spawnError = error; });
  fs.writeFileSync(path.join(root, 'login.pid'), String(login.pid));
  const pending = await until('actual unauthenticated Agent enrollment', async () => {
    if (spawnError) throw spawnError;
    assert.equal(login.exitCode, null, 'Agent exited before approval');
    const result = await rpc(admin, 'enrollment.v1.EnrollmentAdminService', 'ListPendingEnrollments');
    assert.equal(result.status, 200);
    return result.body.enrollments || [];
  }, items => items.length === 1);
  const approved = await rpc(admin, 'enrollment.v1.EnrollmentAdminService', 'ApproveEnrollment', {
    userCode: pending[0].userCode,
    newAgent: { name: 'Private-site entry Agent' },
  });
  assert.equal(approved.status, 200);
  assert.equal(approved.body.created, true);
  await until('actual Agent saves credentials', async () => {
    if (spawnError) throw spawnError;
    return login.exitCode;
  }, code => code === 0, 90000);
  fs.unlinkSync(path.join(root, 'login.pid'));
  checks.push('installed-agent-begins-polls-and-enrolls-without-access-token');

  const initial = JSON.parse(fs.readFileSync(credentialsPath, 'utf8'));
  assert(initial.access_token && initial.refresh_token, 'enrollment did not persist usable credentials');
  fs.writeFileSync(credentialsPath, JSON.stringify({
    ...initial, access_token_expires_at: new Date(Date.now() - 60000).toISOString(),
  }), { mode: 0o600 });
  const nginxLog = path.join(artifacts, 'nginx-access.log');
  const accessOffset = fs.statSync(nginxLog).size;
  const newAccessEntries = () => fs.readFileSync(nginxLog).subarray(accessOffset).toString();
  fs.writeFileSync(denyRefresh, '', { mode: 0o600 });
  const daemonLog = fs.openSync(path.join(root, 'entry-agent-run.log'), 'w', 0o600);
  try {
    daemon = spawn('/usr/lib/komari/komari-e2e/komari-agent', [
      '--endpoint', base, '--credentials-file', credentialsPath,
      '--runtime-state-file', path.join(root, 'entry-agent-runtime.json'),
      '--privileged-state-file', path.join(root, 'entry-agent-privileged.json'),
      '--replay-state-file', path.join(root, 'entry-agent-replay.json'),
    ], { stdio: ['ignore', daemonLog, daemonLog] });
  } finally {
    fs.closeSync(daemonLog);
  }
  daemon.on('error', error => { spawnError = error; });
  await until('installed Agent received a native entry 403 on refresh', async () => {
    if (spawnError) throw spawnError;
    assert.equal(daemon.exitCode, null, 'Agent exited during refresh');
    return /POST \/komari\.enrollment\.v1\.EnrollmentService\/RefreshCredentials 403 upstream=-/.test(newAccessEntries());
  }, Boolean, 30000);
  fs.unlinkSync(denyRefresh);
  await until('same Agent process rotates and persists credentials', async () => {
    assert.equal(daemon.exitCode, null, 'Agent stopped after the entry refusal');
    const identity = JSON.parse(fs.readFileSync(credentialsPath, 'utf8'));
    return identity.access_token !== initial.access_token && identity.refresh_token !== initial.refresh_token &&
      Date.parse(identity.access_token_expires_at) > Date.now();
  }, Boolean, 90000);
  await until('successful upstream refresh recorded by Nginx', async () =>
    /POST \/komari\.enrollment\.v1\.EnrollmentService\/RefreshCredentials 200 upstream=200/.test(newAccessEntries()), Boolean);
  checks.push('installed-agent-with-expired-access-retries-after-entry-403-and-persists-rotation');
  daemon.kill();

  const { publicKey } = crypto.generateKeyPairSync('ed25519');
  const rawKey = publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
  const forwarded = await rpc(guest, 'enrollment.v1.EnrollmentService', 'BeginEnrollment', {
    agentPublicKey: { algorithm: 'SIGNATURE_ALGORITHM_ED25519', value: rawKey },
    device: { hostname: 'entry-forwarding-probe' },
  }, {
    Forwarded: 'host=evil.invalid;proto=http',
    'X-Forwarded-Host': 'evil.invalid',
    'X-Forwarded-Proto': 'http',
    'X-Forwarded-For': '192.0.2.99',
  });
  assert.equal(forwarded.status, 200);
  assert.equal(forwarded.body.verificationUri, `${base}/bind`);
  assert.equal(forwarded.body.verificationUriComplete, `${base}/bind?code=${forwarded.body.userCode}`);
  checks.push('nginx-overwrites-untrusted-forwarding-and-approval-link-uses-pinned-origin');

  browser = await chromium.launch();
  context = await browser.newContext({ locale: 'en-US' });
  const page = await context.newPage();
  const failed = [];
  page.on('pageerror', error => failed.push(error.message));
  await page.goto(`${base}/login?returnTo=/admin/dashboard`);
  await page.locator('.km-login-form input[name=username]').fill('entry-user');
  await page.locator('.km-login-form input[name=password]').fill(password);
  const [loginResponse] = await Promise.all([
    page.waitForResponse(response => new URL(response.url()).pathname === '/komari.credential.v1.PasswordLoginService/BeginPasswordLogin' && response.request().method() === 'POST'),
    page.locator('.km-login-form button[type=submit]').click(),
  ]);
  assert.equal(loginResponse.status(), 200, 'native login request failed');
  await page.waitForURL(url => url.pathname === '/admin/dashboard', { timeout: 30000 });
  const cookies = await context.cookies();
  const cookie = cookies.find(item => item.name === '__Host-komari_session' || item.name === 'komari_session');
  assert(cookie?.secure && cookie.httpOnly, `built Web did not receive a secure native session cookie (found: ${cookies.map(c => c.name).join(',')})`);
  assert.equal(failed.length, 0, failed.join('\n'));
  await page.screenshot({ path: path.join(artifacts, 'private-site-native-login.png'), fullPage: true });
  checks.push('built-web-native-private-site-login-and-session');

  const sharedContext = await browser.newContext({ baseURL: base });
  try {
    const sharePath = '/api/admin/settings/temporary-share';
    assert.equal((await sharedContext.request.get('/api/nodes')).status(), 401);
    assert.equal((await sharedContext.request.get(sharePath)).status(), 401);
    const { data: share } = await post(admin, sharePath, { hours: 1 });
    assert.match(share.token, /^[A-Za-z0-9_-]{43}$/);
    console.log(`::add-mask::${share.token}`);

    const sharePage = await sharedContext.newPage();
    await sharePage.goto(`/share?temp_key=${encodeURIComponent(share.token)}`);
    assert.equal(new URL(sharePage.url()).pathname, '/');
    assert.equal(new URL(sharePage.url()).search, '');
    const cookie = (await sharedContext.cookies()).find(item => item.name === '__Host-komari_temp_key');
    assert(cookie?.secure && cookie.httpOnly);
    assert.equal(cookie.sameSite, 'Lax');
    assert.notEqual(cookie.value, share.token);
    assert.equal((await sharedContext.request.get('/api/nodes')).status(), 200);
    assert.equal((await sharedContext.request.get(sharePath)).status(), 401);

    const revoked = await admin.delete(sharePath, { headers: { Origin: base } });
    assert.equal(revoked.status(), 200);
    assert.equal((await sharedContext.request.get('/api/nodes')).status(), 401);
    checks.push('private-status-share-via-https-browser-redeems-and-revokes-without-admin-access');
  } finally {
    await sharedContext.close();
  }

  // Loopback alias on the same disposable Nginx listener exercises real browser CORS across origins.
  const callerOrigin = 'https://localhost:29443';
  await post(admin, '/api/admin/settings', { cors_origin_check_enabled: true, cors_allowed_origins: callerOrigin });
  await until('CORS policy reload', async () => {
    const response = await guest.fetch('/komari.browser.v1.BrowserService/GetPublicInfo', {
      method: 'OPTIONS',
      headers: {
        Origin: callerOrigin,
        'Access-Control-Request-Method': 'POST',
        'Access-Control-Request-Headers': 'authorization,content-type,connect-protocol-version,x-komari-workspace-grant,x-komari-credential-authorization',
      },
    });
    const status = response.status();
    await response.dispose();
    return status;
  }, status => status === 204);

  const crossPage = await context.newPage();
  const aliasResponse = await crossPage.goto(`${callerOrigin}/api/version`);
  assert.equal(aliasResponse.status(), 200, 'expected loopback alias page load to succeed before cross-origin fetch test');
  const results = await crossPage.evaluate(async targetBase => {
    const call = async () => {
      const response = await fetch(`${targetBase}/komari.browser.v1.BrowserService/GetPublicInfo`, {
        method: 'POST',
        credentials: 'include',
        headers: {
          'Content-Type': 'application/json',
          'Connect-Protocol-Version': '1',
          Authorization: 'Bearer invalid-cross-origin-entry',
          'X-Komari-Workspace-Grant': 'invalid-fixture-grant',
          'X-Komari-Credential-Authorization': 'invalid-fixture-proof',
        },
        body: '{}',
      });
      return {
        status: response.status,
        body: await response.json(),
        retry: response.headers.get('Retry-After'),
        cookie: response.headers.get('Set-Cookie'),
      };
    };
    const initial = await call();
    const burst = await Promise.all(Array.from({ length: 12 }, call));
    return { initial, burst };
  }, base);
  assert.equal(results.initial.status, 401);
  assert.equal(results.initial.body.code, 'unauthenticated');
  const limited = results.burst.find(item => item.status === 429);
  assert(limited, 'actual browser burst did not reach the authentication failure budget');
  assert.equal(limited.body.code, 'resource_exhausted');
  assert(Number(limited.retry) > 0, 'browser cannot read Retry-After');
  assert.equal(limited.cookie, null, 'browser exposed a cookie header');

  await post(admin, '/api/admin/settings', { cors_allowed_origins: '' });
  await until('CORS removal reload', async () => {
    const response = await guest.fetch('/komari.browser.v1.BrowserService/GetThemeContract', {
      method: 'OPTIONS',
      headers: {
        Origin: callerOrigin,
        'Access-Control-Request-Method': 'POST',
        'Access-Control-Request-Headers': 'authorization,content-type,connect-protocol-version',
      },
    });
    const status = response.status();
    await response.dispose();
    return status;
  }, status => status === 403);

  const blocked = await crossPage.evaluate(async targetBase => {
    try {
      await fetch(`${targetBase}/komari.browser.v1.BrowserService/GetThemeContract`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Connect-Protocol-Version': '1',
          Authorization: 'Bearer invalid-cross-origin-entry',
        },
        body: '{}',
      });
      return false;
    } catch (error) {
      return error instanceof TypeError;
    }
  }, base);
  assert(blocked, 'removed origin remained browser-authorized');
  await crossPage.close();
  checks.push('real-browser-preflight-rpc-headers-readable-401-and-429-retry-after', 'removed-origin-cannot-bypass-cors-with-authorization');
}

run().then(() => {
  fs.writeFileSync(path.join(artifacts, 'entry-acceptance.json'), JSON.stringify({ passed: true, checks, cloudflare_live_tested: false }, null, 2));
  console.log(JSON.stringify({ passed: true, checks }));
}).catch(error => {
  fs.writeFileSync(path.join(artifacts, 'entry-acceptance.json'), JSON.stringify({ passed: false, checks, error: error.stack }, null, 2));
  console.error(error);
  process.exitCode = 1;
}).finally(async () => {
  if (daemon && daemon.exitCode === null) daemon.kill();
  fs.rmSync(denyRefresh, { force: true });
  fs.rmSync(credentialsPath, { force: true });
  if (login && login.exitCode === null) login.kill();
  if (context) await context.close();
  if (browser) await browser.close();
  if (guest) await guest.dispose();
  if (admin) await admin.dispose();
});
