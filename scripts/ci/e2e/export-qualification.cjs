// Long qualification uses actual production worker code and real HTTPS reads.
// Synthetic cold load is labelled in its manifest; Agent samples stay real.
const fs = require('node:fs');
const path = require('node:path');
const https = require('node:https');
const crypto = require('node:crypto');
const { execFileSync } = require('node:child_process');
const { request } = require('playwright');
const root = process.env.E2E_ROOT, artifacts = process.env.E2E_ARTIFACTS;
require('./guard.cjs');
const base = 'https://127.0.0.1:29443';
const manifest = JSON.parse(fs.readFileSync(path.join(root, 'qualification-manifest.json')));
const groupPath = unit => '/sys/fs/cgroup'+execFileSync('systemctl',['show',unit,'--property=ControlGroup','--value'],{encoding:'utf8'}).trim();
const workerGroup = groupPath('komari-e2e-export.service');
const slice = groupPath('komari-e2e.slice');
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const assert = (ok, message) => { if (!ok) throw Error(message); };
const stat = dir => Object.fromEntries(fs.readFileSync(path.join(dir, 'cpu.stat'), 'utf8').trim().split('\n').map(s => { const [k, v] = s.split(/\s+/); return [k, Number(v)]; }));
const percentile = (xs, q) => [...xs].sort((a,b) => a-b)[Math.min(xs.length-1, Math.floor(xs.length*q))];
const output = fs.createWriteStream(path.join(artifacts, 'export-resources.jsonl'));
const reads = fs.createWriteStream(path.join(artifacts, 'export-foreground.jsonl'));
let timer, context, job, failed;
let capacity = 1, previous, samples = 0, maxShare1s = 0, maxShare10s = 0, maxRSS = 0, cpuTotal = 0, runningMS = 0;
const ring = [];
const latencies = {}, baseline = {};
const services = {metrics:'komari.metrics.v1.MetricsService',browser:'komari.browser.v1.BrowserService',dashboard:'komari.admin.v1.DashboardService'};
async function run() {
  context = await request.newContext({baseURL:base,storageState:path.join(root,'qualification-browser-state.json')});
  const api = async (url, data, method='POST') => {
    const started = performance.now();
    const r = await context.fetch(url, {method, data, headers:{Origin:base,'Connect-Protocol-Version':'1'},timeout:30_000});
    try {
      assert(r.ok(), `${url}: ${r.status()} ${(await r.text()).slice(0,300)}`);
      return {body:await r.json(),ms:performance.now()-started};
    } finally {
      // Playwright retains response bodies until disposed. This is a multi-
      // hour load generator, so do not accumulate every historical response.
      await r.dispose();
    }
  };
  // Creating the cold fixture deliberately stops the panel. The installed
  // Agent then backs off its real transport; wait for that transport to recover
  // before measuring the no-export baseline. During qualification freshness
  // remains mandatory on every foreground request.
  const warmupDeadline = Date.now()+180_000;
  let warmup, initialNewest;
  const newestObservation = (body, metric) => {
    const observed=(body.series||[]).filter(s=>s.metric===metric).flatMap(s=>(s.queryPoints||[]).filter(p=>p.value!==undefined).map(p=>Date.parse(p.observedAt))).filter(Number.isFinite);
    return observed.length ? Math.max(...observed) : 0;
  };
  do {
    warmup = await api(`/${services.metrics}/QueryMetrics`,{agentIds:[manifest.agentId],metrics:['cpu.usage','ping.latency_ms'],startTime:new Date(Date.now()-3600_000).toISOString(),endTime:new Date().toISOString(),maxPoints:360,downsample:true});
    const newest=['cpu.usage','ping.latency_ms'].map(metric=>newestObservation(warmup.body,metric));
    initialNewest ??= newest;
    // A cached pre-shutdown point may still be <120s old. Require an actual
    // advance after this process starts, not merely a still-fresh old bucket.
    if(newest.every((value,i)=>value>initialNewest[i] && value>Date.now()-120_000)) break;
    fs.writeFileSync(path.join(artifacts,'export-agent-warmup.json'),JSON.stringify(warmup.body,null,2));
    assert(Date.now()<warmupDeadline,'Installed Agent did not recover after the fixture panel restart');
    await sleep(2000);
  } while(true);
  const foreground = async target => {
    const end = new Date(), start = new Date(end.getTime()-3600_000);
    const requests = {
      ping: [`/${services.metrics}/QueryMetrics`,{agentIds:[manifest.agentId],metrics:['ping.latency_ms'],startTime:start.toISOString(),endTime:end.toISOString(),maxPoints:360,downsample:true}],
      ping_stats: [`/${services.metrics}/GetPingStats`,{agentIds:[manifest.agentId],taskIds:[String(manifest.pingTaskId)],startTime:start.toISOString(),endTime:end.toISOString(),maxPoints:360}],
      chart: [`/${services.metrics}/QueryMetrics`,{agentIds:[manifest.agentId],metrics:['cpu.usage'],startTime:start.toISOString(),endTime:end.toISOString(),maxPoints:360,downsample:true}],
      traffic: [`/${services.browser}/GetTrafficTrend`,{window:'86400s',interval:'1200s'}],
      billing: [`/${services.dashboard}/GetDashboardCharts`,{charts:['DASHBOARD_CHART_TRAFFIC'],rankingLimit:5}],
    };
    await Promise.all(Object.entries(requests).map(async ([key,args]) => {
      const result = await api(...args); (target[key] ||= []).push(result.ms);
      reads.write(JSON.stringify({at:new Date().toISOString(),phase:target===baseline?'baseline':'during_export',query:key,ms:result.ms})+'\n');
      if(key==='chart'||key==='ping') {
        if(newestObservation(result.body,key==='chart'?'cpu.usage':'ping.latency_ms')<=Date.now()-120_000) {
          fs.writeFileSync(path.join(artifacts,`export-agent-${key}-stale.json`),JSON.stringify(result.body,null,2));
          throw Error(`Installed Agent ${key} stopped receiving fresh observations`);
        }
      }
      if(key==='ping_stats') assert(result.body.stats?.some(s=>String(s.taskId)===String(manifest.pingTaskId)&&s.valid>0),'Real Agent TCP probe statistics missing');
      if(key==='traffic') assert(result.body.buckets?.length===72,'Traffic buckets missing');
      if(key==='billing') assert(!result.body.charts?.traffic?.error,'Billing unavailable');
    }));
    execFileSync('systemctl',['is-active','--quiet','komari-e2e.service']);
  };
  // Identical concurrency and request shapes before and during export.
  for (let i=0;i<40;i++) { await foreground(baseline); await sleep(500); }
  fs.writeFileSync(path.join(artifacts,'export-baseline.json'),JSON.stringify(baseline));
  for(let i=0;i<30;i++) {
    const available = (await api('/api/admin/history/export/retention',undefined,'GET')).body.data.worker_available;
    if(available) break; if(i===29) throw Error('Worker unavailable'); await sleep(2000);
  }
  const created = (await api('/api/admin/history/export',{uuid:manifest.agentId,category:'resource',mode:'raw',format_version:2,start:manifest.start,end:manifest.end})).body.data;
  assert(created?.id,'Large job not accepted'); job=created;
  fs.writeFileSync(path.join(artifacts,'qualification-active.json'),JSON.stringify({id:job.id,created_at:job.created_at}));
  const started = Date.now(); let latestStatus='running', userPausedMS=0, lastStatusAt=started;
  timer=setInterval(() => {
    try {
      const now=Date.now(), cpu=stat(workerGroup), rss=Number(fs.readFileSync(path.join(workerGroup,'memory.current'),'utf8'));
      const sample={at:new Date(now).toISOString(),...cpu,rss,capacity,status:latestStatus,phase:job.phase,decoded:job.decoded_records,rows:job.written_rows,output_bytes:job.output_bytes,checkpoint:job.confirmed_checkpoint,wait_reason:job.wait_reason,throughput:job.throughput,phase_eta:job.phase_eta,resources:job.resources,whole_instance_memory:Number(fs.readFileSync(path.join(slice,'memory.current'),'utf8'))};
      if(previous && cpu.usage_usec>=previous.usage_usec) {
        const dt=(now-previous.now)/1000, used=(cpu.usage_usec-previous.usage_usec)/1e6;
        cpuTotal+=used; const share=used/dt/capacity; maxShare1s=Math.max(maxShare1s,share);sample.cpu_share=share;
        assert(share<=.10,`CPU limit exceeded: ${share}`);
        ring.push({used,dt,capacity});if(ring.length>10)ring.shift();
        if(ring.length===10){const share10=ring.reduce((s,x)=>s+x.used,0)/ring.reduce((s,x)=>s+x.dt*x.capacity,0);maxShare10s=Math.max(maxShare10s,share10);assert(share10<=.10,'10s CPU budget exceeded');}
      }
      maxRSS=Math.max(maxRSS,rss);assert(rss<=128*1048576,'Worker memory exceeded');
      assert(sample.whole_instance_memory<=1073741824,'Combined instance memory exceeded');
      previous={now,...cpu};samples++;output.write(JSON.stringify(sample)+'\n');
    } catch(e) { failed=e; }
  },1000);
  let paused=false,restarted=false,reduced=false,restored=false;
  while(true) {
    if(failed)throw failed;
    assert(Date.now()-started<4.5*3600_000,'Large export did not finish within qualification budget');
    job=(await api(`/api/admin/history/export/${job.id}`,undefined,'GET')).body.data;
    assert(job.phase_started_at && !job.phase_started_at.startsWith('0001-'),'Durable phase timestamp missing');
    assert(job.phase_eta?.scope==='phase' && job.throughput,'Progress observations missing');
    if(job.current_input) assert(job.current_input.index<=job.current_input.total,'Input unit identity exceeds manifest');
    const now=Date.now();if(latestStatus==='running'||latestStatus==='publishing')runningMS+=now-lastStatusAt;latestStatus=job.status;lastStatusAt=now;
    assert(!['failed','cancelled','expired'].includes(job.status),JSON.stringify(job));
    if(job.status==='done')break;
    await foreground(latencies);
    const elapsed=Date.now()-started;
    if(!paused && elapsed>300_000){
      const at=Date.now();await api(`/api/admin/history/export/${job.id}/pause`,{});
      for(let i=0;i<40;i++){const p=(await api(`/api/admin/history/export/${job.id}`,undefined,'GET')).body.data;if(p.status==='paused')break;await sleep(25);}
      const observed=(await api(`/api/admin/history/export/${job.id}`,undefined,'GET')).body.data;
      assert(observed.status==='paused' && Date.now()-at<=1000,'Running pause did not respond within one second');
      await api(`/api/admin/history/export/${job.id}/resume`,{});userPausedMS+=Date.now()-at;paused=true;
    }
    if(!restarted && elapsed>600_000){execFileSync('sudo',['systemctl','restart','komari-e2e-export.service']);restarted=true;previous=undefined;}
    if(!reduced && elapsed>900_000){execFileSync('sudo',['systemctl','set-property','--runtime','komari-e2e-panel.service','CPUQuota=50%']);capacity=.5;ring.length=0;reduced=true;}
    if(reduced&&!restored&&elapsed>960_000){execFileSync('sudo',['systemctl','set-property','--runtime','komari-e2e-panel.service','CPUQuota=100%']);capacity=1;ring.length=0;restored=true;}
    if(samples%60<6) console.log(JSON.stringify({elapsed_seconds:Math.round(elapsed/1000),status:job.status,phase:job.phase,rows:job.written_rows,decoded:job.decoded_records,wait:job.wait_reason,max_cpu_share:maxShare1s}));
    await sleep(5000);
  }
  assert(runningMS-userPausedMS>=7200_000,'Qualification finished before two hours of actual running work');
  assert(job.written_rows===manifest.records,'Missing or duplicate synthetic rows');
  assert(job.decoded_records>=manifest.records && job.retries===0,'Invalid decoding/recovery counters');
  const cookies=(await context.storageState()).cookies, cookie=cookies.filter(c=>c.domain==='127.0.0.1').map(c=>`${c.name}=${c.value}`).join('; ');
  const download=await new Promise((resolve,reject)=>{https.get(base+`/api/admin/history/export/${job.id}/download`,{headers:{Cookie:cookie}},r=>{
    if(r.statusCode!==200){r.resume();reject(Error('Download rejected'));return;}const h=crypto.createHash('sha256');let bytes=0,lines=0;
    r.on('data',b=>{h.update(b);bytes+=b.length;for(const x of b)if(x===10)lines++;});r.on('end',()=>resolve({sha256:h.digest('hex'),bytes,lines}));r.on('error',reject);
  }).on('error',reject);});
  assert(download.sha256===job.sha256 && download.lines===manifest.records+1,'Downloaded output checksum/row count differs');
  const comparison={};for(const key of Object.keys(baseline)) { const a=percentile(baseline[key],.95),b=percentile(latencies[key],.95);comparison[key]={baseline_p95_ms:a,during_p95_ms:b,ratio:b/a,p99_ms:percentile(latencies[key],.99),requests:latencies[key].length};assert(b<=a*1.2,`${key} p95 regression exceeds 20%: ${b/a}`); }
  const result={job,download,manifest,elapsed_seconds:(Date.now()-started)/1000,running_seconds:(runningMS-userPausedMS)/1000,samples,cpu_seconds:cpuTotal,max_cpu_share_1s:maxShare1s,max_cpu_share_10s:maxShare10s,max_worker_memory_current:maxRSS,memory_basis:'cgroup v2 memory.current, including page cache; not process RSS',foreground:comparison,paused,restarted,reduced,restored,hard_quota:'4000 100000, burst=0',shared_limit:'1 CPU / 1 GiB',io_budget_bytes_per_second:131072};
  fs.writeFileSync(path.join(artifacts,'export-qualification.json'),JSON.stringify(result,null,2));console.log(JSON.stringify(result));
}
run().catch(e=>{console.error(e);process.exitCode=1;fs.writeFileSync(path.join(artifacts,'export-qualification-error.txt'),e.stack||String(e));}).finally(async()=>{clearInterval(timer);output.end();reads.end();if(context)await context.dispose();});
