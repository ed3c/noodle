package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

func (h *noodleHost) controlRoot() string {
	if h.rolling != nil {
		return h.rolling.root
	}
	return h.dir
}

func poolRequestOwner(root string, req hostRequest) (hostGeneration, error) {
	p, err := readHostPool(root)
	if err != nil {
		return hostGeneration{}, err
	}
	id := p.Active
	if req.ContextID != "" && req.InstanceID != "" {
		id = req.InstanceID
	} else if owner, ok := p.Owners[req.RequestID]; ok {
		if !hostReceiptMatches(req, owner.Original) {
			return hostGeneration{}, errors.New("original task binding differs from native version owner")
		}
		id = owner.Current.HostInstanceID
	}
	g, ok := p.Generations[id]
	if !ok {
		return g, errors.New("native request generation is unknown")
	}
	return g, nil
}
func poolBindingCompatible(root string, b hostRequestBinding, caller string) bool {
	p, err := readHostPool(root)
	if err != nil {
		return false
	}
	owner, ok := p.Owners[b.RequestID]
	if !ok {
		if _, waiting := waitingPoolAdmission(root, b, p); !waiting {
			return false
		}
		for _, g := range p.Generations {
			if g.SHA == caller {
				return true
			}
		}
		return false
	}
	req := hostRequest{ProjectDir: b.ProjectDir, RequestID: b.RequestID, ConfigPath: b.ConfigPath, ConfigSHA256: b.ConfigSHA256, Mode: b.Mode, CleanupCompleted: b.CleanupCompleted}
	if owner.Original.BinarySHA256 != b.BinarySHA256 || !hostReceiptMatches(req, owner.Original) {
		return false
	}
	// The selected frontend can be newer than the generation still serving this
	// round. It may query/control that owner, but cannot change the task binding.
	for _, generation := range p.Generations {
		if generation.SHA == caller {
			return true
		}
	}
	return false
}

func hostOwnerMatches(req hostRequest, owner hostTaskOwner) bool {
	if req.ContextID == "" {
		return hostReceiptMatches(req, owner.Original)
	}
	if req.RequestID != "" {
		binding := req
		binding.ContextID, binding.InstanceID = "", ""
		if !hostReceiptMatches(binding, owner.Original) {
			return false
		}
	}
	if hostReceiptMatches(req, owner.Current) || hostReceiptMatches(req, owner.Original) {
		return true
	}
	for _, previous := range owner.Previous {
		if hostReceiptMatches(req, previous) {
			return true
		}
	}
	return false
}

// Routing uses the one original task owner; no competing carrier acquires it.
func (h *noodleHost) forwardRolling(ctx context.Context, req hostRequest) (any, bool) {
	if h.rolling == nil || req.Operation == "resources" || req.Operation == "view_resources" || req.Operation == "round_prepare" || req.Operation == "round_rebuild" || (req.Operation == "round_admit" || req.Operation == "round_reconcile") {
		return nil, false
	}
	p, err := readHostPool(h.rolling.root)
	if err != nil {
		return hostError("unknown_effect", err), true
	}
	owner, owned := p.Owners[req.RequestID]
	if !owned && req.RequestID == "" && req.ContextID != "" {
		for _, candidate := range p.Owners {
			if candidate.Current.ContextID == req.ContextID && candidate.Current.HostInstanceID == req.InstanceID {
				owner, owned = candidate, true
				break
			}
		}
	}
	if owned {
		if !hostOwnerMatches(req, owner) {
			return hostError("refused", errors.New("original task binding differs")), true
		}
		if owner.MaintenanceHold != "" && req.Operation != "status" {
			return hostResponse{owner.Current, "busy", "original work is held by restricted maintenance; no dispatch or rebuild allowed"}, true
		}
	}
	if req.AdmissionSHA == "" {
		req.AdmissionSHA = req.BinarySHA256
	}
	g, err := poolRequestOwner(h.rolling.root, req)
	if err != nil {
		return hostError("refused", err), true
	}
	if g.ID == h.instanceID {
		return nil, false
	}
	if g.State == "exited" {
		if req.Operation == "status" || req.Operation == "detach" || req.Operation == "cleanup" {
			req.BinarySHA256 = g.SHA
			return offlineHostStatus(g.Directory, req, errors.New("native generation has exited")), true
		}
		return hostError("refused", errors.New("original generation ended; use its original task continuation")), true
	}
	req.BinarySHA256 = g.SHA
	raw, err := nativeHostRPC(ctx, g.Directory, req)
	if err != nil {
		return hostError("unknown_effect", err), true
	}
	return json.RawMessage(raw), true
}

func routeBoundHost(root string, req *hostRequest) (string, error) {
	if _, err := os.Stat(filepath.Join(root, "pool.json")); os.IsNotExist(err) {
		return root, nil
	} else if err != nil {
		return "", err
	}
	g, err := poolRequestOwner(root, *req)
	if err != nil {
		return "", err
	}
	req.BinarySHA256 = g.SHA
	return g.Directory, nil
}
