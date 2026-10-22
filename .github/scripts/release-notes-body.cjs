const MANUAL_MARKER = '<!-- komari-release-notes: manual -->';
const VERSION_MARKER = /<!--\s*komari-version-hash:\s*[A-Za-z0-9]{7}\s*-->/gi;

function shouldPreserveReleaseBody(body, overwriteExisting = false) {
  const text = typeof body === 'string' ? body : '';
  if (text.includes(MANUAL_MARKER)) return true;
  return !overwriteExisting && text.replace(VERSION_MARKER, '').trim().length > 0;
}

module.exports = { MANUAL_MARKER, shouldPreserveReleaseBody };
