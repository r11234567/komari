const { test } = require('node:test');
const assert = require('node:assert/strict');
const { aggregateObservations } = require('./storage-curve-oracle.cjs');
test('raw reference filters bounds, weights valid samples and counts losses', () => {
  const points = [-1,10,20,90].map((value,i) => ({timestamp:new Date(i*10_000).toISOString(), value}));
  assert.deepEqual(aggregateObservations(points,new Date(0),new Date(20_000),60,true), [
    {bucket:new Date(0).toISOString(),count:2,valid:2,sum:30,value:15,attempts:3,loss:1/3},
  ]);
  assert.equal(aggregateObservations(points,new Date(10_000),new Date(20_000),60)[0].value,15);
  assert.equal(aggregateObservations(points,new Date(40_000),new Date(50_000),60).length,0);
});
