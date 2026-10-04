package clang

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

const contextOption = "omnilsp.clang.context"

const (
	maxCompileContexts = 4096
	maxCompileDBBytes  = int64(32 << 20)
)

type compileContext struct {
	key              string
	compiler         model.ToolIdentity
	libclang         model.ToolIdentity
	args             []string
	dir              string
	files            map[string]struct{}
	tests            bool
	problem          string
	unavailable      bool
	extractorVersion string
	headerManifest   string
}

// BuildIndexRequest discovers one semantic scope per distinct compile command
// context from the immutable view. Compiler executable tokens must identify an
// explicit path; this builder never resolves them through PATH.
func BuildIndexRequest(ctx context.Context, view model.WorkspaceView, rootURI string, backend identity.BackendID, epoch identity.BackendEpoch) (model.Request, error) {
	if view == nil {
		return model.Request{}, model.ErrMissingView
	}
	if rootURI == "" {
		return model.Request{}, model.ErrInvalidScope
	}
	if err := ctx.Err(); err != nil {
		return model.Request{}, err
	}
	if backend.Language == "" {
		backend.Language = "cpp"
	}
	if backend.Name == "" {
		backend.Name = "ccls"
	}
	if backend.Language != "cpp" {
		return model.Request{}, model.ErrInvalidProvenance
	}
	files, err := walkFiles(ctx, view, rootURI)
	if err != nil {
		if ctx.Err() != nil {
			return model.Request{}, ctx.Err()
		}
		return requestForContexts(ctx, view, rootURI, backend, epoch, []*compileContext{{problem: "immutable workspace walk failed: " + err.Error()}})
	}
	entries, found, err := readRawCommands(ctx, view, files)
	if err != nil {
		if ctx.Err() != nil {
			return model.Request{}, ctx.Err()
		}
		return requestForContexts(ctx, view, rootURI, backend, epoch, []*compileContext{{problem: "compile_commands.json could not be read from the captured view: " + err.Error()}})
	}
	if !found {
		return requestForContexts(ctx, view, rootURI, backend, epoch, []*compileContext{{problem: "compile_commands.json is absent from the captured workspace view", unavailable: true}})
	}
	root, err := workspaceuri.Parse(rootURI)
	if err != nil {
		return model.Request{}, err
	}
	rootPath, err := root.Path()
	if err != nil {
		return model.Request{}, err
	}
	rootPath, err = filepath.Abs(rootPath)
	if err != nil {
		return model.Request{}, err
	}

	knownFiles := make(map[string]string, len(files))
	for _, file := range files {
		parsed, err := workspaceuri.Parse(file.URI)
		if err != nil {
			continue
		}
		path, err := parsed.Path()
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootPath, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			knownFiles[canonicalRelKey(rel)] = normalizeRel(rel)
		}
	}
	identities := make(map[string]toolPair)
	contexts := make(map[string]*compileContext)
	var discoveryProblems []string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return model.Request{}, err
		}
		command, sourceRel, problem := canonicalCommand(ctx, entry, rootPath, knownFiles, identities)
		if err := ctx.Err(); err != nil {
			return model.Request{}, err
		}
		if problem != "" {
			appendDiscoveryProblem(&discoveryProblems, problem)
			continue
		}
		key := compileContextDigest(command.compiler.Path, command.dir, command.args)
		group := contexts[key]
		if group == nil {
			if len(contexts) >= maxCompileContexts {
				appendDiscoveryProblem(&discoveryProblems, fmt.Sprintf("compile context limit %d exceeded", maxCompileContexts))
				continue
			}
			group = &compileContext{
				key: key, compiler: command.compiler, libclang: command.libclang,
				args: command.args, dir: command.dir, files: make(map[string]struct{}),
			}
			contexts[key] = group
		}
		group.files[sourceRel] = struct{}{}
		group.tests = group.tests || looksLikeTestTranslationUnit(sourceRel)
		if command.problem != "" {
			group.problem = command.problem
			group.unavailable = command.unavailable
		}
	}
	if len(contexts) == 0 {
		reason := "compile_commands.json contains no usable C/C++ translation units"
		if len(discoveryProblems) > 0 {
			reason += ": " + strings.Join(uniqueStrings(discoveryProblems), "; ")
		}
		return requestForContexts(ctx, view, rootURI, backend, epoch, []*compileContext{{problem: reason}})
	}
	ordered := make([]*compileContext, 0, len(contexts)+1)
	for _, group := range contexts {
		ordered = append(ordered, group)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].key < ordered[j].key })
	if len(discoveryProblems) > 0 {
		ordered = append(ordered, &compileContext{problem: "some compile commands could not be assigned to a complete immutable context: " + strings.Join(uniqueStrings(discoveryProblems), "; ")})
	}
	return requestForContexts(ctx, view, rootURI, backend, epoch, ordered)
}

type canonicalCompileCommand struct {
	compiler    model.ToolIdentity
	libclang    model.ToolIdentity
	args        []string
	dir         string
	problem     string
	unavailable bool
}

type toolPair struct {
	compiler    model.ToolIdentity
	libclang    model.ToolIdentity
	problem     string
	unavailable bool
}

func canonicalCommand(ctx context.Context, entry rawCompileCommand, root string, knownFiles map[string]string, identities map[string]toolPair) (canonicalCompileCommand, string, string) {
	return canonicalCommandWithPins(ctx, entry, root, knownFiles, identities, true)
}

func canonicalCommandWithPins(ctx context.Context, entry rawCompileCommand, root string, knownFiles map[string]string, identities map[string]toolPair, allowIdentify bool) (canonicalCompileCommand, string, string) {
	args := append([]string(nil), entry.Arguments...)
	if len(args) == 0 && strings.TrimSpace(entry.Command) != "" {
		var err error
		args, err = splitCommand(entry.Command)
		if err != nil {
			return canonicalCompileCommand{}, "", "compile command could not be parsed: " + err.Error()
		}
	}
	if len(args) < 2 || strings.TrimSpace(entry.File) == "" {
		return canonicalCompileCommand{}, "", "compile command omitted its compiler or source path"
	}
	directory := entry.Directory
	if directory == "" {
		directory = root
	} else if !filepath.IsAbs(directory) {
		directory = filepath.Join(root, directory)
	}
	directory, err := filepath.Abs(filepath.Clean(directory))
	if err != nil || !pathWithin(root, directory) {
		return canonicalCompileCommand{}, "", "compile command working directory is outside the captured workspace"
	}
	source := entry.File
	if !filepath.IsAbs(source) {
		source = filepath.Join(directory, source)
	}
	source, err = filepath.Abs(filepath.Clean(source))
	if err != nil || !pathWithin(root, source) {
		return canonicalCompileCommand{}, "", "translation unit is outside the captured workspace"
	}
	rel, err := filepath.Rel(root, source)
	if err != nil {
		return canonicalCompileCommand{}, "", "translation unit path could not be mapped"
	}
	rel = normalizeRel(rel)
	actualRel, known := knownFiles[canonicalRelKey(rel)]
	if !known {
		return canonicalCompileCommand{}, "", "translation unit is absent from the captured workspace: " + rel
	}
	rel = actualRel
	compilerPath, explicit := explicitCommandPath(args[0], directory)
	if !explicit {
		return canonicalCompileCommand{}, "", "compiler token is not an explicit path; PATH lookup is not allowed: " + args[0]
	}
	base := strings.ToLower(filepath.Base(compilerPath))
	if !strings.HasPrefix(base, "clang") || strings.Contains(base, "clang-cl") {
		return canonicalCompileCommand{}, "", "compile command does not name a supported clang driver: " + args[0]
	}
	canonicalArgs := normalizeBuildArgs(args, source, directory)
	directoryKey := normalizeRel(mustRel(root, directory))
	if directoryKey == "." {
		directoryKey = ""
	}
	pair, cached := identities[compilerPath]
	if !cached {
		if !allowIdentify {
			return canonicalCompileCommand{}, rel, "compile context does not match a pinned compiler identity: " + compilerPath
		}
		pair.compiler = model.ToolIdentity{Name: compilerToolName(compilerPath), Path: compilerPath}
		identifiedCompiler, identityErr := identifyCompiler(ctx, compilerPath)
		if identityErr != nil {
			pair.problem = "pinned compiler identity is unavailable: " + identityErr.Error()
			pair.unavailable = true
		} else {
			pair.compiler = identifiedCompiler
			libclangPath := filepath.Join(filepath.Dir(compilerPath), "libclang.dll")
			if !strings.EqualFold(filepath.Ext(compilerPath), ".exe") {
				libclangPath = filepath.Join(filepath.Dir(compilerPath), "libclang.so")
			}
			libclangHash, hashErr := fileSHA256(libclangPath)
			if hashErr != nil {
				pair.problem = "libclang beside the pinned compiler is unavailable: " + hashErr.Error()
				pair.unavailable = true
			} else {
				pair.libclang = model.ToolIdentity{Name: "libclang", Path: libclangPath, Version: pair.compiler.Version, SHA256: libclangHash}
			}
		}
		identities[compilerPath] = pair
	}
	if pair.problem != "" {
		return canonicalCompileCommand{compiler: pair.compiler, libclang: pair.libclang, args: canonicalArgs, dir: directoryKey, problem: pair.problem, unavailable: pair.unavailable}, rel, ""
	}
	compiler, libclang := pair.compiler, pair.libclang
	command := canonicalCompileCommand{compiler: compiler, libclang: libclang, args: canonicalArgs, dir: directoryKey}
	if hasPrecompiledInput(canonicalArgs) {
		command.problem = "precompiled header or module input is not available as captured source"
	}
	return command, rel, ""
}

func requestForContexts(ctx context.Context, view model.WorkspaceView, rootURI string, backend identity.BackendID, epoch identity.BackendEpoch, contexts []*compileContext) (model.Request, error) {
	request := model.Request{WorkspaceRootURI: rootURI, View: view, Scopes: make([]model.Scope, 0, len(contexts)), Provenance: make(map[string]model.Provenance, len(contexts))}
	usedIDs := make(map[string]bool)
	environment := captureCompilerEnvironment()
	type pinnedHeaderSet struct {
		version  string
		manifest string
	}
	versionByToolchain := make(map[string]pinnedHeaderSet)
	for _, group := range contexts {
		extractorVersion := ExtractorVersionPinned()
		if group.problem == "" {
			key := group.compiler.Path + "\x00" + group.libclang.Path
			if group.extractorVersion != "" && group.headerManifest != "" {
				extractorVersion = group.extractorVersion
				if cached, ok := versionByToolchain[key]; ok && (cached.version != group.extractorVersion || cached.manifest != group.headerManifest) {
					group.problem = "clang helper header manifest differs across one pinned toolchain"
					group.unavailable = true
				} else {
					versionByToolchain[key] = pinnedHeaderSet{version: group.extractorVersion, manifest: group.headerManifest}
				}
			} else if cached, ok := versionByToolchain[key]; ok {
				extractorVersion = cached.version
				group.extractorVersion, group.headerManifest = cached.version, cached.manifest
			} else {
				version, manifest, err := extractorVersionAndHeaderManifest(ctx, group.compiler.Path, group.libclang.Path, environment)
				if err != nil {
					if ctx.Err() != nil {
						return model.Request{}, ctx.Err()
					}
					group.problem = "clang helper header environment could not be pinned: " + err.Error()
					group.unavailable = true
				} else {
					manifestJSON, marshalErr := encodeHelperHeaderManifest(manifest)
					if marshalErr != nil {
						group.problem = "clang helper header manifest could not be canonicalized: " + marshalErr.Error()
						group.unavailable = true
					} else {
						extractorVersion = version
						group.extractorVersion, group.headerManifest = version, manifestJSON
						versionByToolchain[key] = pinnedHeaderSet{version: version, manifest: manifestJSON}
					}
				}
			}
		}
		idDigest := group.key
		if idDigest == "" {
			sum := sha256.Sum256([]byte(group.problem))
			idDigest = hex.EncodeToString(sum[:])
		}
		id := "clang-" + idDigest[:16]
		if usedIDs[id] {
			return model.Request{}, model.ErrInvalidScope
		}
		usedIDs[id] = true
		scope := model.Scope{ID: id, Language: "cpp", RootURI: rootURI, Build: model.BuildInputs{Options: make(map[string]string)}}
		tools := []model.ToolIdentity(nil)
		toolchain := "clang/unavailable"
		if group.problem != "" {
			state := "unknown"
			if group.unavailable {
				state = "unavailable"
			}
			scope.Build.Options["omnilsp.clang."+state] = group.problem
		} else {
			files := make([]string, 0, len(group.files))
			for file := range group.files {
				files = append(files, file)
			}
			sort.Strings(files)
			scope.Build.PackagePatterns = files
			scope.Build.Environment = environment
			scope.Build.Arguments = append([]string(nil), group.args...)
			scope.Build.Options[contextOption] = group.key
			scope.Build.Options[headerManifestOption] = group.headerManifest
			scope.Build.Tests = group.tests
			scope.Build.IncludePaths, scope.Build.Defines = extractBuildInputs(group.args)
			tools = []model.ToolIdentity{group.compiler, group.libclang}
			toolchain = "clang/" + group.compiler.Version
		}
		provenance := model.Provenance{
			SchemaVersion: model.SchemaVersion,
			Identity:      view.Identity(),
			Scope:         scope,
			Extractor:     ExtractorName,
			ExtractorVer:  extractorVersion,
			Backend:       backend,
			BackendEpoch:  epoch,
			Toolchain:     toolchain,
			Tools:         tools,
		}
		scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
		provenance.Scope = scope
		request.Scopes = append(request.Scopes, scope)
		request.Provenance[scope.ID] = provenance
	}
	return request, nil
}

func readRawCommands(ctx context.Context, view model.WorkspaceView, files []model.File) ([]rawCompileCommand, bool, error) {
	databases := make([]model.File, 0, 2)
	for _, file := range files {
		if strings.EqualFold(filepath.Base(uriPath(file.URI)), "compile_commands.json") {
			databases = append(databases, file)
		}
	}
	if len(databases) == 0 {
		return nil, false, nil
	}
	sort.Slice(databases, func(i, j int) bool {
		return pathDepth(uriPath(databases[i].URI)) < pathDepth(uriPath(databases[j].URI))
	})
	var entries []rawCompileCommand
	for _, database := range databases {
		if database.Size < 0 || database.Size > maxCompileDBBytes {
			return nil, true, fmt.Errorf("compile_commands.json exceeds the %d-byte indexing limit", maxCompileDBBytes)
		}
		reader, err := view.Read(ctx, database.URI)
		if err != nil {
			return nil, true, err
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, 32<<20))
		closeErr := reader.Close()
		if readErr != nil {
			return nil, true, readErr
		}
		if closeErr != nil {
			return nil, true, closeErr
		}
		var raw []rawCompileCommand
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, true, fmt.Errorf("clang semantic index: parse captured compile_commands.json: %w", err)
		}
		entries = append(entries, raw...)
	}
	return entries, true, nil
}

func appendDiscoveryProblem(problems *[]string, reason string) {
	if len(reason) > maxIssueBytes {
		reason = reason[:maxIssueBytes-3] + "..."
	}
	if len(*problems) < maxContextIssues {
		*problems = append(*problems, reason)
	} else if len(*problems) == maxContextIssues {
		*problems = append(*problems, "additional compile context discovery problems omitted")
	}
}

func identifyCompiler(ctx context.Context, path string) (model.ToolIdentity, error) {
	path, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return model.ToolIdentity{}, err
	}
	digest, err := fileSHA256(path)
	if err != nil {
		return model.ToolIdentity{}, err
	}
	versionCtx, cancel := context.WithTimeout(ctx, 10_000_000_000)
	defer cancel()
	cmd := exec.CommandContext(versionCtx, path, "--version")
	var stdout, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if versionCtx.Err() != nil {
			return model.ToolIdentity{}, versionCtx.Err()
		}
		return model.ToolIdentity{}, fmt.Errorf("run explicit clang driver --version: %w (%s)", err, stderr.String())
	}
	version := parseClangVersion(stdout.String())
	if version == "" {
		return model.ToolIdentity{}, errors.New("clang driver version output is unparseable")
	}
	return model.ToolIdentity{Name: compilerToolName(path), Path: path, Version: version, SHA256: digest}, nil
}

func explicitCommandPath(token, directory string) (string, bool) {
	if filepath.IsAbs(token) {
		return filepath.Clean(token), true
	}
	if filepath.Dir(token) == "." && !strings.ContainsAny(token, `/\`) {
		return "", false
	}
	return filepath.Clean(filepath.Join(directory, token)), true
}

func compilerToolName(path string) string {
	if strings.Contains(strings.ToLower(filepath.Base(path)), "clang++") {
		return "clang++"
	}
	return "clang"
}

func normalizeBuildArgs(args []string, source, directory string) []string {
	if len(args) == 0 {
		return nil
	}
	result := make([]string, 0, len(args)-1)
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "-c" || arg == "/c" || arg == "-M" || arg == "-MM" || arg == "-MD" || arg == "-MMD" {
			continue
		}
		if arg == "-o" || arg == "-MF" || arg == "-MT" || arg == "-MQ" || arg == "/Fo" || arg == "/Fe" || arg == "-MJ" {
			i++
			continue
		}
		if isSourceArg(arg, source, directory) {
			continue
		}
		if strings.HasPrefix(arg, "-o") && len(arg) > 2 || strings.HasPrefix(strings.ToLower(arg), "/fo") && len(arg) > 3 {
			continue
		}
		result = append(result, arg)
	}
	return result
}

func isSourceArg(arg, source, directory string) bool {
	if arg == source {
		return true
	}
	resolved := arg
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(directory, resolved)
	}
	left, leftErr := filepath.Abs(filepath.Clean(resolved))
	right, rightErr := filepath.Abs(filepath.Clean(source))
	return leftErr == nil && rightErr == nil && samePath(left, right)
}

func compileContextDigest(compiler, directory string, args []string) string {
	data, _ := json.Marshal(struct {
		Compiler  string
		Directory string
		Arguments []string
	}{compiler, filepath.ToSlash(directory), args})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func canonicalRelKey(rel string) string {
	return filepath.ToSlash(canonicalPath(filepath.FromSlash(normalizeRel(rel))))
}

func extractBuildInputs(args []string) ([]string, map[string]string) {
	var includes []string
	defines := make(map[string]string)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-I" || arg == "/I" || arg == "-isystem" || arg == "-iquote" || arg == "-idirafter" {
			if i+1 < len(args) {
				i++
				includes = append(includes, args[i])
			}
			continue
		}
		for _, prefix := range []string{"-I", "/I", "-isystem", "-iquote", "-idirafter"} {
			if strings.HasPrefix(arg, prefix) && len(arg) > len(prefix) {
				includes = append(includes, arg[len(prefix):])
				break
			}
		}
		value := ""
		found := false
		for _, prefix := range []string{"-D", "/D"} {
			if strings.HasPrefix(arg, prefix) && len(arg) > len(prefix) {
				value, found = strings.TrimPrefix(arg, prefix), true
				break
			}
		}
		if found {
			name, defineValue, hasValue := strings.Cut(value, "=")
			if hasValue {
				defines[name] = defineValue
			} else {
				defines[name] = ""
			}
		}
	}
	sort.Strings(includes)
	return uniqueStrings(includes), defines
}
