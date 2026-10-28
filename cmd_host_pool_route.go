package main

import (
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
