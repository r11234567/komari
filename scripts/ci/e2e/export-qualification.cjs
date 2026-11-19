// Long qualification uses actual production worker code and real HTTPS reads.
// Synthetic cold load is labelled in its manifest; Agent samples stay real.
const fs = require('node:fs');
const path = require('node:path');
const https = require('node:https');
const crypto = require('node:crypto');
const { execFileSync } = require('node:child_process');
const { request } = require('playwright');
const { newestObservation, agentRecovered } = require('./qualification-observations.cjs');
const { pairedWindow, populationFingerprint, comparePerformancePairs } = require('./qualification-performance.cjs');
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
const output = fs.createWriteStream(path.join(artifacts, 'export-resources.jsonl'));
const reads = fs.createWriteStream(path.join(artifacts, 'export-foreground.jsonl'));
let timer, context, job, failed;
let capacity = 1, previous, samples = 0, maxShare1s = 0, maxShare10s = 0, maxRSS = 0, cpuTotal = 0, runningMS = 0;
const ring = [];
const latencies = {}, baseline = {};
const performancePairs = [];
let measurementPhase = 'live', lastStatusAt;
function observeStatus(status) {
  const now = Date.now();
  if (lastStatusAt && (job.status === 'running' || job.status === 'publishing')) runningMS += now - lastStatusAt;
  job.status = status;
  lastStatusAt = now;
}
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
  const warmupStarted = Date.now();
  const warmupDeadline = warmupStarted+180_000;
  let warmup;
  do {
    const end = new Date();
    warmup = await api(`/${services.metrics}/QueryMetrics`,{agentIds:[manifest.agentId],metrics:['cpu.usage','ping.latency_ms'],startTime:new Date(end.getTime()-3600_000).toISOString(),endTime:end.toISOString(),maxPoints:360,downsample:true});
    // Bucket timestamps can advance when output resolution changes, even
    // while the Agent is still backing off. Both metrics must contain an
    // observation after warmup began before starting the baseline.
    if(agentRecovered(warmup.body,warmupStarted,Date.now())) break;
    fs.writeFileSync(path.join(artifacts,'export-agent-warmup.json'),JSON.stringify(warmup.body,null,2));
    assert(Date.now()<warmupDeadline,'Installed Agent did not recover after the fixture panel restart');
    await sleep(2000);
  } while(true);
  fs.writeFileSync(path.join(artifacts,'export-agent-recovered.json'),JSON.stringify({
    warmup_started_at:new Date(warmupStarted).toISOString(),
    recovered_at:new Date().toISOString(),body:warmup.body,
  },null,2));
  const foreground = async (target, options = {}) => {
    const end = new Date(), start = new Date(end.getTime()-3600_000);
    const range = options.range || { startTime:start.toISOString(), endTime:end.toISOString() };
    const requests = {
      ping: [`/${services.metrics}/QueryMetrics`,{agentIds:[manifest.agentId],metrics:['ping.latency_ms'],...range,maxPoints:360,downsample:true}],
      ping_stats: [`/${services.metrics}/GetPingStats`,{agentIds:[manifest.agentId],taskIds:[String(manifest.pingTaskId)],...range,maxPoints:360}],
      chart: [`/${services.metrics}/QueryMetrics`,{agentIds:[manifest.agentId],metrics:['cpu.usage'],...range,maxPoints:360,downsample:true}],
      traffic: [`/${services.browser}/GetTrafficTrend`,{window:'86400s',interval:'1200s'}],
      billing: [`/${services.dashboard}/GetDashboardCharts`,{charts:['DASHBOARD_CHART_TRAFFIC'],rankingLimit:5}],
    };
    const fingerprints = {};
    const results = await Promise.allSettled(Object.entries(requests).map(async ([key,args]) => {
      const result = await api(...args); (target[key] ||= []).push(result.ms);
      reads.write(JSON.stringify({at:new Date().toISOString(),phase:options.phase||(target===baseline?'initial_baseline':'live_during_export'),pair:options.pair,query:key,ms:result.ms,range})+'\n');
      fingerprints[key] = populationFingerprint(key, result.body);
      if(options.expected && fingerprints[key] !== options.expected[key]) {
        fs.writeFileSync(path.join(artifacts,`export-pair-${options.pair}-${key}-changed.json`),JSON.stringify(result.body,null,2));
        throw Error(`Paired ${key} input population changed; latency comparison is invalid`);
      }
      if(options.fresh!==false && (key==='chart'||key==='ping')) {
        if(newestObservation(result.body,key==='chart'?'cpu.usage':'ping.latency_ms')<=Date.now()-120_000) {
          fs.writeFileSync(path.join(artifacts,`export-agent-${key}-stale.json`),JSON.stringify(result.body,null,2));
          throw Error(`Installed Agent ${key} stopped receiving fresh observations`);
        }
      }
      if(key==='ping_stats') assert(result.body.stats?.some(s=>String(s.taskId)===String(manifest.pingTaskId)&&s.valid>0),'Real Agent TCP probe statistics missing');
      if(key==='traffic') assert(result.body.buckets?.length===72,'Traffic buckets missing');
      if(key==='billing') assert(!result.body.charts?.traffic?.error,'Billing unavailable');
    }));
    // Keep diagnostics open until every concurrent response has settled.
    // Preserve the first real request failure after the other reads finish.
    const rejected = results.find(result => result.status === 'rejected');
    if (rejected) throw rejected.reason;
    execFileSync('systemctl',['is-active','--quiet','komari-e2e.service']);
    return fingerprints;
  };
  // Initial rolling-history measurements are diagnostic only: the installed
  // Agent's history grows during the run. Acceptance uses adjacent paired
  // export-off/export-on reads with identical fixed metric input below.
  for (let i=0;i<40;i++) { await foreground(baseline); await sleep(500); }
  fs.writeFileSync(path.join(artifacts,'export-baseline.json'),JSON.stringify(baseline));
  for(let i=0;i<30;i++) {
    const available = (await api('/api/admin/history/export/retention',undefined,'GET')).body.data.worker_available;
    if(available) break; if(i===29) throw Error('Worker unavailable'); await sleep(2000);
  }
  const created = (await api('/api/admin/history/export',{uuid:manifest.agentId,category:'resource',mode:'raw',format_version:2,start:manifest.start,end:manifest.end})).body.data;
  assert(created?.id,'Large job not accepted'); job=created;
  fs.writeFileSync(path.join(artifacts,'qualification-active.json'),JSON.stringify({id:job.id,created_at:job.created_at}));
  const started = Date.now(); let latestStatus=job.status, userPausedMS=0;
  lastStatusAt=started;
  timer=setInterval(() => {
    let sample;
    try {
      const now=Date.now(), cpu=stat(workerGroup), rss=Number(fs.readFileSync(path.join(workerGroup,'memory.current'),'utf8'));
      const memory = {};
      for (const line of fs.readFileSync(path.join(workerGroup,'memory.stat'),'utf8').trim().split('\n')) {
        const [key,value] = line.split(/\s+/);
        memory[key] = Number(value);
      }
      sample={at:new Date(now).toISOString(),...cpu,rss,capacity,status:latestStatus,measurement:measurementPhase,phase:job.phase,decoded:job.decoded_records,rows:job.written_rows,output_bytes:job.output_bytes,checkpoint:job.confirmed_checkpoint,wait_reason:job.wait_reason,throughput:job.throughput,phase_eta:job.phase_eta,resources:job.resources,whole_instance_memory:Number(fs.readFileSync(path.join(slice,'memory.current'),'utf8')),
        memory: Object.fromEntries(['anon','file','file_dirty','file_writeback','kernel','pagetables','slab'].map(key=>[key,memory[key]])),
        memory_events:fs.readFileSync(path.join(workerGroup,'memory.events'),'utf8').trim()};
      if(previous && cpu.usage_usec>=previous.usage_usec) {
        const dt=(now-previous.now)/1000, used=(cpu.usage_usec-previous.usage_usec)/1e6;
        cpuTotal+=used; const share=used/dt/capacity; maxShare1s=Math.max(maxShare1s,share);sample.cpu_share=share;
        assert(share<=.10,`CPU limit exceeded: ${share}`);
        ring.push({used,dt,capacity});if(ring.length>10)ring.shift();
        if(ring.length===10){const share10=ring.reduce((s,x)=>s+x.used,0)/ring.reduce((s,x)=>s+x.dt*x.capacity,0);maxShare10s=Math.max(maxShare10s,share10);assert(share10<=.10,'10s CPU budget exceeded');}
      }
      maxRSS=Math.max(maxRSS,rss);assert(rss<=128*1048576,'Worker memory exceeded');
      assert(sample.whole_instance_memory<=1073741824,'Combined instance memory exceeded');
      previous={now,...cpu};samples++;
    } catch(e) { failed=e; }
    finally {
      if (sample) output.write(JSON.stringify(sample)+'\n');
    }
  },1000);
  let paused=false,restarted=false,reduced=false,restored=false,reducedAt;
  let nextPairAt=started+300_000;
  const measurePair = async () => {
    const id=performancePairs.length+1, pair={id,anchor:Date.now(),capacity,state:'pausing',baseline:{},during:{},fingerprints:[],control_observations:[]};
    performancePairs.push(pair);
    const savePair = () => fs.writeFileSync(path.join(artifacts,'export-performance-pairs.json'),JSON.stringify(performancePairs,null,2));
    const readPairState = async () => {
      const current=(await api(`/api/admin/history/export/${job.id}`,undefined,'GET')).body.data;
      job=current;latestStatus=current.status;
      pair.control_observations.push({at:new Date().toISOString(),status:current.status,phase:current.phase,wait_reason:current.wait_reason,output_bytes:current.output_bytes,verification:current.checkpoint_verification});
      savePair();return current;
    };
    savePair();
    const pauseAt=Date.now();
    await api(`/api/admin/history/export/${job.id}/pause`,{});
    let state;
    do {
      state=await readPairState();
      if(state.status==='paused') break;
      assert(Date.now()-pauseAt<1000,'Running pause did not respond within one second');
      await sleep(25);
    } while(true);
    assert(Date.now()-pauseAt<=1000,'Running pause did not respond within one second');
    observeStatus('paused');latestStatus='paused';measurementPhase='paired_export_off';
    const offAt=Date.now(), pausedBytes=state.output_bytes;
    pair.state='export_off';pair.output_bytes_before=pausedBytes;
    for(let i=0;i<40;i++) {
      pair.fingerprints.push(await foreground(pair.baseline,{range:pairedWindow(pair.anchor,i),fresh:false,phase:'paired_export_off',pair:id}));
      savePair();
      await sleep(500);
    }
    state=await readPairState();
    assert(state.status==='paused' && state.output_bytes===pausedBytes,'Export advanced during its no-export baseline');
    await api(`/api/admin/history/export/${job.id}/resume`,{});
    userPausedMS+=Date.now()-offAt;
    const resumeAt=Date.now();
    do {
      state=await readPairState();
      assert(!['done','failed','cancelled','expired'].includes(state.status),'Export ended before the paired running window');
      if(state.status==='running') break;
      assert(Date.now()-resumeAt<30000,'Export did not resume for paired measurement');
      await sleep(100);
    } while(true);
    observeStatus('running');latestStatus='running';measurementPhase='paired_export_on';
    pair.state='export_on';savePair();
    // Every replay follows all forty baseline requests (>20s), beyond the 5s
    // application cache TTL. Both phases execute the same five-way concurrency.
    for(let i=0;i<40;i++) {
      await foreground(pair.during,{range:pairedWindow(pair.anchor,i),fresh:false,phase:'paired_export_on',pair:id,expected:pair.fingerprints[i]});
      savePair();
      await sleep(500);
    }
    state=await readPairState();
    assert(state.output_bytes>pausedBytes,'No actual CSV work occurred during paired measurement');
    pair.output_bytes_after=state.output_bytes;pair.state='complete';
    paused=true;measurementPhase='live';savePair();
    await foreground(latencies); // Continue checking live Agent freshness.
  };
  while(true) {
    if(failed)throw failed;
    assert(Date.now()-started<4.5*3600_000,'Large export did not finish within qualification budget');
    const observed=(await api(`/api/admin/history/export/${job.id}`,undefined,'GET')).body.data;
    observeStatus(observed.status);job=observed;
    assert(job.phase_started_at && !job.phase_started_at.startsWith('0001-'),'Durable phase timestamp missing');
    assert(job.phase_eta?.scope==='phase' && job.throughput,'Progress observations missing');
    if(job.current_input) assert(job.current_input.index<=job.current_input.total,'Input unit identity exceeds manifest');
    latestStatus=job.status;
    assert(!['failed','cancelled','expired'].includes(job.status),JSON.stringify(job));
    if(job.status==='done')break;
    await foreground(latencies);
    const elapsed=Date.now()-started;
    if(Date.now()>=nextPairAt){await measurePair();nextPairAt+=900_000;}
    if(!restarted && elapsed>600_000){execFileSync('sudo',['systemctl','restart','komari-e2e-export.service']);restarted=true;previous=undefined;}
    if(!reduced && elapsed>900_000){execFileSync('sudo',['systemctl','set-property','--runtime','komari-e2e-panel.service','CPUQuota=50%']);capacity=.5;ring.length=0;reduced=true;reducedAt=Date.now();}
    if(reduced&&!restored&&Date.now()-reducedAt>60000){execFileSync('sudo',['systemctl','set-property','--runtime','komari-e2e-panel.service','CPUQuota=100%']);capacity=1;ring.length=0;restored=true;}
    if(samples%60<6) console.log(JSON.stringify({elapsed_seconds:Math.round(elapsed/1000),status:job.status,phase:job.phase,rows:job.written_rows,decoded:job.decoded_records,wait:job.wait_reason,max_cpu_share:maxShare1s}));
    await sleep(5000);
  }
  assert(runningMS>=7200_000,'Qualification finished before two hours of actual running work');
  assert(job.written_rows===manifest.records,'Missing or duplicate synthetic rows');
  assert(job.decoded_records>=manifest.records && job.retries===0,'Invalid decoding/recovery counters');
  const cookies=(await context.storageState()).cookies, cookie=cookies.filter(c=>c.domain==='127.0.0.1').map(c=>`${c.name}=${c.value}`).join('; ');
  const download=await new Promise((resolve,reject)=>{https.get(base+`/api/admin/history/export/${job.id}/download`,{headers:{Cookie:cookie}},r=>{
    if(r.statusCode!==200){r.resume();reject(Error('Download rejected'));return;}const h=crypto.createHash('sha256');let bytes=0,lines=0;
    r.on('data',b=>{h.update(b);bytes+=b.length;for(const x of b)if(x===10)lines++;});r.on('end',()=>resolve({sha256:h.digest('hex'),bytes,lines}));r.on('error',reject);
  }).on('error',reject);});
  assert(download.sha256===job.sha256 && download.lines===manifest.records+1,'Downloaded output checksum/row count differs');
  const comparison=comparePerformancePairs(performancePairs);
  const result={job,download,manifest,elapsed_seconds:(Date.now()-started)/1000,running_seconds:runningMS/1000,user_paused_seconds:userPausedMS/1000,samples,cpu_seconds:cpuTotal,max_cpu_share_1s:maxShare1s,max_cpu_share_10s:maxShare10s,max_worker_memory_current:maxRSS,memory_basis:'cgroup v2 memory.current, including page cache; not process RSS',foreground:comparison,foreground_method:'adjacent export-off/on pairs; identical fixed metric population, five-way concurrency and cache-miss request keys; live freshness checked separately',paused,restarted,reduced,restored,hard_quota:'4000 100000, burst=0',shared_limit:'1 CPU / 1 GiB',io_budget_bytes_per_second:131072};
  fs.writeFileSync(path.join(artifacts,'export-qualification.json'),JSON.stringify(result,null,2));console.log(JSON.stringify(result));
  assert(performancePairs.length>=8,'Missing sustained paired foreground measurements');
  for(const [key,values] of Object.entries(comparison)) assert(values.ratio<=1.2,`${key} p95 regression exceeds 20%: ${values.ratio}`);
}
run().catch(e=>{console.error(e);process.exitCode=1;fs.writeFileSync(path.join(artifacts,'export-qualification-error.txt'),e.stack||String(e));}).finally(async()=>{clearInterval(timer);output.end();reads.end();if(context)await context.dispose();});
