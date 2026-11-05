const test = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync, mkdtempSync, writeFileSync, mkdirSync, rmSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { join } = require('node:path');
const { spawnSync } = require('node:child_process');

const workflow = readFileSync(join(__dirname, '../workflows/release.yml'), 'utf8');
function step(name) {
  return workflow.split(`      - name: ${name}\n`)[1].split('        run: |\n')[1]
    .split('\n      - name:')[0].split('\n  build-')[0]
    .split('\n').map(line => line.startsWith('          ') ? line.slice(10) : line).join('\n');
}
function run(name, env) {
  const dir = mkdtempSync(join(tmpdir(), 'komari-prerelease-'));
  const output = join(dir, 'outputs').replaceAll('\\', '/');
  try {
    mkdirSync(join(dir, 'bin'));
    writeFileSync(join(dir, 'bin/gh'), '#!/bin/bash\nif [ "$STUB_API_FAIL" = true ]; then exit 42; fi\nprintf "%s\\n" "$STUB_PRERELEASE"\n', { mode: 0o755 });
    writeFileSync(output, '');
    const result = spawnSync(process.env.KOMARI_TEST_BASH || 'bash', ['--noprofile', '--norc', '-c', 'export PATH="$(pwd)/bin:$PATH"\n' + step(name)], {
      cwd: dir, encoding: 'utf8', env: { ...process.env,
        GITHUB_OUTPUT: output, GITHUB_REPOSITORY: 'example/komari',
        EVENT_NAME: 'release', RELEASE_TAG: '2.4.7-dev.20261103.1',
        RELEASE_BODY: 'komari-version-hash: abc1234', INPUT_TAG: '',
        INPUT_VERSION_HASH: '', INPUT_WEB_REF: '', GITHUB_SHA: 'abc1234abcdef',
        STUB_PRERELEASE: 'true', ...env,
      },
    });
    if (result.error) throw result.error;
    return { ...result, outputs: readFileSync(output, 'utf8') };
  } finally { rmSync(dir, { recursive: true, force: true }); }
}

test('dev release retains exact source tag and prerelease metadata', () => {
  const result = run('Validate release metadata', {});
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.outputs, /source_ref=2\.4\.7-dev\.20261103\.1/);
  assert.match(result.outputs, /prerelease=true/);
});
test('stable release remains supported and dev cannot be marked stable', () => {
  assert.equal(run('Validate release metadata', { RELEASE_TAG: '2.4.7', STUB_PRERELEASE: 'false' }).status, 0);
  assert.notEqual(run('Validate release metadata', { STUB_PRERELEASE: 'false' }).status, 0);
});
test('manual dispatch consults actual release metadata and keeps explicit Web ref', () => {
  const result = run('Validate release metadata', { EVENT_NAME: 'workflow_dispatch', INPUT_TAG: '2.4.7-dev.20261103.1', INPUT_VERSION_HASH: 'abc1234', INPUT_WEB_REF: 'exact-web-sha' });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.outputs, /prerelease=true/);
  assert.match(result.outputs, /web_ref=exact-web-sha/);
  assert.notEqual(run('Validate release metadata', { EVENT_NAME: 'workflow_dispatch', INPUT_TAG: '2.4.7-dev.20261103.1', STUB_PRERELEASE: 'false' }).status, 0);
});
test('invalid tags and failed or malformed release lookup fail closed', () => {
  for (const RELEASE_TAG of ['main', '2.4.7-dev', '2.4.7;echo bad', '2.4.7-rc.1']) {
    assert.notEqual(run('Validate release metadata', { RELEASE_TAG }).status, 0);
  }
  assert.notEqual(run('Validate release metadata', { STUB_API_FAIL: 'true' }).status, 0);
  assert.notEqual(run('Validate release metadata', { STUB_PRERELEASE: 'null' }).status, 0);
});
test('prerelease never promotes Docker latest, even when latest API is unavailable', () => {
  const result = run('Check whether this tag should become Docker latest', { TAG: '2.4.7-dev.20261103.1', PRERELEASE: 'true', STUB_API_FAIL: 'true' });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.outputs, /^is_latest=false\s*$/);
});
