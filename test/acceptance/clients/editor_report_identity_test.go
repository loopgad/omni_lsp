//go:build clients

package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditorExitOutcomePreservesProducerIdentity(t *testing.T) {
	for _, exitCode := range []int{0, 1} {
		t.Run(string(rune('0'+exitCode)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.json")
			original := editorResult{
				RunID: "native-run", CandidateSHA256: strings.Repeat("a", 64),
				Client: "sublime-lsp", Status: "not_verified", ClientTestsCompleted: true,
				Cases: []editorCaseResult{{Name: "go", LanguageID: "go", Status: "passed"}},
			}
			data, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := recordEditorExitOutcome(path, exitCode, true, "managed tree exited"); err != nil {
				t.Fatal(err)
			}
			got, err := readEditorResult(path)
			if err != nil {
				t.Fatal(err)
			}
			if got.RunID != original.RunID || got.CandidateSHA256 != original.CandidateSHA256 {
				t.Fatalf("exit recording discarded producer identity: %+v", got)
			}
			if got.Status != "not_verified" || len(got.Cases) != 1 || got.ClientExitCode == nil ||
				*got.ClientExitCode != exitCode || got.CleanExit != (exitCode == 0) {
				t.Fatalf("exit recording changed case/terminal outcome: %+v", got)
			}
		})
	}
}
