// Actual restricted listener + built browser + native password login.
// Full-feature runs may also exercise configured TOTP; database-only runs do not.
// Only the historical metric input is explicitly synthetic legacy test data.
const { chromium, expect } = require('@playwright/test');
const fs=require('node:fs'),path=require('node:path'),crypto=require('node:crypto');
const {execFileSync,spawn}=require('node:child_process');
const root=process.env.E2E_ROOT,artifacts=process.env.E2E_ARTIFACTS,base='https://127.0.0.1:29443';
const skipCSV=process.env.KOMARI_E2E_SKIP_CSV==='1';
require('./guard.cjs');
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
const system=(...args)=>execFileSync('sudo',['systemctl',...args],{timeout:30000});
const source=path.join(root,'migration-panel/data/metrics.db');
const credentials=JSON.parse(fs.readFileSync(path.join(root,'migration-credentials.json')));
if(skipCSV&&credentials.secret)throw Error('Database-only migration fixture must use password login without TOTP');
function totp(secret){const alphabet='ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';const bits=[...secret.toUpperCase().replace(/=+$/,'')].map(c=>alphabet.indexOf(c).toString(2).padStart(5,'0')).join('');const key=Buffer.from((bits.match(/.{8}/g)||[]).map(b=>parseInt(b,2)));const t=Buffer.alloc(8);t.writeBigUInt64BE(BigInt(Math.floor(Date.now()/30000)));const h=crypto.createHmac('sha1',key).update(t).digest();return String((h.readUInt32BE(h.at(-1)&15)&0x7fffffff)%1000000).padStart(6,'0');}
function startPanel(directory){execFileSync('sudo',['systemd-run','--unit=komari-e2e-panel','--collect','--slice=komari-e2e.slice',`--property=User=${process.env.KOMARI_E2E_USER || process.env.USER}`,`--property=WorkingDirectory=${directory}`,'--property=CPUQuota=100%','--property=MemoryMax=1G',path.join(root,'bin/komari'),'server','--listen','127.0.0.1:29444'],{timeout:30000});}
function startWorker(directory=path.join(root,'panel')){execFileSync('sudo',['systemd-run','--unit=komari-e2e-export','--collect','--slice=komari-e2e.slice',`--property=User=${process.env.KOMARI_E2E_USER || process.env.USER}`,`--property=WorkingDirectory=${directory}`,'--property=CPUQuota=4%','--property=CPUQuotaPeriodSec=100ms','--property=MemoryMax=128M','--property=MemorySwapMax=0','--setenv=GOMEMLIMIT=90MiB','--setenv=GOMAXPROCS=1',path.join(root,'bin/komari'),'export-worker','--queue',path.join(directory,'data/exports-v2'),`--instance-cgroup=/sys/fs/cgroup${execFileSync('systemctl',['show','komari-e2e-panel.service','--property=ControlGroup','--value'],{encoding:'utf8'}).trim()}`],{timeout:30000});}
let browser,context,locker;
async function run(){
 if(skipCSV)system('stop','komari-e2e-panel.service');
 else system('stop','komari-e2e-export.service','komari-e2e-panel.service');
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
 await expect(page.getByRole('heading',{name:'Telemetry storage upgrade',exact:true,includeHidden:true})).toBeVisible();
 // An unauthenticated visitor can see the maintenance explanation, but not
 // counts, source paths, errors or control endpoints.
 expect((await context.request.get('/api/admin/update/storage-v4/status')).status()).toBe(401);
 for(const endpoint of ['/api/public','/komari.metrics.v1.MetricsService/QueryMetrics','/api/admin/client']){
  const response=await context.request.post(endpoint,{data:{}});expect(response.status()).toBe(503);expect(response.headers()['retry-after']).toBe('5');
 }
 await page.locator('#restricted-login-username').fill(credentials.username);
 await page.locator('#restricted-login-password').fill(credentials.password);
 await page.locator('.km-restricted-login-form button[type=submit]').click();
 if(credentials.secret){
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
 }
 await expect(page.locator('.km-restricted-login-dialog')).toBeHidden({timeout:15000});
 await expect(page.getByRole('heading',{name:'Migration stopped; original history preserved',exact:true})).toBeVisible({timeout:20000});
 await page.screenshot({path:path.join(artifacts,'storage-upgrade-failed.png'),fullPage:true});
 // The failed migration remains restricted after a real process restart.
 system('restart','komari-e2e-panel.service');
 await expect.poll(async()=>{try{return(await context.request.get('/api/admin/update/storage-v4/status')).status()}catch{return 0}},{timeout:20000}).toBe(200);
 // Keep the real SQLite lock until the restarted process has independently
 // failed too. Releasing on the first 200 races automatic startup conversion.
 await expect.poll(async()=>{try{return(await(await context.request.get('/api/admin/update/storage-v4/status')).json()).data.state}catch{return ''}},{timeout:30000}).toBe('failed');
 fs.writeFileSync(release,'');
 await new Promise((resolve,reject)=>{if(locker.exitCode!==null)return locker.exitCode===0?resolve():reject(Error('Locker failed'));locker.once('exit',code=>code===0?resolve():reject(Error('Locker failed')));});
 await page.reload();
 await expect(page.getByRole('button',{name:'Retry migration',exact:true})).toBeVisible();
 await page.getByRole('button',{name:'Retry migration',exact:true}).click();
 // No operational request is used to repair/consume history. Startup must
 // finish its own conversion and hand the port back to the ordinary router.
 await expect.poll(async()=>{try{return(await context.request.get('/api/public')).status()}catch{return 0}},{timeout:180000}).toBe(200);
 // storage_update.tsx hands off to /admin; routes.ts then replaces that
 // intermediate URL with /admin/dashboard. Wait for the completed route.
 await expect(page).toHaveURL(`${base}/admin/dashboard`,{timeout:20000});
 const checks=JSON.parse(execFileSync('python3',['-c',`import sqlite3,json,sys\nc=sqlite3.connect(sys.argv[1])\np=dict(c.execute("SELECT task,value FROM metric_telemetry_progress WHERE task IN ('legacy_migration_complete','legacy_ledger_complete')"))\nprint(json.dumps({'progress':p,'source_rows':c.execute('SELECT COUNT(*) FROM metric_points').fetchone()[0],'summary_rows':c.execute('SELECT COUNT(*) FROM metric_telemetry_summary').fetchone()[0]}))`,source],{encoding:'utf8'}));
 expect(checks.progress.legacy_migration_complete).toBe(1);expect(checks.progress.legacy_ledger_complete).toBe(1);expect(checks.source_rows).toBe(43200);expect(checks.summary_rows).toBeGreaterThan(0);
 await expect(page.getByText('Daily billable traffic',{exact:true})).toBeVisible({timeout:30000});
 await expect(page.getByRole('heading',{name:'Telemetry storage upgrade',exact:true})).toHaveCount(0);
 await page.screenshot({path:path.join(artifacts,'storage-upgrade-complete.png'),fullPage:true});
 fs.writeFileSync(path.join(artifacts,'storage-upgrade.json'),JSON.stringify({checks,responses,final_url:page.url(),synthetic_input:true,native_password:true,totp:!!credentials.secret,restarted_after_failure:true},null,2));
 // Now exercise the server's actual staged ZIP restore, not just an in-process
 // reader. The downloaded package contains real Agent history and output.
 system('stop','komari-e2e-panel.service');
 // Exercise the shipped offline maintenance command after real startup
 // migration. Install only its verified new file, restart the actual server,
 // then query through the same authenticated HTTP session. No TOTP scenario.
 const compacted=path.join(root,'migration-compacted.db'),rollback=path.join(root,'migration-rollback');
 execFileSync(path.join(root,'bin/komari'),['telemetry-compact','--confirm-offline','--retire-legacy','--output',compacted,'--backup-dir',rollback],{
  cwd:path.join(root,'migration-panel'),timeout:180000,stdio:['ignore',fs.openSync(path.join(artifacts,'storage-compact-command.log'),'w'),'inherit'],
 });
 const compactProof=JSON.parse(fs.readFileSync(path.join(rollback,'compaction.json')));
 expect(compactProof.retired_legacy).toBe(true);expect(compactProof.bytes).toBeGreaterThan(0);
 expect(crypto.createHash('sha256').update(fs.readFileSync(compacted)).digest('hex')).toBe(compactProof.sha256);
 for(const suffix of ['-wal','-shm'])expect(fs.existsSync(source+suffix)).toBe(false);
 const compactChecks=JSON.parse(execFileSync('python3',['-c',`import sqlite3,json,sys
# The command has exited and all three files are standalone. Immutable reads
# must not create fresh WAL/SHM files immediately before the file exchange.
source=sqlite3.connect('file:'+sys.argv[1]+'?mode=ro&immutable=1',uri=True)
target=sqlite3.connect('file:'+sys.argv[2]+'?mode=ro&immutable=1',uri=True)
backup=sqlite3.connect('file:'+sys.argv[3]+'?mode=ro&immutable=1',uri=True)
print(json.dumps({'source_rows':source.execute('SELECT COUNT(*) FROM metric_points').fetchone()[0],
'backup_rows':backup.execute('SELECT COUNT(*) FROM metric_points').fetchone()[0],
'retired':target.execute("SELECT value FROM metric_telemetry_progress WHERE task='legacy_tables_retired'").fetchone()[0],
'legacy_objects':target.execute("SELECT COUNT(*) FROM sqlite_master WHERE name IN ('metric_points','metric_rollups','metric_point_blocks','metric_rollup_blocks')").fetchone()[0],
'integrity':target.execute('PRAGMA integrity_check').fetchone()[0],
'summary_rows':target.execute('SELECT COUNT(*) FROM metric_telemetry_summary').fetchone()[0]}))`,source,compacted,path.join(rollback,'metrics.db')],{encoding:'utf8'}));
 expect(compactChecks.source_rows).toBe(43200);expect(compactChecks.backup_rows).toBe(43200);
 expect(compactChecks.retired).toBe(1);expect(compactChecks.legacy_objects).toBe(0);expect(compactChecks.integrity).toBe('ok');expect(compactChecks.summary_rows).toBeGreaterThan(0);
 for(const suffix of ['-wal','-shm'])expect(fs.existsSync(source+suffix)).toBe(false);
 fs.renameSync(source,source+'.pre-compact');fs.renameSync(compacted,source);
 startPanel(path.join(root,'migration-panel'));
 await expect.poll(async()=>{try{return(await context.request.get('/api/public')).status()}catch{return 0}},{timeout:30000}).toBe(200);
 const migrationScope=JSON.parse(fs.readFileSync(path.join(root,'migration-manifest.json')));
 const compactEnd=new Date();
 const compactHistory=await context.request.post('/komari.metrics.v1.MetricsService/QueryMetrics',{
  data:{agentIds:[migrationScope.agentId],metrics:['cpu.usage'],startTime:new Date(compactEnd.getTime()-86400_000).toISOString(),endTime:compactEnd.toISOString(),maxPoints:360,downsample:true},
  headers:{Origin:base,'Connect-Protocol-Version':'1'},
 });
 expect(compactHistory.status()).toBe(200);const compactHistoryBody=await compactHistory.json();
 expect(compactHistoryBody.series?.some(series=>series.queryPoints?.some(point=>Number(point.sampleCount)>0))).toBe(true);
 const compactTraffic=await context.request.post('/komari.admin.v1.DashboardService/GetDashboardCharts',{
  data:{charts:['DASHBOARD_CHART_TRAFFIC'],rankingLimit:5},headers:{Origin:base,'Connect-Protocol-Version':'1'},
 });
 expect(compactTraffic.status()).toBe(200);const compactBilling=await compactTraffic.json();
 expect(compactBilling.charts?.traffic?.error||'').toBe('');expect(compactBilling.charts?.traffic?.daily).toHaveLength(30);
 fs.writeFileSync(path.join(artifacts,'storage-compact.json'),JSON.stringify({proof:compactProof,checks:compactChecks,history:compactHistoryBody,billing:compactBilling},null,2));
 system('stop','komari-e2e-panel.service');
 const restored=path.join(root,'restore-panel');fs.mkdirSync(path.join(restored,'data'),{recursive:true});
 fs.copyFileSync(path.join(root,'real-full-backup.zip'),path.join(restored,'data/backup.zip'));
 if(process.env.KOMARI_E2E_LOCAL==='1')execFileSync('chown',['-R',process.env.KOMARI_E2E_USER,restored]);
 startPanel(restored);
 const original=await browser.newContext({baseURL:base,storageState:path.join(root,'qualification-browser-state.json')});
 try {
  await expect.poll(async()=>{try{return(await original.request.get('/api/public')).status()}catch{return 0}},{timeout:60000}).toBe(200);
  if(skipCSV){
   const scope=JSON.parse(fs.readFileSync(path.join(root,'qualification-scope.json')));
   const end=new Date();
   const history=await original.request.post('/komari.metrics.v1.MetricsService/QueryMetrics',{
    data:{agentIds:[scope.agentId],metrics:['cpu.usage'],startTime:new Date(end.getTime()-3600_000).toISOString(),endTime:end.toISOString(),maxPoints:360,downsample:true},
    headers:{Origin:base,'Connect-Protocol-Version':'1'},
   });
   expect(history.status()).toBe(200);
   const body=await history.json();
   expect(body.series?.some(series=>series.queryPoints?.some(point=>Number(point.sampleCount)>0))).toBe(true);
   const traffic=await original.request.post('/komari.admin.v1.DashboardService/GetDashboardCharts',{
    data:{charts:['DASHBOARD_CHART_TRAFFIC'],rankingLimit:5},headers:{Origin:base,'Connect-Protocol-Version':'1'},
   });
   expect(traffic.status()).toBe(200);
   const billing=await traffic.json();
   expect(billing.charts?.traffic?.error||'').toBe('');
   expect(billing.charts?.traffic?.daily).toHaveLength(30);
   fs.writeFileSync(path.join(artifacts,'database-restored-http.json'),JSON.stringify({history:body,billing,csv_scenarios:false},null,2));
   return;
  }
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
  expect((await original.request.post(`/api/admin/history/export/${proof.paused.id}/resume`,{data:{}})).status()).toBe(202);
  let resumed;
  await expect.poll(async()=>{resumed=(await (await original.request.get(`/api/admin/history/export/${proof.paused.id}`)).json()).data;if(['failed','cancelled'].includes(resumed.status))throw Error(JSON.stringify(resumed));return resumed.status;},{timeout:120000}).toBe('done');
  const resumedBytes=await (await original.request.get(`/api/admin/history/export/${resumed.id}/download`)).body();
  expect(crypto.createHash('sha256').update(resumedBytes).digest('hex')).toBe(resumed.sha256);
  fs.writeFileSync(path.join(artifacts,'export-restored-tasks.json'),JSON.stringify({verified,resumed},null,2));
 } finally { await original.close();if(!skipCSV)system('stop','komari-e2e-export.service'); }
}
run().catch(async e=>{console.error(e);process.exitCode=1;fs.writeFileSync(path.join(artifacts,'storage-upgrade-error.txt'),e.stack||String(e));if(context)for(const page of context.pages())await page.screenshot({path:path.join(artifacts,'storage-upgrade-error.png'),fullPage:true}).catch(()=>{});}).finally(async()=>{
 if(locker&&locker.exitCode===null)locker.kill();if(context){await context.tracing.stop({path:path.join(artifacts,'storage-upgrade-trace.zip')});await context.close();}if(browser)await browser.close();
 execFileSync('sudo',['journalctl','-u','komari-e2e-panel.service','--no-pager','-o','short-iso'],{stdio:['ignore',fs.openSync(path.join(artifacts,'storage-upgrade-server.log'),'w'),'inherit']});
 system('stop','komari-e2e-panel.service');startPanel(path.join(root,'panel'));if(!skipCSV)startWorker();
});
