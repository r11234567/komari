// Only formula-generated runner data is accepted. HTTP responses and shipped
// UI screenshots are retained together; no browser routing or React injection.
const fs = require('node:fs');
const path = require('node:path');
const { expect } = require('@playwright/test');
const { aggregateObservations } = require('./storage-curve-oracle.cjs');
const { decodeMetricsRequest, decodeMetricsResponse } = require('./storage-metrics-wire.cjs');

async function verifyStorageCurves({ context, page, root, artifacts, base, agentId }) {
  require('./guard.cjs');
  const oracle = JSON.parse(fs.readFileSync(path.join(root, 'migration-curve-oracle.json')));
  expect(oracle.origin).toBe('formula-generated synthetic observations only');
  const headers = { Origin: base, 'Connect-Protocol-Version': '1' }, results = [], ui = [];
  const sliding = [{age:30,hours:6},{age:4*24,hours:24},{age:20*24,hours:7*24},{age:90*24,hours:7*24}];
  for (const window of [...oracle.windows,...sliding.map(w=>({...w,interval_seconds:600}))]) {
    // Migration takes real time. Test a current preset instead of pretending
    // that a stale fixed 1h window still lies inside 1h fine-tier retention.
    const step = window.interval_seconds * 1000;
    const end = window.age ? new Date(Math.floor((Date.now()-(window.age-window.hours)*3600_000)/86400_000)*86400_000-1) : new Date();
    const start = window.age ? new Date(end.getTime()+1-window.hours*3600_000) : new Date(Math.ceil((end.getTime()-window.hours*3600_000)/step)*step);
    const request = { agentIds:[agentId], metrics:['cpu.usage','ping.latency_ms'], startTime:start.toISOString(), endTime:end.toISOString(), maxPoints:Math.ceil(window.hours*3600/window.interval_seconds), downsample:true, aggregation:'avg' };
    let response = await context.request.post('/komari.metrics.v1.MetricsService/QueryMetrics', { data:request, headers });
    expect(response.status(), `${window.hours}h`).toBe(200);
    let body = await response.json();
    // A complete historical population requires whole physical cells. The
    // advertised interval is the API contract; values still use the independent
    // raw oracle, never another invocation of the storage implementation.
    const intervals=(body.series||[]).map(s=>Number.parseFloat(s.interval));
    expect(intervals.length).toBeGreaterThan(0);
    const grid=Math.max(...intervals)*1000;
    expect(Number.isFinite(grid)&&grid>0).toBe(true);
    const aligned=new Date(Math.ceil(start.getTime()/grid)*grid).toISOString();
    if(aligned!==request.startTime){
      request.startTime=aligned;
      response=await context.request.post('/komari.metrics.v1.MetricsService/QueryMetrics',{data:request,headers});
      expect(response.status()).toBe(200);body=await response.json();
    }
    const comparisons = [];
    for (const [metric, observations, ping] of [['cpu.usage',oracle.cpu_observations,false],['ping.latency_ms',oracle.ping_observations,true]]) {
      const selected = (body.series || []).filter(s => s.metric === metric);
      expect(selected, `${metric}/${window.hours}h`).toHaveLength(1);
      const interval=Number.parseFloat(selected[0].interval);
      expect(interval).toBeGreaterThanOrEqual(window.interval_seconds);
      expect(interval%10).toBe(0);
      if(window.hours===24&&!window.age)expect(interval).toBe(300);
      const points = selected[0].queryPoints || [];
      const expected = aggregateObservations(observations, request.startTime, request.endTime, interval, ping);
      expect(expected.length).toBeGreaterThan(0);
      expect(points, `${metric}/${window.hours}h`).toHaveLength(expected.length);
      for (let i=0; i<points.length; i++) {
        expect(new Date(points[i].observedAt).getTime()).toBe(new Date(expected[i].bucket).getTime());
        expect(Number(points[i].sampleCount), `${metric}/${window.hours}h/${expected[i].bucket}`).toBe(expected[i].count);
        expect(points[i]).toHaveProperty('value');
        expect(Number(points[i].value || 0)).toBeCloseTo(expected[i].value, 7);
        expect(points[i].coverage).toBe('complete');
      }
      comparisons.push({ metric, expected });
    }
    results.push({ hours:window.hours, age:window.age, request, comparisons, response:body });
    fs.writeFileSync(path.join(artifacts,'storage-curve-http.json'),JSON.stringify(results,null,2));
  }
  // An unrelated background QueryMetrics response cannot satisfy this wait.
  await page.goto(`/instance/${agentId}`);
  const chart = page.locator('.km-load-chart');
  await expect(chart).toBeVisible();
  for (const {hours} of oracle.windows.filter(w => w.hours!==365*24)) {
    const label = hours < 24 ? new RegExp(`^${hours}\\s*hours?$`,'i') : new RegExp(`^${hours/24}\\s*days?$`,'i');
    const radio = chart.getByRole('radio',{name:label});
    const custom = await radio.count() === 0;
    if (!custom && await radio.isChecked()) await chart.getByRole('radio',{name:/real.?time/i}).click();
    if (custom) {
      await chart.getByRole('radio',{name:/custom range/i}).click();
      const end = new Date(Math.floor(Date.now()/60_000)*60_000);
      // datetime-local is expressed in the browser's local timezone.
      const local = d => new Date(d.getTime()-d.getTimezoneOffset()*60_000).toISOString().slice(0,16);
      await chart.getByLabel(/start time/i).fill(local(new Date(end-hours*3600_000)));
      await chart.getByLabel(/end time/i).fill(local(end));
    }
    const pending = page.waitForResponse(r => {
      if (!new URL(r.url()).pathname.endsWith('/QueryMetrics') || r.status()!==200) return false;
      if (!r.request().headers()['content-type']?.includes('application/proto')) return false;
      const q = decodeMetricsRequest(r.request().postDataBuffer());
      return q.agentIds.includes(agentId) && q.metrics.includes('cpu.usage') && q.maxPoints===600 && Math.abs(new Date(q.endTime)-new Date(q.startTime)-hours*3600_000)<5;
    });
    if (custom) await chart.getByRole('button',{name:/^query$/i}).click(); else await radio.click();
    const actual = await pending, body = decodeMetricsResponse(await actual.body());
    expect(body.series.some(s => s.queryPoints.length > 0)).toBe(true);
    await expect(chart.locator('.recharts-line-curve, .recharts-line-dots .recharts-dot').first()).toBeVisible();
    const screenshot = `storage-curve-${hours}h.png`;
    await page.screenshot({path:path.join(artifacts,screenshot),fullPage:true,animations:'disabled'});
    ui.push({hours,screenshot,request:decodeMetricsRequest(actual.request().postDataBuffer()),response:body});
    fs.writeFileSync(path.join(artifacts,'storage-curve-screenshots.json'),JSON.stringify(ui,null,2));
  }
  fs.writeFileSync(path.join(artifacts,'storage-curve-oracle.json'),JSON.stringify(oracle,null,2));
  fs.writeFileSync(path.join(artifacts,'storage-curve-http.json'),JSON.stringify(results,null,2));
  fs.writeFileSync(path.join(artifacts,'storage-curve-screenshots.json'),JSON.stringify(ui,null,2));
}

async function verifyRealtimeRetention({context,page,root,artifacts,base,agentId,phase}) {
  require('./guard.cjs');
  const oracle=JSON.parse(fs.readFileSync(path.join(root,'migration-curve-oracle.json')));
  expect(oracle.origin).toBe('formula-generated synthetic observations only');
  const results=[];
  async function compare(request,body){
    const expected=oracle.cpu_observations.filter(p=>new Date(p.timestamp)>=new Date(request.startTime)&&new Date(p.timestamp)<=new Date(request.endTime)).sort((a,b)=>new Date(a.timestamp)-new Date(b.timestamp));
    expect(expected.length,`${phase}: fresh synthetic live window`).toBeGreaterThan(0);
    const points=(body.series||[]).find(s=>s.metric==='cpu.usage')?.queryPoints||[];
    expect(points).toHaveLength(expected.length);
    points.forEach((p,i)=>{expect(new Date(p.observedAt).getTime()).toBe(new Date(expected[i].timestamp).getTime());expect(Number(p.value||0)).toBe(expected[i].value);});
    results.push({request,response:body,expected_count:expected.length});
  }
  const end=new Date(),request={agentIds:[agentId],metrics:['cpu.usage'],startTime:new Date(end-14.5*60_000).toISOString(),endTime:end.toISOString(),maxPoints:300,downsample:false};
  const response=await context.request.post('/komari.metrics.v1.MetricsService/QueryMetrics',{data:request,headers:{Origin:base,'Connect-Protocol-Version':'1'}});
  if(response.status()!==200)throw Error(JSON.stringify({request,status:response.status(),headers:response.headers(),body:await response.text()}));
  expect(response.status(),await response.text()).toBe(200);await compare(request,await response.json());
  await page.goto(`/instance/${agentId}`);
  const chart=page.locator('.km-load-chart');await expect(chart).toBeVisible();
  await chart.getByRole('radio',{name:/real.?time/i}).click();
  const pending=page.waitForResponse(r=>{
    if(!new URL(r.url()).pathname.endsWith('/QueryMetrics')||r.status()!==200||!r.request().headers()['content-type']?.includes('application/proto'))return false;
    const q=decodeMetricsRequest(r.request().postDataBuffer());return q.agentIds.includes(agentId)&&q.metrics.includes('cpu.usage')&&!q.downsample;
  });
  await page.reload();
  const actual=await pending;
  await compare(decodeMetricsRequest(actual.request().postDataBuffer()),decodeMetricsResponse(await actual.body()));
  await expect(chart.locator('.recharts-line-curve, .recharts-line-dots .recharts-dot').first()).toBeVisible();
  await page.screenshot({path:path.join(artifacts,`storage-realtime-${phase}.png`),fullPage:true,animations:'disabled'});
  fs.writeFileSync(path.join(artifacts,`storage-realtime-${phase}.json`),JSON.stringify(results,null,2));
}
module.exports = { verifyStorageCurves,verifyRealtimeRetention };
