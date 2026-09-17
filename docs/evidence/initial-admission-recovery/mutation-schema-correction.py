from pathlib import Path
import hashlib,json,os,shutil,subprocess,tempfile,time
r=Path(__file__).parent;sel=json.loads((r/'execution-selection.json').read_text());source=Path(sel['control_root'])/'.worktrees/noodle-84-0-execute';out=r/'mutation-schema-correction';out.mkdir()
env={**os.environ,'GOMODCACHE':'/private/tmp/codex-go-mod','GOCACHE':'/private/tmp/codex-go-cache','GOPROXY':'file:///Users/neon/go/pkg/mod/cache/download','GOSUMDB':'off'}
recovery='loop/admission_recovery.go';evidence='loop/admission_evidence.go'
cases=[
 ('canonical-schema',evidence,'snapshot.State.SchemaVersion != statever.Current','(snapshot.State.SchemaVersion != statever.Current && false)','TestAdmissionRecoveryRefusalGates/unknown_schema'),
 ('projection',evidence,'if err := validateAdmissionProjection(snapshot.State, orders); err != nil {','if err := error(nil); err != nil {','TestAdmissionRecoveryRefusalGates/projection_mismatch'),
 ('canonical-ownership',evidence,'if exists || admitted[initialAdmissionEffectID(o.ID)]','if (exists && false) || admitted[initialAdmissionEffectID(o.ID)]','TestAdmissionRecoveryRefusalGates/owned'),
 ('admission-ledger',evidence,'admitted[initialAdmissionEffectID(o.ID)] || effectSubjects[o.ID]','(admitted[initialAdmissionEffectID(o.ID)] && false) || (effectSubjects[o.ID] && false)','TestAdmissionRecoveryRefusalGates/admitted'),
 ('subject-dispatch',evidence,'|| effectSubjects[o.ID] ||','|| (effectSubjects[o.ID] && false) ||','TestAdmissionPendingIntentBoundaries/subject_dispatch'),
 ('valid-proposal',recovery,'if *compact.InitialRevision == r.Revision {','if *compact.InitialRevision == r.Revision && false {','TestAdmissionRecoveryRefusalGates/valid_initial'),
 ('digest-binding',recovery,'if digest != "" && digest != r.Subject.SHA256 {','if digest != "" && digest != r.Subject.SHA256 && false {','TestAdmissionRecoveryBindingsAndLock/digest'),
 ('revision-binding',recovery,'if revision != "" && revision != r.Revision {','if revision != "" && revision != r.Revision && false {','TestAdmissionRecoveryBindingsAndLock/revision'),
 ('owner-lock',recovery,'lockfile.TryLock(filepath.Join(dir, "noodle.lock"))','lockfile.TryLock(filepath.Join(dir, "mutant-unused.lock"))','TestAdmissionRecoveryBindingsAndLock/lock'),
 ('process-group',evidence,'if !absent {','if !absent && false {','TestAdmissionRecoveryDeadRootLiveGroup'),
 ('metadata-identity',evidence,'meta.SessionID != entry.Name() ||','(meta.SessionID != entry.Name() && false) ||','TestAdmissionPendingIntentBoundaries/metadata_identity'),
 ('durable-intent',recovery,'barrier("before_intent")\n\t}\n\tif err := persistAdmissionReceipt(dir, *receipt); err != nil {','barrier("before_intent")\n\t}\n\tif err := error(nil); err != nil {','TestAdmissionRetirementPhysicalCrash/after_intent'),
 ('pending-discovery',recovery,'receipt, err = pendingAdmissionReceipt(dir)','receipt, err = (*admissionReceipt)(nil), error(nil)','TestAdmissionRetirementPhysicalCrash/after_remove'),
 ('pending-revision',recovery,'if receipt.Revision != r.Revision {','if receipt.Revision != r.Revision && false {','TestAdmissionPendingIntentBoundaries/changed_revision'),
 ('pending-later-mailbox',recovery,'if !missing && !bytes.Equal(data, receipt.Proposal) {','if !missing && !bytes.Equal(data, receipt.Proposal) && false {','TestAdmissionPendingIntentBoundaries/later_mailbox'),
 ('pending-ambiguity',recovery,'if pending != nil {','if pending != nil && false {','TestAdmissionPendingIntentBoundaries/multiple_intents'),
 ('retired-later-mailbox',recovery,'receipt.Retired = true\n\t\t\t\tif err := persistAdmissionReceipt','_ = os.Remove(nextPath)\n\t\t\t\treceipt.Retired = true\n\t\t\t\tif err := persistAdmissionReceipt','TestAdmissionRecoveryRetiresExactBytes'),
 ('historical-effect-scope',evidence,'case reducer.EffectLedgerPending:\n','case reducer.EffectLedgerPending:\n\t\t\treturn fmt.Errorf("mutant rejects all pending history")\n','TestAdmissionRecoveryHistoricalEvidenceScope/unrelated_pending_effect'),
 ('stale-meta-scope',evidence,'if meta.SessionID != entry.Name() ||','if meta.Alive || meta.Status == monitor.SessionStatusRunning || meta.SessionID != entry.Name() ||','TestAdmissionRecoveryHistoricalEvidenceScope/stale_running_metadata'),
]
cases=[cases[0]]
(out/'selection.json').write_text(json.dumps({'selected_at':time.time(),'cases':cases,'script_sha256':hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),'scope':'isolated source-copy planted defects; never candidate or production state edits'},indent=2)+'\n')
def run(root,regex,name):
 q=subprocess.run(['go','test','./loop','-run','^'+regex+'$','-count=1','-v'],cwd=root,env=env,capture_output=True,text=True,timeout=90)
 raw=q.stdout+q.stderr;(out/(name+'.txt')).write_text(raw)
 return {'exit':q.returncode,'log':name+'.txt','sha256':hashlib.sha256(raw.encode()).hexdigest(),'assertion_failed':'--- FAIL:' in raw,'build_failed':'[build failed]' in raw}
results=[]
with tempfile.TemporaryDirectory(prefix='noodle84-mutants-') as scratch:
 root=Path(scratch)
 paths=subprocess.check_output(['git','ls-files','--cached','--others','--exclude-standard','-z'],cwd=source).decode().split('\0')
 for name in paths:
  if not name:continue
  p=source/name
  if p.is_file():dst=root/name;dst.parent.mkdir(parents=True,exist_ok=True);shutil.copy2(p,dst)
 shutil.copytree(source/'ui/dist',root/'ui/dist',dirs_exist_ok=True)
 legal=run(root,'TestAdmissionRecoveryLegalNoncases|TestAdmissionRecoveryHistoricalEvidenceScope|TestInitialAdmissionCompletedProjectionBoundary|TestAdmissionNoProposalEndsRecovery','legal-noncases')
 assert legal['exit']==0,legal
 for name,path,old,new,test in cases:
  p=root/path;original=p.read_text();assert original.count(old)==1,(name,original.count(old))
  mutant=original.replace(old,new,1);p.write_text(mutant)
  red=run(root,test,name+'-red');p.write_text(original)
  green=run(root,test,name+'-green')
  result={'gate':name,'control':test,'mutated_file':path,'original_sha256':hashlib.sha256(original.encode()).hexdigest(),'mutant_sha256':hashlib.sha256(mutant.encode()).hexdigest(),'replacement':{'before':old,'after':new},'red':red,'green':green,'legal_noncase':legal,'passed':red['exit']!=0 and red['assertion_failed'] and not red['build_failed'] and green['exit']==0}
  results.append(result);(out/'partial.json').write_text(json.dumps(results,indent=2)+'\n');print(json.dumps({'gate':name,'passed':result['passed']}),flush=True)
receipt={'passed':all(x['passed'] for x in results),'gates':results,'scratch_removed':True,'authorizes_landing':False}
(out/'receipt.json').write_text(json.dumps(receipt,indent=2)+'\n');raise SystemExit(not receipt['passed'])
