const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');
const { spawn, execFileSync, spawnSync } = require('node:child_process');
const { chromium, expect } = require(path.join(process.env.NODE_PATH, '@playwright/test'));
const { freshTOTP } = require('./totp.cjs');

const dir = process.argv[2];
const browserURL = 'https://127.0.0.1:29483';
const agentURL = 'https://10.0.2.100:8443';
const manifest = '/etc/komari/services/komari-agent.json';
const binaryDir = '/usr/lib/komari/komari-agent';
const archiveDir = '/root/fixture/archives';
const protocol = '/komari.diag.v1.SealedRecordService/';
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

async function verifyRootApproval(context, page, agentId) {
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
  const node = page.getByRole('row').filter({ hasText: 'Synthetic QGA node' });
  await node.getByRole('button', { name: '配置下发', exact: true }).click();
  const deployment = page.getByRole('dialog', { name: '配置下发', exact: true });
  await expect(deployment.getByText('特权配置下发状态', { exact: true })).toBeVisible();
  const applied = page.getByText('已应用', { exact: true });
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
  execFileSync('python3', [path.join(__dirname, 'forensic-vms-capture.py'), dir, command, 'root-approval'],
    { timeout: 85000, stdio: 'inherit' });
  await expect(applied).toBeVisible({ timeout: 30000 });
  await expect(page.getByText('允许 WebSSH 和远程执行使用 root：开启', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: '复制本机授权命令', exact: true })).toHaveCount(0);
  console.log('Verified an existing root Agent with WebSSH: panel TOTP, clipboard command, SSH/PAM confirmation and applied acknowledgement');
}

async function main() {
  const keys = fs.readFileSync(path.join(dir, 'panel-public-keys'), 'utf8');
  const recipientKey = keys.match(/Recipient public key: (\S+)/)[1];
  const receiptKey = keys.match(/Receipt public key: (\S+)/)[1];
  const password = `Ci9-${crypto.randomBytes(24).toString('hex')}`;
  const username = 'qga-proof-fixture';
  const browser = await chromium.launch();
  let enrollment;
  try {
    const context = await browser.newContext({ baseURL: browserURL, ignoreHTTPSErrors: true, locale: 'en-US', acceptDownloads: true });
    const page = await context.newPage();
    await expect.poll(async () => (await context.request.get('/api/install/status')).status(), { timeout: 25000 }).toBe(200);
    await receive(context, '/api/install/complete', { username, password, sitename: 'QGA fixture', description: '', metric_dsn: './data/metrics.db' });
    await expect.poll(async () => (await context.request.get('/api/version')).status()).toBe(200);
    const anon = await rpc(context, protocol + 'List', JSON.stringify(''));
    assert([401, 403].includes(anon.status()));
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
      userCode: candidate.userCode, newAgent: { name: 'Synthetic QGA node' },
    });
    assert.match(agentId, /^[0-9a-f-]{36}$/);
    await expect.poll(() => enrollment.exitCode, { timeout: 45000 }).toBe(0);
    assert.equal(await done, 0);
    ssh(29480, `printf '%s\n' 'KOMARI_PANEL_DIAGNOSTIC_NODE=${agentId}' > /root/fixture/node.env && systemctl restart komari-forensic-panel.service`);
    await expect.poll(async () => (await context.request.get('/api/version')).status()).toBe(200);
    const [panelKeyID, panelKey] = ssh(29482, `python3 -c 'import json; k=json.load(open("/var/lib/komari/komari-agent/credentials.json"))["control_plane_keys"][0]; print(k["key_id"],k["value"])'`).split(' ');
    assert.match(panelKeyID, /^[\w-]+$/);
    assert.match(panelKey, /^[a-zA-Z0-9+/=]+$/);
    ssh(29482, `${binaryDir}/komari-host trust --manifest ${manifest} --agent-id ${agentId} --endpoint ${agentURL} --panel-key-id ${panelKeyID} --panel-public-key ${panelKey} --service-user forensic-fixture && systemctl start komari-agent.service`);

    if (process.argv[3] === 'root-approval') {
      await verifyRootApproval(context, page, agentId);
      return;
    }
    const command = `${binaryDir}/komari-host qga-forensics --manifest ${manifest} --recipient-public-key ${recipientKey} --receipt-public-key ${receiptKey} --duration 45s`;
    const withoutTTY = spawnSync('ssh', args(29482, `SSH_TTY=/dev/pts/1 SSH_CONNECTION='192.0.2.2 2345 192.0.2.1 22' ${command}`), { encoding: 'utf8', timeout: 10000 });
    assert.equal(withoutTTY.status, 1);
    assert.match(withoutTTY.stderr, /direct interactive root SSH/);
    const nonroot = spawnSync('ssh', args(29482, `runuser -u forensic-fixture -- ${command}`), { encoding: 'utf8', timeout: 10000 });
    assert.equal(nonroot.status, 1);
    assert.match(nonroot.stderr, /activation requires root/);
    execFileSync('python3', [path.join(__dirname, 'forensic-vms-capture.py'), dir, command], { timeout: 65000, stdio: 'inherit' });
    const verify = `for item in ${archiveDir}/*.tar.gz; do /root/komari verify-forensic-archive --receipt-public-key ${receiptKey} --decryption-key-file /root/fixture/keys/forensic-recipient.key --record-output /run/forensic-"$(basename "$item")".json "$item" >/dev/null || exit; cat /run/forensic-"$(basename "$item")".json; printf '\\n'; done`;
    const files = ssh(29480, `find ${archiveDir} -maxdepth 1 -name '*.tar.gz' -printf '%f\\n' | sort`).split('\n');
    assert.equal(files.length, 6);
    const observed = ssh(29480, verify).split('\n').map(line => JSON.parse(line));
    assert.deepEqual(observed.map(event => event.command), ['guest-file-open', 'guest-file-open', 'guest-file-read', 'guest-file-read', 'guest-file-close', 'guest-file-close']);
    const raw = observed.map(event => Buffer.from(event.raw, 'base64'));
    for (let i = 0; i < observed.length; i++) {
      assert.deepEqual(Buffer.from(observed[i].sha256), crypto.createHash('sha256').update(raw[i]).digest());
    }
    const response = JSON.parse(raw[3]);
    assert.deepEqual(Buffer.from(response.return['buf-b64'], 'base64'), fs.readFileSync(path.join(dir, 'requested')));
    assert(!raw.some(value => value.includes(fs.readFileSync(path.join(dir, 'unrequested')).subarray(0, -1))));
    assert(!raw.some(value => value.includes('/root/unrequested')));

    await page.goto('/admin/forensic-archives');
    const row = page.getByRole('row').filter({ hasText: files[3] });
    await expect(row).toBeVisible();
    const rejected = await rpc(context, protocol + 'Download', JSON.stringify(Buffer.from(files[3]).toString('base64')));
    assert.equal(rejected.status(), 403);
    const qr = await context.request.post('/api/admin/2fa/generate', { headers: { Origin: browserURL } });
    assert.equal(qr.status(), 200);
    const cookie = (await context.storageState()).cookies.find(({ name }) => name === '__Host-komari_2fa_secret');
    assert(cookie);
    const initial = await freshTOTP(cookie.value, -1);
    await receive(context, `/api/admin/2fa/enable?code=${initial.code}`);
    await page.reload();
    await row.getByRole('button').click();
    const dialog = page.getByRole('dialog');
    await dialog.getByRole('button', { name: 'Start verification / send code' }).click();
    const code = await freshTOTP(cookie.value, initial.counter);
    await dialog.locator('input').fill(code.code);
    const started = page.waitForEvent('download');
    await dialog.getByRole('button', { name: 'Verify and continue' }).click();
    const download = await started;
    assert.equal(download.suggestedFilename(), files[3]);
    const downloaded = fs.readFileSync(await download.path());
    const digest = ssh(29480, `sha256sum ${archiveDir}/${files[3]} | cut -d ' ' -f 1`);
    assert.equal(crypto.createHash('sha256').update(downloaded).digest('hex'), digest);
    console.log('Two signed Ubuntu guests verified root SSH + PAM, QGA-only scope, enrolled Agent upload, signed archive and administrator TOTP download');
  } finally {
    if (enrollment && enrollment.exitCode === null) enrollment.kill();
    await browser.close();
  }
}

main().catch(error => { console.error(error.message); process.exitCode = 1; });
