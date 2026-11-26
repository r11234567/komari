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
const agentState = path.join(root, 'traffic-agent-state');
const credentials = path.join(agentState, 'credentials.json');
const inbox = path.join(root, 'traffic-inbox');
const distro = process.env.KOMARI_TRAFFIC_E2E_DISTRO || 'native';
assert(['native', 'debian13', 'rocky9', 'alpine322'].includes(distro), 'Invalid traffic E2E distribution');
const image = `komari-traffic-e2e:${distro}`;
const fixtureID = crypto.randomBytes(6).toString('hex');
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
    return { child, result, stop: () => child.kill() };
  } finally {
    fs.closeSync(fd);
  }
}

function containerProcess(name, options, command, log) {
  const container = `komari-traffic-${fixtureID}-${name}`;
  const fixture = privateProcess('docker', ['run', '--rm', '--name', container, ...options, image, ...command], log);
  fixture.stop = () => {
    try {
      execFileSync('docker', ['stop', '--time', '8', container], { stdio: 'ignore', timeout: 15000 });
    } catch {
      fixture.child.kill();
    }
  };
  return fixture;
}

function bind(source, target, writable = false) {
  return ['--mount', `type=bind,src=${source},dst=${target}${writable ? '' : ',readonly'}`];
}

function startAgent(name, args, log, evidenceEnabled = false) {
  if (distro === 'native') {
    const env = evidenceEnabled ? { ...process.env, KOMARI_TRAFFIC_EVIDENCE_INBOX: inbox } : process.env;
    return privateProcess(agent, args, log, env);
  }
  const options = [
    '--network', 'host', '--user', `${process.getuid()}:${process.getgid()}`,
    '--cap-drop', 'all', '--security-opt', 'no-new-privileges',
    '-e', `HOME=${agentState}`, '-e', 'SSL_CERT_FILE=/fixtures/panel-ca.crt',
    ...bind(agentState, agentState, true),
    ...bind(path.join(root, 'tls', 'ca.crt'), '/fixtures/panel-ca.crt'),
    ...bind(path.join(root, 'bin', 'traffic-container-agent'), '/fixtures/komari-agent'),
  ];
  if (evidenceEnabled) {
    options.push('-e', `KOMARI_TRAFFIC_EVIDENCE_INBOX=${inbox}`, ...bind(inbox, inbox));
  }
  return containerProcess(name, options, ['/fixtures/komari-agent', ...args], log);
}

function startCollector() {
  const fixture = path.resolve('e2e-agent/scripts/ci/test-traffic-anomaly.py');
  const xray = path.join(root, 'bin', 'xray');
  if (distro === 'native') {
    return privateProcess('sudo', ['-n', 'python3', '-B', fixture, agent, xray, inbox], 'traffic-collector.log');
  }
  return containerProcess('capture', [
    '--privileged', '--network', 'none', '--ulimit', 'memlock=335544320:335544320',
    ...bind(inbox, inbox, true),
    ...bind(fixture, '/fixtures/test-traffic-anomaly.py'),
    ...bind(xray, '/fixtures/xray'),
    ...bind(path.join(root, 'bin', 'traffic-container-agent'), '/fixtures/komari-agent'),
  ], ['python3', '-B', '/fixtures/test-traffic-anomaly.py', '/fixtures/komari-agent', '/fixtures/xray', inbox], 'traffic-collector.log');
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
    fs.mkdirSync(agentState, { mode: 0o700 });
    login = startAgent('enroll', ['login', '--endpoint', base, '--credentials-file', credentials], 'traffic-enrollment.log');
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
      '--runtime-state-file', path.join(agentState, 'runtime.json'),
      '--privileged-state-file', path.join(agentState, 'privileged.json'),
      '--replay-state-file', path.join(agentState, 'replay.json'),
    ];
    daemon = startAgent('upload', daemonArgs, 'traffic-agent.log', true);
    const list = () => jsonPost(session.request, metrics + 'ListTrafficEvidence', { agentId });
    const empty = await list();
    assert.equal(empty.incidents?.length ?? 0, 0);
    assert.equal(empty.hours?.length ?? 0, 0);

    collector = startCollector();
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

    daemon.stop();
    await daemon.result;
    const initialUploads = successfulUploads();
    execFileSync('sudo', ['systemctl', 'restart', 'komari-e2e-panel.service'], { timeout: 30000 });
    await until('panel after restart', 45, async () => {
      try { return await list(); } catch { return null; }
    }, body => body?.incidents?.some(row => row.id === incident.id));
    const afterRestart = await jsonPost(session.request, metrics + 'GetTrafficEvidenceIncident', payload);
    assert(afterRestart.incident.ingressSources.some(peer => peer.ip === '2001:db8:1::2'));
    daemon = startAgent('replay', daemonArgs, 'traffic-agent-replay.log', true);
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
      if (p?.child.exitCode === null && p.child.signalCode === null) p.stop();
    }
    if (collector?.child.exitCode === null && collector.child.signalCode === null) {
      collector.stop();
    }
    if (browser) await browser.close();
    await bootstrap.dispose();
  }
}

main().catch(error => {
  console.error(error);
  process.exitCode = 1;
});
