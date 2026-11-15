// Decode the shipped browser's binary Connect messages using the pinned
// protobuf runtime. HTTP requests made explicitly by the test still use JSON.
const path = require('node:path');
const { createRequire } = require('node:module');
const webRequire = createRequire(path.resolve(__dirname, '../../../third_party/komari-web/package.json'));
const { BinaryReader } = webRequire('@bufbuild/protobuf/wire');

function fields(bytes, read) {
  const r = new BinaryReader(bytes);
  while (r.pos < r.len) {
    const [field, wire] = r.tag();
    if (!read(r, field, wire)) r.skip(wire);
  }
}
function timestamp(bytes) {
  let seconds = 0, nanos = 0;
  fields(bytes, (r, f) => {
    if (f === 1) { seconds = Number(r.int64()); return true; }
    if (f === 2) { nanos = r.int32(); return true; }
  });
  return new Date(seconds * 1000 + nanos / 1e6).toISOString();
}
function duration(bytes) {
  let seconds = 0, nanos = 0;
  fields(bytes, (r, f) => {
    if (f === 1) { seconds = Number(r.int64()); return true; }
    if (f === 2) { nanos = r.int32(); return true; }
  });
  return `${seconds + nanos / 1e9}s`;
}
function request(bytes) {
  const out = { agentIds: [], metrics: [], maxPoints: 0 };
  fields(bytes, (r, f) => {
    if (f === 1 || f === 2) { out[f === 1 ? 'agentIds' : 'metrics'].push(r.string()); return true; }
    if (f === 3 || f === 4) { out[f === 3 ? 'startTime' : 'endTime'] = timestamp(r.bytes()); return true; }
    if (f === 5) { out.maxPoints = r.uint32(); return true; }
    if (f === 7) { out.downsample = r.bool(); return true; }
    if (f === 8) { out.aggregation = r.string(); return true; }
  });
  return out;
}
function point(bytes) {
  const out = { sampleCount: 0 };
  fields(bytes, (r, f) => {
    if (f === 1) { out.observedAt = timestamp(r.bytes()); return true; }
    if (f === 2) { out.value = r.double(); return true; }
    if (f === 3) { out.sampleCount = r.uint32(); return true; }
    if (f === 5) { out.sourceInterval = duration(r.bytes()); return true; }
    if (f === 6 || f === 7) { out[f === 6 ? 'coverage' : 'coverageReason'] = r.string(); return true; }
  });
  return out;
}
function series(bytes) {
  const out = { queryPoints: [] };
  fields(bytes, (r, f) => {
    if (f === 1 || f === 2) { out[f === 1 ? 'agentId' : 'metric'] = r.string(); return true; }
    if (f === 5) { out.queryPoints.push(point(r.bytes())); return true; }
    if (f === 11) { out.interval = duration(r.bytes()); return true; }
    if (f === 13) { out.coverage = r.string(); return true; }
  });
  return out;
}
function response(bytes) {
  const out = { series: [] };
  fields(bytes, (r, f) => {
    if (f === 1) { out.series.push(series(r.bytes())); return true; }
  });
  return out;
}
module.exports = { decodeMetricsRequest: request, decodeMetricsResponse: response };
