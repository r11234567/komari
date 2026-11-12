const crypto = require('node:crypto');

function pairedWindow(anchor, index) {
  // Leave five minutes for real reports and summary delivery to settle. Vary
  // each request key, then replay the exact range after the cache TTL expires.
  // QueryMetrics canonicalizes cache boundaries to five-second quanta.
  // Millisecond-only variation would repeatedly hit the same cached result.
  const end = anchor - 300_000 - index * 5_000;
  return { startTime: new Date(end - 3600_000).toISOString(), endTime: new Date(end).toISOString() };
}

function canonical(value) {
  if (Array.isArray(value)) return value.map(canonical);
  if (value && typeof value === 'object') return Object.fromEntries(Object.keys(value).sort().map(key => [key, canonical(value[key])]));
  return value;
}

function populationFingerprint(key, body) {
  let population;
  if (key === 'ping_stats') population = body.stats;
  else if (key === 'ping' || key === 'chart') population = (body.series || []).map(series => ({
    metric: series.metric, agentId: series.agentId, tags: series.tags, queryPoints: series.queryPoints,
  }));
  else return null; // Rolling billing/traffic endpoints have no fixed-time input.
  return crypto.createHash('sha256').update(JSON.stringify(canonical(population))).digest('hex');
}

function percentile(values, quantile) {
  if (!values.length || values.some(value => !Number.isFinite(value) || value <= 0)) throw Error('Missing or invalid foreground latency samples');
  return [...values].sort((a, b) => a - b)[Math.min(values.length - 1, Math.floor(values.length * quantile))];
}

function comparePerformancePairs(pairs) {
  if (!pairs.length) throw Error('No contemporaneous export-off/export-on measurement pairs');
  const keys = ['ping', 'ping_stats', 'chart', 'traffic', 'billing'];
  const result = {};
  for (const key of keys) {
    for (const pair of pairs) {
      if (pair.baseline[key]?.length !== 40 || pair.during[key]?.length !== 40) throw Error(`Incomplete paired samples: ${key}`);
    }
    const baseline = pairs.flatMap(pair => pair.baseline[key]), during = pairs.flatMap(pair => pair.during[key]);
    const before = percentile(baseline, .95), after = percentile(during, .95);
    result[key] = { baseline_p95_ms: before, during_p95_ms: after, ratio: after / before,
      p99_ms: percentile(during, .99), requests: during.length, pairs: pairs.length };
  }
  return result;
}

module.exports = { pairedWindow, populationFingerprint, comparePerformancePairs };
