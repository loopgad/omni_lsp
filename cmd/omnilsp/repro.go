package main

// omnilsp repro — §P10 复现包构建子命令，§N13 三级隐私（默认 redacted）。

import (
	"context"
	"flag"
	"fmt"

	"github.com/omnilsp/omni/internal/config"
)

func cmdRepro(args []string) error {
	fs := flag.NewFlagSet("repro", flag.ExitOnError)
	mode := fs.String("mode", string(ReproRedacted), "privacy mode: metadata | redacted | full-source (§N13)")
	workspace := fs.String("workspace", "", "workspace root (verbatim copy only under --mode full-source)")
	trace := fs.String("trace", "", "recorded session file (.jsonl) to embed")
	out := fs.String("out", "", "output bundle path (default repro-<timestamp>.zip)")
	configPath := fs.String("config", "", "path to omnilsp.json")
	_ = fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("repro: %w", err)
	}
	ws := *workspace
	if ws == "" {
		ws = cfg.WorkspaceDir
	}

	path, err := Build(context.Background(), BundleOptions{
		Mode:       PrivacyMode(*mode),
		Workspace:  ws,
		TracePath:  *trace,
		Out:        *out,
		ConfigPath: *configPath,
		Config:     cfg,
	})
	if err != nil {
		return err
	}
	fmt.Printf("REPRO-OK  %s (mode=%s)\n", path, *mode)
	return nil
}
