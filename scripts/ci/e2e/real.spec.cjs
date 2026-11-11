// No mocked routes/transports, injected terminal handles, DB seeding, skipped
// proofs, future TOTPs or modified signed state. Exercise the shipped binaries.
const { test, expect } = require('@playwright/test');
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const { spawn, execFileSync } = require('node:child_process');
const { createRequire } = require('node:module');
// Use the pinned Web's already-installed protobuf runtime for its wire format.
const webRequire = createRequire(path.resolve(__dirname, '../../../third_party/komari-web/package.json'));
const { BinaryReader } = webRequire('@bufbuild/protobuf/wire');

const root = process.env.E2E_ROOT;
const artifacts = process.env.E2E_ARTIFACTS;
const base = 'https://127.0.0.1:29443';
if (process.env.GITHUB_ACTIONS !== 'true' || process.env.RUNNER_ENVIRONMENT !== 'github-hosted' ||
    root !== path.join(process.env.RUNNER_TEMP || '', 'komari-e2e')) {
  throw new Error('This integration test is restricted to the disposable GitHub-hosted fixture.');
}
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const shellQuote = text => `'${text.replaceAll("'", "'\\''")}'`;
const sha256 = bytes => crypto.createHash('sha256').update(bytes).digest('hex');
const services = {
  enrollment: 'komari.enrollment.v1.EnrollmentAdminService',
  trust: 'komari.enrollment.v1.EnrollmentService',
  config: 'komari.config.v1.PrivilegedDeliveryService',
  browser: 'komari.browser.v1.BrowserService',
  file: 'komari.file.v1.FileService',
  metrics: 'komari.metrics.v1.MetricsService',
  dashboard: 'komari.admin.v1.DashboardService',
  workspace: 'komari.workspace.v1.WorkspaceService',
};

function verificationResource(request) {
  expect(request.headers()['content-type']).toContain('application/proto');
  const reader = new BinaryReader(request.postDataBuffer());
  let resource;
  while (reader.pos < reader.len) {
    const [field, wireType] = reader.tag();
    // BeginVerificationRequest.resource is field 3 in the pinned protocol.
    if (field === 3) { expect(wireType).toBe(2); resource = reader.string(); }
    else reader.skip(wireType);
  }
  return resource;
}

function totp(secret, counter = Math.floor(Date.now() / 30_000)) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  const bits = [...secret.toUpperCase().replace(/=+$/, '')].map(c => {
    const n = alphabet.indexOf(c);
    if (n < 0) throw new Error('Invalid fixture TOTP secret');
    return n.toString(2).padStart(5, '0');
  }).join('');
  const key = Buffer.from((bits.match(/.{8}/g) || []).map(b => parseInt(b, 2)));
  const time = Buffer.alloc(8);
  time.writeBigUInt64BE(BigInt(counter));
  const digest = crypto.createHmac('sha1', key).update(time).digest();
  const offset = digest.at(-1) & 15;
  return String((digest.readUInt32BE(offset) & 0x7fffffff) % 1_000_000).padStart(6, '0');
}

test('real HTTPS panel and installed Agent: authorization, workspace and files', async ({ page, context }, info) => {
  const events = [];
  const metrics = { createdSessions: 0, closedSessions: 0, uploadChunks: 0 };
  page.on('console', m => events.push({ type: m.type(), text: m.text() }));
  page.on('pageerror', e => events.push({ type: 'pageerror', text: e.stack }));
  page.on('requestfailed', r => events.push({ type: 'requestfailed', url: r.url(), error: r.failure() }));
  page.on('response', r => {
    const url = new URL(r.url());
    events.push({ type: 'response', path: url.pathname, status: r.status() });
    if (!r.ok()) return;
    if (url.pathname.endsWith('.WebSSHService/CreateSession')) metrics.createdSessions++;
    if (url.pathname.endsWith('.WebSSHService/CloseSession')) metrics.closedSessions++;
    if (url.pathname.endsWith('/file/upload') && url.searchParams.get('operation') === 'chunk') metrics.uploadChunks++;
  });
  await context.addInitScript(() => localStorage.setItem('i18nextLng', 'en'));
  const http = context.request;
  let secret;
  let lastCounter = -1;
  let agentId;
  let workspaceToken;
  page.on('request', request => {
    // Observe the grant obtained by the actual UI; never inject signed state.
    const grant = request.headers()['x-komari-workspace-grant'];
    if (grant) workspaceToken = grant;
  });
  let exportCount = 0;
  const fixture = name => path.join(root, 'files', name);

  async function request(url, data, options = {}) {
    const { allowFailure = false, method = 'POST', headers = {} } = options;
    const workspaceHeaders = url.startsWith(`/${services.file}/`) && workspaceToken
      ? { 'X-Komari-Workspace-Grant': workspaceToken } : {};
    const response = await http.fetch(url, {
      method, data, headers: { Origin: base, 'Connect-Protocol-Version': '1', ...workspaceHeaders, ...headers }, timeout: 45_000,
    });
    console.log(`${method} ${new URL(url, base).pathname}: ${response.status()}`);
    if (!allowFailure) {
      expect(response.ok(), `${url}: HTTP ${response.status()} ${response.ok() ? '' : (await response.text()).slice(0, 1500)}`).toBeTruthy();
    }
    return response;
  }
  async function rpc(service, method, data = {}, options) {
    const response = await request(`/${services[service]}/${method}`, data, options);
    return response.json();
  }
  async function freshCode() {
    // Real time only. Leave enough window for the request to reach the server.
    while (true) {
      const counter = Math.floor(Date.now() / 30_000);
      if (counter > lastCounter && Date.now() % 30_000 < 25_000) {
        lastCounter = counter;
        return totp(secret, counter);
      }
      await delay(500);
    }
  }
  async function mutate(method, args) {
    return rpc('file', method, { agentId, ...args });
  }
  async function fileDenied(method, args, headers = {}) {
    const response = await request(`/${services.file}/${method}`, { agentId, ...args }, { allowFailure: true, headers });
    expect(response.ok(), `${method} must reject this operation`).toBeFalsy();
    return response.json();
  }
  async function tabMenu(label, tab = page.getByRole('tab', { selected: true })) {
    // A closed Radix menu remains mounted during its exit animation. Wait for
    // that owner to unmount before reopening a context menu on the same tab.
    await expect(page.getByRole('menu', { includeHidden: true })).toHaveCount(0);
    await tab.click({ button: 'right' });
    await page.getByRole('menuitem', { name: label, exact: true }).click();
    await expect(page.getByRole('menu', { includeHidden: true })).toHaveCount(0);
  }
  async function exportText() {
    const download = page.waitForEvent('download');
    await tabMenu('Export text');
    const item = await download;
    const destination = info.outputPath(`terminal-${++exportCount}.txt`);
    await item.saveAs(destination);
    return fs.readFileSync(destination, 'utf8');
  }
  async function beginVerification(container) {
    await container.getByRole('button', { name: 'Start verification / send code', exact: true }).click();
    await expect(container.getByRole('textbox', { name: 'Enter verification code', exact: true })).toHaveValue('');
  }
  async function verifyCredential(container, code) {
    await container.getByRole('textbox', { name: 'Enter verification code', exact: true }).fill(code);
    await container.getByRole('button', { name: 'Verify and continue', exact: true }).click();
  }
  async function confirmTerminal() {
    const dialog = page.getByRole('dialog');
    await dialog.getByRole('textbox', { name: 'System account on this machine', exact: true }).fill(process.env.USER);
    await dialog.getByRole('button', { name: 'Verify and unlock', exact: true }).click();
    await beginVerification(dialog);
    await verifyCredential(dialog, await freshCode());
    await expect(dialog).toBeHidden();
    await expect.poll(() => page.evaluate(() => getComputedStyle(document.body).pointerEvents)).not.toBe('none');
    await expect(page.locator('.km-terminal-session:visible textarea.xterm-helper-textarea')).toBeAttached();
    // Mounting xterm precedes Agent attachment. Input is intentionally disabled
    // until then; wait for the real shell, not a fixed delay or a test handle.
    await expect(page.locator('.km-terminal-session:visible .xterm-rows'))
      .toContainText(`${process.env.USER}@`, { timeout: 45_000 });
  }
  async function downloadFile(name) {
    const response = await request(`/api/admin/client/${agentId}/file/download?${new URLSearchParams({ path: fixture(name), chunk_size: '1048576' })}`, undefined, { method: 'GET' });
    return response.body();
  }

  try {
    await test.step('first-run setup, Secure admin cookie and actual Agent enrollment', async () => {
      const password = `Ci9-${crypto.randomBytes(24).toString('hex')}`;
      console.log(`::add-mask::${password}`);
      await request('/api/install/complete', {
        username: 'e2e-user', password, sitename: 'Komari real E2E', description: '', metric_dsn: './data/metrics.db',
      });
      await expect.poll(async () => (await http.get('/api/version')).status(), { timeout: 60_000 }).toBe(200);
      await request('/api/login', { username: 'e2e-user', password, '2fa_code': '' });
      const cookie = (await context.cookies()).find(c => c.name === 'session_token');
      expect(cookie?.secure).toBe(true);
      expect(cookie?.httpOnly).toBe(true);
      const ticket = (await (await request('/api/bind/ticket', {})).json()).data.ticket;
      expect(ticket).toBeTruthy();
      const log = fs.openSync(path.join(artifacts, 'enrollment.log'), 'w', 0o600);
      const login = spawn('/usr/lib/komari/komari-e2e/komari-agent', ['login', '--endpoint', base], { stdio: ['ignore', log, log] });
      fs.closeSync(log);
      fs.writeFileSync(path.join(root, 'login.pid'), String(login.pid));
      let pending;
      await expect.poll(async () => {
        pending = (await rpc('enrollment', 'ListPendingEnrollments')).enrollments || [];
        return pending.length;
      }, { timeout: 60_000 }).toBe(1);
      const approved = await rpc('enrollment', 'ApproveEnrollment', {
        userCode: pending[0].userCode, newAgent: { name: 'Real E2E Agent' },
      }, { headers: { 'X-Bind-Ticket': ticket } });
      expect(approved.created).toBe(true);
      agentId = approved.agentId;
      expect(agentId).toMatch(/^[a-f0-9-]{36}$/);
      await expect.poll(() => login.exitCode, { timeout: 90_000 }).toBe(0);
      const bundle = await rpc('trust', 'GetTrustBundle');
      const key = bundle.signingKeys.find(k => k.algorithm === 'SIGNATURE_ALGORITHM_ED25519' || k.algorithm === 1);
      expect(key).toBeTruthy();
      fs.writeFileSync(path.join(artifacts, 'public-trust-bundle.json'), JSON.stringify(bundle, null, 2));
      const trustLog = execFileSync('sudo', ['/usr/lib/komari/komari-e2e/komari-host', 'trust',
        '--manifest', '/etc/komari/services/komari-e2e.json', '--endpoint', base,
        '--agent-id', agentId, '--panel-public-key', key.value, '--panel-key-id', key.keyId,
        '--service-user', process.env.USER], { encoding: 'utf8', timeout: 30_000 });
      fs.writeFileSync(path.join(artifacts, 'trust-install.log'), trustLog);
      execFileSync('sudo', ['systemctl', 'start', 'komari-e2e.service'], { timeout: 30_000 });
    });

    await test.step('real Agent history remains available across live metric-store reloads', async () => {
      function window() {
        const end = new Date();
        return { agentIds: [agentId], startTime: new Date(end.getTime() - 300_000).toISOString(), endTime: end.toISOString(), maxPoints: 100 };
      }
      await expect.poll(async () => {
        const result = await rpc('metrics', 'QueryMetrics', { ...window(), metrics: ['cpu.usage'], downsample: false });
        return (result.series || []).some(series => (series.queryPoints || []).length > 0);
      }, { timeout: 120_000, intervals: [2000, 5000] }).toBe(true);
      const original = (await rpc('metrics', 'GetDownsamplingPolicy')).policy;
      const policy = {
        enabled: !!original.enabled,
        minuteRetentionMinutes: original.minuteRetentionMinutes,
        fiveMinuteRetentionMinutes: original.fiveMinuteRetentionMinutes,
        hourRetentionHours: original.hourRetentionHours,
      };
      const results = [];
      for (let iteration = 0; iteration < 6; iteration++) {
        const [history, definitions, ping, reloaded] = await Promise.all([
          rpc('metrics', 'QueryMetrics', { ...window(), metrics: ['cpu.usage'], downsample: false }),
          rpc('metrics', 'ListMetricDefinitions'),
          rpc('metrics', 'GetPingStats', window()),
          rpc('metrics', 'SetDownsamplingPolicy', policy),
        ]);
        expect(history.series?.some(series => series.queryPoints?.length > 0)).toBe(true);
        expect(definitions.definitions?.some(item => item.name === 'cpu.usage')).toBe(true);
        expect(ping.endTime).toBeTruthy();
        expect(!!reloaded.policy.enabled).toBe(policy.enabled);
        results.push({ iteration, historySeries: history.series.length, definitions: definitions.definitions.length });
        // Bypass the short response cache before verifying the next instance.
        await delay(5500);
      }
      const after = await rpc('metrics', 'QueryMetrics', { ...window(), metrics: ['cpu.usage'], downsample: false });
      expect(after.series?.some(series => series.queryPoints?.length > 0)).toBe(true);
      fs.writeFileSync(path.join(artifacts, 'metric-store-reloads.json'), JSON.stringify(results, null, 2));
    });

    await test.step('real raw and summary input ownership agrees and built charts expose source precision', async () => {
      const end = new Date(Date.now() - 15_000);
      const start = new Date(end.getTime() - 5*60_000);
      const window = { agentIds:[agentId], metrics:['cpu.usage'], startTime:start.toISOString(), endTime:end.toISOString(), maxPoints:30 };
      const raw = await rpc('metrics','QueryMetrics',{...window, downsample:false});
      const summarized = await rpc('metrics','QueryMetrics',{...window, downsample:true, aggregation:'count'});
      const rawCount = (raw.series || []).reduce((n,s)=>n+(s.queryPoints || []).length,0);
      expect(rawCount).toBeGreaterThan(0);
      expect(summarized.series.length).toBeGreaterThan(0);
      let total = 0;
      for (const series of summarized.series) {
        expect(series.requestedInterval).toBe(series.interval);
        expect(series.coverage).toBe('complete');
        expect(series.sources?.length).toBeGreaterThan(0);
        expect(BigInt(series.acceptedThrough || '0')).toBeGreaterThanOrEqual(BigInt(series.summarizedThrough || '0'));
        for (const point of series.queryPoints || []) {
          expect(Number(point.value)).toBe(Number(point.sampleCount));
          expect(point.coverage).toBe('complete');
          total += Number(point.sampleCount);
        }
      }
      expect(total).toBe(rawCount);
      await page.goto(`/instance/${agentId}`);
      await page.locator('.km-load-chart').getByRole('radio', { name:/^1\s*hour$/i }).click();
      await expect(page.getByTestId('chart-source-precision').first()).toBeVisible();
      await expect(page.locator('.recharts-line-curve').first()).toBeVisible();
      await page.screenshot({path:path.join(artifacts,'chart-source-precision.png'),fullPage:true});
      const windows = [];
      for (const [hours, interval] of [[1,10],[6,60],[12,120],[24,240],[168,1800],[360,3600],[720,7200]]) {
        const last = new Date(Date.now()-15_000);
        const result = await rpc('metrics','QueryMetrics',{agentIds:[agentId],metrics:['cpu.usage'],startTime:new Date(last.getTime()-hours*3600_000).toISOString(),endTime:last.toISOString(),maxPoints:360,downsample:true});
        expect(result.series.length).toBeGreaterThan(0);
        for (const series of result.series) expect(series.interval).toBe(`${interval}s`);
        windows.push({hours,result});
      }
      for (const hours of [6,12]) {
        await page.locator('.km-load-chart').getByRole('radio',{name:new RegExp(`^${hours}\\s*hours?$`,'i')}).click();
        await expect(page.getByTestId('chart-source-precision').first()).toBeVisible();
        await expect(page.locator('.recharts-line-curve').first()).toBeVisible();
      }
      await page.screenshot({path:path.join(artifacts,'chart-window-12h.png'),fullPage:true});
      fs.writeFileSync(path.join(artifacts,'metric-chart-windows.json'),JSON.stringify(windows,null,2));
      fs.writeFileSync(path.join(artifacts,'metric-query-ownership.json'),JSON.stringify({rawCount,summarized},null,2));
    });

    await test.step('enabled rate limits allow 32 concurrent charts and Ping reads', async () => {
      await request('/api/admin/settings/', { rate_limit_enabled: true });
      const end = new Date();
      // Distinct maxPoints bypass shared cache keys: this exercises actual
      // bounded SQL work as well as HTTP admission through the real proxy.
      const began = Date.now();
      const results = await Promise.all(Array.from({ length: 32 }, async (_, i) => {
        const ping = i >= 24;
        const duration = i < 8 ? 30 * 86400_000 : 300_000;
        const window = {
          agentIds: [agentId], startTime: new Date(end.getTime() - duration).toISOString(),
          endTime: end.toISOString(), maxPoints: 100 + i,
        };
        const response = await rpc('metrics', ping ? 'GetPingStats' : 'QueryMetrics',
          ping ? window : { ...window, metrics: ['cpu.usage'], downsample: true });
        if (ping) expect(response.endTime).toBeTruthy();
        else expect(response.series?.some(series => series.queryPoints?.length > 0)).toBe(true);
        return { kind: ping ? 'ping' : 'chart', duration, elapsedMs: Date.now() - began };
      }));
      fs.writeFileSync(path.join(artifacts, 'concurrent-charts-ping.json'), JSON.stringify(results, null, 2));
      // Leave rate limiting enabled for the remaining real Agent, terminal,
      // file, history and backup assertions as an integration regression.
    });

    await test.step('enable real 2FA and apply a signed same-identity WebSSH grant', async () => {
      await request('/api/admin/2fa/generate', undefined, { method: 'GET' });
      secret = (await context.cookies()).find(c => c.name === '2fa_secret')?.value;
      expect(secret).toBeTruthy();
      console.log(`::add-mask::${secret}`);
      await request(`/api/admin/2fa/enable?code=${await freshCode()}`);
      let state;
      await expect.poll(async () => {
        state = await rpc('config', 'GetNodeConfigState', { agentId });
        return state.protocolVersion === 2 && Number(state.appliedRevision) > 0 && !!state.observation;
      }, { timeout: 150_000, intervals: [2000, 5000] }).toBe(true);
      const change = await rpc('config', 'UpdatePrivilegedDelivery', {
        agentId, expectedRevision: state.desired?.revision || state.appliedRevision,
        reason: 'Isolated GitHub-hosted E2E; preserve nonroot service identity', protocolVersion: 2,
        privileged: { remoteControlEnabled: true, websshEnabled: true, executionEnabled: false,
          enableGpu: false, rescueHelperEnabled: false, requiredPrivilegeMode: 'PRIVILEGE_MODE_LINUX_NON_ROOT' },
      });
      const revision = change.revision.revision;
      await rpc('config', 'ConfirmPrivilegedDelivery', { agentId, revision, twoFactor: { code: await freshCode() } });
      await expect.poll(async () => {
        state = await rpc('config', 'GetNodeConfigState', { agentId });
        return String(state.appliedRevision);
      }, { timeout: 150_000, intervals: [2000, 5000] }).toBe(String(revision));
      fs.writeFileSync(path.join(artifacts, 'applied-config.json'), JSON.stringify(state, null, 2));
      await expect.poll(async () => (await rpc('browser', 'GetAgent', { agentId })).agent?.capabilities?.webssh?.available,
        { timeout: 60_000 }).toBe(true);
    });

    await test.step('built account page rejects wrong OTP and password change requires its own challenge', async () => {
      await page.goto('/admin/account');
      const gate = page.locator('.km-account-2fa-gate');
      await expect(gate).toBeVisible();
      await beginVerification(gate);
      const counter = Math.floor(Date.now() / 30_000);
      const validCodes = [-1, 0, 1].map(n => totp(secret, counter + n));
      let invalid = '000000';
      while (validCodes.includes(invalid)) invalid = String(Number(invalid) + 1).padStart(6, '0');
      const rejected = page.waitForResponse(r => r.url().endsWith('.TrustedCredentialService/CompleteVerification'));
      await verifyCredential(gate, invalid);
      expect((await rejected).ok()).toBe(false);
      await expect(gate).toBeVisible();
      await expect(gate.getByRole('alert')).toBeVisible();
      await expect(gate.getByPlaceholder('000000')).toHaveValue('');
      const accepted = page.waitForResponse(r => new URL(r.url()).pathname === '/api/admin/2fa/verify');
      await verifyCredential(gate, await freshCode());
      expect((await accepted).ok()).toBe(true);
      await expect(gate).toHaveCount(0);
      const draft = `Ci9-${crypto.randomBytes(24).toString('hex')}`;
      let passwordWrites = 0;
      const onWrite = r => { if (new URL(r.url()).pathname === '/api/admin/update/user') passwordWrites++; };
      page.on('request', onWrite);
      await page.locator('#password').fill(draft);
      await page.locator('#password_repeat').fill(draft);
      await page.locator('.km-account-password-form').getByRole('button', { name: 'Change Password', exact: true }).click();
      const passwordDialog = page.getByRole('dialog');
      await expect(passwordDialog).toBeVisible();
      await expect(passwordDialog.getByPlaceholder('000000')).toHaveCount(0);
      const challenge = page.waitForRequest(r => r.url().endsWith('.TrustedCredentialService/BeginVerification'));
      await beginVerification(passwordDialog);
      expect(verificationResource(await challenge)).toMatch(/^account:password:/);
      expect(passwordWrites).toBe(0);
      await passwordDialog.getByRole('button', { name: 'Cancel', exact: true }).click();
      await expect(passwordDialog).toBeHidden();
      await expect(page.locator('#password')).toHaveValue('');
      await expect(page.locator('#password_repeat')).toHaveValue('');
      expect(passwordWrites).toBe(0);
      page.off('request', onWrite);
    });

    await test.step('real xterm input, remote output, export, search and system clipboard', async () => {
      await context.addInitScript(({ base, agentId }) => {
        localStorage.setItem('komari:file-transfer:chunk-size:v1', JSON.stringify({
          [`${base}|upload|${agentId}`]: 1048576, [`${base}|download|${agentId}`]: 1048576,
        }));
      }, { base, agentId });
      await page.goto(`/terminal?uuid=${agentId}`);
      await confirmTerminal();
      const terminal = page.locator('.km-terminal-session:visible textarea.xterm-helper-textarea');
      await terminal.focus();
      const suffix = crypto.randomBytes(6).toString('hex');
      const marker = `KOMARI-REAL-${suffix}`;
      await page.keyboard.type(`printf '%s%s\\n' 'KOMARI-' 'REAL-${suffix}'`);
      await page.keyboard.press('Enter');
      await expect.poll(exportText, { timeout: 45_000, intervals: [1000, 2000] }).toContain(marker);
      await tabMenu('Find');
      await page.locator('.km-terminal-search-floating input').fill(marker);
      await expect(page.locator('.km-terminal-search-floating')).toContainText('1/1');
      await terminal.focus();
      await page.keyboard.press('Control+Shift+C');
      await expect.poll(() => page.evaluate(() => navigator.clipboard.readText())).toBe(marker);
      // A real paste appends exactly one line to a runner-owned disposable file.
      const command = `printf '%s%s\\n' 'PASTE-' '${suffix}' >> ${shellQuote(fixture('paste.txt'))}`;
      await page.evaluate(text => navigator.clipboard.writeText(text), command);
      const pasted = page.waitForResponse(r => r.url().endsWith('.WebSSHService/SendSessionCommand'));
      await page.keyboard.press('Control+Shift+V');
      expect((await pasted).ok()).toBe(true);
      // Bash bracketed paste deliberately does not execute pasted input.
      // Confirm it with the same Enter keystroke a real user sends.
      await page.keyboard.press('Enter');
      await expect.poll(() => fs.existsSync(fixture('paste.txt')) ? fs.readFileSync(fixture('paste.txt'), 'utf8') : '')
        .toBe(`PASTE-${suffix}\n`);
      await delay(1000);
      expect(fs.readFileSync(fixture('paste.txt'), 'utf8')).toBe(`PASTE-${suffix}\n`);
      await page.locator('.km-terminal-search-floating button').last().click();
    });

    await test.step('rename, color, duplicate, drag and bulk close real terminal tabs', async () => {
      const active = () => page.getByRole('tab', { selected: true });
      await active().dblclick();
      await page.getByRole('textbox', { name: 'Rename tab', exact: true }).fill('Original E2E');
      await page.getByRole('textbox', { name: 'Rename tab', exact: true }).press('Enter');
      await expect(active()).toContainText('Original E2E');
      await active().click({ button: 'right' });
      await page.getByRole('menuitem', { name: 'Change tab color', exact: true }).hover();
      await page.getByRole('button', { name: '#dc143c', exact: true }).click();
      // Color swatches keep their submenu open so users can try more colors.
      // Radix hides the background accessibility tree while the menu is open.
      await page.keyboard.press('Escape');
      await page.keyboard.press('Escape');
      await expect(active().locator('div').first()).toHaveCSS('background-color', 'rgb(220, 20, 60)');
      await tabMenu('Duplicate tab');
      // A second tab shares the already authorized workspace identity.
      await expect(page.getByRole('dialog')).toHaveCount(0);
      await expect(page.locator('.km-terminal-session:visible textarea.xterm-helper-textarea')).toBeAttached();
      await expect(page.getByRole('tab')).toHaveCount(2);
      await active().dblclick();
      await page.getByRole('textbox', { name: 'Rename tab', exact: true }).fill('Duplicate E2E');
      await page.getByRole('textbox', { name: 'Rename tab', exact: true }).press('Enter');
      const first = await page.getByRole('tab').first().boundingBox();
      const second = await page.getByRole('tab').last().boundingBox();
      await page.mouse.move(second.x + second.width / 2, second.y + second.height / 2);
      await page.mouse.down();
      await page.mouse.move(first.x + first.width / 3, first.y + first.height / 2, { steps: 20 });
      await page.mouse.up();
      await expect(page.getByRole('tab').first()).toContainText('Duplicate E2E');
      await active().click({ button: 'right' });
      await page.getByRole('menuitem', { name: 'Close', exact: true }).hover();
      await page.getByRole('menuitem', { name: 'Others', exact: true }).click();
      await expect(page.getByRole('tab')).toHaveCount(1);
      await expect.poll(() => metrics.closedSessions).toBeGreaterThanOrEqual(1);
      expect(metrics.createdSessions).toBe(2);
      await expect(page.getByRole('menu', { includeHidden: true })).toHaveCount(0);
    });

    await test.step('real file browsing and multi-chunk browser upload/download with SHA256 equality', async () => {
      await tabMenu('File Manager');
      const location = page.getByRole('textbox', { name: 'Path', exact: true });
      await expect(location).toBeVisible();
      await location.fill(path.join(root, 'files'));
      await location.press('Enter');
      await expect(page.locator('[data-file-path]').filter({ hasText: 'editor.txt' })).toBeVisible();
      const bytes = crypto.randomBytes(2 * 1048576 + 73);
      await page.locator('input[type=file][multiple]').setInputFiles({
        name: 'roundtrip.bin', mimeType: 'application/octet-stream', buffer: bytes,
      });
      await expect.poll(() => fs.existsSync(fixture('roundtrip.bin')) ? sha256(fs.readFileSync(fixture('roundtrip.bin'))) : '',
        { timeout: 90_000 }).toBe(sha256(bytes));
      await expect.poll(() => metrics.uploadChunks).toBeGreaterThanOrEqual(3);
      expect(sha256(await downloadFile('roundtrip.bin'))).toBe(sha256(bytes));
      fs.writeFileSync(path.join(artifacts, 'file-roundtrip.json'), JSON.stringify({
        size: bytes.length, sha256: sha256(bytes), chunks: metrics.uploadChunks,
      }, null, 2));
    });

    await test.step('actual Monaco editor saves UTF-8 and zero-byte content through the Agent', async () => {
      await page.locator('[data-file-path]').filter({ hasText: 'editor.txt' }).dblclick();
      // Current Monaco uses a native EditContext surface in Chromium; address
      // its accessible input instead of assuming the old hidden textarea.
      const editor = page.locator('.km-file-editor').getByRole('textbox', { name: 'Editor content', exact: true });
      // EditContext's focus target has no visual box; Monaco draws the text
      // separately. Focus the real native target and use normal keyboard input.
      await expect(editor).toBeAttached();
      await editor.focus();
      await page.keyboard.press('Control+a');
      // Chromium EditContext synchronizes selection asynchronously. Send real
      // keystrokes at typing speed rather than a zero-delay CDP burst, and
      // verify the rendered buffer before testing the separate save transport.
      await page.keyboard.type('Monaco UTF-8: ', { delay: 50 });
      await page.keyboard.insertText('联调 ✓\n');
      await expect(page.locator('.km-file-editor .view-lines')).toContainText('Monaco UTF-8: 联调 ✓');
      await page.keyboard.press('Control+s');
      await expect.poll(() => fs.readFileSync(fixture('editor.txt'), 'utf8')).toBe('Monaco UTF-8: 联调 ✓\n');
      expect((await downloadFile('editor.txt')).toString()).toBe('Monaco UTF-8: 联调 ✓\n');
      await editor.focus();
      await page.keyboard.press('Control+a');
      await page.keyboard.press('Backspace');
      await page.keyboard.press('Control+s');
      await expect.poll(() => fs.statSync(fixture('editor.txt')).size).toBe(0);
      expect((await downloadFile('editor.txt')).length).toBe(0);
    });

    await test.step('file authorization requires the real workspace grant and rejects unsafe overwrite/recursive flags', async () => {
      expect(workspaceToken).toBeTruthy();
      const created = fixture('created.txt');
      const missing = await fileDenied('CreateEntry', { path: created, kind: 'ENTRY_KIND_FILE' }, { 'X-Komari-Workspace-Grant': '' });
      expect(missing.code).toBe('unauthenticated');
      expect(fs.existsSync(created)).toBe(false);
      const invalid = await fileDenied('CreateEntry', { path: created, kind: 'ENTRY_KIND_FILE' }, { 'X-Komari-Workspace-Grant': 'invalid-grant' });
      expect(invalid.code).toBe('unauthenticated');
      expect(fs.existsSync(created)).toBe(false);
      await mutate('CreateEntry', { path: created, kind: 'ENTRY_KIND_FILE', mode: '0600' });
      expect(fs.statSync(created).mode & 0o777).toBe(0o600);
      const overwrite = await fileDenied('CopyEntry', { source: fixture('roundtrip.bin'), destination: fixture('existing.txt'),
        overwrite: false });
      expect(overwrite.message).toMatch(/already exists|file exists/i);
      expect(fs.readFileSync(fixture('existing.txt'), 'utf8')).toBe('destination must survive\n');
      const recursive = await fileDenied('DeleteEntry', { path: fixture('nonempty'), recursive: false });
      expect(recursive.message).toMatch(/directory not empty/i);
      expect(fs.readFileSync(fixture('nonempty/keep.txt'), 'utf8')).toBe('keep\n');
    });

    await test.step('Agent executes copy, move, chmod, search and delete under the authorized OS identity', async () => {
      await mutate('CopyEntry', { source: fixture('roundtrip.bin'), destination: fixture('copy.bin'), overwrite: false });
      expect(sha256(fs.readFileSync(fixture('copy.bin')))).toBe(sha256(fs.readFileSync(fixture('roundtrip.bin'))));
      await mutate('MoveEntry', { source: fixture('copy.bin'), destination: fixture('moved.bin'), overwrite: false });
      expect(fs.existsSync(fixture('copy.bin'))).toBe(false);
      expect(fs.existsSync(fixture('moved.bin'))).toBe(true);
      await mutate('ChmodEntry', { path: fixture('moved.bin'), mode: '0600' });
      expect(fs.statSync(fixture('moved.bin')).mode & 0o777).toBe(0o600);
      const search = await rpc('file', 'SearchFiles', { agentId, path: path.join(root, 'files'), query: 'moved', maxResults: 10 });
      expect(search.matches.map(m => m.path)).toContain(fixture('moved.bin'));
      await mutate('DeleteEntry', { path: fixture('moved.bin'), recursive: false });
      expect(fs.existsSync(fixture('moved.bin'))).toBe(false);
    });

    await test.step('restart the disposable Agent and verify identity, authorization and file channel recovery', async () => {
      const before = await rpc('config', 'GetNodeConfigState', { agentId });
      execFileSync('sudo', ['systemctl', 'restart', 'komari-e2e.service'], { timeout: 30_000 });
      await expect.poll(async () => (await rpc('browser', 'GetAgent', { agentId })).agent?.capabilities?.webssh?.available,
        { timeout: 90_000, intervals: [2000, 5000] }).toBe(true);
      await expect.poll(async () => {
        try { return (await rpc('file', 'StatFile', { agentId, path: fixture('roundtrip.bin') })).entry?.size; }
        catch { return null; }
      }, { timeout: 90_000, intervals: [2000, 5000] }).toBe(String(2 * 1048576 + 73));
      const after = await rpc('config', 'GetNodeConfigState', { agentId });
      expect(after.appliedRevision).toBe(before.appliedRevision);
      expect(after.observation?.serviceId).toBe(before.observation?.serviceId);
    });

    await test.step('workspace revocation rejects old file grants and re-unlock requires fresh UI verification', async () => {
      const old = workspaceToken;
      expect((await rpc('workspace', 'LockWorkspace', { grantToken: old })).locked).toBe(true);
      const blockedPath = fixture('revoked-must-not-exist.txt');
      const denied = await fileDenied('CreateEntry', { path: blockedPath, kind: 'ENTRY_KIND_FILE' });
      expect(denied.code).toBe('unauthenticated');
      expect(fs.existsSync(blockedPath)).toBe(false);
      const download = await request(`/api/admin/client/${agentId}/file/download?${new URLSearchParams({ path: fixture('roundtrip.bin') })}`,
        undefined, { method: 'GET', allowFailure: true });
      expect(download.status()).toBe(401);
      await page.reload();
      await confirmTerminal();
      await expect.poll(() => workspaceToken).not.toBe(old);
      expect(sha256(await downloadFile('roundtrip.bin'))).toBe(sha256(fs.readFileSync(fixture('roundtrip.bin'))));
    });

    await test.step('minute traffic trends and daily billing remain independently available', async () => {
      // Real network traffic after the installed Agent's baseline. Fetch public
      // pinned source bytes; no forged timestamps, metric writes or DB seeding.
      execFileSync('curl', ['--fail', '--location', '--max-time', '30', '--output',
        path.join(root, 'traffic-observation.txt'),
        'https://raw.githubusercontent.com/r11234567/komari/cfed6d1f67abcd3316983095391728b88ae1652a/scripts/ci/e2e/real.spec.cjs']);
      await expect.poll(async () => {
        const trend = await rpc('browser', 'GetTrafficTrend', { window: '86400s', interval: '1200s' });
        return (trend.buckets || []).reduce((sum, b) => sum + BigInt(b.uploadBytes || 0) + BigInt(b.downloadBytes || 0), 0n) > 0n;
      }, { timeout: 120_000, intervals: [3000, 5000] }).toBe(true);
      const results = [];
      for (const seconds of [600, 1200, 1800]) {
        const trend = await rpc('browser', 'GetTrafficTrend', { window: '86400s', interval: `${seconds}s` });
        expect(trend.requestedInterval).toBe(`${seconds}s`);
        expect(trend.interval).toBe(`${seconds}s`);
        expect(['complete', 'partial']).toContain(trend.coverage);
        expect(trend.buckets).toHaveLength(86400 / seconds);
        expect(trend.dataEnd).toBeTruthy();
        expect(trend.accountingThrough).toBeTruthy();
        for (let i = 1; i < trend.buckets.length; i++) {
          expect(Date.parse(trend.buckets[i].startTime) - Date.parse(trend.buckets[i - 1].startTime)).toBe(seconds * 1000);
        }
        results.push(trend);
      }
      const billing = await rpc('dashboard', 'GetDashboardCharts', { charts: ['DASHBOARD_CHART_TRAFFIC'], rankingLimit: 5 });
      expect(billing.charts.traffic.error || '').toBe('');
      expect(billing.charts.traffic.todayError || '').toBe('');
      expect(billing.charts.traffic.trendError || '').toBe('');
      expect(billing.charts.traffic.daily).toHaveLength(30);
      expect(billing.charts.traffic.daily.at(-1).available).not.toBe(false);
      await page.goto('/admin');
      const live = page.locator('section').filter({ has: page.getByText('Live traffic over the last 24 hours', { exact: true }) });
      const daily = page.locator('section').filter({ has: page.getByText('Daily billable traffic', { exact: true }) });
      await expect(live).toBeVisible();
      await expect(daily).toBeVisible();
      await expect(live.getByText('Data unavailable', { exact: true })).toHaveCount(0);
      await expect(daily.getByText('Data unavailable', { exact: true })).toHaveCount(0);
      await page.screenshot({ path: path.join(artifacts, 'traffic-and-billing.png'), fullPage: true });
      fs.writeFileSync(path.join(artifacts, 'traffic-and-billing.json'), JSON.stringify({ trends: results, billing }, null, 2));
    });

    await test.step('real reports archive, charts survive raw deletion, CSV and complete backup use cold history', async () => {
      // Public settings, not forged timestamps or seeded history, shorten the
      // fixture rotation. All samples still come from the installed Agent.
      await request('/api/admin/settings/', { metric_hot_window_minutes: 1, metric_archive_segment_minutes: 1 });
      let storage;
      await expect.poll(() => {
        storage = JSON.parse(execFileSync('python3', ['scripts/ci/e2e/check-telemetry-retirement.py'], { encoding: 'utf8' }));
        return storage.complete_archives;
      }, { timeout: 210_000, intervals: [5000] }).toBeGreaterThan(0);
      const end = new Date();
      const start = new Date(end.getTime() - 20 * 60_000);
      const history = await rpc('metrics', 'QueryMetrics', {
        agentIds: [agentId], metrics: ['cpu.usage'], startTime: start.toISOString(), endTime: end.toISOString(), maxPoints: 100, downsample: true,
      });
      expect(history.series?.some(series => series.queryPoints?.some(point => Number(point.sampleCount) > 0))).toBe(true);
      const created = await (await request('/api/admin/history/export', {
        uuid: agentId, category: 'resource', start: start.toISOString(), end: end.toISOString(),
      })).json();
      expect(created.data?.id).toBeTruthy();
      let job;
      await expect.poll(async () => {
        job = (await (await request(`/api/admin/history/export/${created.data.id}`, undefined, { method: 'GET' })).json()).data;
        if (job.status === 'failed') throw new Error(job.error);
        return job.status;
      }, { timeout: 90_000 }).toBe('done');
      const csv = await (await request(`/api/admin/history/export/${job.id}/download`, undefined, { method: 'GET' })).body();
      expect(csv.toString('utf8').split('\n').length).toBeGreaterThan(2);
      fs.writeFileSync(path.join(artifacts, 'real-history.csv'), csv);
      const backup = await request('/api/admin/download/backup?scope=full', undefined, { method: 'GET' });
      // Credentials stay inside the disposable fixture, outside uploaded artifacts.
      fs.writeFileSync(path.join(root, 'real-full-backup.zip'), await backup.body());
      fs.writeFileSync(path.join(artifacts, 'real-storage-roundtrip.json'), JSON.stringify({ ...storage, csvBytes: csv.length }, null, 2));
    });
  } finally {
    fs.writeFileSync(path.join(artifacts, 'browser-events.json'), JSON.stringify(events, null, 2));
    fs.writeFileSync(path.join(artifacts, 'browser-metrics.json'), JSON.stringify(metrics, null, 2));
    await info.attach('browser-events', { body: JSON.stringify(events, null, 2), contentType: 'application/json' });
    await page.screenshot({ path: info.outputPath('last-state.png'), fullPage: true }).catch(() => {});
  }
});
