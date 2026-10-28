#!/usr/bin/env python3
"""Gate the exact Web gitlink on all independent Web verification workflows."""
import json, os, pathlib, subprocess, sys, time
required = ['development.yaml', 'codeql.yml', 'privileged-delivery.yml',
 'terminal-workbench.yml', 'admin-ui.yml', 'lite-alerts.yml', 'lite-billing.yml',
 'native-otp.yml', 'native-password-login.yml', 'trusted-actions.yml', 'workspace-gate.yml']
sha=subprocess.check_output(['git','-C','third_party/komari-web','rev-parse','HEAD'],text=True).strip()
out=pathlib.Path('ci-artifacts/web-workflow-evidence.json');out.parent.mkdir(exist_ok=True)
evidence=[]
for workflow in required:
 raw=subprocess.check_output(['gh','api',f'repos/r11234567/komari-web/actions/workflows/{workflow}/runs?per_page=50'],text=True)
 candidates=[r for r in json.loads(raw)['workflow_runs'] if r['head_sha']==sha and r['event'] in ['push','pull_request','workflow_dispatch']]
 latest=max(candidates,key=lambda r:r['id']) if candidates else None
 evidence.append({'workflow':workflow,'sha':sha,'run':None if latest is None else {k:latest[k] for k in ['id','status','conclusion','html_url','run_attempt']}})
result={'web_sha':sha,'workflows':evidence}
out.write_text(json.dumps(result,indent=2)+'\n');print(json.dumps(result,indent=2))
if any(e['run'] is None or e['run']['status']!='completed' or e['run']['conclusion']!='success' for e in evidence):
 sys.exit('Exact Web commit must pass every Web workflow before backend integration')
