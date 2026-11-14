// Reuse the same fixture/path checks in browser and standalone Node stages.
const { execFileSync } = require('node:child_process');
const path = require('node:path');
execFileSync('bash', [path.join(__dirname, 'guard.sh')], {stdio: 'inherit'});
