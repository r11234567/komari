// Exercise the shipped 1.2.7 wizard directly into telemetry. The installation
// credentials stay in the disposable fixture; history below is synthetic.
const {chromium,expect}=require('@playwright/test');
const {execFileSync}=require('node:child_process');
const {decodeMetricsRequest,decodeMetricsResponse}=require('./storage-metrics-wire.cjs');
const fs=require('node:fs'),path=require('node:path');
require('./guard.cjs');
const root=process.env.E2E_ROOT,artifacts=process.env.E2E_ARTIFACTS,base='https://127.0.0.1:29443';
const directory=path.join(root,'monitoring-panel');
const system=(...args)=>execFileSync('sudo',['systemctl',...args],{timeout:30000});
function stopIfLoaded(unit){if(execFileSync('systemctl',['show',unit,'--property=LoadState','--value'],{encoding:'utf8'}).trim()!=='not-found')system('stop',unit);}
const scope=JSON.parse(fs.readFileSync(path.join(root,'qualification-scope.json')));
let browser,context;
async function run(){
 stopIfLoaded('komari-e2e-panel.service');
 execFileSync('python3',['-c',`
import sys,json,zipfile,pathlib,sqlite3,datetime
root=pathlib.Path(sys.argv[1]);agent=sys.argv[2];directory=root/'monitoring-panel'/'data'
directory.mkdir(parents=True,exist_ok=False)
with zipfile.ZipFile(root/'real-full-backup.zip') as archive:
 for name in ['komari.db','secret.key']:
  (directory/name).write_bytes(archive.read(name))
c=sqlite3.connect(directory/'komari.db')
c.execute("UPDATE users SET two_factor=''")
for key,value in {'metric_store_driver':'sqlite','metric_store_dsn':'./data/metrics.db','metric_archive_directory':'./data/telemetry','metric_hot_window_minutes':'15','metric_archive_segment_minutes':'1440'}.items():
 c.execute('INSERT INTO configs(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value',(key,value))
c.execute('CREATE TABLE records (client TEXT,time DATETIME,cpu REAL,gpu REAL,ram INTEGER,ram_total INTEGER,swap INTEGER,swap_total INTEGER,load REAL,temp REAL,disk INTEGER,disk_total INTEGER,net_in INTEGER,net_out INTEGER,net_total_up INTEGER,net_total_down INTEGER,process INTEGER,connections INTEGER,connections_udp INTEGER,traffic_up INTEGER,traffic_down INTEGER)')
c.execute('CREATE TABLE ping_records (client TEXT,task_id INTEGER,time DATETIME,value INTEGER)')
now=datetime.datetime.now(datetime.timezone.utc).replace(second=0,microsecond=0)
now=now.replace(minute=now.minute//5*5)
task=int(sys.argv[3])
for i in range(289):
 at=(now-datetime.timedelta(minutes=(288-i)*5)).strftime('%Y-%m-%d %H:%M:%S+00:00')
 c.execute('INSERT INTO records VALUES ('+','.join(['?']*21)+')',(agent,at,42,0,512,1024,0,0,.5,30,512,1024,10,20,1000+i*10,2000+i*20,4,2,1,10,20))
 c.execute('INSERT INTO ping_records VALUES (?,?,?,?)',(agent,task,at,17 if i%5 else -1))
c.commit();c.close()
(root/'monitoring-window.json').write_text(json.dumps({'start':(now-datetime.timedelta(days=1)).isoformat(),'end':now.isoformat()}))
`,root,scope.agentId,String(scope.pingTaskId)],{timeout:30000});
 if(process.env.KOMARI_E2E_LOCAL==='1')execFileSync('chown',['-R',process.env.KOMARI_E2E_USER,directory]);
 execFileSync('sudo',['systemd-run','--unit=komari-e2e-panel','--collect','--slice=komari-e2e.slice',`--property=User=${process.env.KOMARI_E2E_USER||process.env.USER}`,`--property=WorkingDirectory=${directory}`,'--property=CPUQuota=100%','--property=MemoryMax=1G','--setenv=KOMARI_PUBLIC_ORIGIN=https://127.0.0.1:29443','--setenv=KOMARI_TRUSTED_PROXIES=127.0.0.1,::1',path.join(root,'bin/komari'),'server','--listen','127.0.0.1:29444'],{timeout:30000});
 browser=await chromium.launch({headless:true});context=await browser.newContext({baseURL:base});
 await context.addInitScript(()=>localStorage.setItem('i18nextLng','en'));
 const page=await context.newPage();
 await expect.poll(async()=>{try{return(await context.request.get('/api/admin/update/1.2.7/auth')).status()}catch{return 0}},{timeout:30000}).toBe(200);
 await page.goto('/admin/update/1.2.7');
 await expect(page.getByRole('heading',{name:'Monitoring database upgrade',exact:true,includeHidden:true})).toBeVisible();
 const credentials=JSON.parse(fs.readFileSync(path.join(root,'migration-credentials.json')));
 await page.locator('#restricted-login-username').fill(credentials.username);
 await page.locator('#restricted-login-password').fill(credentials.password);
 await page.locator('.km-restricted-login-form button[type=submit]').click();
 await expect(page.locator('.km-restricted-login-dialog')).toBeHidden({timeout:15000});
 await page.getByRole('button',{name:'Start upgrade',exact:true}).click();
 const dialog=page.getByRole('dialog');await expect(dialog).toBeVisible();
 for(const box of await dialog.getByRole('checkbox').all())await box.check();
 await dialog.getByRole('button',{name:'Confirm and start',exact:true}).click();
 await expect.poll(async()=>{try{return(await context.request.get('/api/public')).status()}catch{return 0}},{timeout:180000}).toBe(200);
 expect((await context.request.get('/api/admin/update/storage-v4/status')).status()).not.toBe(200);
 const proof=JSON.parse(execFileSync('python3',['-c',`
import sys,sqlite3,json,pathlib
data=pathlib.Path(sys.argv[1])/'data';main=sqlite3.connect(data/'komari.db');db=sqlite3.connect(data/'metrics.db')
legacy={name:db.execute('SELECT COUNT(*) FROM '+name).fetchone()[0] for name in ['metric_point_values','metric_point_blocks','metric_rollup_values','metric_rollup_blocks']}
old=main.execute("SELECT COUNT(*) FROM sqlite_master WHERE name IN ('records','records_long_term','gpu_records','ping_records')").fetchone()[0]
print(json.dumps({'legacy':legacy,'monitoring_tables':old,'rollup':db.execute('SELECT COUNT(*) FROM metric_telemetry_rollup').fetchone()[0]}))
`,directory],{encoding:'utf8'}));
 expect(proof.monitoring_tables).toBe(0);expect(proof.rollup).toBeGreaterThan(0);
 Object.values(proof.legacy).forEach(n=>expect(n).toBe(0));
 const headers={Origin:base,'Connect-Protocol-Version':'1'},end=new Date(),start=new Date(end-24*3600_000);
 const chartResponse=await context.request.post('/komari.metrics.v1.MetricsService/QueryMetrics',{headers,data:{agentIds:[scope.agentId],metrics:['cpu.usage'],startTime:start.toISOString(),endTime:end.toISOString(),maxPoints:600,downsample:true}});
 expect(chartResponse.status()).toBe(200);const charts=await chartResponse.json();
 const points=charts.series.flatMap(s=>s.queryPoints||[]).filter(p=>Number(p.sampleCount)>0);
 expect(points.length).toBeGreaterThan(200);points.forEach(p=>expect(Number(p.value)).toBe(42));
 const ping=await context.request.post('/komari.metrics.v1.MetricsService/GetPingStats',{headers,data:{agentIds:[scope.agentId],startTime:new Date(end-15*60_000+1000).toISOString(),endTime:end.toISOString()}});
 expect(ping.status()).toBe(200);const stats=await ping.json();expect(stats.stats.length).toBeGreaterThan(0);
 await page.goto(`/instance/${scope.agentId}`);const chart=page.locator('.km-load-chart');await expect(chart).toBeVisible();
 const pending=page.waitForResponse(response=>{
  if(!new URL(response.url()).pathname.endsWith('/QueryMetrics')||response.status()!==200||!response.request().headers()['content-type']?.includes('application/proto'))return false;
  const q=decodeMetricsRequest(response.request().postDataBuffer());
  return q.agentIds.includes(scope.agentId)&&q.metrics.includes('cpu.usage')&&q.maxPoints===600&&new Date(q.endTime)-new Date(q.startTime)===86400_000;
 });
 await chart.getByRole('radio',{name:/^1\s*day$/i}).click();
 const rendered=decodeMetricsResponse(await(await pending).body());
 const renderedCPU=rendered.series.find(s=>s.metric==='cpu.usage');
 expect(renderedCPU.queryPoints.length).toBeGreaterThan(200);expect(renderedCPU.coverage).toBe('complete');
 renderedCPU.queryPoints.forEach(p=>expect(Number(p.value)).toBe(42));
 // A horizontal SVG stroke has zero bounding-box height. Validate its full
 // path and painted container instead of Playwright's area-based visibility.
 await expect(chart.locator('.recharts-surface').first()).toBeVisible();
 await expect.poll(()=>chart.locator('.recharts-line-curve').first().getAttribute('d')).toMatch(/^(?:M[-\d.,]+)(?:L[-\d.,]+){200,}$/);
 await page.screenshot({path:path.join(artifacts,'monitoring-upgrade-chart.png'),fullPage:true,animations:'disabled'});
 await page.goto('/admin/dashboard');await expect(page.getByText('Daily billable traffic',{exact:true})).toBeVisible();
 const dashboard=await context.request.post('/komari.admin.v1.DashboardService/GetDashboardCharts',{headers,data:{charts:['DASHBOARD_CHART_TRAFFIC'],rankingLimit:5}});
 expect(dashboard.status()).toBe(200);const billing=await dashboard.json();expect(billing.charts?.traffic?.error||'').toBe('');
 await page.screenshot({path:path.join(artifacts,'monitoring-upgrade-dashboard.png'),fullPage:true,animations:'disabled'});
 fs.writeFileSync(path.join(artifacts,'monitoring-upgrade.json'),JSON.stringify({synthetic_input:true,proof,charts,rendered,stats,billing},null,2));
 if(process.env.KOMARI_E2E_PERIPHERAL_CSV==='1'){
  const group=execFileSync('systemctl',['show','komari-e2e-panel.service','--property=ControlGroup','--value'],{encoding:'utf8'}).trim();
  execFileSync('sudo',['systemd-run','--unit=komari-e2e-export','--collect','--slice=komari-e2e.slice',`--property=User=${process.env.KOMARI_E2E_USER||process.env.USER}`,`--property=WorkingDirectory=${directory}`,'--property=CPUQuota=4%','--property=CPUQuotaPeriodSec=100ms','--property=MemoryMax=128M','--property=MemorySwapMax=0','--setenv=GOMEMLIMIT=90MiB','--setenv=GOMAXPROCS=1',path.join(root,'bin/komari'),'export-worker','--queue',path.join(directory,'data/exports-v2'),`--instance-cgroup=/sys/fs/cgroup${group}`],{timeout:30000});
  try {
   await expect.poll(async()=>{const response=await context.request.get('/api/admin/history/export/retention');if(response.status()!==200)return false;const body=await response.json();return body.data?.worker_available===true;},{timeout:30000}).toBe(true);
   const window=JSON.parse(fs.readFileSync(path.join(root,'monitoring-window.json'))),exports=[];
   for(const mode of ['raw','sample30s','summary']){
    const created=await context.request.post('/api/admin/history/export',{headers:{Origin:base},data:{uuid:scope.agentId,mode,category:'resource',start:window.start,end:window.end,interval_seconds:180,format_version:2}});
    expect(created.status(),await created.text()).toBe(202);const id=(await created.json()).data.id;let job;
    await expect.poll(async()=>{const response=await context.request.get(`/api/admin/history/export/${id}`);job=(await response.json()).data;if(job.status==='failed')throw Error(job.error);return job.status;},{timeout:120000}).toBe('done');
    const response=await context.request.get(`/api/admin/history/export/${id}/download`);expect(response.status()).toBe(200);const bytes=await response.body();
    const check=JSON.parse(execFileSync('python3',['-c',`
import sys,csv,io,json
rows=list(csv.DictReader(io.StringIO(sys.stdin.buffer.read().decode('utf-8-sig'))));mode=sys.argv[1]
if mode=='sample30s':
 columns=[name for name in rows[0] if name.startswith('cpu.usage ')]
 assert len(columns)==1,columns
 values=[float(row[columns[0]]) for row in rows if row.get(columns[0])]
else:
 selected=[row for row in rows if row['metric']=='cpu.usage'];values=[float(row['value']) for row in selected]
 if mode=='summary':
  assert all(float(row['output_interval_seconds'])==300 for row in selected)
  assert all(row['coverage']=='complete' for row in selected)
assert values and all(value==42 for value in values)
assert len(values)==289,(mode,len(values))
print(json.dumps({'mode':mode,'cpu_points':len(values),'closed_end_preserved':True}))
`,mode],{input:bytes,encoding:'utf8'}));
    exports.push(check);
   }
   fs.writeFileSync(path.join(artifacts,'monitoring-export.json'),JSON.stringify(exports,null,2));
  } finally {stopIfLoaded('komari-e2e-export.service');}
 }
}
run().catch(async error=>{console.error(error);process.exitCode=1;if(context)for(const page of context.pages())await page.screenshot({path:path.join(artifacts,'monitoring-upgrade-error.png'),fullPage:true}).catch(()=>{});}).finally(async()=>{if(context)await context.close();if(browser)await browser.close();});
