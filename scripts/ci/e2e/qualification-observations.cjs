const freshnessMilliseconds = 120_000;

function newestObservation(body, metric) {
  const observed = (body.series || [])
    .filter(series => series.metric === metric)
    .flatMap(series => (series.queryPoints || [])
      .filter(point => point.value !== undefined)
      .map(point => Date.parse(point.observedAt)))
    .filter(Number.isFinite);
  return observed.length ? Math.max(...observed) : 0;
}

function agentRecovered(body, startedAt, now) {
  return ['cpu.usage', 'ping.latency_ms'].every(metric => {
    const newest = newestObservation(body, metric);
    return newest > startedAt && newest > now - freshnessMilliseconds;
  });
}

module.exports = { newestObservation, agentRecovered };
