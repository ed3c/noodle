package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	"github.com/poteto/noodle/loop"
)

type returnRecoveryFixture struct {
	t *testing.T
	root, host, generation, request, frame, endpoint string
	owner hostOwnerBinding
	original loop.ReturnRecoveryEvent
	plan stoppedMergeRef
	before map[string][]byte
}
func newReturnRecoveryFixture(t *testing.T) *returnRecoveryFixture {
	t.Helper()
	shortRoot := os.TempDir()
	if info, statErr := os.Stat("/tmp"); statErr == nil && info.IsDir() { shortRoot = "/tmp" }
	base, err := os.MkdirTemp(shortRoot, "nrc-")
	if err != nil { t.Fatal(err) }
	base, err = filepath.EvalSymlinks(base)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	f := &returnRecoveryFixture{t: t, root: filepath.Join(base, "project"), host: filepath.Join(base, "host"), endpoint: filepath.Join(base, "r.sock"), before: map[string][]byte{}}
	for _, dir := range []string{f.root, filepath.Join(f.root, ".noodle"), filepath.Join(base, "admission")} { if err = os.MkdirAll(dir, 0700); err != nil { t.Fatal(err) } }
	actual, err := returnRecoveryActual()
	if err != nil { t.Fatal(err) }
	// A distinct executor path with the same compiled typed producer. The actual
	// controller still comes exclusively from os.Executable on every operation.
	old := filepath.Join(base, "producer")
	producerBytes, err := os.ReadFile(actual.Path)
	if err != nil { t.Fatal(err) }
	if err = os.WriteFile(old, producerBytes, 0700); err != nil { t.Fatal(err) }
	executor, err := hostReadbackBinaryRef(old)
	if err != nil { t.Fatal(err) }
	request := stoppedTestPut(t, filepath.Join(base, "request.json"), map[string]any{"schema": 1, "fixture": "isolated-original-request"})
	binding := stoppedTestPut(t, filepath.Join(base, "admission", "binding.json"), map[string]any{"schema": 1, "request": returnRecoverySource(request), "goal": "isolated-Q12-control", "main": map[string]string{"threadId": "same-Main", "hostId": "local"}, "connection_instance": "original", "consumer_revision": "v1", "producer": returnRecoverySource(executor), "host_instance_id": strings.Repeat("a",32)})
	descriptor := stoppedTestPut(t, filepath.Join(base, "admission", "descriptor.json"), map[string]any{"schema":1, "binding":returnRecoverySource(binding), "endpoint":f.endpoint, "producer":returnRecoverySource(executor), "host_instance_id":strings.Repeat("a",32)})
	cfgPath := filepath.Join(base, "admission", "noodle.toml")
	cfg := fmt.Sprintf("mode=\"manual\"\n[runtime]\nadmitted_orders=[\"original\"]\n[runtime.return_handoff]\npath=%q\nsha256=%q\n", descriptor.Path, descriptor.SHA256)
	stoppedTestBytes(t, cfgPath, []byte(cfg))
	cfgRef, err := hostReadbackBinaryRef(cfgPath)
	if err != nil { t.Fatal(err) }
	if _, err = config.SaveRuntimeSource(f.root, returnRecoverySource(cfgRef)); err != nil { t.Fatal(err) }
	id := strings.Repeat("a",32)
	f.generation = filepath.Join(f.host, "g", id)
	if err = os.MkdirAll(f.generation,0700); err != nil { t.Fatal(err) }
	receipt := hostReceipt{Schema:1, Protocol:hostProtocol, ProjectDir:f.root, HostDir:f.host, RequestID:"request", HostPID:123, HostInstanceID:id, ContextID:"context", ContextState:"active", ConfigPath:cfgPath, ConfigSHA256:cfgRef.SHA256, Mode:"manual", BinaryPath:executor.Path, BinarySHA256:executor.SHA256}
	f.request = filepath.Join(base,"admission","host-request.json")
	stoppedTestPut(t, f.request, hostRequestBinding{Schema:1, Operation:"attach", ProjectDir:f.root, HostDir:f.host, RequestID:"request", ConfigPath:cfgPath, ConfigSHA256:cfgRef.SHA256, Mode:"manual", BinarySHA256:executor.SHA256, EnvironmentSHA256:"fixture", Status:"accepted"})
	stoppedTestPut(t, filepath.Join(f.generation,"receipts.json"), []hostReceipt{receipt})
	stoppedTestPut(t, filepath.Join(f.host,"pool.json"), hostPool{Schema:1, Active:id, Generations:map[string]hostGeneration{id:{ID:id, Directory:f.generation, Binary:executor.Path, SHA:executor.SHA256, State:"active"}}, Owners:map[string]hostTaskOwner{"request":{Original:receipt, Current:receipt, State:"owned", EnvironmentSHA:"fixture"}}})
	owner, err := readHostOwnerBinding(f.request)
	if err != nil { t.Fatal(err) }
	f.owner = *owner
	at := time.Now().UTC().Truncate(time.Millisecond)
	intent := &state.ReturnHandoffIntent{BindingPath:binding.Path, BindingSHA256:binding.SHA256, EventID:"6", Status:"unknown"}
	attempt := state.AttemptNode{AttemptID:"original-0-attempt-0", SessionID:"original-session", Status:state.AttemptCompleted, StartedAt:at.Add(-time.Minute), CompletedAt:at, WorktreeName:"original", ReturnHandoff:intent}
	review := state.PendingReviewNode{OrderID:"original", StageIndex:0, SessionID:attempt.SessionID, WorktreeName:"original", WorktreePath:filepath.Join(f.root,".worktrees","original"), ReturnHandoff:intent}
	order := state.OrderNode{OrderID:"original", Status:state.OrderActive, Stages:[]state.StageNode{{StageIndex:0, Status:state.StageReview, Attempts:[]state.AttemptNode{attempt}}}}
	snapshot := reducer.DurableSnapshot{OrderRevision:strings.Repeat("b",32), State:state.State{Orders:map[string]state.OrderNode{"original":order}, PendingReviews:map[string]state.PendingReviewNode{"original":review}, SchemaVersion:statever.Current, LastEventID:"6"}, EffectLedger:[]reducer.EffectLedgerRecord{}, GeneratedAt:at}
	stoppedTestPut(t, filepath.Join(f.root,".noodle","state.snapshot.json"), snapshot)
	stoppedTestBytes(t, filepath.Join(f.root,".noodle","control-ack.ndjson"), []byte("original ACK history\n"))
	f.frame = filepath.Join(base,"frame.json")
	stoppedTestPut(t, f.frame, map[string]any{"owner":"soodles.loop", "request":returnRecoverySource(request), "inner":map[string]any{"native_host":map[string]any{"owner_binding":owner}, "order":order, "pending_reviews":map[string]state.PendingReviewNode{"original":review}}})
	line := fmt.Sprintf("time=%s level=WARN msg=%q error=%q transport_phase=dial_before_encode event_id=6 attempt_id=%s session_id=%s\n", at.Add(time.Millisecond).Format(time.RFC3339Nano), "return handoff unknown; original owner readback required", "dial unix "+f.endpoint+": connect: connection refused", attempt.AttemptID, attempt.SessionID)
	stoppedTestBytes(t, filepath.Join(f.generation,"host.log"), []byte(line))
	raw, _ := json.Marshal(snapshot)
	f.original, err = loop.MaterializeReturnRecovery(raw,nil,"original","6",returnRecoverySource(descriptor),returnRecoverySource(request),id,returnRecoverySource(executor))
	if err != nil { t.Fatal(err) }
	for _, path := range []string{request.Path, binding.Path, descriptor.Path, cfgPath, f.request, f.frame, filepath.Join(f.root,".noodle","state.snapshot.json"), filepath.Join(f.root,".noodle","control-ack.ndjson")} { f.before[path], err = os.ReadFile(path); if err != nil { t.Fatal(err) } }
	return f
}
func (f *returnRecoveryFixture) prepare() returnRecoveryResult {
	f.t.Helper()
	r, err := prepareReturnRecovery(f.root, f.request,"original","6",f.frame)
	if err != nil { f.t.Fatal(err) }
	f.plan = r.Plan
	return r
}
func (f *returnRecoveryFixture) unchanged() {
	f.t.Helper()
	for path, before := range f.before { after, err := os.ReadFile(path); if err != nil || !bytes.Equal(before,after) { f.t.Fatal("original source changed",path,err) } }
}

type returnRecoveryReceiver struct {
	mu sync.Mutex
	status string
	qualified bool
	consumer string
	lostACK bool
	custodied bool
	packets, events, reads int
	rawPacket []byte
	replaceOnRead bool
	refuseReplacement bool
}
func (f *returnRecoveryFixture) serve(receiver *returnRecoveryReceiver) {
	f.t.Helper()
	listener, err := net.Listen("unix",f.endpoint)
	if err != nil { f.t.Fatal(err) }
	done := make(chan struct{})
	f.t.Cleanup(func() { _ = listener.Close(); <-done })
	go func() {
		defer close(done)
		for {
			c, err := listener.Accept(); if err != nil { return }
			_ = c.SetDeadline(time.Now().Add(3*time.Second))
			var raw json.RawMessage
			if json.NewDecoder(c).Decode(&raw) != nil { _ = c.Close(); continue }
			var request struct { Operation string `json:"operation"`; Schema int `json:"schema"`; Subject loop.ReturnRecoverySubject `json:"subject"`; ActualController config.RuntimeSource `json:"actual_controller"` }
			_ = json.Unmarshal(raw,&request)
			receiver.mu.Lock()
			if request.Operation == "worker_return_custody_readback" {
				receiver.reads++
				status := receiver.status; if receiver.custodied { status = "custodied" }
				result := loop.ReturnCustodyReadback{Operation:request.Operation, Schema:1, Owner:"soodles.loop", Status:status, ConsumerQualified:receiver.qualified, Subject:request.Subject, ActualController:request.ActualController, OriginalTargetReceiver:json.RawMessage(`{"instance":"original"}`), ActualCurrentConsumer:json.RawMessage(fmt.Sprintf(`{"instance":%q}`,receiver.consumer)), Effects:[]string{}}
				if status == "custodied" { result.Custody = &loop.ReturnCustody{Registry:config.RuntimeSource{Path:filepath.Join(f.host,"isolated-custody.json"),SHA256:strings.Repeat("c",64)}, ReceiverInstance:receiver.consumer, Event:f.original.Subject.Packet} }
				_ = json.NewEncoder(c).Encode(result)
				if receiver.replaceOnRead { receiver.consumer = "fresh-independently-qualified"; receiver.qualified = !receiver.refuseReplacement; receiver.replaceOnRead = false }
			} else if request.Operation == "worker_return" {
				receiver.packets++
				receiver.rawPacket = append([]byte(nil),raw...)
				var packet loop.ReturnRecoveryPacket
				if json.Unmarshal(raw,&packet) == nil && packet == f.original.Subject.Packet && receiver.qualified {
					if !receiver.custodied { receiver.events++; receiver.custodied = true }
					if !receiver.lostACK { _ = json.NewEncoder(c).Encode(map[string]string{"status":"acknowledged"}) }
				} else { _ = json.NewEncoder(c).Encode(map[string]string{"status":"refused"}) }
			}
			receiver.mu.Unlock()
			_ = c.Close()
		}
	}()
}
func TestReturnRecoveryPrepareAndReadOnlyStates(t *testing.T) {
	for _, mode := range []string{"absent","unknown","custodied","unqualified"} {
		t.Run(mode,func(t *testing.T) {
			f := newReturnRecoveryFixture(t)
			receiver := &returnRecoveryReceiver{status:mode,qualified:mode!="unqualified",consumer:"current"}
			if mode == "unqualified" { receiver.status = "absent" }
			f.serve(receiver)
			r := f.prepare()
			if r.Status != "prepared" || len(r.Effects)!=0 || r.Producer.Path == r.ActualController.Path { t.Fatal("prepare provenance/effects",r) }
			if receiver.reads != 0 || receiver.packets != 0 { t.Fatal("prepare performed transport") }
			r, err := operateReturnRecovery(f.root,f.plan.Path,"","inspect",nil)
			if mode == "unqualified" { if err == nil { t.Fatal("unqualified absent accepted") } } else if err != nil { t.Fatal(err) }
			if mode == "absent" && r.Status != "qualified_absent" || mode == "unknown" && r.Status != "unknown" || mode == "custodied" && r.Status != "custodied" { t.Fatal("readback state",mode,r.Status) }
			receiver.mu.Lock(); packets := receiver.packets; receiver.mu.Unlock()
			if packets != 0 || len(r.Effects)!=0 || r.MainWake!="NOT_OBSERVED" || r.Acceptance!="NOT_RUN" { t.Fatal("read-only custody claimed effects",r) }
			f.unchanged()
		})
	}
}
func TestReturnRecoveryOnceAcrossBreakpointsAndLostACK(t *testing.T) {
	for _, stop := range []string{"none","before_intent","after_intent","after_ack","after_receipt","lost_ack"} {
		t.Run(stop,func(t *testing.T) {
			f := newReturnRecoveryFixture(t); f.prepare()
			receiver := &returnRecoveryReceiver{status:"absent",qualified:true,consumer:"current",lostACK:stop=="lost_ack"}
			f.serve(receiver)
			barrier := func(point string) error { if point==stop { return errors.New("isolated interruption") }; return nil }
			first, err := operateReturnRecovery(f.root,f.plan.Path,f.plan.SHA256,"continue",barrier)
			if stop=="none" && (err!=nil || first.Status!="acknowledged") { t.Fatal(first,err) }
			if stop!="none" && err==nil { t.Fatal("breakpoint not observed",stop) }
			for i:=0;i<2;i++ { if _, err = operateReturnRecovery(f.root,f.plan.Path,f.plan.SHA256,"continue",nil); err!=nil { t.Fatal("reentry",err) } }
			receiver.mu.Lock(); packets, events, raw := receiver.packets,receiver.events,append([]byte(nil),receiver.rawPacket...); receiver.mu.Unlock()
			expected:=1; if stop=="after_intent" { expected=0 }
			if packets!=expected || events!=expected { t.Fatal("bounded write repeated",stop,packets,events) }
			if packets>0 { want,_:=json.Marshal(f.original.Subject.Packet); if !bytes.Equal(want,raw) { t.Fatal("original packet bytes changed") } }
			f.unchanged()
		})
	}
}
func TestReturnRecoveryFreshConsumerReplacementNeighbors(t *testing.T) {
	for _, qualified:=range []bool{false,true} {
		t.Run(fmt.Sprint(qualified),func(t *testing.T) {
			f:=newReturnRecoveryFixture(t); f.prepare()
			receiver:=&returnRecoveryReceiver{status:"absent",qualified:true,consumer:"old-snapshot",replaceOnRead:true,refuseReplacement:!qualified}
			f.serve(receiver)
			r,err:=operateReturnRecovery(f.root,f.plan.Path,f.plan.SHA256,"continue",nil)
			if qualified && (err!=nil || r.Status!="acknowledged") { t.Fatal("fresh independently qualified consumer refused",err) }
			if !qualified && err==nil { t.Fatal("old snapshot qualified replacement") }
			_,_=operateReturnRecovery(f.root,f.plan.Path,f.plan.SHA256,"continue",nil)
			receiver.mu.Lock(); packets,events:=receiver.packets,receiver.events; receiver.mu.Unlock()
			expected:=0; if qualified { expected=1 }
			if packets!=1 || events!=expected { t.Fatal("replacement replay",packets,events) }
		})
	}
}
func TestReturnRecoveryCustodyAfterCanonicalProgress(t *testing.T) {
	f:=newReturnRecoveryFixture(t); f.prepare()
	var s reducer.DurableSnapshot
	raw,err:=os.ReadFile(filepath.Join(f.root,".noodle","state.snapshot.json")); if err!=nil { t.Fatal(err) }
	if err=json.Unmarshal(raw,&s);err!=nil { t.Fatal(err) }
	o:=s.State.Orders["original"];o.Status=state.OrderCompleted;o.Stages[0].Status=state.StageCompleted;s.State.Orders[o.OrderID]=o;delete(s.State.PendingReviews,o.OrderID)
	stoppedTestPut(t,filepath.Join(f.root,".noodle","state.snapshot.json"),s)
	receiver:=&returnRecoveryReceiver{status:"custodied",qualified:true,consumer:"fresh"};f.serve(receiver)
	r,err:=operateReturnRecovery(f.root,f.plan.Path,f.plan.SHA256,"continue",nil)
	if err!=nil || r.Status!="custodied" || len(r.Effects)!=0 { t.Fatal("custody demanded canonical rollback",r,err) }
}

func TestReturnRecoveryControllerProcess(t *testing.T) {
	project:=os.Getenv("NOODLE_RETURN_TEST_PROJECT")
	if project=="" { return }
	r,err:=operateReturnRecovery(project,os.Getenv("NOODLE_RETURN_TEST_PLAN"),os.Getenv("NOODLE_RETURN_TEST_SHA"),"continue",nil)
	if err!=nil && !strings.Contains(err.Error(),"already locked") { t.Fatal(err) }
	if r.MainWake!="NOT_OBSERVED" { t.Fatal("claimed Main wake") }
}
func TestReturnRecoveryCompetingProcessesAndCopiedController(t *testing.T) {
	f:=newReturnRecoveryFixture(t);f.prepare()
	receiver:=&returnRecoveryReceiver{status:"absent",qualified:true,consumer:"fresh"};f.serve(receiver)
	actual,err:=returnRecoveryActual();if err!=nil {t.Fatal(err)}
	run:=func(binary string) *exec.Cmd {
		cmd:=exec.Command(binary,"-test.run=^TestReturnRecoveryControllerProcess$","-test.count=1")
		cmd.Env=append(os.Environ(),"NOODLE_RETURN_TEST_PROJECT="+f.root,"NOODLE_RETURN_TEST_PLAN="+f.plan.Path,"NOODLE_RETURN_TEST_SHA="+f.plan.SHA256)
		return cmd
	}
	x:=filepath.Join(f.host,"copied-controller");if err=os.Link(actual.Path,x);err!=nil {t.Fatal(err)}
	if out,err:=run(x).CombinedOutput();err==nil || !bytes.Contains(out,[]byte("actual return controller differs")) {t.Fatal("same bytes/different path consumed plan",string(out),err)}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i:=0;i<2;i++ {wg.Add(1);go func(){defer wg.Done();if out,err:=run(actual.Path).CombinedOutput();err!=nil {t.Errorf("competing controller: %s %v",out,err)}else{successes.Add(1)}}()};wg.Wait()
	if successes.Load()!=2 {t.Fatal("controllers did not finish")}
	receiver.mu.Lock();packets,events:=receiver.packets,receiver.events;receiver.mu.Unlock()
	if packets!=1 || events!=1 {t.Fatal("competing processes crossed original once gate",packets,events)}
	f.unchanged()
}
func TestReturnRecoveryCLIProducerNext(t *testing.T) {
	f:=newReturnRecoveryFixture(t)
	app:=&App{projectDir:f.root}
	cmd:=newReviewCmd(app);var output bytes.Buffer;cmd.SetOut(&output);cmd.SetErr(&output)
	cmd.SetArgs([]string{"prepare-return-recovery",f.request,"original","6",f.frame})
	if err:=cmd.Execute();err!=nil {t.Fatal(err,output.String())}
	var result returnRecoveryResult;if err:=json.Unmarshal(output.Bytes(),&result);err!=nil {t.Fatal(err)}
	want:=[]string{result.ActualController.Path,"--project-dir",f.root,"review","continue-return-recovery",result.Plan.Path,"--expected-version",result.Plan.SHA256}
	raw,_:=json.Marshal(result.Next["argv"]);var got []string;_ = json.Unmarshal(raw,&got)
	if !reflect.DeepEqual(got,want) {t.Fatal("producer next differs",got,want)}
}
func TestReturnRecoverySourceQualificationRefusesDrift(t *testing.T) {
	for _, mode:=range []string{"missing-log","foreign-endpoint","duplicate-log","truncated-log","write-after","lost-ACK","unsupported-producer","frame-source","frame-tuple","frame-host","descriptor-source","request-source","foreign-generation-owner"} {
		t.Run(mode,func(t *testing.T) {
			f:=newReturnRecoveryFixture(t)
			logPath:=filepath.Join(f.generation,"host.log")
			raw,err:=os.ReadFile(logPath);if err!=nil {t.Fatal(err)}
			switch mode {
			case "missing-log": err=os.Remove(logPath)
			case "foreign-endpoint": stoppedTestBytes(t,logPath,bytes.ReplaceAll(raw,[]byte(f.endpoint),[]byte("/foreign.sock")))
			case "duplicate-log": stoppedTestBytes(t,logPath,append(raw,raw...))
			case "truncated-log": stoppedTestBytes(t,logPath,raw[:len(raw)-1])
			case "write-after": stoppedTestBytes(t,logPath,bytes.ReplaceAll(raw,[]byte("dial_before_encode"),[]byte("write_unknown")))
			case "lost-ACK": stoppedTestBytes(t,logPath,bytes.ReplaceAll(raw,[]byte("dial_before_encode"),[]byte("ack_unknown")))
			case "unsupported-producer": stoppedTestBytes(t,f.owner.ExecutionBinary.Path,[]byte("changed source"))
			case "descriptor-source": stoppedTestBytes(t,f.original.Subject.Target.Descriptor.Path,[]byte("{}"))
			case "request-source": stoppedTestBytes(t,f.original.Subject.Request.Path,[]byte("{}"))
			case "frame-source": stoppedTestBytes(t,f.frame,[]byte("{}"))
			case "frame-tuple": frame,readErr:=os.ReadFile(f.frame);if readErr!=nil {t.Fatal(readErr)};stoppedTestBytes(t,f.frame,bytes.ReplaceAll(frame,[]byte("original-session"),[]byte("attempt1-session")))
			case "frame-host": frame,readErr:=os.ReadFile(f.frame);if readErr!=nil {t.Fatal(readErr)};stoppedTestBytes(t,f.frame,bytes.ReplaceAll(frame,[]byte(f.owner.Current.HostInstanceID),[]byte(strings.Repeat("d",32))))
			case "foreign-generation-owner": pool,readErr:=readHostPool(f.host);if readErr!=nil {t.Fatal(readErr)};pool.Owners["foreign"]=hostTaskOwner{Original:f.owner.Current};stoppedTestPut(t,filepath.Join(f.host,"pool.json"),pool)
			}
			if err!=nil {t.Fatal(err)}
			r,err:=prepareReturnRecovery(f.root,f.request,"original","6",f.frame)
			if err==nil || r.Status!="unknown" || len(r.Effects)!=0 {t.Fatal("unqualified source permitted",mode,r,err)}
		})
	}
}
func TestReturnRecoveryVisibleUnsyncedIntentIsNotResent(t *testing.T) {
	f:=newReturnRecoveryFixture(t);f.prepare()
	receiver:=&returnRecoveryReceiver{status:"absent",qualified:true,consumer:"fresh"};f.serve(receiver)
	r,err:=operateReturnRecovery(f.root,f.plan.Path,"","inspect",nil)
	if err!=nil || r.Readback==nil {t.Fatal("qualified readback missing",err)}
	intent:=returnRecoveryIntent{1,f.plan,f.original.Subject,r.ActualController,*r.Readback}
	raw,err:=json.MarshalIndent(intent,"","  ");if err!=nil {t.Fatal(err)}
	path:=filepath.Join(filepath.Dir(f.plan.Path),"intent.json")
	_,err=stoppedMergeArchiveUsing(path,append(raw,'\n'),func(string) error {return errors.New("isolated sync failure")})
	if err==nil {t.Fatal("sync failure missing")}
	before,err:=os.ReadFile(path);if err!=nil {t.Fatal(err)}
	r,err=operateReturnRecovery(f.root,f.plan.Path,f.plan.SHA256,"continue",nil)
	if err!=nil || r.Status!="unknown" {t.Fatal("intent readback",r,err)}
	after,err:=os.ReadFile(path);if err!=nil || !bytes.Equal(before,after) {t.Fatal("original intent overwritten",err)}
	receiver.mu.Lock();packets:=receiver.packets;receiver.mu.Unlock()
	if packets!=0 {t.Fatal("visible intent allowed send")}
}

func TestReturnRecoveryConcurrentCustodyWinsWithoutSecondEvent(t *testing.T) {
	f:=newReturnRecoveryFixture(t);f.prepare()
	receiver:=&returnRecoveryReceiver{status:"absent",qualified:true,consumer:"fresh"};f.serve(receiver)
	r,err:=operateReturnRecovery(f.root,f.plan.Path,f.plan.SHA256,"continue",func(point string) error {
		if point=="after_intent" {receiver.mu.Lock();receiver.custodied=true;receiver.events=1;receiver.mu.Unlock()};return nil
	})
	if err!=nil || r.Status!="acknowledged" {t.Fatal("same-packet custody race",r,err)}
	r,err=operateReturnRecovery(f.root,f.plan.Path,f.plan.SHA256,"continue",nil)
	if err!=nil || r.Status!="custodied" || r.CustodyEvidence.Path=="" {t.Fatal("original custody receipt missing",r,err)}
	receiver.mu.Lock();packets,events:=receiver.packets,receiver.events;receiver.mu.Unlock()
	if packets!=1 || events!=1 {t.Fatal("custody race duplicated original event",packets,events)}
}

func TestReturnRecoveryOriginalStorageAlias(t *testing.T) {
	f:=newReturnRecoveryFixture(t)
	storage:=filepath.Join(f.host,"storage")
	if err:=os.MkdirAll(storage,0700);err!=nil {t.Fatal(err)}
	stoppedTestPut(t,filepath.Join(storage,"source.json"),map[string]string{"project":f.root,"storage_root":storage})
	logical:=filepath.Join(f.root,".noodle");physical:=filepath.Join(storage,".noodle")
	if err:=os.Rename(logical,physical);err!=nil {t.Fatal(err)}
	if err:=os.Symlink(physical,logical);err!=nil {t.Fatal(err)}
	f.prepare()
	if !strings.HasPrefix(f.plan.Path,physical+string(filepath.Separator)) {t.Fatal("plan lost original physical runtime custody",f.plan)}
	receiver:=&returnRecoveryReceiver{status:"absent",qualified:true,consumer:"fresh"};f.serve(receiver)
	r,err:=operateReturnRecovery(f.root,f.plan.Path,f.plan.SHA256,"continue",nil)
	if err!=nil || r.Status!="acknowledged" {t.Fatal("bound runtime alias refused",r,err)}
	f.unchanged()
}
