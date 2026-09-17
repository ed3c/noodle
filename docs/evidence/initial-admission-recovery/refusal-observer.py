"""External release-CLI controls. Every fixture is isolated and explicitly synthetic."""
from pathlib import Path
import argparse,hashlib,json,os,shutil,signal,subprocess,sys,tempfile,time
p=argparse.ArgumentParser();p.add_argument('binary');p.add_argument('output');a=p.parse_args();binary=str(Path(a.binary).resolve());r=Path(__file__).parent;out=Path(a.output);out.mkdir(parents=True,exist_ok=False)
cases=[]
for name in ['wrong-digest','wrong-revision','admitted-ledger','valid-proposal','live-session','orphan-live-group']:
 with tempfile.TemporaryDirectory(prefix='noodle84-negative-') as scratch:
  root=Path(scratch)/'control';shutil.copytree(r/'preserved-soodles-input',root);runtime=root/'.noodle';(root/'.noodle.toml').write_text('mode="manual"\n[server]\nenabled=false\n')
  state=json.loads((runtime/'state.snapshot.json').read_text());proposal=json.loads((runtime/'orders-next.json').read_text());subject=proposal['orders'][0]['id'];revision=state['order_revision'];proc=None
  if name=='admitted-ledger':
   effect='initial-admission-'+hashlib.sha256(subject.encode()).hexdigest()
   state['effect_ledger'].append({'effect_id':effect,'effect':{'effect_id':effect,'type':'initial_admission','payload':{'order_id':subject,'initial_revision':proposal['initial_revision']},'created_at':'2026-09-17T00:00:00Z'},'status':'done','attempts':1,'result':{'effect_id':effect,'status':'completed','error':'','timestamp':'2026-09-17T00:00:00Z'}})
   (runtime/'state.snapshot.json').write_text(json.dumps(state))
  if name=='valid-proposal':
   proposal['initial_revision']=revision;(runtime/'orders-next.json').write_text(json.dumps(proposal))
  if name in ['live-session','orphan-live-group']:
   if name=='live-session': code='import time; time.sleep(60)'
   else: code='import os,time; child=os.fork(); time.sleep(60) if child==0 else None'
   proc=subprocess.Popen([sys.executable,'-c',code],start_new_session=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
   if name=='orphan-live-group':proc.wait(timeout=5)
   session='external-live-guard';folder=runtime/'sessions'/session;folder.mkdir()
   (folder/'process.json').write_text(json.dumps({'pid':proc.pid,'session_id':session}))
   (folder/'meta.json').write_text(json.dumps({'status':'exited','alive':False,'runtime':'process','session_id':session}))
  mailbox=(runtime/'orders-next.json').read_bytes();before=(runtime/'state.snapshot.json').read_bytes();digest=hashlib.sha256(mailbox).hexdigest()
  if name=='wrong-digest':digest='0'*64
  if name=='wrong-revision':revision='0'*32
  argv=[binary,'--project-dir',str(root),'admission','retire',digest,revision]
  try:
   q=subprocess.run(argv,cwd=root,capture_output=True,text=True,timeout=15)
   try:result=json.loads(q.stdout)
   except ValueError:result={}
   preserved=(runtime/'orders-next.json').exists() and (runtime/'orders-next.json').read_bytes()==mailbox and (runtime/'state.snapshot.json').read_bytes()==before
   expected={'wrong-digest':'proposal_sha256 changed','wrong-revision':'current_order_revision changed','admitted-ledger':'owned, admitted','valid-proposal':'currently valid','live-session':'process or process group is present','orphan-live-group':'process or process group is present'}[name]
   passed=q.returncode!=0 and result.get('status')=='refused' and preserved and expected in result.get('invalid','')
   cases.append({'case':name,'argv':argv,'exit':q.returncode,'stdout':q.stdout,'stderr':q.stderr,'preserved':preserved,'passed':passed})
  finally:
   if proc:
    try:os.killpg(proc.pid,signal.SIGTERM)
    except ProcessLookupError:pass
    if proc.poll() is None:proc.wait(timeout=5)
receipt={'scope':'release CLI refusals over synthetic variants of preserved input; no production mutation','binary_sha256':hashlib.sha256(Path(binary).read_bytes()).hexdigest(),'observer_sha256':hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),'cases':cases,'passed':all(x['passed'] for x in cases),'authorizes_landing':False}
(out/'receipt.json').write_text(json.dumps(receipt,indent=2)+'\n');print(json.dumps({'passed':receipt['passed'],'cases':[{k:v for k,v in c.items() if k in ['case','passed','exit']} for c in cases]}));raise SystemExit(not receipt['passed'])
