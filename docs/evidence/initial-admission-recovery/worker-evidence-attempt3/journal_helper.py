import json,subprocess,time,pathlib
E=pathlib.Path(__file__).parent
P=E/'journal.json'
def save(j): P.write_text(json.dumps(j,indent=2)+'\n')
def run(argv,label):
 j=json.loads(P.read_text()); started=time.time()
 proc=subprocess.Popen(argv,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
 timed_out=False
 try: out,err=proc.communicate(timeout=20)
 except subprocess.TimeoutExpired:
  timed_out=True; proc.kill(); out,err=proc.communicate(timeout=5)
 r={'label':label,'argv':argv,'cwd':str(pathlib.Path.cwd()),'started_at':started,'duration_seconds':time.time()-started,'stdout':out,'stderr':err,'returncode':proc.returncode,'timed_out':timed_out}
 j['invocations'].append(r); save(j)
 print(json.dumps(r,indent=2))
 return r
