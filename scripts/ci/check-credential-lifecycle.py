"""Require observed, non-skipped tests; a green command alone is insufficient."""
import json
from pathlib import Path
root = Path('ci-artifacts')
required = {
 'credential-rotation': [
  'TestCredentialRotationConcurrentSameRequestHasOneResult',
  'TestCredentialRotationRecoversAfterDatabaseReopen',
  'TestCredentialRotationAuthenticatedReuseRevokesChain',
  'TestCredentialRotationRejectsGenerationDowngradeAndChangedKey',
  'TestCredentialRotationRollbackAndExpiredAccessHandoff',
  'TestCredentialRotationLegacyRecoveryDoesNotRotateAgain',
 ],
 'credential-refresh-connect': [
  'TestCredentialRefreshV2SignedFullResponseAndLostResponseRecovery',
  'TestCredentialRefreshV2InvalidProofCannotRevokeOrRotate',
  'TestCredentialRefreshV2RejectsExpiredHandoffAndProtocolDowngrade',
  'TestCredentialRefreshV2RejectsMalformedVersionAndBindings',
  'TestDualEnd_RefreshCredentials_ProofRequiredAndNoDestruction',
  'TestDualEnd_RefreshCredentials_ValidProofRotatesTokens',
 ],
 'credential-rotation-repeat': ['TestCredentialRotationConcurrentSameRequestHasOneResult'] * 20,
}
evidence = {}
for label, names in required.items():
 path = root / (label + '-go') / 'tests.jsonl'
 events = [json.loads(line) for line in path.read_text().splitlines() if line.strip()]
 failures = [e for e in events if e.get('Action') in ('skip', 'fail')]
 assert not failures, (label, failures)
 counts = {}
 for e in events:
  if e.get('Action') == 'pass' and e.get('Test'):
   counts[e['Test']] = counts.get(e['Test'], 0) + 1
 for name in set(names):
  assert counts.get(name, 0) >= names.count(name), (label, name, counts)
 assert (path.parent / 'exit-code.txt').read_text().strip() == '0', label
 evidence[label] = counts
(root / 'credential-stage-one-evidence.json').write_text(json.dumps(evidence, indent=2) + '\n')
print(json.dumps(evidence, indent=2))
