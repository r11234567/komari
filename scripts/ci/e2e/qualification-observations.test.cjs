const assert = require('node:assert/strict');
const test = require('node:test');
const { newestObservation, agentRecovered } = require('./qualification-observations.cjs');

const startedAt = Date.parse('2026-11-12T01:39:41Z');
const now = Date.parse('2026-11-12T01:40:11Z');
const response = (cpu, ping) => ({
  series: [
    { metric: 'cpu.usage', acceptedThrough: '650', queryPoints: [{ observedAt: cpu, value: 2.6 }] },
    { metric: 'ping.latency_ms', acceptedThrough: '650', queryPoints: [{ observedAt: ping, value: 0 }] },
  ],
});

test('the failed run old 20s/10s bucket timestamps cannot pass recovery', () => {
  const coarse = response('2026-11-12T01:38:00Z', '2026-11-12T01:38:00Z');
  const fine = response('2026-11-12T01:38:10Z', '2026-11-12T01:38:10Z');
  // Both responses have the same accepted watermark. Changing bucket size
  // moves the displayed timestamp forward without a new Agent observation.
  assert.ok(newestObservation(fine, 'ping.latency_ms') > newestObservation(coarse, 'ping.latency_ms'));
  assert.equal(agentRecovered(coarse, startedAt, now), false);
  assert.equal(agentRecovered(fine, startedAt, now), false);
});

test('both CPU and Ping must advance beyond the warmup start', () => {
  assert.equal(agentRecovered(response('2026-11-12T01:40:00Z', '2026-11-12T01:38:10Z'), startedAt, now), false);
  assert.equal(agentRecovered(response('2026-11-12T01:38:10Z', '2026-11-12T01:40:00Z'), startedAt, now), false);
  assert.equal(agentRecovered(response('2026-11-12T01:40:00Z', '2026-11-12T01:40:00Z'), startedAt, now), true);
});

test('zero latency remains a valid observation and missing points stay unavailable', () => {
  assert.equal(newestObservation(response('2026-11-12T01:40:00Z', '2026-11-12T01:40:00Z'), 'ping.latency_ms'), Date.parse('2026-11-12T01:40:00Z'));
  assert.equal(agentRecovered({ series: [] }, startedAt, now), false);
  assert.equal(agentRecovered({ series: [{ metric: 'cpu.usage', queryPoints: [{ observedAt: 'invalid', value: 1 }] }] }, startedAt, now), false);
});

test('recovery still rejects observations older than the 120s freshness bound', () => {
  assert.equal(agentRecovered(response('2026-11-12T01:40:00Z', '2026-11-12T01:40:00Z'), startedAt, Date.parse('2026-11-12T01:42:01Z')), false);
});
