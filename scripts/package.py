#!/usr/bin/env python3
"""Produce a source-only ZIP, never a runtime/database/credential bundle."""
import argparse, hashlib, json, pathlib, stat, zipfile
root=pathlib.Path(__file__).resolve().parents[1]
p=argparse.ArgumentParser();p.add_argument('output',type=pathlib.Path);args=p.parse_args()
allowed={'README.md','.gitignore','.dockerignore','Dockerfile','go.mod','go.sum','sqlc.yaml','cmd','internal','db','web','converters','deploy','docs','tests','scripts','evidence','examples'}
blocked={'node_modules','__pycache__','.git','secrets','generated','dist','test-results','dumps','.venv','models','data'}
runtime_names={'credentials.json','service-credentials.json','demo-state.json','seed-journal.json','preflight-journal.json','readiness-result.json','demo-result.json','native-preflight-result.json','preflight-result.json','.deployment-state.json','.launcher.lock','.seed.lock'}
files=[]
for f in sorted(root.rglob('*')):
 rel=f.relative_to(root)
 if not f.is_file() or f.is_symlink() or rel.parts[0] not in allowed:continue
 if any(x in blocked or x.startswith('.venv') for x in rel.parts):continue
 if any(x.startswith('_') for x in rel.parts[:-1]):continue
 if f.name=='.env' or f.name in runtime_names or f.suffix in {'.pyc','.zip','.db','.pem','.key','.crt','.secret'}:continue
 if f.stat().st_size>10*1024*1024:raise SystemExit(f'Refusing unexpectedly large source file: {rel}')
 data=f.read_bytes()
 if b'-----BEGIN '+b'PRIVATE KEY-----' in data or b'-----BEGIN '+b'RSA PRIVATE KEY-----' in data:raise SystemExit(f'Refusing embedded private key: {rel}')
 files.append((rel.as_posix(),data,f.stat().st_mode))
args.output.parent.mkdir(parents=True,exist_ok=True)
if args.output.exists():raise SystemExit('Refusing to overwrite existing delivery ZIP; choose a fresh filename')
manifest={n:hashlib.sha256(b).hexdigest() for n,b,_ in files}
with zipfile.ZipFile(args.output,'w',compression=zipfile.ZIP_DEFLATED,compresslevel=9) as z:
 for n,b,mode in files:
  zi=zipfile.ZipInfo('agent-clouddrive/'+n,date_time=(2026,10,23,0,0,0));zi.compress_type=zipfile.ZIP_DEFLATED;zi.external_attr=(stat.S_IFREG|(0o755 if mode&0o111 else 0o644))<<16;z.writestr(zi,b)
 z.writestr('agent-clouddrive/SHA256SUMS.json',json.dumps(manifest,indent=2,sort_keys=True)+'\n')
with zipfile.ZipFile(args.output) as z:
 if z.testzip():raise SystemExit('ZIP integrity check failed')
print(json.dumps({'path':str(args.output.resolve()),'files':len(files)+1,'bytes':args.output.stat().st_size,'sha256':hashlib.sha256(args.output.read_bytes()).hexdigest()},indent=2))
