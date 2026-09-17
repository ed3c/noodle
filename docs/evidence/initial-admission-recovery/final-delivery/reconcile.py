from pathlib import Path
import fcntl,json,os,signal,subprocess,time,uuid,shutil
r=Path(__file__).parent
root=Path(json.loads((r/'execution-selection.json').read_text())['control_root']);runtime=root/'.noodle';out=r/'reconciliation';out.mkdir()
assert subprocess.check_output(['git','rev-parse','main'],cwd=root,text=True).strip()=='ca81f942f478e8e4afcbbce6ca69640867efe753'
assert not subprocess.check_output(['git','cherry','main','noodle-84-0-execute'],cwd=root)
before=json.loads((runtime/'state.snapshot.json').read_text());order=before['state']['orders']['noodle-84']
assert order['stages'][0]['status']=='review'
assert order['stages'][0]['attempts'][-1]['status']=='completed'
(out/'before.json').write_text(json.dumps(before,indent=2)+'\n')
requests=[{'id':'issue84-reconcile-'+uuid.uuid4().hex,'action':'mode','value':'manual'},{'id':'issue84-reconcile-'+uuid.uuid4().hex,'action':'merge','order_id':'noodle-84'}]
(out/'requests.json').write_text(json.dumps(requests,indent=2)+'\n')
with (runtime/'control.lock').open('a') as lock:
 fcntl.flock(lock,fcntl.LOCK_EX)
 with (runtime/'control.ndjson').open('a') as f:
  for request in requests:f.write(json.dumps(request)+'\n')
  f.flush();os.fsync(f.fileno())
t=time.monotonic();status='deadline'
with (out/'stdout.log').open('w') as stdout,(out/'stderr.log').open('w') as stderr:
 p=subprocess.Popen(['/Users/neon/.codex/experiments/noodle82-20260916/noodle','start'],cwd=root,env={**os.environ,'NOODLE_NO_BROWSER':'1'},stdout=stdout,stderr=stderr,start_new_session=True)
 (out/'launch.json').write_text(json.dumps({'argv':p.args,'pid':p.pid,'cwd':str(root)},indent=2)+'\n')
 while time.monotonic()-t<45:
  state=json.loads((runtime/'state.snapshot.json').read_text())
  if state['state']['orders']['noodle-84']['status']=='completed':status='completed';break
  if p.poll() is not None:status='loop_exited';break
  time.sleep(.25)
 if p.poll() is None:
  p.send_signal(signal.SIGTERM)
  try:p.wait(timeout=10)
  except subprocess.TimeoutExpired:os.killpg(p.pid,signal.SIGTERM);p.wait(timeout=5)
 after=json.loads((runtime/'state.snapshot.json').read_text())
 report={'status':status,'loop_exit':p.returncode,'elapsed_seconds':time.monotonic()-t,'order_status':after['state']['orders']['noodle-84']['status'],'original_order':'noodle-84','authorizes_landing':False}
 (out/'receipt.json').write_text(json.dumps(report,indent=2)+'\n');shutil.copytree(runtime,out/'runtime',ignore=shutil.ignore_patterns('*.lock'))
 print(json.dumps(report))
