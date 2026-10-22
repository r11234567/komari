const assert = require('node:assert/strict');
const test = require('node:test');
const { readFileSync, mkdtempSync, writeFileSync, rmSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { join, resolve } = require('node:path');
const { spawnSync } = require('node:child_process');

// Execute the actual workflow shell with a stub GitHub CLI, so this regression
// tests the dispatched arguments and exit handling without API mutations.
const workflow = readFileSync(resolve(__dirname, '../workflows/rebuild-release.yml'), 'utf8');
const step = workflow.split('      - name: Dispatch unified release build\n')[1];
const shell = step.split('        run: |\n')[1].split('\n      - name:')[0]
  .split('\n').map(line => line.startsWith('          ') ? line.slice(10) : line).join('\n');

for (const status of [0, 42]) {
  test(`rebuild dispatch uses the moved version tag and retains exit ${status}`, () => {
    const dir = mkdtempSync(join(tmpdir(), 'komari-dispatch-'));
    try {
      writeFileSync(join(dir, 'gh'), '#!/bin/bash\nprintf "%s\\n" "$@" > "$CAPTURE"\nexit "$STUB_EXIT"\n', { mode: 0o755 });
      const source = '68ec15e7e37fa387926f2185e17994516fe2630c';
      const frontend = 'c8791f98749114941e744f336f075d6b0685a7c3';
      const result = spawnSync('bash', ['-c', shell], { cwd: dir, encoding: 'utf8', env: {
        ...process.env, PATH: `${dir}:${process.env.PATH}`, CAPTURE: join(dir, 'args'),
        STUB_EXIT: String(status), GITHUB_REPOSITORY: 'r11234567/komari',
        VERSION: '2.4.4', VERSION_HASH: '68ec15e', SERVER_REF: source, FRONTEND_REF: frontend,
      }});
      assert.equal(result.status, status, result.stderr);
      const args = readFileSync(join(dir, 'args'), 'utf8').trim().split('\n');
      assert.deepEqual(args, ['workflow', 'run', 'release.yml', '--repo', 'r11234567/komari',
        '--ref', '2.4.4', '-f', 'tag=2.4.4', '-f', 'version_hash=68ec15e', '-f', `web_ref=${frontend}`]);
      assert.equal(args.includes(source), false);
      assert.equal(readFileSync(join(dir, 'ci-artifacts/release-dispatch-exit.txt'), 'utf8').trim(), String(status));
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
}
