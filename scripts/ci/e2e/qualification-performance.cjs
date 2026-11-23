const { isDeepStrictEqual } = require('node:util');

// Use the same one-minute output grid on both sides of the first retention
// transition (10s source cells to 1m cells). Rolling live reads keep their
// original point budget.
const pairedMaxPoints = 60;

function pairedWindow(anchor, index) {
  // Leave five minutes for real reports and summary delivery to settle. Vary
  // each request key, then replay the exact range after the cache TTL expires.
  // Stagger the requested edges so each sample has its own cache key.
  const end = anchor - 300_000 - index * 5_000;
  return { startTime: new Date(end - 3600_000).toISOString(), endTime: new Date(end).toISOString() };
}

function pairedPopulation(key, body) {
  const averages = [];
  const average = value => {
    if (value === undefined) return false;
    averages.push(value);
    return true;
  };
  if (key === 'ping_stats') {
    const { stats, ...window } = body;
    return { exact: { ...window, stats: (stats || []).map(({ average: value, ...stat }) => ({
      ...stat, averaged: average(value),
    })) }, averages };
  }
  if (key === 'ping' || key === 'chart') {
    const exact = (body.series || []).map(series => ({
      metric: series.metric, agentId: series.agentId, labels: series.labels,
      interval: series.interval, coverage: series.coverage,
      coverageStart: series.coverageStart, coverageEnd: series.coverageEnd,
      queryPoints: (series.queryPoints || []).map(({ value, sourceInterval, ...point }) => ({
        ...point, averaged: average(value),
      })),
    }));
    return { exact, averages };
  }
  return null; // Rolling billing/traffic endpoints have no fixed-time input.
}

function samePopulation(before, after) {
  if (before === null || after === null) return before === after;
  if (!isDeepStrictEqual(before.exact, after.exact) || before.averages.length !== after.averages.length) return false;
  return before.averages.every((value, index) => {
    const other = after.averages[index];
    // Promotion can regroup floating-point sums without changing any sample.
    const roundoff = 64 * Number.EPSILON * Math.max(1, Math.abs(value), Math.abs(other));
    return Number.isFinite(value) && Number.isFinite(other) && Math.abs(value - other) <= roundoff;
  });
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

module.exports = { pairedMaxPoints, pairedWindow, pairedPopulation, samePopulation, comparePerformancePairs };
