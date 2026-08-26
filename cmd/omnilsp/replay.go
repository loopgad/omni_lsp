package main

// omnilsp replay — §P9/P10 session reproduction.
//
// Replay feeds the recorded "in" stream, in logical order, to a freshly
// constructed server (same config and backend discovery as serve) and
// compares every produced response against the recording after stripping
// volatile fields. Deterministic: wall-clock never participates.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/omnilsp/omni/internal/config"
	ccls "github.com/omnilsp/omni/internal/languages/ccls"
	"github.com/omnilsp/omni/internal/languages/golang"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/replay"
	"github.com/omnilsp/omni/internal/runtime/server"
)

func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	input := fs.String("input", "", "recorded session file (.jsonl)")
	configPath := fs.String("config", "", "path to omnilsp.json")
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
	ws := *workspace
	if ws == "" {
		ws = cfg.WorkspaceDir
	}
	if ws == "" {
		ws, _ = os.Getwd()
	}
	cfg.WorkspaceDir = ws

	srv := server.New(server.DefaultConfig())
	registerBackendsForReplay(srv, cfg)

	player := replay.NewPlayer()
	fed := 0
	for _, e := range sess.Entries {
		if e.Dir != "in" {
			continue
		}
		var msg jsonrpc.Message
		if err := json.Unmarshal(e.Payload, &msg); err != nil {
			return fmt.Errorf("replay: entry %d: %w", e.Seq, err)
		}
		player.Feed(&msg)
		fed++
	}
	go player.EndFeed()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := srv.Run(ctx, player.Transport())
	if runErr != nil && ctx.Err() == nil {
		return fmt.Errorf("replay: server exited: %v", runErr)
	}

	if err := player.CompareOut(sess.Entries); err != nil {
		fmt.Println("REPLAY-FAIL")
		return err
	}
	fmt.Printf("REPLAY-OK  (%d requests replayed, %d responses matched)\n", fed, len(player.Out()))
	return nil
}

// registerBackendsForReplay mirrors serve's discovery but tolerates missing
// toolchains silently — a replay must not fail because clangd left the machine.
func registerBackendsForReplay(srv *server.Server, cfg config.Config) {
	workDir, _ := filepath.Abs(cfg.WorkspaceDir)
	gb := golang.New(workDir)
	srv.RegisterBackend(gb.LanguageID(), gb)
	if cb, cerr := ccls.New(workDir); cerr == nil {
		srv.RegisterBackend(cb.LanguageID(), cb)
		srv.RegisterBackend("c", cb)
	}
}
