const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const path = require('node:path');
const { spawn, execFileSync, spawnSync } = require('node:child_process');
const { chromium, expect } = require(path.join(process.env.NODE_PATH, '@playwright/test'));
const { freshTOTP } = require('./totp.cjs');

const dir = process.argv[2];
const browserURL = 'https://127.0.0.1:29483';
const agentURL = 'https://10.0.2.100:8443';
const manifest = '/etc/komari/services/komari-agent.json';
const binaryDir = '/usr/lib/komari/komari-agent';
const sshOptions = ['-i', path.join(dir, 'client'), '-o', `UserKnownHostsFile=${path.join(dir, 'known_hosts')}`,
  '-o', 'StrictHostKeyChecking=yes', '-o', 'BatchMode=yes', '-o', 'LogLevel=ERROR'];

function args(port, action) {
  return [...sshOptions, '-p', String(port), 'root@127.0.0.1', action];
}

function ssh(port, action) {
  return execFileSync('ssh', args(port, action), { encoding: 'utf8', timeout: 25000 }).trim();
}

async function rpc(context, url, data = {}, contentType = 'application/json') {
  return context.request.post(url, { data, headers: {
    Origin: browserURL, 'Content-Type': contentType, 'Connect-Protocol-Version': '1',
  } });
}

async function receive(context, url, data) {
  const response = await rpc(context, url, data);
  assert(response.ok(), `${url} returned HTTP ${response.status()}`);
  return response.json();
}

async function verifyRootApproval(context, page) {
  const qr = await context.request.post('/api/admin/2fa/generate', { headers: { Origin: browserURL } });
  assert.equal(qr.status(), 200);
  const cookie = (await context.cookies()).find(({ name }) => name === '__Host-komari_2fa_secret');
  assert(cookie);
  let otp = await freshTOTP(cookie.value, -1);
  await receive(context, `/api/admin/2fa/enable?code=${otp.code}`);
  const confirm = async () => {
    await page.getByRole('button', { name: '确认此版本', exact: true }).click();
    const dialog = page.getByRole('dialog').filter({ hasText: '确认特权配置版本' });
    await dialog.getByRole('button', { name: 'Start verification / send code' }).click();
    otp = await freshTOTP(cookie.value, otp.counter);
    await dialog.locator('input').fill(otp.code);
    await dialog.getByRole('button', { name: 'Verify and continue' }).click();
    await expect(dialog).toHaveCount(0, { timeout: 15000 });
  };
  await context.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: browserURL });
  await page.goto('/admin/servers');
  const node = page.getByRole('row').filter({ hasText: 'Host approval fixture node' });
  await node.getByRole('button', { name: '配置下发', exact: true }).click();
  const deployment = page.getByRole('dialog', { name: '配置下发', exact: true });
  const heading = deployment.getByText('特权配置下发状态', { exact: true });
  await expect(heading).toBeVisible();
  const privilege = heading.locator('..');
  const applied = privilege.getByText('已应用', { exact: true });
  await expect(applied).toBeVisible({ timeout: 30000 });
  const edit = page.getByRole('button', { name: '编辑新版本', exact: true });
  const save = page.getByRole('button', { name: '保存并等待确认', exact: true });
  const checkbox = name => page.getByRole('checkbox', { name, exact: true });
  await edit.click();
  await checkbox('远程控制').check();
  await checkbox('WebSSH').check();
  await save.click();
  await confirm();
  await expect(applied).toBeVisible({ timeout: 30000 });
  await expect(deployment.getByText(/^期望配置（r/)).toHaveCount(0);
  await expect(page.getByText('WebSSH：开启', { exact: true })).toBeVisible();
  await expect(page.getByText('允许 WebSSH 和远程执行使用 root：关闭', { exact: true })).toBeVisible();

  await edit.click();
  await checkbox('允许 WebSSH 和远程执行使用 root').check();
  await save.click();
  await confirm();
  await expect(page.getByText('等待本机管理员操作', { exact: true })).toBeVisible();
  await expect(page.getByText(/Komari WebSSH 不支持此授权/)).toBeVisible();
  await expect(page.getByText('允许 WebSSH 和远程执行使用 root：关闭', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: '复制本机授权命令', exact: true }).click();
  const command = await page.evaluate(() => navigator.clipboard.readText());
  assert.match(command, /^sudo komari-host apply --manifest \/etc\/komari\/services\/komari-agent\.json --revision [1-9][0-9]*$/);
  const unattended = spawnSync('ssh', args(29482, command), { encoding: 'utf8', timeout: 10000 });
  assert.equal(unattended.status, 1);
  assert.match(unattended.stderr, /direct interactive|direct SSH|active SSH/);
  execFileSync('python3', [path.join(__dirname, 'host-approval-pam.py'), dir, command],
    { timeout: 85000, stdio: 'inherit' });
  await expect(applied).toBeVisible({ timeout: 30000 });
  await expect(deployment.getByText(/^期望配置（r/)).toHaveCount(0);
  await expect(page.getByText('允许 WebSSH 和远程执行使用 root：开启', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: '复制本机授权命令', exact: true })).toHaveCount(0);
  console.log('Verified an existing root Agent with WebSSH: panel TOTP, clipboard command, SSH/PAM confirmation and applied acknowledgement');
}

async function main() {
  const password = `Ci9-${crypto.randomBytes(24).toString('hex')}`;
  const username = 'host-approval-fixture';
  const browser = await chromium.launch();
  let enrollment;
  try {
    const context = await browser.newContext({ baseURL: browserURL, ignoreHTTPSErrors: true, locale: 'en-US' });
    const page = await context.newPage();
    await expect.poll(async () => (await context.request.get('/api/install/status')).status(), { timeout: 25000 }).toBe(200);
    await receive(context, '/api/install/complete', { username, password, sitename: 'Komari Host approval', description: '', metric_dsn: './data/metrics.db' });
    await expect.poll(async () => (await context.request.get('/api/version')).status()).toBe(200);
    await page.goto('/login?returnTo=/admin/dashboard');
    await page.locator('.km-login-form input[name=username]').fill(username);
    await page.locator('.km-login-form input[name=password]').fill(password);
    await page.locator('.km-login-form button[type=submit]').click();
    await page.waitForURL(url => url.pathname === '/admin/dashboard');
    await receive(context, '/api/bind/ticket');
    enrollment = spawn('ssh', args(29482, `${binaryDir}/komari-agent login --endpoint ${agentURL}`), { stdio: 'ignore' });
    const done = new Promise(resolve => enrollment.once('exit', resolve));
    const admin = '/komari.enrollment.v1.EnrollmentAdminService/';
    let candidate;
    await expect.poll(async () => {
      const result = await receive(context, admin + 'ListPendingEnrollments');
      candidate = result.enrollments?.[0];
      return candidate?.userCode;
    }, { timeout: 30000 }).toBeTruthy();
    const { agentId } = await receive(context, admin + 'ApproveEnrollment', {
      userCode: candidate.userCode, newAgent: { name: 'Host approval fixture node' },
    });
    assert.match(agentId, /^[0-9a-f-]{36}$/);
    await expect.poll(() => enrollment.exitCode, { timeout: 45000 }).toBe(0);
    assert.equal(await done, 0);
    await expect.poll(async () => (await context.request.get('/api/version')).status()).toBe(200);
    const [panelKeyID, panelKey] = ssh(29482, `python3 -c 'import json; k=json.load(open("/var/lib/komari/komari-agent/credentials.json"))["control_plane_keys"][0]; print(k["key_id"],k["value"])'`).split(' ');
    assert.match(panelKeyID, /^[\w-]+$/);
    assert.match(panelKey, /^[a-zA-Z0-9+/=]+$/);
    ssh(29482, `${binaryDir}/komari-host trust --manifest ${manifest} --agent-id ${agentId} --endpoint ${agentURL} --panel-key-id ${panelKeyID} --panel-public-key ${panelKey} --service-user approval-fixture && systemctl start komari-agent.service`);

    await verifyRootApproval(context, page);
  } finally {
    if (enrollment && enrollment.exitCode === null) enrollment.kill();
    await browser.close();
  }
}

main().catch(error => { console.error(error.message); process.exitCode = 1; });
