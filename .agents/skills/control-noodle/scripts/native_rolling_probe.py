#!/usr/bin/env python3
import fcntl,hashlib,http.server,json,os,pathlib,shlex,shutil,signal,socket,subprocess,sys,tempfile,threading,time
base=pathlib.Path(sys.argv[1]);base.mkdir(exist_ok=False)
binroot=pathlib.Path(sys.argv[2]).resolve();codex=pathlib.Path(sys.argv[3]).resolve()
local_task=len(sys.argv)>4 and sys.argv[4]=='local-task'
taskkey='local-task' if local_task else 'probe'
versions=[]
for label in 'abc':
 p=base/f'noodle-{label}';shutil.copy2(binroot/f'noodle-rolling-{label}',p);versions.append(p)
assert len({hashlib.sha256(p.read_bytes()).hexdigest() for p in versions}) == 3, 'Three genuinely different binary SHAs are required'
host=pathlib.Path(tempfile.mkdtemp(prefix='nwork-',dir='/tmp')).resolve();(base/'host-root.txt').write_text(str(host))
home=base/'home';home.mkdir();tools=base/'bin';tools.mkdir();project=base/'project';project.mkdir();runtime=project/'.noodle'
first_gate=threading.Event();first_arrived=threading.Event();second_arrived=threading.Event();provider_requests=[];guard=threading.Lock()
def save(name,data): (base/name).write_text(json.dumps(data,indent=2,default=str))
class Provider(http.server.BaseHTTPRequestHandler):
 def log_message(self,*args):pass
 def do_POST(self):
  body=self.rfile.read(int(self.headers['Content-Length'])).decode()
  with guard:
   new_work='NEW_VERSION_CONTEXT_SENTINEL' in body;index=sum(not r.get('new_work') for r in provider_requests);provider_requests.append({'index':index,'new_work':new_work,'thread_id':self.headers.get('thread-id'),'body':body});save('provider-requests.json',provider_requests)
  if index==0 and not new_work:
   first_arrived.set();first_gate.wait(150)
   data=json.dumps({'error':{'message':'OFFLINE_FIRST_ROUND_KNOWN_FAILURE','type':'invalid_request_error','code':'invalid_request'}}).encode()
   self.send_response(400);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data);return
  if not new_work:second_arrived.set()
  self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
  events=[{'type':'response.created','response':{'id':'offline-response'}},{'type':'response.output_item.done','item':{'type':'message','role':'assistant','id':'offline-message','content':[{'type':'output_text','text':'OFFLINE_NATIVE_ROLLING_SECOND_ROUND'}]}},{'type':'response.completed','response':{'id':'offline-response','usage':{'input_tokens':0,'output_tokens':0,'total_tokens':0}}}]
  for event in events:self.wfile.write(('data: '+json.dumps(event)+'\n\n').encode())
  self.wfile.flush()
provider=http.server.ThreadingHTTPServer(('127.0.0.1',0),Provider);threading.Thread(target=provider.serve_forever,daemon=True).start()
port=provider.server_port
profile=f'(version 1)(allow default)(deny network*)(allow network-outbound (remote ip "localhost:{port}"))'
(tools/'codex').write_text('#!/bin/sh\nexec /usr/bin/sandbox-exec -p '+shlex.quote(profile)+' '+shlex.quote(str(codex))+' "$@"\n');(tools/'codex').chmod(0o700)
(home/'config.toml').write_text(f'thread_unload_delay_secs = 0\nmodel_provider = "offline"\nmodel = "offline"\napproval_policy = "never"\n[model_providers.offline]\nname = "offline"\nbase_url = "http://127.0.0.1:{port}/v1"\nwire_api = "responses"\n')
skills=base/'skills';(skills/taskkey).mkdir(parents=True);(skills/taskkey/'SKILL.md').write_text('---\nname: '+taskkey+'\ndescription: isolated native rolling acceptance\nschedule: Only the fixture admitted order.\n---\nReturn the offline fixture response.\n')
order='rolling-original'
config=f'mode = "manual"\n[server]\nenabled = false\n[routing.defaults]\nprovider = "codex"\nmodel = "offline"\n[skills]\npaths = [{json.dumps(str(skills))}]\n[agents.codex]\npath = {json.dumps(str(tools))}\nrequire_typed_outcome = false\n[runtime]\ndefault = "codex-app-server"\n[runtime.process]\nadmitted_orders = ["{order}"]\n'
(project/'.noodle.toml').write_text(config);(project/'.gitignore').write_text('.noodle/\n.worktrees/\n')
env={'HOME':str(home),'CODEX_HOME':str(home),'PATH':str(tools)+':/usr/bin:/bin:/usr/sbin:/sbin','NOODLE_HOST_DIR':str(host),'LANG':'en_US.UTF-8','ROLLING_CONTEXT_SENTINEL':'private-fixture-context'}
def cmd(*args,binary=None,cwd=None,check=True):
 r=subprocess.run([str(binary or versions[0]),*args],env=env,cwd=cwd,text=True,capture_output=True,timeout=40)
 with (base/'commands.ndjson').open('a') as log:log.write(json.dumps({'argv':[str(binary or versions[0]),*args],'exit':r.returncode,'stdout':r.stdout,'stderr':r.stderr})+'\n')
 if check and r.returncode:raise RuntimeError(r.stdout+r.stderr)
 return r
def call(*args,binary=None):return json.loads(cmd(*args,binary=binary).stdout)
def git(*args):subprocess.run(['/usr/bin/git',*args],cwd=project,env=env,capture_output=True,check=True)
def read(path):return json.loads(path.read_bytes())
def pool():return read(host/'pool.json')
def wait(check,why,seconds=90):
 until=time.monotonic()+seconds
 while time.monotonic()<until:
  result=check()
  if result:return result
  time.sleep(.1)
 raise RuntimeError(why)
def control(commands,target_runtime=None):
 runtime=target_runtime or project/'.noodle'
 with (runtime/'control.lock').open('a+b') as lock:
  fcntl.flock(lock,fcntl.LOCK_EX)
  with (runtime/'control.ndjson').open('ab') as out:
   for value in commands:out.write(json.dumps(value).encode()+b'\n')
   out.flush();os.fsync(out.fileno())
 def acked():
  rows=[json.loads(line) for line in (runtime/'control-ack.ndjson').read_text().splitlines()] if (runtime/'control-ack.ndjson').exists() else []
  found={c['id']:next((a for a in reversed(rows) if a.get('id')==c['id']),None) for c in commands}
  if all(found.values()):
   assert all(a['status']=='ok' for a in found.values()),found
   return found
 return wait(acked,'native control ACK missing',20)
def native_records():
 return [(p,read(p)) for p in (runtime/'sessions').glob('*/native.json')]
def rpc(g,req):
 with socket.socket(socket.AF_UNIX) as sock:
  sock.settimeout(10);sock.connect(str(pathlib.Path(g['directory'])/'host.sock'));sock.sendall(json.dumps(req).encode()+b'\n');return json.loads(sock.makefile('rb').readline())
try:
 save('binary-identities.json',{'codex':{'path':str(codex),'sha256':hashlib.sha256(codex.read_bytes()).hexdigest()},'noodle':[{'path':str(p),'sha256':hashlib.sha256(p.read_bytes()).hexdigest()} for p in versions]})
 git('init','-b','fixture');git('config','user.name','Native Fixture');git('config','user.email','native-fixture@localhost');git('add','.');git('commit','-m','Create isolated rolling fixture')
 request=base/'request.json';first=call('--project-dir',str(project),'host','attach','--mode','manual','--request-file',str(request));save('original.json',first);original_bytes=request.read_bytes()
 doctor=subprocess.run([sys.executable,str(pathlib.Path(__file__).with_name('host_doctor.py')),'--binary',str(versions[0]),'--host-root',str(host),'--request-file',str(request)],env=env,capture_output=True,text=True,timeout=20)
 save('doctor.json',{'exit':doctor.returncode,'stdout':doctor.stdout,'stderr':doctor.stderr});assert doctor.returncode==0,doctor.stdout+doctor.stderr
 checkpoint=read(runtime/'state.snapshot.json')
 proposal={'initial_revision':checkpoint['order_revision'],'orders':[{'id':order,'title':'Native rolling acceptance','stages':[{'do':taskkey,'with':'codex','model':'offline','runtime':'codex-app-server','prompt':json.dumps({'kind':'local_task','goal':'ORIGINAL_ROLLING_CONTEXT_SENTINEL','write_paths':['fixture-output.txt']}) if local_task else 'ORIGINAL_ROLLING_CONTEXT_SENTINEL'}]}]}
 (runtime/'orders-next.json').write_text(json.dumps(proposal))
 wait(lambda: order in read(runtime/'state.snapshot.json')['state']['orders'],'initial native order not admitted',20)
 control([{'id':'fixture-start-mode','action':'mode','value':'supervised'}])
 print(json.dumps({'stage':'awaiting_original_native_worker','evidence':str(base)}),flush=True)
 wait(first_arrived.is_set,'worker not admitted under real Host resource policy',90)
 records=wait(lambda:native_records(),'native worker record missing',10);first_path,active=records[0];save('first-active.json',active)
 source_version=cmd('version').stdout
 selected=call('host','switch','--binary',str(versions[1]));save('selected-b.json',selected)
 p=wait(lambda:(p if p['generations'][p['active']]['binary_path']==str(versions[1]) else None) if (p:=pool()) else None,'new version not ready',20)
 assert p['owners'][first['request_id']]['current']['host_instance_id']==first['host_instance_id']
 assert read(first_path)['state']!='released' and not first_gate.is_set()
 selected_version=cmd('version').stdout
 assert 'control-noodle-b' in selected_version,selected_version
 pinned=subprocess.run([str(versions[0]),'version'],env=dict(env,NOODLE_SESSION_ID=active['session_id']),capture_output=True,text=True,timeout=15)
 assert pinned.returncode==0 and pinned.stdout==source_version,(pinned.stdout,pinned.stderr)
 bound=call('--project-dir',str(project),'host','status','--request-file',str(request),binary=versions[1])
 assert bound['host_instance_id']==first['host_instance_id'] and bound['binary_sha256']==first['binary_sha256'],bound
 save('cross-version-frontend.json',{'selected_version':selected_version,'worker_pinned_version':pinned.stdout,'new_frontend_original_request':bound})
 busy=cmd('host','rebuild','--request-file',str(request),check=False);save('rebuild-active-refusal.json',{'exit':busy.returncode,'stdout':busy.stdout,'stderr':busy.stderr})
 assert busy.returncode!=0 and json.loads(busy.stdout)['status']=='busy' and read(first_path)['state']!='released'
 new_project=base/'new-project';new_project.mkdir();(new_project/'.noodle.toml').write_text(config.replace(order,'new-work'))
 (new_project/'.gitignore').write_text('.noodle/\n.worktrees/\n')
 for args in [('init','-b','fixture'),('config','user.name','Native Fixture'),('config','user.email','native-fixture@localhost'),('add','.'),('commit','-m','Create new-version fixture')]:subprocess.run(['/usr/bin/git',*args],cwd=new_project,env=env,capture_output=True,check=True)
 newer=call('--project-dir',str(new_project),'host','attach','--mode','manual','--request-file',str(base/'new-request.json'))
 assert newer['host_instance_id']==p['active'] and newer['host_instance_id']!=first['host_instance_id'],newer
 new_runtime=new_project/'.noodle';seed=read(new_runtime/'state.snapshot.json')
 new_prompt=json.dumps({'kind':'local_task','goal':'NEW_VERSION_CONTEXT_SENTINEL','write_paths':['fixture-output.txt']}) if local_task else 'NEW_VERSION_CONTEXT_SENTINEL'
 (new_runtime/'orders-next.json').write_text(json.dumps({'initial_revision':seed['order_revision'],'orders':[{'id':'new-work','stages':[{'do':taskkey,'with':'codex','model':'offline','runtime':'codex-app-server','prompt':new_prompt}]}]}))
 wait(lambda:'new-work' in read(new_runtime/'state.snapshot.json')['state']['orders'],'new work not admitted',10)
 control([{'id':'fixture-new-mode','action':'mode','value':'supervised'}],new_runtime)
 def new_finished():
  rows=[read(path) for path in (new_runtime/'sessions').glob('*/native.json')]
  return rows[0] if len(rows)==1 and rows[0]['state']=='released' else None
 new_done=wait(new_finished,'new worker did not complete on selected version',60)
 assert new_done['terminal']=='completed' and new_done['server_exit']==0 and new_done['foreground']['parent']==newer['host_pid']
 assert read(first_path)['state']!='released' and not first_gate.is_set()
 save('new-work-on-selected-version.json',{'native_host':newer,'native_worker':new_done,'old_original_still_active':read(first_path)})
 wait(lambda:rpc(p['generations'][p['active']],{'operation':'resources'})['resources']['owned_allocations']==0,'new worker watcher has not returned its slot',10)
 wait(lambda:read(new_runtime/'state.snapshot.json')['state']['orders']['new-work']['stages'][0]['status']=='review' if local_task else True,'new worker completion not projected',10)
 detach=call('--project-dir',str(new_project),'host','detach','--request-file',str(base/'new-request.json'),'--context-id',newer['context_id'],'--instance-id',newer['host_instance_id'])
 assert detach['context_state']=='detached',detach
 queued=call('host','switch','--binary',str(versions[2]));save('queued-c.json',queued)
 duplicate=call('host','switch','--binary',str(versions[2]));assert duplicate['id']==queued['id']
 time.sleep(1.2);p=pool();assert len([g for g in p['generations'].values() if g['state'] not in ('failed','exited')])==2 and len(p['generations'])==2,p
 g=p['generations'][p['active']];resources=rpc(g,{'operation':'resources'});assert resources['resources']['active_workers']==1 and resources['resources']['owned_allocations']==0,resources
 save('coexist-active.json',{'pool':p,'new_resources':resources,'original_native_worker':read(first_path)})
 control([{'id':'fixture-pause-next','action':'pause'}]);assert read(runtime/'status.json')['loop_state']=='paused'
 assert not second_arrived.is_set() and read(first_path)['state']!='released'
 first_gate.set();print(json.dumps({'stage':'first_round_finishing_with_pause','original_thread':active['thread_id']}),flush=True)

 # The native rolling owner already owns migration. Observe its continuation
 # instead of racing it with a second release/rebuild effect.
 wait(lambda:read(first_path)['state']=='released','original native session did not release',30)
 def paused_converged():
  p=pool();owner=p['owners'][first['request_id']]
  if p['generations'][p['active']]['binary_path']!=str(versions[2]) or owner['current']['host_instance_id']!=p['active'] or owner['state']!='owned':return None
  if any(g['state']!='exited' for i,g in p['generations'].items() if i!=p['active']):return None
  return p
 p=wait(paused_converged,'original owner migration/paused generations did not settle',60)
 rebuild_env={**env,'HOME':'/replacement-host-home','UNRELATED_REBUILD_VALUE':'different'}
 rebuilt_owner=p['owners'][first['request_id']];save('rebuilt-context-owner.json',rebuilt_owner)
 assert rebuilt_owner['environment_sha256']==read(request)['environment_sha256']
 explicit_reconstruction=False
 save('rebuild-path.json',{'explicit_reconstruction_observed':False,'automatic_original_owner_continuation':True,'manual_lost_source_rebuild':'Separate context-rebuild feature'})
 for label in ('rebuild-call','rebuild-duplicate'):
  result=subprocess.run([str(versions[1]),'host','rebuild','--request-file',str(request)],env=rebuild_env,text=True,capture_output=True,timeout=40)
  save(label+'.json',{'exit':result.returncode,'stdout':result.stdout,'stderr':result.stderr})
  response=json.loads(result.stdout)
  assert result.returncode==0 and response['context_id']==rebuilt_owner['current']['context_id'] and request.read_bytes()==original_bytes
 first_done=read(first_path);save('first-released-before-resume.json',first_done);save('paused-pool-before-resume.json',p)
 assert first_done['terminal']=='failed' and first_done['server_exit']==0 and first_done['unsubscribe_ack'] and first_done['unload_readback']['data']==[] and not first_done['archive_intent'],first_done
 assert not second_arrived.is_set() and len(native_records())==1 and read(runtime/'status.json')['loop_state']=='paused'
 quota=read(host/'quota/quota.json');assert len(quota['leases'])==0,quota;save('quota-returned-before-resume.json',quota)
 assert request.read_bytes()==original_bytes and pathlib.Path(first_done['worktree']).exists()
 control([{'id':'fixture-requeue-original','action':'requeue','order_id':order},{'id':'fixture-resume-original','action':'resume'}])
 wait(second_arrived.is_set,'same original task did not resume on selected version',90)
 def second_done():
  rows=native_records()
  return rows if len(rows)==2 and all(d['state']=='released' for _,d in rows) else None
 rows=wait(second_done,'second native foreground did not automatically unload and exit',60)
 resumed=next(d for path,d in rows if path!=first_path);save('second-released.json',resumed)
 assert resumed['terminal']=='completed' and resumed['server_exit']==0 and resumed['thread_id']==first_done['thread_id'] and resumed['worktree']==first_done['worktree'] and resumed['order_id']==order and resumed['task_key']==first_done['task_key']
 original_requests=[r for r in provider_requests if not r['new_work']]
 assert len(original_requests)==2 and len(provider_requests)==3 and original_requests[0]['thread_id']==original_requests[1]['thread_id']==resumed['thread_id']
 assert 'ORIGINAL_ROLLING_CONTEXT_SENTINEL' in original_requests[1]['body']
 wait(lambda:len(read(host/'quota/quota.json')['leases'])==0,'second native exit did not return shared quota',10)
 commands=[{'id':'fixture-requeue-original','action':'requeue','order_id':order},{'id':'fixture-resume-original','action':'resume'}];control(commands)
 time.sleep(.5);assert len(native_records())==2 and len(provider_requests)==3
 if local_task:
  def reviewed():
   state=read(runtime/'state.snapshot.json')['state']
   stage=state['orders'][order]['stages'][0]
   return state if stage['status']=='review' and state['pending_reviews'].get(order,{}).get('session_id')==resumed['session_id'] else None
  review=wait(reviewed,'native local-task completion did not retain candidate review',10)
  save('local-task-review.json',review)
  readback=call('--project-dir',str(project),'host','task','session-readback','--session-id',resumed['session_id'])
  assert readback['process_and_group_absent'] is True and readback['effects']==[]
  save('native-session-readback.json',readback)
 save('summary.json',{'active_worker_survived_version_selection':True,'new_actual_worker_completed_on_new_version_while_old_worker_active':True,'new_generation_ready_while_old_worker_running':True,'third_version_waited':True,'max_live_generations':2,'old_generations_exited':True,'pause_allowed_current_round_to_finish':True,'native_foreground_unloaded_and_exited':True,'quota_returned_before_resume':True,'same_original_request':request.read_bytes()==original_bytes,'same_native_thread':True,'same_worktree':True,'no_duplicate_after_control_replay':True,'provider':'isolated localhost fixture, no remote model','context_rebuilt_with_different_caller_environment':explicit_reconstruction,'native_local_task_review':'PASS' if local_task else 'NOT_COVERED'})

except Exception as error:
 save('failure.json',{'error':str(error),'native_records':native_records(),'pool':pool() if (host/'pool.json').exists() else None});raise
finally:
 first_gate.set()
 # Settle and detach all owned contexts before stopping any generation.
 # The active Host may still be needed to receive an old round.
 if (host/'pool.json').exists():
  clean=[]
  try:
   for request_id in list(pool()['owners']):
    owner=pool()['owners'][request_id];value=owner['current']
    if value['context_state']!='active':continue
    g=pool()['generations'][value['host_instance_id']]
    wait(lambda:rpc(g,{'operation':'resources'})['resources']['active_workers']==0,'cleanup loop watcher remains active',30)
    value=pool()['owners'][request_id]['current']
    if value['context_state']=='active':
     g=pool()['generations'][value['host_instance_id']]
     reply=rpc(g,dict(value,operation='detach',instance_id=g['id']))
     if reply.get('status')!='ok':raise RuntimeError('native detach not confirmed; retain original runtime: '+json.dumps(reply))
   for g in pool()['generations'].values():
    if g['state']=='exited':continue
    pid=g['process']['pid']
    if subprocess.run(['/bin/ps','-p',str(pid)],capture_output=True).returncode!=0:
     if pool()['generations'][g['id']]['state']!='exited':raise RuntimeError('Host PID absent without original group/quota exit confirmation; cleanup unknown')
     continue
    resources=rpc(g,{'operation':'resources'})
    assert resources['contexts']==0 and resources['resources']['active_workers']==0 and resources['resources']['active_residents']==0 and resources['resources']['active_allocations']==0 and resources['resources']['unresolved_residents']==0 and not resources['residents'].get('error') and not resources['resources']['shared_quota'].get('error'),resources
    command=subprocess.check_output(['/bin/ps','-p',str(pid),'-o','command='],text=True).strip();assert command==g['binary_path']+' host serve'
    clean.append({'native_resources':resources,'command':command})
   save('cleanup-before.json',clean)
   for row in clean:
    pid=row['native_resources']['host_pid']
    os.kill(pid,signal.SIGTERM)
    wait(lambda:subprocess.run(['/bin/ps','-p',str(pid)],capture_output=True).returncode!=0,'empty fixture Host exit not observed',5)
   save('cleanup.json',{'original_native_detach':True,'empty_hosts':clean,'exit_observed':True,'shared_original_host_touched':False})
  except Exception as error:
   save('cleanup-pending.json',{'error':str(error),'known_empty_hosts':clean})
   raise RuntimeError('Native rolling cleanup incomplete; retain original readback') from error
 provider.shutdown();provider.server_close()

print(json.dumps({'stage':'passed','evidence':str(base),'thread':resumed['thread_id'],'cleanup':'complete'}),flush=True)
