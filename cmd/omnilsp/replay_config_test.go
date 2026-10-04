package main

import (
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/replay"
)

func TestReplayRejectsConfigHashMismatch(t *testing.T) {
	path := t.TempDir() + "/session.jsonl"
	session := &replay.Session{Meta: replay.Meta{
		FormatVersion: replay.FormatVersion,
		ConfigHash:    "recorded-config",
	}}
	if err := replay.Save(path, session); err != nil {
		t.Fatalf("save session: %v", err)
	}

	err := cmdReplay([]string{"--input", path})
	if err == nil || !strings.Contains(err.Error(), "config hash mismatch") {
		t.Fatalf("replay error = %v, want config hash mismatch", err)
	}
}
