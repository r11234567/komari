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
  for (const window of oracle.windows) {
    // Migration takes real time. Test a current preset instead of pretending
    // that a stale fixed 1h window still lies inside 1h fine-tier retention.
    const step = window.interval_seconds * 1000, end = new Date();
    const start = new Date(Math.ceil((end.getTime()-window.hours*3600_000)/step)*step);
    const request = { agentIds:[agentId], metrics:['cpu.usage','ping.latency_ms'], startTime:start.toISOString(), endTime:end.toISOString(), maxPoints:Math.ceil(window.hours*3600/window.interval_seconds), downsample:true, aggregation:'avg' };
    const response = await context.request.post('/komari.metrics.v1.MetricsService/QueryMetrics', { data:request, headers });
    expect(response.status(), `${window.hours}h`).toBe(200);
    const body = await response.json(), comparisons = [];
    for (const [metric, observations, ping] of [['cpu.usage',oracle.cpu_observations,false],['ping.latency_ms',oracle.ping_observations,true]]) {
      const selected = (body.series || []).filter(s => s.metric === metric);
      expect(selected, `${metric}/${window.hours}h`).toHaveLength(1);
      expect(selected[0].interval).toBe(`${window.interval_seconds}s`);
      const points = selected[0].queryPoints || [];
      const expected = aggregateObservations(observations, request.startTime, request.endTime, window.interval_seconds, ping);
      expect(points, `${metric}/${window.hours}h`).toHaveLength(expected.length);
      for (let i=0; i<points.length; i++) {
        expect(new Date(points[i].observedAt).getTime()).toBe(new Date(expected[i].bucket).getTime());
        expect(Number(points[i].sampleCount)).toBe(expected[i].count);
        expect(points[i]).toHaveProperty('value');
        expect(Number(points[i].value || 0)).toBeCloseTo(expected[i].value, 7);
        expect(points[i].coverage).toBe('complete');
      }
      comparisons.push({ metric, expected });
    }
    results.push({ hours:window.hours, request, comparisons, response:body });
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
    if (!custom && await radio.isChecked()) await chart.getByRole('radio',{name:/^10\s*minutes?$/i}).click();
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
      return q.agentIds.includes(agentId) && q.metrics.includes('cpu.usage') && q.maxPoints===700 && Math.abs(new Date(q.endTime)-new Date(q.startTime)-hours*3600_000)<5;
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
module.exports = { verifyStorageCurves };
