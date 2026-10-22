const assert = require('node:assert/strict');
const test = require('node:test');
const { MANUAL_MARKER, shouldPreserveReleaseBody } = require('./release-notes-body.cjs');

test('new releases containing only version metadata can receive generated notes', () => {
  for (const body of [undefined, '', '  ', '<!-- komari-version-hash: abc1234 -->\n']) {
    assert.equal(shouldPreserveReleaseBody(body), false);
  }
});

test('rebuilds preserve existing release descriptions by default', () => {
  for (const body of ['## 更新说明\n\n- 修复登录。', '<!-- komari-version-hash: abc1234 -->\n\nCustom notes']) {
    assert.equal(shouldPreserveReleaseBody(body), true);
    assert.equal(shouldPreserveReleaseBody(body, true), false);
  }
});

test('explicitly protected manual notes survive even an overwrite request', () => {
  const body = `${MANUAL_MARKER}\n\n## 2.4.4\n\n人工维护的更新说明。`;
  assert.equal(shouldPreserveReleaseBody(body), true);
  assert.equal(shouldPreserveReleaseBody(body, true), true);
});

test('rechecking the latest body protects edits made while generation was running', () => {
  const initialBody = '<!-- komari-version-hash: abc1234 -->';
  assert.equal(shouldPreserveReleaseBody(initialBody), false);
  assert.equal(shouldPreserveReleaseBody(`${initialBody}\n\nNewly saved release notes`), true);
});
