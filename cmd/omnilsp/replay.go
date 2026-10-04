package main

// omnilsp replay — §P9/P10 session reproduction.
//
// Replay feeds the recorded "in" stream, in logical order, to a freshly
// constructed server (same config and backend discovery as serve) and
// compares every produced response against the recording after stripping
// volatile fields. Deterministic: wall-clock never participates.

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/omnilsp/omni/internal/config"
	"github.com/omnilsp/omni/internal/replay"
	"github.com/omnilsp/omni/internal/runtime/server"
)

func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	input := fs.String("input", "", "recorded session file (.jsonl)")
	configPath := fs.String("config", "", "path to omnilsp.json")
	transportName := fs.String("transport", "", "transport override used when recording")
	addr := fs.String("addr", "", "TCP address override used when recording")
	workspace := fs.String("workspace", "", "workspace root used during recording")
	_ = fs.Parse(args)

	if *input == "" {
		return fmt.Errorf("replay: --input is required")
	}
	sess, err := replay.LoadSession(*input)
	if err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	if *transportName != "" {
		cfg.Transport = *transportName
	}
	if *addr != "" {
		cfg.TCPAddr = *addr
	}
	ws := *workspace
	if ws == "" {
		ws = cfg.WorkspaceDir
	}
	if ws == "" {
		ws, _ = os.Getwd()
	}
	cfg.WorkspaceDir = ws
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	configHash := replay.ConfigHash(cfg)
	if sess.Meta.ConfigHash == "" || sess.Meta.ConfigHash != configHash {
		return fmt.Errorf("replay: config hash mismatch: recording %q, current %q", sess.Meta.ConfigHash, configHash)
	}

	srv := server.New(serverConfigFrom(cfg))
	registerBackendsForReplay(srv, cfg)

	player := replay.NewPlayer()
	srv.SetSemanticResponseObserver(indexedSemanticResponseBinder(srv, player.Transport()))
	fed := 0
	for _, e := range sess.Entries {
		if e.Dir == "in" {
			fed++
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		err := srv.Run(ctx, player.Transport())
		runDone <- err
		if err != nil {
			cancel() // Surface an early server failure without waiting for an output timeout.
		}
	}()
	if err := player.Replay(ctx, sess.Entries); err != nil {
		cancel()
		_ = player.Transport().Close()
		if runErr := <-runDone; runErr != nil {
			return fmt.Errorf("replay: server exited: %w (replay: %v)", runErr, err)
		}
		return err
	}
	runErr := <-runDone
	if runErr != nil && ctx.Err() == nil {
		return fmt.Errorf("replay: server exited: %v", runErr)
	}

	if err := player.CompareOut(sess.Entries); err != nil {
		fmt.Println("REPLAY-FAIL")
		return err
	}

	var available *replay.SemanticIdentity
	current, identityErr := srv.CurrentSemanticIndexIdentity(ctx)
	if identityErr == nil {
		available = replaySemanticIdentity(current)
	}
	status, verifyErr := player.CompareSession(sess, available)
	if verifyErr != nil {
		if identityErr != nil {
			verifyErr = fmt.Errorf("%w (current semantic identity unavailable: %v)", verifyErr, identityErr)
		}
		fmt.Printf("REPLAY-WIRE-OK  (%d requests replayed, %d responses matched; semantic=%s)\n", fed, len(player.Out()), status)
		return fmt.Errorf("replay: semantic reproduction gate failed: %w", verifyErr)
	}
	fmt.Printf("REPLAY-OK  (%d requests replayed, %d responses matched; semantic=%s)\n", fed, len(player.Out()), status)
	return nil
}

func replaySemanticIdentity(identity server.SemanticIndexIdentity) *replay.SemanticIdentity {
	tools := make([]replay.ToolIdentity, len(identity.Tools))
	for i, tool := range identity.Tools {
		tools[i] = replay.ToolIdentity{
			Name: tool.Name, Path: tool.Path, Version: tool.Version, SHA256: tool.SHA256,
		}
	}
	return &replay.SemanticIdentity{
		Generation:         identity.Generation,
		IndexContentDigest: identity.IndexContentDigest,
		BuildContexts:      identity.BuildContexts,
		Tools:              tools,
	}
}

// registerBackendsForReplay uses the same tool discovery and semantic-provider
// wiring as serve. Missing toolchains remain degradations, never a false
// semantic-complete replay result.
func registerBackendsForReplay(srv *server.Server, cfg config.Config) {
	registerBackends(srv, cfg)
}
