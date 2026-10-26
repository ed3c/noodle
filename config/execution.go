package config

import (
	"fmt"
	"os"

	"github.com/poteto/noodle/internal/execution"
)

// Bind derives runtime paths without changing the accepted TOML bytes.
func Bind(cfg Config, e *execution.Context) (Config, error) {
	if e == nil {
		return Config{}, fmt.Errorf("config execution context is missing")
	}
	cfg.execution = e
	cfg.Runtime.Sprites.execution = e
	cfg.Runtime.Cursor.execution = e
	cfg.Skills.Paths = append([]string(nil), cfg.Skills.Paths...)
	for i, path := range cfg.Skills.Paths {
		resolved, err := e.ResolvePath(path)
		if err != nil {
			return Config{}, fmt.Errorf("resolve skill path: %w", err)
		}
		cfg.Skills.Paths[i] = resolved
	}
	for _, provider := range []*ProviderConfig{&cfg.Agents.Claude, &cfg.Agents.Codex, &cfg.Agents.Pi} {
		if provider.Path == "" {
			continue
		}
		resolved, err := e.ResolvePath(provider.Path)
		if err != nil {
			return Config{}, fmt.Errorf("resolve agent path: %w", err)
		}
		provider.Path = resolved
	}
	return cfg, nil
}

func contextValue(e *execution.Context, key string) string {
	if e == nil {
		return os.Getenv(key)
	}
	value, _ := e.Lookup(key)
	return value
}
