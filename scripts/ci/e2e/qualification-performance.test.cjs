const assert = require('node:assert/strict');
const test = require('node:test');
const { pairedWindow, populationFingerprint, comparePerformancePairs } = require('./qualification-performance.cjs');
const keys = ['ping', 'ping_stats', 'chart', 'traffic', 'billing'];
const samples = value => Object.fromEntries(keys.map(key => [key, Array(40).fill(value)]));

test('the same settled one-hour ranges replay with unique cache keys', () => {
  const anchor = Date.parse('2026-11-12T12:00:00Z');
  for (let i = 0; i < 40; i++) {
    const range = pairedWindow(anchor, i);
    assert.equal(Date.parse(range.endTime) - Date.parse(range.startTime), 3600_000);
    assert.ok(Date.parse(range.endTime) <= anchor - 300_000);
    assert.deepEqual(range, pairedWindow(anchor, i));
  }
  assert.equal(new Set(Array.from({ length: 40 }, (_, i) => pairedWindow(anchor, i).endTime)).size, 40);
  assert.equal(new Set(Array.from({ length: 40 }, (_, i) => Math.floor(Date.parse(pairedWindow(anchor, i).endTime) / 5_000))).size, 40);
});

test('changing watermarks do not hide changes to observations or counts', () => {
  const response = (value, sampleCount, acceptedThrough) => ({ series: [{ metric: 'cpu.usage', agentId: 'node', queryPoints: [{ observedAt: '2026-11-12T11:00:00Z', value, sampleCount }], acceptedThrough }] });
  assert.equal(populationFingerprint('chart', response(5, 3, '1')), populationFingerprint('chart', response(5, 3, '2')));
  assert.notEqual(populationFingerprint('chart', response(5, 3, '1')), populationFingerprint('chart', response(6, 3, '1')));
  assert.notEqual(populationFingerprint('chart', response(5, 3, '1')), populationFingerprint('chart', response(5, 4, '1')));
  assert.notEqual(populationFingerprint('ping_stats', { stats: [{ valid: 5 }] }), populationFingerprint('ping_stats', { stats: [{ valid: 6 }] }));
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
