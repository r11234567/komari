// Independent raw-observation reference: no storage/query implementation import.
function aggregateObservations(observations, start, end, intervalSeconds, ping = false) {
  const buckets = new Map(), lo = new Date(start).getTime(), hi = new Date(end).getTime();
  for (const p of observations) {
    const at = new Date(p.timestamp).getTime();
    if (at < lo || at > hi) continue;
    const key = Math.floor(at / (intervalSeconds * 1000)) * intervalSeconds * 1000;
    const s = buckets.get(key) || { bucket: new Date(key).toISOString(), count: 0, valid: 0, sum: 0 };
    s.count++;
    if (!ping || p.value >= 0) { s.valid++; s.sum += p.value; }
    buckets.set(key, s);
  }
  return [...buckets.entries()].sort((a,b) => a[0]-b[0]).map(([,s]) => ({ ...s, value: s.valid ? s.sum/s.valid : 0 }));
}
module.exports = { aggregateObservations };
