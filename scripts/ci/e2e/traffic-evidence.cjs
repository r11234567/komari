const assert = require('node:assert/strict');
const { execFileSync, spawn } = require('node:child_process');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { chromium, expect, request } = require('@playwright/test');
require('./guard.cjs');

const root = process.env.E2E_ROOT;
const base = 'https://127.0.0.1:29443';
const metrics = '/komari.metrics.v1.MetricsService/';
const agent = '/usr/lib/komari/komari-e2e/komari-agent';
const credentials = path.join(root, 'traffic-agent-credentials.json');
const inbox = path.join(root, 'traffic-inbox');
const headers = { Origin: base, 'Connect-Protocol-Version': '1' };

async function jsonPost(context, url, data = {}) {
  const reply = await context.post(url, { data, headers, timeout: 15000 });
  try {
    assert(reply.ok(), `${url}: HTTP ${reply.status()}`);
    return await reply.json();
  } finally {
    await reply.dispose();
  }
}

async function until(label, seconds, read, ready) {
  const deadline = Date.now() + seconds * 1000;
  while (Date.now() < deadline) {
    const value = await read();
    if (ready(value)) return value;
    await new Promise(resolve => setTimeout(resolve, 1000));
  }
  throw new Error(`Timed out waiting for ${label}`);
}

function privateProcess(binary, args, name, env = process.env) {
  const fd = fs.openSync(path.join(root, name), 'w', 0o600);
  try {
    const child = spawn(binary, args, { stdio: ['ignore', fd, fd], env });
    const result = new Promise((resolve, reject) => {
      child.once('error', reject);
      child.once('exit', (code, signal) => resolve({ code, signal }));
    });
    return { child, result };
  } finally {
    fs.closeSync(fd);
  }
}

function successfulUploads() {
  const logs = fs.readFileSync(path.join(process.env.E2E_ARTIFACTS, 'nginx-access.log'), 'utf8');
  return logs.split('\n').filter(line => line.includes('/SubmitTrafficEvidence 200 ')).length;
}

async function main() {
  const bootstrap = await request.newContext({ baseURL: base });
  let browser, login, daemon, collector;
  try {
    const username = 'evidence-e2e';
    const password = `Ci9-${crypto.randomBytes(24).toString('hex')}`;
    if (process.env.GITHUB_ACTIONS === 'true') console.log(`::add-mask::${password}`);
    await jsonPost(bootstrap, '/api/install/complete', {
      username, password, sitename: 'Traffic evidence E2E', description: '', metric_dsn: './data/metrics.db',
    });
    await until('installed panel', 60, async () => {
      const result = await bootstrap.get('/api/version');
      try { return result.status(); } finally { await result.dispose(); }
    }, status => status === 200);

    browser = await chromium.launch();
    const session = await browser.newContext({ baseURL: base, locale: 'en-US' });
    const page = await session.newPage();
    await page.goto('/login?returnTo=/admin/dashboard');
    await page.locator('.km-login-form input[name=username]').fill(username);
    await page.locator('.km-login-form input[name=password]').fill(password);
    await page.locator('.km-login-form button[type=submit]').click();
    await page.waitForURL(url => url.pathname === '/admin/dashboard', { timeout: 30000 });

    const ticket = await jsonPost(session.request, '/api/bind/ticket');
    assert(ticket.data.expires_in > 0);
    login = privateProcess(agent, ['login', '--endpoint', base, '--credentials-file', credentials], 'traffic-enrollment.log');
    const enrollment = '/komari.enrollment.v1.EnrollmentAdminService/';
    const candidates = await until('actual Agent enrollment', 60, async () => {
      assert.equal(login.child.exitCode, null, 'Agent exited before enrollment approval');
      return (await jsonPost(session.request, enrollment + 'ListPendingEnrollments')).enrollments || [];
    }, rows => rows.length === 1);
    const approved = await jsonPost(session.request, enrollment + 'ApproveEnrollment', {
      userCode: candidates[0].userCode, newAgent: { name: 'Traffic evidence node' },
    });
    assert(approved.created && approved.agentId);
    const agentId = approved.agentId;
    await until('credential file to be saved', 90, async () => {
      assert([null, 0].includes(login.child.exitCode), 'Agent enrollment failed');
      return login.child.exitCode;
    }, code => code === 0);
    assert.deepEqual(await login.result, { code: 0, signal: null });

    const gid = String(process.getgid());
    execFileSync('sudo', ['install', '-d', '-o', 'root', '-g', gid, '-m', '2750', inbox]);
    const daemonArgs = [
      '--endpoint', base, '--credentials-file', credentials, '--disable-auto-update',
      '--runtime-state-file', path.join(root, 'traffic-agent-runtime.json'),
      '--privileged-state-file', path.join(root, 'traffic-agent-privileged.json'),
      '--replay-state-file', path.join(root, 'traffic-agent-replay.json'),
    ];
    const daemonEnv = { ...process.env, KOMARI_TRAFFIC_EVIDENCE_INBOX: inbox };
    daemon = privateProcess(agent, daemonArgs, 'traffic-agent.log', daemonEnv);
    const list = () => jsonPost(session.request, metrics + 'ListTrafficEvidence', { agentId });
    const empty = await list();
    assert.equal(empty.incidents?.length ?? 0, 0);
    assert.equal(empty.hours?.length ?? 0, 0);

    collector = privateProcess('sudo', [
      '-n', 'python3', '-B', path.resolve('e2e-agent/scripts/ci/test-traffic-anomaly.py'),
      agent, path.join(root, 'bin', 'xray'), inbox,
    ], 'traffic-collector.log');
    const captured = await until('real flow snapshot and Xray hour to arrive via the enrolled Agent', 550, async () => {
      assert.equal(daemon.child.exitCode, null, 'Agent uploader stopped');
      assert([null, 0].includes(collector.child.exitCode), 'Real traffic fixture stopped');
      return list();
    }, body => body.incidents?.length === 1 && body.hours?.some(hour => BigInt(hour.services?.xray?.txBytes || 0) >= 8n << 20n));
    const incident = captured.incidents[0];
    assert.equal(incident.reason, 'fast_unexplained_ingress');
    assert.equal(incident.ingressSources, undefined, 'summary disclosed source addresses');
    assert.equal(incident.egressDestinations, undefined, 'summary disclosed destination addresses');
    assert.equal(incident.interfaceName, 'edge0');
    const payload = { agentId, incidentId: incident.id };
    const guest = await bootstrap.post(metrics + 'GetTrafficEvidenceIncident', { data: payload, headers });
    try {
      assert([401, 403].includes(guest.status()), 'guest can access IP evidence');
      assert(!((await guest.text()).includes('192.0.2.2')));
    } finally {
      await guest.dispose();
    }

    await page.goto('/admin/traffic-evidence');
    await expect(page.getByRole('heading', { name: 'Traffic evidence' })).toBeVisible();
    await expect(page.getByText('Collecting', { exact: true })).toBeVisible();
    await expect(page.getByText('192.0.2.2', { exact: true })).toHaveCount(0);
    await page.getByRole('button', { name: 'View', exact: true }).click();
    const dialog = page.getByRole('dialog');
    const sources = dialog.getByRole('region', { name: 'Ingress source IPs' });
    for (const ip of ['192.0.2.2', '2001:db8:1::2']) {
      const row = sources.getByRole('row').filter({ hasText: ip });
      await expect(row).toBeVisible();
      const label = await row.locator('[title$=" B"]').getAttribute('title');
      const count = Number(label.replace(/[^0-9]/g, ''));
      assert(count >= 1_500_000 && count <= 1_650_000, `Unexpected byte count for ${ip}: ${label}`);
    }
    await dialog.getByRole('button', { name: 'Close' }).click();
    await page.getByRole('tab', { name: 'Hourly breakdown' }).click();
    await expect(page.getByText(/^Xray ·/).first()).toBeVisible();
    await expect(page.getByText('192.0.2.2', { exact: true })).toHaveCount(0);

    daemon.child.kill();
    await daemon.result;
    const initialUploads = successfulUploads();
    execFileSync('sudo', ['systemctl', 'restart', 'komari-e2e-panel.service'], { timeout: 30000 });
    await until('panel after restart', 45, async () => {
      try { return await list(); } catch { return null; }
    }, body => body?.incidents?.some(row => row.id === incident.id));
    const afterRestart = await jsonPost(session.request, metrics + 'GetTrafficEvidenceIncident', payload);
    assert(afterRestart.incident.ingressSources.some(peer => peer.ip === '2001:db8:1::2'));
    daemon = privateProcess(agent, daemonArgs, 'traffic-agent-replay.log', daemonEnv);
    await until('an authenticated duplicate submission after restarting the Agent', 45, async () => {
      assert.equal(daemon.child.exitCode, null);
      return successfulUploads();
    }, count => count >= initialUploads + captured.hours.length + captured.incidents.length);
    assert.equal((await list()).incidents.length, 1, 'retry duplicated the incident');

    const captureExit = await collector.result;
    assert.deepEqual(captureExit, { code: 0, signal: null });
    console.log(fs.readFileSync(path.join(root, 'traffic-collector.log'), 'utf8'));
    console.log('PASS: real capture, authenticated upload, guest denial, browser IP and byte counts, Xray attribution, restart and replay');
  } catch (error) {
    for (const name of ['traffic-agent.log', 'traffic-agent-replay.log', 'traffic-collector.log']) {
      const file = path.join(root, name);
      if (fs.existsSync(file)) console.error(fs.readFileSync(file, 'utf8').slice(-6000));
    }
    throw error;
  } finally {
    for (const p of [daemon, login]) {
      if (p?.child.exitCode === null && p.child.signalCode === null) p.child.kill();
    }
    if (collector?.child.exitCode === null && collector.child.signalCode === null) {
      collector.child.kill();
    }
    if (browser) await browser.close();
    await bootstrap.dispose();
  }
}

main().catch(error => {
  console.error(error);
  process.exitCode = 1;
});
