const assert = require('node:assert/strict');
const test = require('node:test');
const { pairedMaxPoints, pairedWindow, pairedPopulation, samePopulation, comparePerformancePairs } = require('./qualification-performance.cjs');
const keys = ['ping', 'ping_stats', 'chart', 'traffic', 'billing'];
const samples = value => Object.fromEntries(keys.map(key => [key, Array(40).fill(value)]));

test('the same settled one-hour ranges replay with unique cache keys', () => {
  const anchor = Date.parse('2026-11-12T12:00:00Z');
  assert.equal(3600_000 / pairedMaxPoints, 60_000);
  for (let i = 0; i < 40; i++) {
    const range = pairedWindow(anchor, i);
    assert.equal(Date.parse(range.endTime) - Date.parse(range.startTime), 3600_000);
    assert.ok(Date.parse(range.endTime) <= anchor - 300_000);
    assert.deepEqual(range, pairedWindow(anchor, i));
  }
  assert.equal(new Set(Array.from({ length: 40 }, (_, i) => pairedWindow(anchor, i).endTime)).size, 40);
  assert.equal(new Set(Array.from({ length: 40 }, (_, i) => Math.floor(Date.parse(pairedWindow(anchor, i).endTime) / 5_000))).size, 40);
});

test('promotion can change source precision and roundoff but must preserve chart observations', () => {
  const response = (sourceInterval, value, sampleCount = 20, extra = {}) => ({
    series: [{
      metric: 'cpu.usage', agentId: 'node', labels: { device: 'cpu0' },
      interval: '60s', coverage: 'complete', coverageStart: '2026-11-22T10:59:00Z',
      queryPoints: [{ observedAt: '2026-11-22T11:00:00Z', sourceInterval, value, sampleCount, coverage: 'complete' }],
      ...extra,
    }],
  });
  const original = pairedPopulation('chart', response('60s', 0.8339524726398924));
  const equal = candidate => samePopulation(original, pairedPopulation('chart', candidate));
  assert.equal(equal(response('60s', 0.8339524726398923, 20, { acceptedThrough: '2' })), true);
  assert.equal(equal(response('10s', 0.8339524726398923)), true);
  assert.equal(equal(response('60s', 0.84)), false);
  assert.equal(equal(response('60s', 0.8339524726398923, 19)), false);
  assert.equal(equal(response('60s', 0.8339524726398923, 20, { coverage: 'partial' })), false);
  assert.equal(equal(response('60s', 0.8339524726398923, 20, { coverageStart: '2026-11-22T11:00:00Z' })), false);
  assert.equal(equal(response('60s', 0.8339524726398923, 20, { labels: { device: 'cpu1' } })), false);
  assert.equal(equal(response('60s', undefined)), false);
});

test('paired ping statistics preserve the coverage window and probe counts', () => {
  const response = (valid, average, startTime = '2026-11-22T11:00:00Z') => ({
    startTime, endTime: '2026-11-22T12:00:00Z', interval: '60s',
    stats: [{ taskId: '7', total: 14, valid, average, latest: 2 }],
  });
  const original = pairedPopulation('ping_stats', response(8, 2));
  const equal = candidate => samePopulation(original, pairedPopulation('ping_stats', candidate));
  assert.equal(equal(response(8, 2 + 2 * Number.EPSILON)), true);
  assert.equal(equal(response(9, 2)), false);
  assert.equal(equal(response(8, 2, '2026-11-22T11:01:00Z')), false);
  assert.equal(equal(response(8, 2.001)), false);
});

test('comparison matches growing history and still exposes an export regression', () => {
  const matched = [{ baseline: samples(10), during: samples(11) }, { baseline: samples(100), during: samples(110) }];
  assert.equal(comparePerformancePairs(matched).chart.ratio, 1.1);
  assert.equal(comparePerformancePairs([{ baseline: samples(100), during: samples(121) }]).chart.ratio, 1.21);
});

test('missing pairs, incomplete rounds and nonfinite samples cannot pass', () => {
  assert.throws(() => comparePerformancePairs([]));
  assert.throws(() => comparePerformancePairs([{ baseline: samples(10), during: { ...samples(10), ping: [10] } }]));
  assert.throws(() => comparePerformancePairs([{ baseline: samples(NaN), during: samples(10) }]));
});
