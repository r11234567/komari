// Environment preparation for database-only qualification. All samples and
// probes come from the installed Agent; no credentials or databases are artifacts.
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const { spawn, execFileSync } = require('node:child_process');
const { request } = require('playwright');
require('./guard.cjs');

const root = process.env.E2E_ROOT, artifacts = process.env.E2E_ARTIFACTS;
const base = 'https://127.0.0.1:29443';
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const assert = (ok, message) => { if (!ok) throw Error(message); };
let context, login;
async function until(label, seconds, read, ready) {
  const deadline = Date.now() + seconds * 1000;
  while (true) {
    const value = await read();
    if (ready(value)) return value;
    assert(Date.now() < deadline, `${label}: timed out after ${seconds}s`);
    await sleep(2000);
  }
}
async function run() {
  context = await request.newContext({ baseURL: base });
  async function api(url, data) {
    const response = await context.post(url, {
      data, headers: { Origin: base, 'Connect-Protocol-Version': '1' }, timeout: 30_000,
    });
    try {
      assert(response.ok(), `${url}: HTTP ${response.status()} ${(await response.text()).slice(0, 500)}`);
      return await response.json();
    } finally { await response.dispose(); }
  }
  const rpc = (service, method, data = {}) => api(`/komari.${service}/${method}`, data);
  const password = `Ci9-${crypto.randomBytes(24).toString('hex')}`;
  console.log(`::add-mask::${password}`);
  await api('/api/install/complete', {
    username: 'e2e-user', password, sitename: 'Komari database qualification',
    description: '', metric_dsn: './data/metrics.db',
  });
  await until('Panel after installation', 60, async () => {
    const r = await context.get('/api/version');
    try { return r.status(); } finally { await r.dispose(); }
  }, status => status === 200);
  await api('/api/login', { username: 'e2e-user', password, '2fa_code': '' });
  const ticket = (await api('/api/bind/ticket', {})).data.ticket;
  assert(ticket, 'No enrollment ticket');
  console.log(`::add-mask::${ticket}`);
  const log = fs.openSync(path.join(root, 'database-enrollment-private.log'), 'w', 0o600);
  try {
    login = spawn('/usr/lib/komari/komari-e2e/komari-agent', ['login', '--endpoint', base], {
      stdio: ['ignore', log, log],
    });
  } finally { fs.closeSync(log); }
  let spawnError;
  login.on('error', error => { spawnError = error; });
  fs.writeFileSync(path.join(root, 'login.pid'), String(login.pid));
  const pending = await until('Actual Agent enrollment', 60, async () => {
    if (spawnError) throw spawnError;
    assert(login.exitCode === null, `Agent login exited before approval: ${login.exitCode}`);
    return (await rpc('enrollment.v1.EnrollmentAdminService', 'ListPendingEnrollments')).enrollments || [];
  }, items => items.length === 1);
  const approval = await context.post('/komari.enrollment.v1.EnrollmentAdminService/ApproveEnrollment', {
    data: { userCode: pending[0].userCode, newAgent: { name: 'Real database qualification Agent' } },
    headers: { Origin: base, 'Connect-Protocol-Version': '1', 'X-Bind-Ticket': ticket }, timeout: 30_000,
  });
  let approved;
  try {
    assert(approval.ok(), `Enrollment approval: HTTP ${approval.status()}`);
    approved = await approval.json();
  } finally { await approval.dispose(); }
  assert(approved.created && /^[a-f0-9-]{36}$/.test(approved.agentId), 'Actual Agent was not created');
  const agentId = approved.agentId;
  await until('Agent saves real credentials', 90, async () => {
    if (spawnError) throw spawnError;
    assert(login.exitCode === null || login.exitCode === 0, `Agent login failed: ${login.exitCode}`);
    return login.exitCode;
  }, code => code === 0);
  fs.unlinkSync(path.join(root, 'login.pid'));
  const bundle = await rpc('enrollment.v1.EnrollmentService', 'GetTrustBundle');
  const key = bundle.signingKeys?.find(k => k.algorithm === 'SIGNATURE_ALGORITHM_ED25519' || k.algorithm === 1);
  assert(key, 'No real panel trust key');
  execFileSync('sudo', ['/usr/lib/komari/komari-e2e/komari-host', 'trust',
    '--manifest', '/etc/komari/services/komari-e2e.json', '--endpoint', base,
    '--agent-id', agentId, '--panel-public-key', key.value, '--panel-key-id', key.keyId,
    '--service-user', process.env.USER], { timeout: 30_000 });
  execFileSync('sudo', ['systemctl', 'start', 'komari-e2e.service'], { timeout: 30_000 });
  const created = await rpc('admin.v1.PingTaskService', 'CreatePingTask', {
    name: 'Real database qualification TCP', target: '127.0.0.1:29444', type: 'tcp',
    intervalSeconds: 5, clients: [agentId], defaultOn: false,
  });
  const pingTaskId = created.taskId;
  assert(pingTaskId > 0, 'No real TCP probe task');
  const result = await until('Actual Agent TCP samples', 120, async () => {
    const end = new Date();
    return rpc('metrics.v1.MetricsService', 'GetPingStats', {
      agentIds: [agentId], taskIds: [String(pingTaskId)],
      startTime: new Date(end.getTime() - 300_000).toISOString(), endTime: end.toISOString(), maxPoints: 100,
    });
  }, body => body.stats?.some(s => String(s.taskId) === String(pingTaskId) && s.valid > 0));
  fs.writeFileSync(path.join(artifacts, 'real-agent-ping.json'), JSON.stringify(result, null, 2));
  // The retirement check requires real ingested and summarized CPU data.
  // Wait for the production scheduler, without manufacturing a SQL summary.
  await until('Actual Agent summarized CPU samples', 120, async () => {
    const end = new Date();
    return rpc('metrics.v1.MetricsService', 'QueryMetrics', {
      agentIds: [agentId], metrics: ['cpu.usage'],
      startTime: new Date(end.getTime() - 300_000).toISOString(), endTime: end.toISOString(),
      maxPoints: 100, downsample: true,
    });
  }, body => body.series?.some(s => s.queryPoints?.some(p => p.value !== undefined) &&
    BigInt(s.summarizedThrough || '0') > 0n));
  await context.storageState({ path: path.join(root, 'qualification-browser-state.json') });
  fs.chmodSync(path.join(root, 'qualification-browser-state.json'), 0o600);
  fs.writeFileSync(path.join(root, 'qualification-scope.json'), JSON.stringify({ agentId, pingTaskId }), { mode: 0o600 });
  const report = { scope: 'database_only', agentId, pingTaskId, actual_agent_samples: true,
    omitted_scenarios: ['TOTP', 'terminal', 'files', 'Agent upgrade', 'legacy full-feature backup ceremony'],
    qualification_complete: false };
  fs.writeFileSync(path.join(artifacts, 'database-bootstrap.json'), JSON.stringify(report, null, 2));
  console.log(JSON.stringify(report));
}
run().catch(error => {
  console.error(error);
  process.exitCode = 1;
  fs.writeFileSync(path.join(artifacts, 'database-bootstrap-error.txt'), error.stack || String(error));
}).finally(async () => {
  if (login && login.exitCode === null) login.kill();
  if (context) await context.dispose();
});
