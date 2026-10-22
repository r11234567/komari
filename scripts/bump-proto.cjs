// Stage a reviewed gitlink bump; the caller reviews and commits the result.
const assert = require('node:assert/strict');
const { execFileSync } = require('node:child_process');
const { existsSync } = require('node:fs');
const { resolve } = require('node:path');
const { SUBMODULE, checkProtocol } = require('./ci/check-protocol.cjs');

function bump(root, sha) {
  assert.match(sha || '', /^[a-f0-9]{40}$/, 'Usage: node scripts/bump-proto.cjs <reviewed-full-commit-sha>');
  const previous = checkProtocol(root);
  const run = (command, args, cwd = root) => execFileSync(command, args, { cwd, stdio: 'inherit' });
  const submodule = resolve(root, SUBMODULE);
  run('git', ['fetch', '--depth=1', 'origin', sha], submodule);
  run('git', ['checkout', '--detach', sha], submodule);
  const staged = [SUBMODULE];
  if (existsSync(resolve(root, 'package.json'))) {
    // npm.cmd needs a shell on Windows; all arguments are fixed strings.
    const args = ['install', '--package-lock-only', '--ignore-scripts', '--no-audit', '--no-fund'];
    if (process.platform === 'win32') execFileSync('npm.cmd', args, { cwd: root, stdio: 'inherit', shell: true });
    else run('npm', args);
    staged.push('package-lock.json');
  }
  run('git', ['add', '--', ...staged]);
  checkProtocol(root);
  run('git', ['diff', '--cached', '--stat', '--', ...staged]);
  console.log(`Protocol: ${previous} -> ${sha}\nReview the staged bump, then git commit -m "chore(proto): bump to ${sha.slice(0, 12)}"`);
}
if (require.main === module) {
  try { bump(resolve(__dirname, '..'), process.argv[2]); }
  catch (error) { console.error(error.message); process.exitCode = 1; }
}
module.exports = { bump };
