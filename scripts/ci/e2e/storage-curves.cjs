// Only formula-generated runner data is accepted. HTTP responses and shipped
// UI screenshots are retained together; no browser routing or React injection.
const fs = require('node:fs');
const path = require('node:path');
const { expect } = require('@playwright/test');
const { aggregateObservations } = require('./storage-curve-oracle.cjs');
const { decodeMetricsRequest, decodeMetricsResponse } = require('./storage-metrics-wire.cjs');

// Go RFC3339Nano and protobuf JSON preserve nanoseconds, while Date truncates
// to milliseconds. Compare boundaries as instants without rounding a
// sub-interval inward adjustment up to a whole interval.
function timestampNs(value) {
  const match = /^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?Z$/.exec(value);
  expect(match, `UTC timestamp: ${value}`).not.toBeNull();
  return BigInt(Date.parse(`${match[1]}Z`))*1_000_000n + BigInt((match[2]||'').padEnd(9,'0'));
}

async function verifyStorageCurves({ context, page, root, artifacts, base, agentId }) {
  require('./guard.cjs');
  const oracle = JSON.parse(fs.readFileSync(path.join(root, 'migration-curve-oracle.json')));
  expect(oracle.origin).toBe('formula-generated synthetic observations only');
  const headers = { Origin: base, 'Connect-Protocol-Version': '1' }, results = [], ui = [];
  const sliding = [{age:30,hours:6},{age:4*24,hours:24},{age:20*24,hours:7*24},{age:90*24,hours:7*24}];
  for (const window of [...oracle.windows,...sliding.map(w=>({...w,interval_seconds:600}))]) {
    // One unaligned rolling request must advertise its complete effective
    // bounds itself. Do not repair an API response with a second request.
    const end = window.age ? new Date(Math.floor((Date.now()-(window.age-window.hours)*3600_000)/86400_000)*86400_000-1) : new Date();
    const start = new Date(end.getTime()+1-window.hours*3600_000);
    const request = { agentIds:[agentId], metrics:['cpu.usage','ping.latency_ms'], startTime:start.toISOString(), endTime:end.toISOString(), maxPoints:Math.ceil(window.hours*3600/window.interval_seconds), downsample:true, aggregation:'avg' };
    const response = await context.request.post('/komari.metrics.v1.MetricsService/QueryMetrics', { data:request, headers });
    expect(response.status(), `${window.hours}h`).toBe(200);
    const body = await response.json();
    const comparisons = [];
    for (const [metric, observations, ping] of [['cpu.usage',oracle.cpu_observations,false],['ping.latency_ms',oracle.ping_observations,true]]) {
      const selected = (body.series || []).filter(s => s.metric === metric);
      expect(selected, `${metric}/${window.hours}h`).toHaveLength(1);
      const interval=Number.parseFloat(selected[0].interval);
      if (!window.age) expect(interval).toBe(window.interval_seconds);
      else expect(interval).toBeGreaterThanOrEqual(window.interval_seconds);
      expect(interval%10).toBe(0);
      if(window.hours===24&&!window.age)expect(interval).toBe(300);
      const points = selected[0].queryPoints || [];
      expect(selected[0].coverage).toBe('complete');
      const actualStart=selected[0].coverageStart,actualEnd=selected[0].coverageEnd;
      const intervalNs=BigInt(interval)*1_000_000_000n;
      expect(timestampNs(actualStart)).toBeGreaterThanOrEqual(timestampNs(start.toISOString()));
      expect(timestampNs(actualStart)-timestampNs(start.toISOString())).toBeLessThan(intervalNs);
      expect(timestampNs(actualEnd)).toBeLessThanOrEqual(timestampNs(end.toISOString()));
      expect(timestampNs(end.toISOString())-timestampNs(actualEnd)).toBeLessThan(intervalNs);
      const expected = aggregateObservations(observations, actualStart, actualEnd, interval, ping);
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
    if ([1,24,168,360].includes(window.hours) && !window.age) {
      const rpcResponse=await context.request.post('/api/rpc2',{data:{jsonrpc:'2.0',id:window.hours,method:'public:queryMetrics',params:{entity_id:agentId,metrics:request.metrics,start_time:request.startTime,end_time:request.endTime,max_points:request.maxPoints,downsample:true,aggregation:'avg'}},headers:{Origin:base}});
      expect(rpcResponse.status()).toBe(200);
      const rpcBody=await rpcResponse.json();expect(rpcBody.error).toBeUndefined();
      for(const expectedSeries of body.series){
        const actual=rpcBody.result.series.find(s=>s.metric_key===expectedSeries.metric);
        expect(actual.interval_seconds).toBe(Number.parseFloat(expectedSeries.interval));
        expect(timestampNs(actual.coverage_start)).toBe(timestampNs(expectedSeries.coverageStart));
        expect(timestampNs(actual.coverage_end)).toBe(timestampNs(expectedSeries.coverageEnd));
        expect(actual.points).toHaveLength(expectedSeries.queryPoints.length);
        actual.points.forEach((p,i)=>{expect(new Date(p.time).getTime()).toBe(new Date(expectedSeries.queryPoints[i].observedAt).getTime());expect(p.count).toBe(Number(expectedSeries.queryPoints[i].sampleCount));expect(p.value).toBeCloseTo(Number(expectedSeries.queryPoints[i].value||0),7);});
      }
      comparisons.push({protocol:'jsonrpc',response:rpcBody});
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
    // This fixture has a full historical population, so a dot is insufficient.
    expect(body.series.find(s=>s.metric==='cpu.usage')?.queryPoints.length).toBeGreaterThan(1);
    await expect(chart.locator('.recharts-line-curve').first()).toBeVisible();
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
