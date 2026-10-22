// The gitlink is the only protocol revision. No version or expected SHA here.
const assert = require('node:assert/strict');
const { execFileSync } = require('node:child_process');
const { existsSync, readFileSync, realpathSync } = require('node:fs');
const { resolve } = require('node:path');
const MODULE = 'github.com/r11234567/komari-proto';
const SUBMODULE = 'third_party/komari-proto';
const URL = `https://${MODULE}.git`;
const git = (root, ...args) => execFileSync('git', ['-C', root, ...args], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim();
const json = path => JSON.parse(readFileSync(path, 'utf8'));

function verifyGoBinding(declared, resolved) {
  const requirement = declared.Require?.find(entry => entry.Path === MODULE);
  assert.equal(requirement?.Version, 'v0.0.0', 'use a neutral Go requirement; the gitlink pins the protocol revision');
  assert.ok(!requirement.Indirect, 'protocol must be a direct Go requirement');
  const replacements = declared.Replace?.filter(entry => entry.Old.Path === MODULE) || [];
  assert.equal(replacements.length, 1, 'protocol needs exactly one local Go replace');
  assert.ok(!replacements[0].Old.Version, 'protocol replace must apply to every version');
  assert.equal(replacements[0].New.Path, `./${SUBMODULE}`, 'Go must use the protocol submodule');
  assert.ok(!replacements[0].New.Version, 'local protocol replace must not carry a module version');
  assert.equal(resolved.Path, MODULE);
  assert.equal(resolved.Version, 'v0.0.0');
  assert.equal(resolved.Replace?.Path, `./${SUBMODULE}`, 'Go resolved a different protocol source');
}

function checkProtocol(root, resolveGo = false) {
  root = resolve(root);
  const submodule = resolve(root, SUBMODULE);
  assert.ok(existsSync(resolve(submodule, '.git')), 'protocol submodule is missing; run git submodule update --init --recursive');
  assert.equal(realpathSync(git(submodule, 'rev-parse', '--show-toplevel')), realpathSync(submodule), 'protocol directory is not its own Git repository');
  const indexed = git(root, 'ls-files', '--stage', '--', SUBMODULE).match(/^160000 ([a-f0-9]{40}) 0\tthird_party\/komari-proto$/);
  assert.ok(indexed, 'protocol must be an unconflicted Git submodule in the index');
  assert.equal(git(root, 'config', '-f', '.gitmodules', '--get', `submodule.${SUBMODULE}.url`), URL, 'unexpected protocol repository');
  const actual = git(submodule, 'rev-parse', 'HEAD');
  assert.equal(actual, indexed[1], 'protocol checkout differs from the gitlink; run git submodule update --init --recursive');
  assert.equal(git(submodule, 'status', '--porcelain', '--untracked-files=no'), '', 'protocol has uncommitted tracked changes');
  assert.match(readFileSync(resolve(submodule, 'go.mod'), 'utf8'), /^module github\.com\/r11234567\/komari-proto\s*$/m);
  const protocolPackage = json(resolve(submodule, 'package.json'));
  assert.equal(protocolPackage.name, '@komari/proto');

  if (existsSync(resolve(root, 'go.mod'))) {
    const manifest = readFileSync(resolve(root, 'go.mod'), 'utf8');
    assert.match(manifest, /^\s*github\.com\/r11234567\/komari-proto v0\.0\.0\s*$/m, 'Go protocol requirement must use the neutral v0.0.0 placeholder');
    assert.match(manifest, /^replace github\.com\/r11234567\/komari-proto => \.\/third_party\/komari-proto\s*$/m, 'Go protocol replace must point at the submodule');
    assert.doesNotMatch(readFileSync(resolve(root, 'go.sum'), 'utf8'), /^github\.com\/r11234567\/komari-proto /m, 'remove obsolete remote protocol checksums');
    if (resolveGo) {
      const goJSON = args => JSON.parse(execFileSync('go', args, { cwd: root, env: { ...process.env, GOWORK: 'off' }, encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'] }));
      verifyGoBinding(goJSON(['mod', 'edit', '-json']), goJSON(['list', '-mod=readonly', '-m', '-json', MODULE]));
    }
  }
  if (existsSync(resolve(root, 'package.json'))) {
    const declared = json(resolve(root, 'package.json'));
    const lock = json(resolve(root, 'package-lock.json'));
    assert.equal(declared.dependencies?.['@komari/proto'], `file:${SUBMODULE}`, 'npm must use the local protocol package');
    assert.equal(lock.packages?.['']?.dependencies?.['@komari/proto'], `file:${SUBMODULE}`, 'npm lockfile still uses a remote protocol dependency');
    assert.equal(lock.packages?.['node_modules/@komari/proto']?.link, true, 'npm protocol dependency must be a local link');
    assert.equal(lock.packages?.['node_modules/@komari/proto']?.resolved, SUBMODULE);
    assert.equal(lock.packages?.[SUBMODULE]?.version, protocolPackage.version, 'refresh local npm metadata after bumping the submodule');
    assert.deepEqual(lock.packages?.[SUBMODULE]?.peerDependencies, protocolPackage.peerDependencies, 'refresh protocol peer dependency metadata');
  }
  return actual;
}

if (require.main === module) {
  try { console.log(`${SUBMODULE} ${checkProtocol(resolve(__dirname, '../..'), process.argv.includes('--resolve-go'))}`); }
  catch (error) { console.error(`Protocol dependency check failed: ${error.message}`); process.exitCode = 1; }
}
module.exports = { MODULE, SUBMODULE, URL, git, verifyGoBinding, checkProtocol };
