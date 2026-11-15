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
let admin, guest, browser, context, login;
async function rpc(client, service, method, data = {}, headers = {}) {
  const response = await client.post(`/komari.${service}/${method}`, {
    data, headers: { Origin: base, 'Connect-Protocol-Version': '1', ...headers }, timeout: 30000,
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
  await post(admin, '/api/install/complete', { username: 'entry-user', password, sitename: 'Komari entry acceptance', description: '', metric_dsn: './data/metrics.db' });
  await until('installed panel', async () => {
    const response = await admin.get('/api/version');
    const status = response.status(); await response.dispose(); return status;
  }, status => status === 200);
  const signedIn = await rpc(admin, 'credential.v1.PasswordLoginService', 'BeginPasswordLogin', { username: 'entry-user', password });
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
    ...['ListTrustedCredentialProviders', 'BeginVerification', 'CompleteVerification', 'CancelVerification', 'GetCredentialContacts', 'BeginContactBinding', 'SaveCredentialContact', 'DeleteCredentialContact'].map(method => ['credential.v1.TrustedCredentialService', method]),
  ]) {
    const denied = await rpc(guest, service, method);
    assert.equal(denied.status, 401, `${service}/${method}`);
    assert.equal(denied.body.code, 'unauthenticated');
  }
  const invalidBearer = await rpc(guest, 'enrollment.v1.EnrollmentService', 'GetTrustBundle', {}, { Authorization: 'Bearer invalid-entry-fixture' });
  assert.equal(invalidBearer.status, 401);
  assert.equal(invalidBearer.body.code, 'unauthenticated');
  const refresh = await rpc(guest, 'enrollment.v1.EnrollmentService', 'RefreshCredentials');
  assert.equal(refresh.status, 401);
  assert.equal(refresh.body.code, 'unauthenticated');
  assert.match(refresh.body.message, /refresh/);
  assert.equal((await rpc(guest, 'enrollment.v1.EnrollmentService', 'GetTrustBundle')).status, 200);
  checks.push('guest-cannot-approve-revoke-or-use-trusted-credential-service', 'invalid-bearer-does-not-fall-back', 'refresh-and-key-discovery-reach-their-own-validators');

  const ticket = (await post(admin, '/api/bind/ticket', {})).data.ticket;
  assert(ticket);
  console.log(`::add-mask::${ticket}`);
  const log = fs.openSync(path.join(root, 'entry-agent-private.log'), 'w', 0o600);
  try { login = spawn('/usr/lib/komari/komari-e2e/komari-agent', ['login', '--endpoint', base], { stdio: ['ignore', log, log] }); }
  finally { fs.closeSync(log); }
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
  const approved = await rpc(admin, 'enrollment.v1.EnrollmentAdminService', 'ApproveEnrollment', { userCode: pending[0].userCode, newAgent: { name: 'Private-site entry Agent' } }, { 'X-Bind-Ticket': ticket });
  assert.equal(approved.status, 200);
  assert.equal(approved.body.created, true);
  await until('actual Agent saves credentials', async () => {
    if (spawnError) throw spawnError;
    return login.exitCode;
  }, code => code === 0, 90000);
  fs.unlinkSync(path.join(root, 'login.pid'));
  checks.push('installed-agent-begins-polls-and-enrolls-without-access-token');

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
  // A glob ending in /admin/dashboard also matches ?returnTo=/admin/dashboard.
  // Wait for the actual path after the response before inspecting the session.
  await page.waitForURL(url => url.pathname === '/admin/dashboard', { timeout: 30000 });
  const cookie = (await context.cookies()).find(item => item.name === 'session_token');
  assert(cookie?.secure && cookie.httpOnly, 'built Web did not receive a secure native session');
  assert.equal(failed.length, 0, failed.join('\n'));
  await page.screenshot({ path: path.join(artifacts, 'private-site-native-login.png'), fullPage: true });
  checks.push('built-web-native-private-site-login-and-session');
}
run().then(() => {
  fs.writeFileSync(path.join(artifacts, 'entry-acceptance.json'), JSON.stringify({ passed: true, checks, cloudflare_live_tested: false }, null, 2));
  console.log(JSON.stringify({ passed: true, checks }));
}).catch(error => {
  fs.writeFileSync(path.join(artifacts, 'entry-acceptance.json'), JSON.stringify({ passed: false, checks, error: error.stack }, null, 2));
  console.error(error);
  process.exitCode = 1;
}).finally(async () => {
  if (login && login.exitCode === null) login.kill();
  if (context) await context.close();
  if (browser) await browser.close();
  if (guest) await guest.dispose();
  if (admin) await admin.dispose();
});
