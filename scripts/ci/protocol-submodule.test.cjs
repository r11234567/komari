const { test } = require('node:test');
const assert = require('node:assert/strict');
const { mkdtempSync, mkdirSync, writeFileSync, readFileSync, rmSync, renameSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { join } = require('node:path');
const { MODULE, SUBMODULE, URL, checkProtocol, verifyGoBinding, git } = require('./check-protocol.cjs');
const { bump } = require('../bump-proto.cjs');

function fixture(t) {
  const temp = mkdtempSync(join(tmpdir(), 'komari-protocol-'));
  t.after(() => rmSync(temp, { recursive: true, force: true }));
  const source = join(temp, 'source'), root = join(temp, 'consumer');
  for (const path of [source, root]) {
    mkdirSync(path); git(path, 'init', '-q');
    git(path, 'config', 'user.name', 'Protocol CI');
    git(path, 'config', 'user.email', 'protocol@example.invalid');
    git(path, 'config', 'commit.gpgsign', 'false');
  }
  const pkg = { name: '@komari/proto', version: '0.0.1', peerDependencies: { '@bufbuild/protobuf': '^2.10.0' } };
  writeFileSync(join(source, 'go.mod'), `module ${MODULE}\n\ngo 1.24.0\n`);
  writeFileSync(join(source, 'package.json'), JSON.stringify(pkg));
  git(source, 'add', '.'); git(source, 'commit', '-qm', 'initial protocol');
  git(root, '-c', 'protocol.file.allow=always', 'submodule', 'add', '-q', source, SUBMODULE);
  git(root, 'config', '-f', '.gitmodules', `submodule.${SUBMODULE}.url`, URL);
  writeFileSync(join(root, 'go.mod'), `module example.invalid/consumer\n\nrequire (\n  ${MODULE} v0.0.0\n)\n\nreplace ${MODULE} => ./${SUBMODULE}\n`);
  writeFileSync(join(root, 'go.sum'), '');
  writeFileSync(join(root, 'package.json'), JSON.stringify({ dependencies: { '@komari/proto': `file:${SUBMODULE}` } }));
  writeFileSync(join(root, 'package-lock.json'), JSON.stringify({ packages: {
    '': { dependencies: { '@komari/proto': `file:${SUBMODULE}` } },
    'node_modules/@komari/proto': { resolved: SUBMODULE, link: true },
    [SUBMODULE]: { version: pkg.version, peerDependencies: pkg.peerDependencies },
  } }));
  git(root, 'add', '.'); git(root, 'commit', '-qm', 'pin local protocol');
  return { root, source, submodule: join(root, SUBMODULE) };
}

test('local Go/npm dependencies use only the indexed submodule SHA', t => {
  const { root, submodule } = fixture(t);
  assert.equal(checkProtocol(root), git(submodule, 'rev-parse', 'HEAD'));
  const go = { Require: [{ Path: MODULE, Version: 'v0.0.0' }], Replace: [{ Old: { Path: MODULE }, New: { Path: `./${SUBMODULE}` } }] };
  verifyGoBinding(go, { Path: MODULE, Version: 'v0.0.0', Replace: { Path: `./${SUBMODULE}` } });
  assert.throws(() => verifyGoBinding(go, { Path: MODULE, Version: 'v0.0.0' }), /different protocol source/);
  assert.throws(() => verifyGoBinding({ ...go, Replace: [] }, {}), /exactly one local/);
});
test('missing submodules fail with an initialization command', t => {
  const { root, submodule } = fixture(t);
  renameSync(submodule, `${submodule}.missing`);
  assert.throws(() => checkProtocol(root), /git submodule update --init --recursive/);
});
test('changed checkout or dirty generated files cannot pass', t => {
  const { root, submodule } = fixture(t);
  writeFileSync(join(submodule, 'go.mod'), `module ${MODULE}\n// changed\n`);
  assert.throws(() => checkProtocol(root), /uncommitted tracked changes/);
  git(submodule, '-c', 'user.name=Protocol CI', '-c', 'user.email=protocol@example.invalid', '-c', 'commit.gpgsign=false', 'commit', '-qam', 'reviewed protocol change');
  assert.throws(() => checkProtocol(root), /differs from the gitlink/);
  git(root, 'add', SUBMODULE);
  assert.equal(checkProtocol(root), git(submodule, 'rev-parse', 'HEAD'));
});
test('remote npm locks and stale local metadata fail', t => {
  const { root } = fixture(t), path = join(root, 'package-lock.json');
  const lock = JSON.parse(readFileSync(path, 'utf8'));
  lock.packages['node_modules/@komari/proto'] = { resolved: 'git+ssh://example.invalid/proto#old' };
  writeFileSync(path, JSON.stringify(lock));
  assert.throws(() => checkProtocol(root), /local link/);
  lock.packages['node_modules/@komari/proto'] = { resolved: SUBMODULE, link: true };
  lock.packages[SUBMODULE].version = 'stale'; writeFileSync(path, JSON.stringify(lock));
  assert.throws(() => checkProtocol(root), /refresh local npm metadata/);
});
test('Go consumers reject remote requirements and checksums', t => {
  const { root } = fixture(t);
  writeFileSync(join(root, 'go.sum'), `${MODULE} v1.0.0 h1:old\n`);
  assert.throws(() => checkProtocol(root), /obsolete remote protocol checksums/);
  writeFileSync(join(root, 'go.sum'), '');
  const path = join(root, 'go.mod');
  writeFileSync(path, readFileSync(path, 'utf8').replace('v0.0.0', 'v1.0.0'));
  assert.throws(() => checkProtocol(root), /neutral v0.0.0/);
});

test('bump fetches an exact commit and stages only protocol files', t => {
  const { root, source } = fixture(t);
  rmSync(join(root, 'package.json')); rmSync(join(root, 'package-lock.json'));
  writeFileSync(join(root, 'unrelated.txt'), 'keep this outside the bump');
  writeFileSync(join(source, 'reviewed.proto'), 'syntax = "proto3";\n');
  git(source, 'add', '.'); git(source, 'commit', '-qm', 'next reviewed protocol');
  const next = git(source, 'rev-parse', 'HEAD');
  bump(root, next);
  assert.equal(checkProtocol(root), next);
  assert.equal(git(root, 'diff', '--cached', '--name-only'), SUBMODULE);
  assert.throws(() => bump(root, 'main'), /reviewed-full-commit-sha/);
});
