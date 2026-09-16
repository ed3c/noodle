package main
import (
 "context"
 "encoding/json"
 "fmt"
 "os"
 "path/filepath"
 "time"
 "github.com/poteto/noodle/dispatcher"
 "github.com/poteto/noodle/event"
)
func main() {
 // Supervisor-owned observer: use actual candidate process dispatcher and Codex.
 fixture, binDir, mode := os.Args[1], os.Args[2], os.Args[3]
 args := []string{"--ephemeral", "--sandbox", "workspace-write", "--add-dir", fixture, "-c", "approval_policy=\"never\""}
 if mode == "invalid" {args[2] = "invalid-mode"}
 ctx,cancel:=context.WithTimeout(context.Background(),90*time.Second);defer cancel()
 d:=dispatcher.NewProcessDispatcher(dispatcher.ProcessDispatcherConfig{
 ProjectDir:fixture,RuntimeDir:filepath.Join(fixture,".noodle"),RuntimeKind:"process",
 ProviderConfigs:dispatcher.ProviderConfigs{Codex:dispatcher.ProviderConfig{Path:binDir,Args:args}},
 })
 start:=time.Now()
 s,err:=d.Dispatch(ctx,dispatcher.DispatchRequest{Name:"launch-probe",Prompt:`You are running one bounded launch capability probe, not a production Issue. Do not inspect or modify repository source, credentials, or settings. Use your shell tool exactly once to emit the existing Noodle typed event below, then reply NOODLE_CODEX_READY and stop. The supplied environment binds fixture order launch-capability-78, stage 0 and your actual NOODLE_SESSION_ID. Execute:
/private/tmp/noodle78-macos-feqjw78c/source/bin/noodle --project-dir /private/tmp/noodle78-macos-feqjw78c/fixture event emit --session "$NOODLE_SESSION_ID" stage_message --payload '{"message":"Real Codex launch capability completed","blocking":false,"outcome":"completed","order_id":"launch-capability-78","stage_index":0}'
If the tool is unavailable or refused, report that failure; do not claim completion.`, Provider:"codex",Model:"gpt-5.6-sol",WorktreePath:filepath.Join(fixture,".worktrees","launch"),Runtime:"process",EnvVars:map[string]string{"RUST_LOG":"warn,codex_exec=info,codex_core::config=debug,codex_config=debug", "NOODLE_PROJECT_DIR":fixture,"NOODLE_ORDER_ID":"launch-capability-78","NOODLE_STAGE_INDEX":"0"}})
 if err!=nil {json.NewEncoder(os.Stdout).Encode(map[string]any{"dispatch_error":err.Error()});os.Exit(1)}
 events:=[]dispatcher.SessionEvent{}
 for e:=range s.Events(){events=append(events,e)}
 stderr,_:=os.ReadFile(filepath.Join(fixture,".noodle","sessions",s.ID(),"stderr.log"))
 stamped,_:=os.ReadFile(filepath.Join(fixture,".noodle","sessions",s.ID(),"raw.ndjson"))
 records, readErr := event.NewEventReader(filepath.Join(fixture,".noodle")).ReadSession(s.ID(), event.EventFilter{})
 typed := 0
 for _, r := range records {if r.Type == event.EventStageMessage {var p event.StageMessagePayload; if json.Unmarshal(r.Payload,&p)==nil && p.Outcome==event.StageOutcomeCompleted && p.OrderID=="launch-capability-78" && p.StageIndex!=nil && *p.StageIndex==0 && !p.IsBlocking() && r.SessionID==s.ID() {typed++}}}
 json.NewEncoder(os.Stdout).Encode(map[string]any{"event_records":records,"event_read_error":fmt.Sprint(readErr),"matching_typed_outcomes":typed,"source_head":"e6d712a2512547cdf78a85c1ee218e1ba52e3b27","session_id":s.ID(),"mode":mode,"outcome":s.Outcome(),"events":events,"stderr":string(stderr),"raw":string(stamped),"elapsed":time.Since(start).Seconds(),"typed_outcome_observed":typed==1})
 fmt.Fprintln(os.Stderr,"This capability probe is not Issue admission or a typed completion receipt.")
}

