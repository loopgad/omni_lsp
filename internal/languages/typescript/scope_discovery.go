package typescript

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

// DiscoverScopes finds tsconfig.json and jsconfig.json files in the immutable
// workspace view. It records the config bytes, effective compiler options
// supplied by that config, project references, and plugin declarations in the
// scope build identity. It never consults the mutable filesystem.
func DiscoverScopes(ctx context.Context, view model.WorkspaceView, rootURI string) ([]model.Scope, error) {
	if view == nil {
		return nil, model.ErrMissingView
	}
	if rootURI == "" {
		return nil, model.ErrInvalidScope
	}
	configs := make(map[string]discoveredConfigFile)
	manifests := make(map[string]model.File)
	paths := make(map[string]string)
	if err := view.Walk(ctx, rootURI, func(file model.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		underNodeModules := false
		parsedURI, err := uri.Parse(file.URI)
		if err == nil {
			if localPath, pathErr := parsedURI.Path(); pathErr == nil {
				underNodeModules = isNodeModulesPath(localPath)
				key := configPathKey(localPath)
				if prior, exists := paths[key]; exists && prior != file.URI {
					return fmt.Errorf("workspace URIs %q and %q map to the same TypeScript config path", prior, file.URI)
				}
				paths[key] = file.URI
			}
		}
		name := strings.ToLower(filepath.Base(file.URI))
		switch name {
		case "package.json", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml", "bun.lock", "bun.lockb":
			manifests[file.URI] = file
		}
		if (name != "tsconfig.json" && name != "jsconfig.json") || underNodeModules {
			return nil
		}
		reader, err := view.Read(ctx, file.URI)
		if err != nil {
			return fmt.Errorf("read TypeScript project config %q: %w", file.URI, err)
		}
		content, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			return fmt.Errorf("read TypeScript project config %q: %w", file.URI, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close TypeScript project config %q: %w", file.URI, closeErr)
		}
		configs[file.URI] = discoveredConfigFile{uri: file.URI, content: content}
		return nil
	}); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(configs))
	for key := range configs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	scopes := make([]model.Scope, 0, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		config := configs[key]
		parsed, err := parseTypeScriptConfig(config.content)
		if err != nil {
			return nil, fmt.Errorf("parse TypeScript project config %q: %w", config.uri, err)
		}
		configURI, err := uri.Parse(config.uri)
		if err != nil {
			return nil, fmt.Errorf("parse TypeScript project config URI %q: %w", config.uri, err)
		}
		configPath, err := configURI.Path()
		if err != nil {
			return nil, fmt.Errorf("resolve TypeScript project config URI %q: %w", config.uri, err)
		}
		rootPath := filepath.Dir(configPath)
		rootFileURI := uri.FromPath(rootPath).Canonical()
		if !uriWithinRoot(rootURI, rootFileURI) {
			return nil, fmt.Errorf("TypeScript project config %q escapes requested root", config.uri)
		}
		name := strings.ToLower(filepath.Base(config.uri))
		language := "typescript"
		configKey, digestKey := "tsconfig", "tsconfigDigest"
		if name == "jsconfig.json" {
			language, configKey, digestKey = "javascript", "jsconfig", "jsconfigDigest"
		}
		digest := sha256.Sum256(config.content)
		compilerOptions, err := resolveCompilerOptionsClosure(ctx, view, rootURI, rootPath, config, paths)
		if err != nil {
			return nil, fmt.Errorf("resolve TypeScript config closure %q: %w", config.uri, err)
		}
		compilerOptionsJSON, err := json.Marshal(compilerOptions)
		if err != nil {
			return nil, fmt.Errorf("encode TypeScript config closure %q: %w", config.uri, err)
		}
		options := map[string]string{
			configKey:             filepath.Base(config.uri),
			"projectConfigDigest": "sha256:" + hex.EncodeToString(digest[:]),
			digestKey:             "sha256:" + hex.EncodeToString(digest[:]),
			"compilerOptions":     string(compilerOptionsJSON),
			"projectReferences":   string(parsed.projectReferences),
			"plugins":             string(parsed.plugins),
			"packageGraph":        packageGraphDigest(rootURI, manifests),
		}
		build := model.BuildInputs{
			IncludePaths:    append([]string(nil), parsed.include...),
			PackagePatterns: append([]string(nil), parsed.files...),
			Options:         options,
		}
		scopes = append(scopes, model.Scope{
			ID:       "typescript-config:" + config.uri,
			Language: language,
			RootURI:  rootFileURI,
			Build:    build,
		})
	}
	return scopes, nil
}

func packageGraphDigest(rootURI string, manifests map[string]model.File) string {
	keys := make([]string, 0, len(manifests))
	for fileURI := range manifests {
		if uriWithinRoot(rootURI, fileURI) {
			keys = append(keys, fileURI)
		}
	}
	sort.Strings(keys)
	values := make(map[string]string, len(keys))
	for _, fileURI := range keys {
		values[fileURI] = string(manifests[fileURI].SHA256)
	}
	return string(canonicalJSON(values))
}

type parsedTypeScriptConfig struct {
	compilerOptions   []byte
	projectReferences []byte
	plugins           []byte
	extends           []string
	include           []string
	files             []string
}

type discoveredConfigFile struct {
	uri     string
	content []byte
}

type compilerOptionsClosure struct {
	Schema   string                `json:"schema"`
	Complete bool                  `json:"complete"`
	Reason   string                `json:"reason,omitempty"`
	Configs  []compilerConfigInput `json:"configs"`
}

type compilerConfigInput struct {
	Path            string          `json:"path"`
	Content         string          `json:"content"`
	CompilerOptions json.RawMessage `json:"compilerOptions"`
}

func parseTypeScriptConfig(content []byte) (parsedTypeScriptConfig, error) {
	cleaned := stripJSONCommentsAndTrailingCommas(content)
	var raw map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(cleaned))
	if err := decoder.Decode(&raw); err != nil {
		return parsedTypeScriptConfig{}, err
	}
	compilerOptions := json.RawMessage(`{}`)
	var compilerOptionsObject map[string]json.RawMessage
	if value := raw["compilerOptions"]; len(value) != 0 {
		if err := json.Unmarshal(value, &compilerOptionsObject); err != nil {
			return parsedTypeScriptConfig{}, fmt.Errorf("compilerOptions: %w", err)
		}
		compilerOptions = canonicalJSON(compilerOptionsObject)
	}
	projectReferences := canonicalJSON([]any{})
	if value := raw["references"]; len(value) != 0 {
		var references any
		if err := json.Unmarshal(value, &references); err != nil {
			return parsedTypeScriptConfig{}, fmt.Errorf("references: %w", err)
		}
		projectReferences = canonicalJSON(references)
	}
	plugins := canonicalJSON([]any{})
	if value := raw["plugins"]; len(value) != 0 {
		var configured any
		if err := json.Unmarshal(value, &configured); err != nil {
			return parsedTypeScriptConfig{}, fmt.Errorf("plugins: %w", err)
		}
		plugins = canonicalJSON(configured)
	} else if compilerOptionsObject != nil {
		if pluginValue := compilerOptionsObject["plugins"]; len(pluginValue) != 0 {
			var configured any
			if err := json.Unmarshal(pluginValue, &configured); err != nil {
				return parsedTypeScriptConfig{}, fmt.Errorf("compilerOptions.plugins: %w", err)
			}
			plugins = canonicalJSON(configured)
		}
	}
	extends, err := stringOrStringArray(raw["extends"])
	if err != nil {
		return parsedTypeScriptConfig{}, fmt.Errorf("extends: %w", err)
	}
	include := stringArray(raw["include"])
	files := stringArray(raw["files"])
	return parsedTypeScriptConfig{
		compilerOptions: compilerOptions, projectReferences: projectReferences,
		plugins: plugins, extends: extends, include: include, files: files,
	}, nil
}

func stringOrStringArray(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return []string{value}, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func resolveCompilerOptionsClosure(
	ctx context.Context,
	view model.WorkspaceView,
	workspaceRootURI string,
	projectRootPath string,
	root discoveredConfigFile,
	workspacePaths map[string]string,
) (compilerOptionsClosure, error) {
	closure := compilerOptionsClosure{Schema: "omnilsp-tsconfig-v1", Complete: true}
	active := make(map[string]bool)
	seen := make(map[string]bool)
	var visit func(discoveredConfigFile) error
	visit = func(current discoveredConfigFile) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := current.uri
		if active[key] {
			closure.Complete = false
			closure.Reason = "TypeScript config extends chain contains a cycle"
			return nil
		}
		if seen[key] {
			return nil
		}
		active[key] = true
		parsed, err := parseTypeScriptConfig(current.content)
		if err != nil {
			closure.Complete = false
			closure.Reason = "an extended TypeScript config could not be parsed"
			delete(active, key)
			return nil
		}
		currentURI, err := uri.Parse(current.uri)
		if err != nil {
			delete(active, key)
			return err
		}
		currentPath, err := currentURI.Path()
		if err != nil {
			delete(active, key)
			return err
		}
		for _, reference := range parsed.extends {
			parentURI, resolveErr := resolveExtendedConfigPath(currentPath, reference, workspaceRootURI, workspacePaths)
			if resolveErr != nil {
				closure.Complete = false
				if closure.Reason == "" {
					closure.Reason = resolveErr.Error()
				}
				continue
			}
			reader, readErr := view.Read(ctx, parentURI)
			if readErr != nil {
				closure.Complete = false
				if closure.Reason == "" {
					closure.Reason = "an extended TypeScript config is absent from the immutable workspace view"
				}
				continue
			}
			content, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			if err := visit(discoveredConfigFile{uri: parentURI, content: content}); err != nil {
				return err
			}
		}
		relative, err := filepath.Rel(projectRootPath, currentPath)
		if err != nil {
			delete(active, key)
			return err
		}
		closure.Configs = append(closure.Configs, compilerConfigInput{
			Path: filepath.ToSlash(relative), Content: string(current.content),
			CompilerOptions: append(json.RawMessage(nil), parsed.compilerOptions...),
		})
		delete(active, key)
		seen[key] = true
		return nil
	}
	if err := visit(root); err != nil {
		return compilerOptionsClosure{}, err
	}
	return closure, nil
}

func resolveExtendedConfigPath(configPath, reference, workspaceRootURI string, workspacePaths map[string]string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", fmt.Errorf("empty TypeScript extends reference")
	}
	if !filepath.IsAbs(reference) && !strings.HasPrefix(reference, ".") {
		return resolvePackageExtendedConfigPath(configPath, reference, workspaceRootURI, workspacePaths)
	}
	candidate := reference
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(filepath.Dir(configPath), filepath.FromSlash(candidate))
	}
	candidate = filepath.Clean(candidate)
	candidates := []string{candidate}
	if filepath.Ext(candidate) == "" {
		candidates = append(candidates, candidate+".json", filepath.Join(candidate, "tsconfig.json"))
	}
	for _, candidate := range candidates {
		canonical := uri.FromPath(candidate).Canonical()
		if !uriWithinRoot(workspaceRootURI, canonical) {
			continue
		}
		if fileURI, ok := workspacePaths[configPathKey(candidate)]; ok {
			return fileURI, nil
		}
	}
	return "", fmt.Errorf("TypeScript extends %q is not present in the immutable workspace view", reference)
}

// resolvePackageExtendedConfigPath supports package-based extends when the
// compiler-resolved JSON config is present at a conventional node_modules
// location in the immutable workspace. TypeScript validates the actual
// resolution and effective options during export; unsupported package exports
// or layouts remain an incomplete closure.
func resolvePackageExtendedConfigPath(configPath, reference, workspaceRootURI string, workspacePaths map[string]string) (string, error) {
	if filepath.IsAbs(reference) || strings.Contains(reference, "\\") {
		return "", fmt.Errorf("package-based TypeScript extends %q has an unsupported path", reference)
	}
	for _, part := range strings.Split(reference, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("package-based TypeScript extends %q has an unsupported path", reference)
		}
	}
	rootURI, err := uri.Parse(workspaceRootURI)
	if err != nil {
		return "", err
	}
	rootPath, err := rootURI.Path()
	if err != nil {
		return "", err
	}
	rootPath = filepath.Clean(rootPath)
	base := filepath.Clean(filepath.Dir(configPath))
	for {
		if !uriWithinRoot(workspaceRootURI, uri.FromPath(base).Canonical()) {
			break
		}
		candidate := filepath.Join(base, "node_modules", filepath.FromSlash(reference))
		candidates := []string{candidate}
		if filepath.Ext(candidate) == "" {
			candidates = append(candidates, candidate+".json", filepath.Join(candidate, "tsconfig.json"))
		}
		for _, candidate := range candidates {
			candidate = filepath.Clean(candidate)
			canonical := uri.FromPath(candidate).Canonical()
			if !uriWithinRoot(workspaceRootURI, canonical) {
				continue
			}
			if fileURI, ok := workspacePaths[configPathKey(candidate)]; ok {
				return fileURI, nil
			}
		}
		if base == rootPath {
			break
		}
		parent := filepath.Dir(base)
		if parent == base {
			break
		}
		base = parent
	}
	return "", fmt.Errorf("package-based TypeScript extends %q is not present in the immutable workspace view", reference)
}

func configPathKey(value string) string {
	key := filepath.Clean(value)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key
}

func isNodeModulesPath(value string) bool {
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(value)), "/") {
		if part == "node_modules" || (runtime.GOOS == "windows" && strings.EqualFold(part, "node_modules")) {
			return true
		}
	}
	return false
}

func canonicalJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte("null")
	}
	return encoded
}

func stringArray(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	return values
}

// stripJSONCommentsAndTrailingCommas handles the JSONC accepted by
// tsconfig/jsconfig while preserving string contents and line structure. It
// is only used for project metadata; source semantics stay in TypeScript.
func stripJSONCommentsAndTrailingCommas(content []byte) []byte {
	var out bytes.Buffer
	inString, escaped, lineComment, blockComment := false, false, false, false
	for i := 0; i < len(content); i++ {
		current := content[i]
		if lineComment {
			if current == '\n' {
				lineComment = false
				out.WriteByte(current)
			} else {
				out.WriteByte(' ')
			}
			continue
		}
		if blockComment {
			if current == '*' && i+1 < len(content) && content[i+1] == '/' {
				blockComment = false
				out.WriteString("  ")
				i++
			} else if current == '\n' {
				out.WriteByte('\n')
			} else {
				out.WriteByte(' ')
			}
			continue
		}
		if inString {
			out.WriteByte(current)
			if escaped {
				escaped = false
			} else if current == '\\' {
				escaped = true
			} else if current == '"' {
				inString = false
			}
			continue
		}
		if current == '"' {
			inString = true
			out.WriteByte(current)
			continue
		}
		if current == '/' && i+1 < len(content) && content[i+1] == '/' {
			lineComment = true
			out.WriteString("  ")
			i++
			continue
		}
		if current == '/' && i+1 < len(content) && content[i+1] == '*' {
			blockComment = true
			out.WriteString("  ")
			i++
			continue
		}
		out.WriteByte(current)
	}
	value := out.Bytes()
	for i := 0; i+1 < len(value); i++ {
		if value[i] != ',' {
			continue
		}
		j := i + 1
		for j < len(value) && (value[j] == ' ' || value[j] == '\t' || value[j] == '\r' || value[j] == '\n') {
			j++
		}
		if j < len(value) && (value[j] == '}' || value[j] == ']') {
			value[i] = ' '
		}
	}
	return value
}
