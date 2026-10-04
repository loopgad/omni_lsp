package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/omnilsp/omni/internal/workspace/uri"
)

func TestS21CCandidatePersistentQueriesSurviveDisabledBackend(t *testing.T) {
	runPersistentSemanticAcceptance(t, "c")
}

func TestS21CppCandidatePersistentQueriesSurviveDisabledBackend(t *testing.T) {
	runPersistentSemanticAcceptance(t, "cpp")
}

func newPersistentClangFixture(t *testing.T, workspace, language string) (persistentSemanticFixture, string, string, uint32, uint32) {
	t.Helper()
	if language != "c" && language != "cpp" {
		t.Fatalf("unsupported persistent Clang fixture language %q", language)
	}

	root, err := semanticRepoRoot()
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	toolName := "clang"
	standard := "-std=c17"
	headerFile := "include/acceptance/target.h"
	targetFile := "c/target.c"
	useFile := "c/use.c"
	if language == "cpp" {
		toolName = "clang++"
		standard = "-std=c++20"
		headerFile = "include/acceptance/target.hpp"
		targetFile = "cpp/target.cpp"
		useFile = "cpp/use.cpp"
	}
	compiler, err := persistentClangCompilerFromLock(root, toolName)
	if err != nil {
		t.Fatalf("read pinned %s compiler: %v", toolName, err)
	}

	targetSource := "#include <acceptance/" + filepath.Base(headerFile) + ">\nint target(void) { return 7; }\n"
	useSource := "#include <acceptance/" + filepath.Base(headerFile) + ">\nint run(void) { return target(); }\n"
	compileCommands := []struct {
		Directory string   `json:"directory"`
		File      string   `json:"file"`
		Arguments []string `json:"arguments"`
	}{
		{Directory: ".", File: targetFile, Arguments: []string{compiler, standard, "-I", "include", "-c", targetFile}},
		{Directory: ".", File: useFile, Arguments: []string{compiler, standard, "-I", "include", "-c", useFile}},
	}
	compileCommandsJSON, err := json.Marshal(compileCommands)
	if err != nil {
		t.Fatalf("encode compile commands: %v", err)
	}

	fixture := persistentSemanticFixture{
		language: language, symbol: "target", targetFile: targetFile, useFile: useFile,
		callToken: "target()", scopeIDPrefix: "clang-",
		files: map[string]string{
			headerFile:              "int target(void);\n",
			targetFile:              targetSource,
			useFile:                 useSource,
			"compile_commands.json": string(compileCommandsJSON) + "\n",
		},
		definition: semanticLocation{StartLine: 1, StartChar: 4, EndLine: 1, EndChar: 10},
	}
	if err := semanticWriteFiles(workspace, fixture.files); err != nil {
		t.Fatalf("write %s workspace: %v", language, err)
	}
	targetURI := semanticCanonicalURI(uri.FromPath(filepath.Join(workspace, fixture.targetFile)).String())
	useURI := semanticCanonicalURI(uri.FromPath(filepath.Join(workspace, fixture.useFile)).String())
	fixture.definition.URI = targetURI
	callLine, callCharacter := semanticPositionOf(fixture.files[fixture.useFile], fixture.callToken, 0)
	return fixture, targetURI, useURI, callLine, callCharacter
}

func persistentClangCompilerFromLock(root, toolName string) (string, error) {
	if toolName != "clang" && toolName != "clang++" {
		return "", fmt.Errorf("unsupported pinned compiler %q", toolName)
	}
	data, err := os.ReadFile(filepath.Join(root, "test", "acceptance", "tools", "tools.lock.json"))
	if err != nil {
		return "", err
	}
	var lock struct {
		Binaries map[string]struct {
			Path string `json:"path"`
		} `json:"resolvedBinaries"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return "", err
	}
	path := lock.Binaries[toolName].Path
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s path is not absolute in the tool lock", toolName)
	}
	return path, nil
}

func TestPersistentClangFixturesHaveSharedCompileContextAndCorrectPositions(t *testing.T) {
	for _, language := range []string{"c", "cpp"} {
		t.Run(language, func(t *testing.T) {
			fixture, targetURI, useURI, callLine, callCharacter := newPersistentClangFixture(t, t.TempDir(), language)
			if fixture.language != language || fixture.symbol != "target" || fixture.scopeIDPrefix != "clang-" || fixture.definition.URI != targetURI || useURI == "" {
				t.Fatalf("fixture identity is incomplete: fixture=%+v target=%q use=%q", fixture, targetURI, useURI)
			}
			if fixture.definition.StartLine != 1 || fixture.definition.StartChar != 4 || fixture.definition.EndLine != 1 || fixture.definition.EndChar != 10 {
				t.Fatalf("definition position = %+v, want line 1 chars 4..10", fixture.definition)
			}
			if callLine != 1 || callCharacter != 23 {
				t.Fatalf("call position = %d:%d, want 1:23", callLine, callCharacter)
			}

			var commands []struct {
				Directory string   `json:"directory"`
				File      string   `json:"file"`
				Arguments []string `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(fixture.files["compile_commands.json"]), &commands); err != nil {
				t.Fatalf("decode compile commands: %v", err)
			}
			if len(commands) != 2 || commands[0].Directory != commands[1].Directory {
				t.Fatalf("compile commands do not share one compiler context: %+v", commands)
			}
			for i := range commands {
				args, peerArgs := commands[i].Arguments, commands[1-i].Arguments
				if len(args) < 2 || len(args) != len(peerArgs) || args[0] != peerArgs[0] || commands[i].File != args[len(args)-1] {
					t.Fatalf("compile command %d differs beyond its translation unit: %+v", i, commands[i])
				}
				for arg := 1; arg < len(args)-1; arg++ {
					if args[arg] != peerArgs[arg] {
						t.Fatalf("compile command %d has a distinct semantic context: %+v", i, commands[i])
					}
				}
			}
		})
	}
}
