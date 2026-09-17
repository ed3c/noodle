"""Frozen external CLI consumer over an explicitly copied production failure.

One entry, then consume owner next argv. Does not modify the original root.
"""
from pathlib import Path
import argparse,base64,hashlib,json,os,shutil,subprocess,tempfile,time
p=argparse.ArgumentParser();p.add_argument('binary');p.add_argument('output');a=p.parse_args()
r=Path(__file__).parent;binary=Path(a.binary).resolve();output=Path(a.output);output.mkdir(parents=True,exist_ok=False)
selection=json.loads((r/'preserved-input-selection.json').read_text());source=r/'preserved-soodles-input'
for name,digest in selection['files'].items():assert hashlib.sha256((source/name).read_bytes()).hexdigest()==digest
records=[];errors=[]
def invoke(root,argv):
    q=subprocess.run(argv,cwd=root,capture_output=True,text=True,timeout=20)
    record={'argv':argv,'exit':q.returncode,'stdout':q.stdout,'stderr':q.stderr};records.append(record)
    try:record['json']=json.loads(q.stdout)
    except ValueError:record['json']=None
    return record
with tempfile.TemporaryDirectory(prefix='noodle84-external-') as scratch:
    root=Path(scratch)/'control';shutil.copytree(source,root)
    (root/'.noodle.toml').write_text('mode="manual"\n[server]\nenabled=false\n')
    runtime=root/'.noodle';proposal=(runtime/'orders-next.json').read_bytes();before=(runtime/'state.snapshot.json').read_bytes();orders=(runtime/'orders.json').read_bytes()
    try:
        first=invoke(root,[str(binary),'--project-dir',str(root),'admission','inspect'])
        assert first['exit']==0 and isinstance(first['json'],dict), 'inspection did not return structured continuation'
        assert first['json'].get('status')=='recoverable', 'preserved unpromoted rejected input lacks recoverable classification'
        next_action=first['json'].get('next');assert isinstance(next_action,dict) and isinstance(next_action.get('argv'),list),'owner supplied no exact argv'
        argv=next_action['argv'];assert argv and Path(argv[0]).resolve()==binary,'next changed selected owner binary'
        retired=invoke(root,argv)
        assert retired['exit']==0 and retired['json'].get('status')=='retired','exact owner operation did not retire'
        assert not (runtime/'orders-next.json').exists(),'retired mailbox remains'
        def contains_original(value):
            if isinstance(value, dict): return any(contains_original(v) for v in value.values())
            if isinstance(value, list): return any(contains_original(v) for v in value)
            if isinstance(value, str):
                if value.encode() == proposal: return True
                try: return base64.b64decode(value, validate=True) == proposal
                except (ValueError, UnicodeError): return False
            return False
        preserved = False
        for archived in runtime.rglob('*'):
            if not archived.is_file(): continue
            raw = archived.read_bytes()
            if raw == proposal: preserved = True; break
            try: preserved = preserved or contains_original(json.loads(raw))
            except (ValueError, UnicodeError): pass
        assert preserved, 'original proposal bytes not preserved exactly in raw or lossless encoded evidence' 
        assert (runtime/'state.snapshot.json').read_bytes()==before,'canonical checkpoint changed'
        assert (runtime/'orders.json').read_bytes()==orders,'orders projection changed'
        repeated=invoke(root,argv)
        assert repeated['exit']==0 and repeated['json'].get('status')=='retired','repeat readback failed'
        assert (runtime/'state.snapshot.json').read_bytes()==before,'repeat changed canonical checkpoint'
    except (AssertionError,KeyError,TypeError) as e:errors.append(str(e))
receipt={'classification':'GREEN' if not errors else 'RED','binary':str(binary),'binary_sha256':hashlib.sha256(binary.read_bytes()).hexdigest(),'input_selection_sha256':hashlib.sha256((r/'preserved-input-selection.json').read_bytes()).hexdigest(),'observer_sha256':hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),'records':records,'errors':errors,'scratch_removed':True,'production_mutations':0,'authorizes_landing':False}
(output/'receipt.json').write_text(json.dumps(receipt,indent=2)+'\n');print(json.dumps({'classification':receipt['classification'],'errors':errors,'calls':len(records)}));raise SystemExit(bool(errors))
