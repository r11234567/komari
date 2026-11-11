// Actual restricted listener + built browser + native password/TOTP ceremony.
// Only the historical metric input is explicitly synthetic legacy test data.
const { chromium, expect } = require('@playwright/test');
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto');
const {execFileSync,spawn}=require('node:child_process');
const root=process.env.E2E_ROOT,artifacts=process.env.E2E_ARTIFACTS,base='https://127.0.0.1:29443';
if(process.env.GITHUB_ACTIONS!=='true'||process.env.RUNNER_ENVIRONMENT!=='github-hosted'||root!==path.join(process.env.RUNNER_TEMP||'','komari-e2e'))throw Error('Disposable fixture only');
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
const system=(...args)=>execFileSync('sudo',['systemctl',...args],{timeout:30000});
const source=path.join(root,'migration-panel/data/metrics.db');
const credentials=JSON.parse(fs.readFileSync(path.join(root,'migration-credentials.json')));
function totp(secret){const alphabet='ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';const bits=[...secret.toUpperCase().replace(/=+$/,'')].map(c=>alphabet.indexOf(c).toString(2).padStart(5,'0')).join('');const key=Buffer.from((bits.match(/.{8}/g)||[]).map(b=>parseInt(b,2)));const t=Buffer.alloc(8);t.writeBigUInt64BE(BigInt(Math.floor(Date.now()/30000)));const h=crypto.createHmac('sha1',key).update(t).digest();return String((h.readUInt32BE(h.at(-1)&15)&0x7fffffff)%1000000).padStart(6,'0');}
function startPanel(directory){execFileSync('sudo',['systemd-run','--unit=komari-e2e-panel','--collect','--slice=komari-e2e.slice',`--property=User=${process.env.USER}`,`--property=WorkingDirectory=${directory}`,'--property=CPUQuota=100%','--property=MemoryMax=1G',path.join(root,'bin/komari'),'server','--listen','127.0.0.1:29444'],{timeout:30000});}
function startWorker(directory=path.join(root,'panel')){execFileSync('sudo',['systemd-run','--unit=komari-e2e-export','--collect','--slice=komari-e2e.slice',`--property=User=${process.env.USER}`,`--property=WorkingDirectory=${directory}`,'--property=CPUQuota=4%','--property=CPUQuotaPeriodSec=100ms','--property=MemoryMax=128M','--property=MemorySwapMax=0','--setenv=GOMEMLIMIT=90MiB','--setenv=GOMAXPROCS=1',path.join(root,'bin/komari'),'export-worker','--queue',path.join(directory,'data/exports-v2'),`--instance-cgroup=/sys/fs/cgroup${execFileSync('systemctl',['show','komari-e2e-panel.service','--property=ControlGroup','--value'],{encoding:'utf8'}).trim()}`],{timeout:30000});}
let browser,context,locker;
async function run(){
 system('stop','komari-e2e-export.service','komari-e2e-panel.service');
 const marker=path.join(root,'migration-lock-ready'),release=path.join(root,'migration-lock-release');
 locker=spawn('python3',['-c',`import sqlite3,time,pathlib,sys\nc=sqlite3.connect(sys.argv[1],timeout=5,isolation_level=None)\nc.execute('BEGIN IMMEDIATE')\npathlib.Path(sys.argv[2]).touch()\nwhile not pathlib.Path(sys.argv[3]).exists(): time.sleep(.05)\nc.rollback()`,source,marker,release],{stdio:['ignore','ignore','inherit']});
 await expect.poll(()=>fs.existsSync(marker),{timeout:10000}).toBe(true);
 startPanel(path.join(root,'migration-panel'));
 browser=await chromium.launch({headless:true});context=await browser.newContext({baseURL:base});
 await context.tracing.start({screenshots:true,snapshots:true});
 await context.addInitScript(()=>localStorage.setItem('i18nextLng','en'));
 const page=await context.newPage(),responses=[];
 page.on('response',r=>{const url=new URL(r.url());if(url.pathname.startsWith('/api')||url.pathname.startsWith('/komari.'))responses.push({path:url.pathname,status:r.status()});});
 await expect.poll(async()=>{try{return(await context.request.get('/api/admin/update/storage-v4/auth')).status()}catch{return 0}},{timeout:30000}).toBe(200);
 await page.goto('/admin/update/storage-v4');
 await expect(page.getByRole('heading',{name:'Telemetry storage upgrade',exact:true})).toBeVisible();
 // An unauthenticated visitor can see the maintenance explanation, but not
 // counts, source paths, errors or control endpoints.
 expect((await context.request.get('/api/admin/update/storage-v4/status')).status()).toBe(401);
 for(const endpoint of ['/api/public','/komari.metrics.v1.MetricsService/QueryMetrics','/api/admin/client']){
  const response=await context.request.post(endpoint,{data:{}});expect(response.status()).toBe(503);expect(response.headers()['retry-after']).toBe('5');
 }
 await page.locator('#restricted-login-username').fill(credentials.username);
 await page.locator('#restricted-login-password').fill(credentials.password);
 await page.locator('.km-restricted-login-form button[type=submit]').click();
 const code=page.getByRole('textbox',{name:'Enter verification code',exact:true});
 await expect(code).toBeVisible();
 const wrong=totp(credentials.secret);await code.fill(String((Number(wrong)+1)%1000000).padStart(6,'0'));
 await page.locator('.km-password-credential-step').getByRole('button',{name:'Continue',exact:true}).click();
 await expect(page.locator('.km-restricted-login-dialog')).toBeVisible();
 // A rejected proof may consume the current challenge. Reset and begin the
 // actual login again through the shipped controls if that provider requires it.
 if(!(await code.isVisible()))throw Error('OTP challenge disappeared after rejection');
 if(Date.now()%30000>27000)await sleep(3100);
 await code.fill(totp(credentials.secret));
 await page.locator('.km-password-credential-step').getByRole('button',{name:'Continue',exact:true}).click();
 await expect(page.locator('.km-restricted-login-dialog')).toBeHidden({timeout:15000});
 await expect(page.getByRole('heading',{name:'Migration stopped; original history preserved',exact:true})).toBeVisible({timeout:20000});
 await page.screenshot({path:path.join(artifacts,'storage-upgrade-failed.png'),fullPage:true});
 // The failed migration remains restricted after a real process restart.
 system('restart','komari-e2e-panel.service');
 await expect.poll(async()=>{try{return(await context.request.get('/api/admin/update/storage-v4/status')).status()}catch{return 0}},{timeout:20000}).toBe(200);
 fs.writeFileSync(release,'');
 await new Promise((resolve,reject)=>{if(locker.exitCode!==null)return locker.exitCode===0?resolve():reject(Error('Locker failed'));locker.once('exit',code=>code===0?resolve():reject(Error('Locker failed')));});
 await page.reload();
 await expect(page.getByRole('button',{name:'Retry migration',exact:true})).toBeVisible();
 await page.getByRole('button',{name:'Retry migration',exact:true}).click();
 // No operational request is used to repair/consume history. Startup must
 // finish its own conversion and hand the port back to the ordinary router.
 await expect.poll(async()=>{try{return(await context.request.get('/api/public')).status()}catch{return 0}},{timeout:180000}).toBe(200);
 await expect(page).toHaveURL(/\/admin(?:\/|$)/,{timeout:20000});
 const checks=JSON.parse(execFileSync('python3',['-c',`import sqlite3,json,sys\nc=sqlite3.connect(sys.argv[1])\np=dict(c.execute("SELECT task,value FROM metric_telemetry_progress WHERE task IN ('legacy_migration_complete','legacy_ledger_complete')"))\nprint(json.dumps({'progress':p,'source_rows':c.execute('SELECT COUNT(*) FROM metric_points').fetchone()[0],'summary_rows':c.execute('SELECT COUNT(*) FROM metric_telemetry_summary').fetchone()[0]}))`,source],{encoding:'utf8'}));
 expect(checks.progress.legacy_migration_complete).toBe(1);expect(checks.progress.legacy_ledger_complete).toBe(1);expect(checks.source_rows).toBe(43200);expect(checks.summary_rows).toBeGreaterThan(0);
 await page.screenshot({path:path.join(artifacts,'storage-upgrade-complete.png'),fullPage:true});
 fs.writeFileSync(path.join(artifacts,'storage-upgrade.json'),JSON.stringify({checks,responses,synthetic_input:true,native_password_and_totp:true,restarted_after_failure:true},null,2));
 // Now exercise the server's actual staged ZIP restore, not just an in-process
 // reader. The downloaded package contains real Agent history and output.
 system('stop','komari-e2e-panel.service');
 const restored=path.join(root,'restore-panel');fs.mkdirSync(path.join(restored,'data'),{recursive:true});
 fs.copyFileSync(path.join(root,'real-full-backup.zip'),path.join(restored,'data/backup.zip'));
 startPanel(restored);
 const original=await browser.newContext({baseURL:base,storageState:path.join(root,'qualification-browser-state.json')});
 try {
  await expect.poll(async()=>{try{return(await original.request.get('/api/public')).status()}catch{return 0}},{timeout:60000}).toBe(200);
  const proof=JSON.parse(fs.readFileSync(path.join(root,'export-restore-proof.json'))),verified=[];
  for(const expected of proof.ready){
   const response=await original.request.get(`/api/admin/history/export/${expected.id}`);expect(response.status()).toBe(200);
   const job=(await response.json()).data;expect(job.status).toBe('done');expect(job.sha256).toBe(expected.sha256);
   const download=await original.request.get(`/api/admin/history/export/${expected.id}/download`);expect(download.status()).toBe(200);
   const bytes=await download.body();expect(crypto.createHash('sha256').update(bytes).digest('hex')).toBe(expected.sha256);
   verified.push({id:job.id,mode:job.mode,sha256:job.sha256,bytes:bytes.length});
  }
  const paused=await original.request.get(`/api/admin/history/export/${proof.paused.id}`);expect(paused.status()).toBe(200);expect((await paused.json()).data.status).toBe('paused');
  expect((await original.request.get(`/api/admin/history/export/${proof.paused.id}/download`)).status()).toBe(409);
  startWorker(restored);
  expect((await original.request.post(`/api/admin/history/export/${proof.paused.id}/resume`,{data:{}})).status()).toBe(200);
  let resumed;
  await expect.poll(async()=>{resumed=(await (await original.request.get(`/api/admin/history/export/${proof.paused.id}`)).json()).data;if(['failed','cancelled'].includes(resumed.status))throw Error(JSON.stringify(resumed));return resumed.status;},{timeout:120000}).toBe('done');
  const resumedBytes=await (await original.request.get(`/api/admin/history/export/${resumed.id}/download`)).body();
  expect(crypto.createHash('sha256').update(resumedBytes).digest('hex')).toBe(resumed.sha256);
  fs.writeFileSync(path.join(artifacts,'export-restored-tasks.json'),JSON.stringify({verified,resumed},null,2));
 } finally { await original.close();system('stop','komari-e2e-export.service'); }
}
run().catch(async e=>{console.error(e);process.exitCode=1;fs.writeFileSync(path.join(artifacts,'storage-upgrade-error.txt'),e.stack||String(e));if(context)for(const page of context.pages())await page.screenshot({path:path.join(artifacts,'storage-upgrade-error.png'),fullPage:true}).catch(()=>{});}).finally(async()=>{
 if(locker&&locker.exitCode===null)locker.kill();if(context){await context.tracing.stop({path:path.join(artifacts,'storage-upgrade-trace.zip')});await context.close();}if(browser)await browser.close();
 execFileSync('sudo',['journalctl','-u','komari-e2e-panel.service','--no-pager','-o','short-iso'],{stdio:['ignore',fs.openSync(path.join(artifacts,'storage-upgrade-server.log'),'w'),'inherit']});
 system('stop','komari-e2e-panel.service');startPanel(path.join(root,'panel'));startWorker();
});
