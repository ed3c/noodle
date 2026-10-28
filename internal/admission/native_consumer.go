package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/procx"
)

type nativeConsumerBinding struct {
	SessionID string          `json:"session_id"`
	TurnID    string          `json:"turn_id"`
	CWD       string          `json:"cwd"`
	Request   SourceReference `json:"request"`
}

type nativeConsumerRegistry struct {
	Connections map[string]struct {
		SessionID string `json:"session_id"`
		State     string `json:"state"`
		Worktree  struct {
			ThreadID string `json:"thread_id"`
			Root     string `json:"root"`
			Source   string `json:"source"`
		} `json:"native_worktree"`
		Process struct {
			PID     int    `json:"pid"`
			Started string `json:"started"`
		} `json:"process"`
	} `json:"mcp_connections"`
	Bindings       map[string]nativeConsumerBinding `json:"bindings"`
	Authorizations map[string]struct {
		Parent   nativeConsumerBinding `json:"parent_binding"`
		Request  SourceReference       `json:"request"`
		Target   string                `json:"target_thread"`
		Worktree string                `json:"worktree"`
		State    string                `json:"state"`
	} `json:"consumer_authorizations"`
	Agents map[string]struct {
		AgentID         string `json:"agent_id"`
		ParentSessionID string `json:"parent_session_id"`
		Assignment      struct {
			Request SourceReference `json:"request"`
		} `json:"assignment"`
	} `json:"native_agents"`
}

// ResolveNativeConsumer follows a kernel peer -> original MCP parent -> native
// self registration -> original request binding. Public inputs contain no caller
// Session, Agent, process or worktree identity to guess or borrow.
func ResolveNativeConsumer(ctx context.Context, registryPath string, peerPID int, requestID, contextID, project string) (ResidentConsumer, error) {
	var c ResidentConsumer
	peer, err := procx.ObserveNativeIdentity(peerPID)
	if err != nil {
		return c, err
	}
	parent, err := procx.ObserveNativeIdentity(peer.ParentPID)
	if err != nil {
		return c, err
	}
	if peer.UID != os.Getuid() || parent.UID != peer.UID {
		return c, errors.New("native consumer user scope mismatch")
	}
	data, err := os.ReadFile(registryPath)
	if err != nil {
		return c, err
	}
	var registry nativeConsumerRegistry
	if err = json.Unmarshal(data, &registry); err != nil {
		return c, err
	}
	worktreeRoot := ""
	for instance, row := range registry.Connections {
		if row.Process.PID != parent.PID || row.Process.Started != fixtureStarted(parent) || row.State != "connected" || row.SessionID == "" {
			continue
		}
		if c.Connection != "" {
			return c, errors.New("native MCP parent attribution ambiguous")
		}
		c.Connection = instance
		c.SessionID = row.SessionID
		c.Process = parent
		if row.Worktree.ThreadID != row.SessionID || row.Worktree.Source != "original_codex_app" || !filepath.IsAbs(row.Worktree.Root) {
			return c, errors.New("original App consumer worktree readback unavailable; request directory is not caller identity")
		}
		worktreeRoot = row.Worktree.Root
	}
	if c.Connection == "" {
		return c, errors.New("original native MCP parent registration unavailable; no caller fallback")
	}
	allowed := false
	for _, binding := range registry.Bindings {
		owns := binding.SessionID == c.SessionID
		if !owns {
			for _, authorization := range registry.Authorizations {
				if authorization.State == "authorized" && authorization.Parent == binding && authorization.Request == binding.Request && authorization.Target == c.SessionID && authorization.Worktree == worktreeRoot {
					owns = true
					break
				}
			}
		}
		if !owns {
			for _, agent := range registry.Agents {
				if agent.AgentID == c.SessionID && agent.ParentSessionID == binding.SessionID && agent.Assignment.Request == binding.Request {
					owns = true
					break
				}
			}
		}
		if !owns {
			continue
		}
		if err = verifySourceReference(binding.Request); err != nil {
			continue
		}
		raw, readErr := os.ReadFile(binding.Request.Path)
		if readErr != nil || sourceDigest(raw) != binding.Request.SHA256 {
			continue
		}
		var request struct {
			Repository string `json:"repository"`
			Output     string `json:"output"`
		}
		if json.Unmarshal(raw, &request) != nil {
			continue
		}
		canonical, readErr := filepath.EvalSymlinks(request.Repository)
		if readErr != nil || canonical != project {
			continue
		}
		raw, readErr = os.ReadFile(filepath.Join(request.Output, "admission", "host-request.json"))
		if readErr != nil {
			continue
		}
		var host struct {
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(raw, &host) == nil && host.RequestID == requestID {
			allowed = true
			break
		}
	}
	if !allowed {
		return c, errors.New("native consumer has no original request or assigned-agent binding")
	}
	c.Worktree, err = ObserveWorktree(ctx, worktreeRoot)
	if err != nil {
		return c, err
	}
	again, err := procx.ObserveNativeIdentity(parent.PID)
	if err != nil || !parent.SameGeneration(again) {
		return c, fmt.Errorf("MCP producer changed during consumer binding: %v", err)
	}
	currentPeer, peerErr := procx.ObserveNativeIdentity(peerPID)
	if peerErr != nil || !peer.SameGeneration(currentPeer) || currentPeer.ParentPID != parent.PID {
		return c, errors.New("original CLI peer changed during consumer binding")
	}
	c.ContextID = contextID
	c.RequestID = requestID
	c.verified = true
	return c, nil
}
