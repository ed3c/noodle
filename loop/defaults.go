package loop

import (
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/poteto/noodle/adapter"
	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/stringx"
	"github.com/poteto/noodle/mise"
	"github.com/poteto/noodle/monitor"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/skill"
	"github.com/poteto/noodle/worktree"
)

type unavailableWorktree struct{ err error }

func (w unavailableWorktree) Create(string, ...worktree.CreateOpts) error   { return w.err }
func (w unavailableWorktree) Merge(string, ...worktree.MergeOpts) error     { return w.err }
func (w unavailableWorktree) MergeRemoteBranch(string) error                { return w.err }
func (w unavailableWorktree) Cleanup(string, ...worktree.CleanupOpts) error { return w.err }
func (w unavailableWorktree) HasUnmergedCommits(string) (bool, error)       { return false, w.err }

func defaultDependencies(projectDir, runtimeDir, noodleBin string, cfg config.Config, logger *slog.Logger, sink dispatcher.SessionEventSink, executionContext *execution.Context) Dependencies {
	resolver := skill.Resolver{SearchPaths: cfg.Skills.Paths}
	local := dispatcher.NewProcessDispatcher(dispatcher.ProcessDispatcherConfig{
		ProjectDir:    projectDir,
		Execution:     executionContext,
		RuntimeDir:    runtimeDir,
		NoodleBin:     noodleBin,
		SkillResolver: resolver,
		RuntimeKind:   "process",
		Sink:          sink,
		ProviderConfigs: dispatcher.ProviderConfigs{
			Claude: dispatcher.ProviderConfig{
				Path: cfg.Agents.Claude.Path,
				Args: cfg.Agents.Claude.Args,
			},
			Codex: dispatcher.ProviderConfig{
				Path: cfg.Agents.Codex.Path,
				Args: cfg.Agents.Codex.Args,
			},
		},
	})
	runtimes := map[string]loopruntime.Runtime{
		"process": loopruntime.NewProcessRuntime(local, runtimeDir, cfg.Runtime.Process.MaxConcurrent),
	}
	if runtimeEnabled(cfg.AvailableRuntimes(), "sprites") {
		spriteName := strings.TrimSpace(cfg.Runtime.Sprites.SpriteName)
		if spriteName == "" {
			logger.Warn("sprites runtime unavailable: sprite_name not set")
		} else {
			sd := dispatcher.NewSpritesDispatcher(dispatcher.SpritesDispatcherConfig{
				ProjectDir:    projectDir,
				RuntimeDir:    runtimeDir,
				NoodleBin:     noodleBin,
				SkillResolver: resolver,
				SpriteName:    spriteName,
				Token:         cfg.Runtime.Sprites.Token(),
				GitToken:      cfg.Runtime.Sprites.GitToken(),
				Sink:          sink,
			})
			r := loopruntime.NewDispatcherRuntime("sprites", sd, runtimeDir)
			r.SetMaxConcurrent(cfg.Runtime.Sprites.MaxConcurrent)
			runtimes["sprites"] = r
		}
	}

	wtApp, wtErr := worktree.NewAppWithExecution(executionContext)
	var wt WorktreeManager
	if wtApp != nil {
		wtApp.CmdPrefix = "noodle worktree"
		wtApp.Quiet = true
		wt = wtApp
	} else {
		wt = unavailableWorktree{err: wtErr}
	}
	return Dependencies{
		Runtimes:       runtimes,
		Worktree:       wt,
		Adapter:        adapter.NewRunnerWithExecution(projectDir, cfg, executionContext),
		Mise:           mise.NewBuilderWithExecution(projectDir, cfg, executionContext),
		Monitor:        monitor.NewMonitor(runtimeDir),
		Now:            time.Now,
		OrdersFile:     filepath.Join(runtimeDir, "orders.json"),
		OrdersNextFile: filepath.Join(runtimeDir, "orders-next.json"),
		StatusFile:     filepath.Join(runtimeDir, "status.json"),
	}
}

func runtimeEnabled(available []string, kind string) bool {
	kind = stringx.Normalize(kind)
	for _, runtime := range available {
		if stringx.Normalize(runtime) == kind {
			return true
		}
	}
	return false
}
