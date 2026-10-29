#!/usr/bin/env python3
from pathlib import Path
import argparse, os, subprocess, tempfile, socket, json, time, signal, threading, sys
parser=argparse.ArgumentParser()
parser.add_argument('--evidence',type=Path,required=True)
parser.add_argument('--old',type=Path,required=True)
parser.add_argument('--new',type=Path,required=True)
parser.add_argument('--paused',action='store_true')
parser.add_argument('--queue',action='store_true')
parser.add_argument('--missing-source',action='store_true')
args=parser.parse_args()
E=args.evidence.resolve();E.mkdir(parents=True,exist_ok=False)
old,new=str(args.old.resolve()),str(args.new.resolve())
p=Path(tempfile.mkdtemp(prefix='nboot-',dir='/tmp')).resolve();(E/'fixture-path.txt').write_text(str(p))
project=p/'repo';project.mkdir();subprocess.run(['git','init',str(project)],capture_output=True,check=True)
subprocess.run(['git','-C',str(project),'-c','user.name=Probe','-c','user.email=probe@invalid','commit','--allow-empty','-m','fixture'],capture_output=True,check=True)
home=p/'home';home.mkdir();host=p/'host';host.mkdir()
env=dict(os.environ,HOME=str(home),NOODLE_HOST_DIR=str(host));env.pop('NOODLE_HOST_POOL_ROOT',None);env.pop('NOODLE_HOST_GENERATION',None)
config=project/'.noodle.toml';config.write_text("mode='manual'\n[server]\nenabled=false\n")
log=(p/'old.log').open('wb');proc=subprocess.Popen([old,'host','serve'],env=env,stdout=log,stderr=log,start_new_session=True)
threading.Thread(target=proc.wait,daemon=True).start()
def rpc(directory,**req):
 s=socket.socket(socket.AF_UNIX);s.settimeout(5);s.connect(str(directory/'host.sock'));s.sendall((json.dumps(req)+'\n').encode());f=s.makefile();r=json.loads(f.readline());s.close();return r
def until(fn,seconds=15):
 end=time.monotonic()+seconds
 while time.monotonic()<end:
  try:
   value=fn()
   if value:return value
  except (OSError,ValueError):pass
  time.sleep(.1)
 raise TimeoutError()
try:
 until(lambda:rpc(host,operation='resources'))
 doctor=subprocess.run([sys.executable,str(Path(__file__).with_name('host_doctor.py')),'--binary',new,'--host-root',str(host)],env=env,capture_output=True,text=True,timeout=20)
 (E/'doctor-empty-legacy.json').write_text(json.dumps({'exit':doctor.returncode,'stdout':doctor.stdout,'stderr':doctor.stderr},indent=2))
 assert doctor.returncode==0,(doctor.stdout,doctor.stderr)
 request=p/'host-request.json'
 a=subprocess.run([old,'--project-dir',str(project),'host','attach','--mode','manual','--request-file',str(request)],env=env,capture_output=True,text=True,timeout=15)
 (E/'fixture-attach.json').write_text(a.stdout);assert a.returncode==0,(a.stdout,a.stderr)
 receipt=json.loads(a.stdout);assert receipt['status']=='ok',receipt
 runtime=project/'.noodle';(runtime/'sentinel.txt').write_text('original runtime bytes')
 if '--paused' in sys.argv:
  commands=[{'id':'fixture-pause','action':'pause'},{'id':'fixture-mode','action':'mode','value':'supervised'}]
  (runtime/'control.ndjson').write_text(''.join(json.dumps(c)+'\n' for c in commands))
  until(lambda:(runtime/'control-ack.ndjson').exists() and 'fixture-mode' in (runtime/'control-ack.ndjson').read_text())
 if '--missing-source' in sys.argv:
  (project/'.git').rename(p/'retained-git');config.unlink();(runtime/'status.json').unlink(missing_ok=True)
  (project/'.worktrees/original').mkdir(parents=True);(project/'.worktrees/original/dirty').write_text('uncommitted bytes')
 switch_process=subprocess.Popen([new,'host','switch','--binary',new,'--initial-maintenance','--request-file',str(request)],env=env,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
 queued_request=None
 if '--queue' in sys.argv:
  qproject=p/'queued';qproject.mkdir();subprocess.run(['git','init',str(qproject)],capture_output=True,check=True);(qproject/'.noodle.toml').write_text("mode='manual'\n[server]\nenabled=false\n")
  queued_request=p/'queued-request.json'
  until(lambda:(host/'bootstrap.json').exists() and json.loads((host/'bootstrap.json').read_text())['phase']=='draining')
  queued=subprocess.run([old,'--project-dir',str(qproject),'host','attach','--mode','manual','--request-file',str(queued_request)],env=env,capture_output=True,text=True,timeout=10)
  assert json.loads(queued.stdout)['status']=='busy',(queued.stdout,queued.stderr)
  queued_before=json.loads(queued_request.read_text())
 stdout,stderr=switch_process.communicate(timeout=40);switch=subprocess.CompletedProcess(switch_process.args,switch_process.returncode,stdout,stderr)
 (E/'fixture-switch.stdout').write_text(switch.stdout);(E/'fixture-switch.stderr').write_text(switch.stderr)
 print(json.dumps({'path':str(p),'exit':switch.returncode,'phase':json.loads((host/'bootstrap.json').read_text())['phase'],'stderr':switch.stderr[-1200:]}))
 if switch.returncode:raise SystemExit(1)
 b=json.loads((host/'bootstrap.json').read_text());assert b['phase']=='active',b
 pool=json.loads((host/'pool.json').read_text());owner=pool['owners'][receipt['request_id']];assert (owner['current']['host_instance_id']!=receipt['host_instance_id']) if '--missing-source' not in sys.argv else bool(owner['rebuild_error']);assert owner['round_state']==({'mode':'supervised','dispatch_state':'paused'} if '--paused' in sys.argv else {'mode':'manual','dispatch_state':'running'}),owner
 assert (project/'.noodle/sentinel.txt').read_text()=='original runtime bytes'
 assert (project/'.noodle').is_symlink()
 if proc is not None:proc.wait(timeout=10)
 resources=rpc(host,operation='resources');assert resources['rolling_host_version']==1 and resources['resources']['active_workers']==0,resources
 if queued_request is not None:
  accepted=subprocess.run([new,'--project-dir',str(qproject),'host','attach','--mode','manual','--request-file',str(queued_request)],env=env,capture_output=True,text=True,timeout=15)
  assert accepted.returncode==0,(accepted.stdout,accepted.stderr)
  qreceipt=json.loads(accepted.stdout);queued_after=json.loads(queued_request.read_text())
  assert queued_after['request_id']==queued_before['request_id'] and queued_after['binary_sha256']==queued_before['binary_sha256'] and qreceipt['binary_path']==new
  qdetach=subprocess.run([new,'--project-dir',str(qproject),'host','detach','--request-file',str(queued_request),'--context-id',qreceipt['context_id'],'--instance-id',qreceipt['host_instance_id']],env=env,capture_output=True,text=True,timeout=15)
  assert qdetach.returncode==0,(qdetach.stdout,qdetach.stderr)
  (E/'fixture-queue-pass.json').write_text(json.dumps({'before':queued_before,'after':queued_after,'receipt':qreceipt},indent=2))
 # Restore the original public entry from the completed maintenance record.
 (host/'host.sock').unlink();(host/'host.sock').symlink_to(host/'maintenance.sock')
 repeat=subprocess.run([new,'host','switch','--binary',new,'--initial-maintenance','--request-file',str(request)],env=env,capture_output=True,text=True,timeout=10)
 assert repeat.returncode==0,repeat.stderr
 assert json.loads((host/'pool.json').read_text())['owners'][receipt['request_id']]['current']['context_id']==owner['current']['context_id']
 status=subprocess.run([new,'--project-dir',str(project),'host','status','--request-file',str(request)],env=env,capture_output=True,text=True,timeout=10)
 assert status.returncode==0,(status.stdout,status.stderr)
 assert json.loads(status.stdout)['context_id']==owner['current']['context_id']
 (E/'bootstrap.json').write_bytes((host/'bootstrap.json').read_bytes())
 (E/'pool.json').write_bytes((host/'pool.json').read_bytes())
 drive_result={'status':'PASS','original':receipt,'current':owner['current'],'resources':resources,'same_context_after_socket_repair':True}
finally:
 try:
  # Preserve every completed action before touching owned residue.
  for name in ('bootstrap.json','pool.json'):
   if (host/name).exists():(E/('final-'+name)).write_bytes((host/name).read_bytes())
  if (host/'pool.json').exists():
   pstate=json.loads((host/'pool.json').read_bytes())
   for owner in pstate['owners'].values():
    current=owner['current']
    if current['context_state']=='active':
     g=pstate['generations'][current['host_instance_id']]
     reply=rpc(Path(g['directory']),operation='detach',project_dir=current['project_dir'],context_id=current['context_id'],instance_id=g['id'],binary_sha256=g['binary_sha256'])
     assert reply['status']=='ok',reply
   g=pstate['generations'][pstate['active']]
   r=rpc(Path(g['directory']),operation='resources')
   assert r['contexts']==0 and r['resources']['active_allocations']==0 and r['resources']['active_workers']==0 and r['resources']['active_residents']==0 and r['resources']['unresolved_residents']==0 and not r['residents'].get('error') and not r['resources'].get('shared_quota',{}).get('error'),r
   pid=r['host_pid']
   command=subprocess.check_output(['/bin/ps','-p',str(pid),'-o','command='],text=True).strip()
   assert command==new+' host serve',command
   (E/'cleanup-before.json').write_text(json.dumps({'resources':r,'command':command},indent=2))
   os.kill(pid,signal.SIGTERM)
   until(lambda:subprocess.run(['/bin/ps','-p',str(pid)],capture_output=True).returncode!=0)
   (E/'cleanup.json').write_text(json.dumps({'native_detach':True,'empty_fixture_host_exit':True,'evidence_survives':(E/'fixture-switch.stdout').exists()},indent=2))
  else:
   (E/'cleanup-pending.json').write_text(json.dumps({'reason':'Original maintenance not complete; retain original readback, no signal sent','host':str(host)}))
 except Exception as error:
  (E/'cleanup-pending.json').write_text(json.dumps({'reason':str(error),'host':str(host)}))

if not (E/'cleanup.json').exists():
 raise RuntimeError('Legacy drive cleanup incomplete; retain original readback and report blocked')
(E/'result.json').write_text(json.dumps(drive_result,indent=2))
print(json.dumps({'stage':'passed','evidence':str(E),'cleanup':'complete'}))
