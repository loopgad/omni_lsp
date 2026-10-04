package pyright

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

const defaultPyrightConfigSentinel = "omnilsp-pyright-default-config-v1"

type pythonProjectFile struct {
	file    model.File
	path    string
	content []byte
}

// DiscoverPythonScopes finds pyrightconfig.json projects in the immutable
// workspace. A workspace with Python files and no project config gets one
// scope using Pyright's versioned default settings. TOML and unsupported
// config settings are retained as scopes that the exporter reports Unknown.
func DiscoverPythonScopes(ctx context.Context, view model.WorkspaceView, rootURI string, pythonPath string) ([]model.Scope, error) {
	if view == nil {
		return nil, model.ErrMissingView
	}
	if rootURI == "" {
		return nil, model.ErrInvalidScope
	}
	root, err := uri.Parse(rootURI)
	if err != nil {
		return nil, fmt.Errorf("parse Python workspace root: %w", err)
	}
	if _, err := root.Path(); err != nil {
		return nil, fmt.Errorf("resolve Python workspace root: %w", err)
	}
	var pythonFiles []pythonProjectFile
	var configFiles []pythonProjectFile
	var tomlFiles []pythonProjectFile
	if err := view.Walk(ctx, rootURI, func(file model.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		parsed, parseErr := uri.Parse(file.URI)
		if parseErr != nil {
			return parseErr
		}
		localPath, pathErr := parsed.Path()
		if pathErr != nil {
			return pathErr
		}
		if strings.EqualFold(filepath.Ext(localPath), ".py") || strings.EqualFold(filepath.Ext(localPath), ".pyi") {
			pythonFiles = append(pythonFiles, pythonProjectFile{file: file, path: localPath})
		}
		base := strings.ToLower(filepath.Base(localPath))
		if base != "pyrightconfig.json" && base != "pyproject.toml" {
			return nil
		}
		reader, readErr := view.Read(ctx, file.URI)
		if readErr != nil {
			return fmt.Errorf("read Python project config %q: %w", file.URI, readErr)
		}
		content, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			return fmt.Errorf("read Python project config %q: %w", file.URI, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close Python project config %q: %w", file.URI, closeErr)
		}
		entry := pythonProjectFile{file: file, path: localPath, content: content}
		if base == "pyrightconfig.json" {
			configFiles = append(configFiles, entry)
		} else {
			tomlFiles = append(tomlFiles, entry)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(pythonFiles) == 0 {
		return nil, nil
	}
	sort.Slice(configFiles, func(i, j int) bool { return configFiles[i].file.URI < configFiles[j].file.URI })
	sort.Slice(tomlFiles, func(i, j int) bool { return tomlFiles[i].file.URI < tomlFiles[j].file.URI })

	scopes := make([]model.Scope, 0, len(configFiles)+len(tomlFiles)+1)
	configuredRoots := make(map[string]bool)
	for _, config := range configFiles {
		projectRoot := filepath.Dir(config.path)
		projectURI := uri.FromPath(projectRoot).Canonical()
		if !uriWithinRoot(rootURI, projectURI) {
			return nil, fmt.Errorf("Python project config %q escapes requested root", config.file.URI)
		}
		build := model.BuildInputs{Options: map[string]string{}, IncludePaths: []string{"."}}
		setPythonInterpreter(&build, pythonPath)
		configDigest := digestPythonConfig(config.content)
		build.Options["pyrightConfig"] = filepath.Base(config.path)
		build.Options["pyrightConfigDigest"] = configDigest
		unsupported, options, includePaths := parseSupportedPyrightConfig(config.content)
		for key, value := range options {
			build.Options[key] = value
		}
		build.IncludePaths = append(build.IncludePaths, includePaths...)
		if unsupported != "" {
			build.Options["pyrightConfigUnsupported"] = unsupported
		}
		if build.Options["stubPath"] == "" {
			build.Options["stubPath"] = "none"
		}
		scope := model.Scope{ID: "python-config:" + config.file.URI, Language: langID, RootURI: projectURI, Build: build}
		scopes = append(scopes, scope)
		configuredRoots[scopeRootKey(projectURI)] = true
	}
	for _, config := range tomlFiles {
		projectRoot := filepath.Dir(config.path)
		projectURI := uri.FromPath(projectRoot).Canonical()
		if !uriWithinRoot(rootURI, projectURI) || configuredRoots[scopeRootKey(projectURI)] {
			continue
		}
		build := model.BuildInputs{Options: map[string]string{
			"pyrightConfig": filepath.Base(config.path), "pyrightConfigDigest": digestPythonConfig(config.content),
			"stubPath": "none", "pyrightConfigUnsupported": "Pyright settings in pyproject.toml are not decoded by this exporter",
		}, IncludePaths: []string{"."}}
		setPythonInterpreter(&build, pythonPath)
		scopes = append(scopes, model.Scope{ID: "python-config:" + config.file.URI, Language: langID, RootURI: projectURI, Build: build})
		configuredRoots[scopeRootKey(projectURI)] = true
	}
	needsDefault := len(scopes) == 0
	if !needsDefault {
		for _, file := range pythonFiles {
			owned := false
			for _, scope := range scopes {
				if uriWithinRoot(scope.RootURI, file.file.URI) {
					owned = true
					break
				}
			}
			if !owned {
				needsDefault = true
				break
			}
		}
	}
	if needsDefault {
		digest := sha256.Sum256([]byte(defaultPyrightConfigSentinel))
		build := model.BuildInputs{Options: map[string]string{
			"pyrightConfig": "<default>", "pyrightConfigDigest": "sha256:" + hex.EncodeToString(digest[:]), "stubPath": "none",
		}, IncludePaths: []string{"."}}
		setPythonInterpreter(&build, pythonPath)
		scopes = append(scopes, model.Scope{ID: "python-default:" + rootURI, Language: langID, RootURI: rootURI, Build: build})
	}
	for i := range scopes {
		if scopes[i].RootURI == "" || !uriWithinRoot(rootURI, scopes[i].RootURI) {
			return nil, model.ErrInvalidScope
		}
	}
	return scopes, nil
}

func setPythonInterpreter(build *model.BuildInputs, pythonPath string) {
	if build.Environment == nil {
		build.Environment = make(map[string]string)
	}
	if filepath.IsAbs(pythonPath) {
		build.Environment["pythonInterpreter"] = filepath.Clean(pythonPath)
	}
}

func digestPythonConfig(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func scopeRootKey(rootURI string) string {
	key := path.Clean(rootURI)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	return key
}

func parseSupportedPyrightConfig(content []byte) (string, map[string]string, []string) {
	var raw map[string]json.RawMessage
	cleaned := stripPythonJSONTrailingCommas(stripPythonJSONComments(string(content)))
	decoder := json.NewDecoder(strings.NewReader(cleaned))
	if err := decoder.Decode(&raw); err != nil || raw == nil {
		return "Pyright JSON config is not strict JSON/JSONC that this exporter can parse", nil, nil
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "Pyright JSON config contains trailing data", nil, nil
	}
	options := make(map[string]string)
	var unsupported []string
	allowed := map[string]bool{
		"pythonVersion": true, "pythonPlatform": true, "stubPath": true,
		"extraPaths": true, "typeCheckingMode": true, "strict": true,
		"include": true, "exclude": true,
	}
	for key, value := range raw {
		if !allowed[key] {
			unsupported = append(unsupported, key)
			continue
		}
		switch key {
		case "pythonVersion", "pythonPlatform", "stubPath", "typeCheckingMode":
			var parsed string
			if err := json.Unmarshal(value, &parsed); err != nil {
				unsupported = append(unsupported, key)
				continue
			}
			if key == "pythonVersion" && !validPythonVersion(parsed) {
				unsupported = append(unsupported, key)
				continue
			}
			if key == "pythonPlatform" && !validPythonPlatform(parsed) {
				unsupported = append(unsupported, key)
				continue
			}
			if key == "typeCheckingMode" && parsed != "off" && parsed != "basic" && parsed != "standard" && parsed != "strict" {
				unsupported = append(unsupported, key)
				continue
			}
			if key == "stubPath" && !safeRelativePythonPath(parsed) {
				unsupported = append(unsupported, key)
				continue
			}
			if key != "typeCheckingMode" {
				options[key] = parsed
			}
		case "strict":
			var paths []string
			if err := json.Unmarshal(value, &paths); err != nil {
				unsupported = append(unsupported, key)
				continue
			}
			for _, candidate := range paths {
				if !safeRelativePythonPath(candidate) {
					unsupported = append(unsupported, key)
					break
				}
			}
		case "include", "exclude":
			paths, ok := parsePyrightFileSpecs(value)
			if !ok {
				unsupported = append(unsupported, key)
				continue
			}
			encoded, _ := json.Marshal(paths)
			option := "pyright" + strings.ToUpper(key[:1]) + key[1:]
			options[option] = string(encoded)
		case "extraPaths":
			var paths []string
			if err := json.Unmarshal(value, &paths); err != nil {
				unsupported = append(unsupported, key)
				continue
			}
			for _, candidate := range paths {
				if !safeRelativePythonPath(candidate) {
					unsupported = append(unsupported, key)
					paths = nil
					break
				}
			}
			if len(paths) != 0 {
				encoded, _ := json.Marshal(paths)
				options["extraPaths"] = string(encoded)
			}
		}
	}
	sort.Strings(unsupported)
	reason := ""
	if len(unsupported) != 0 {
		reason = "unsupported Pyright config settings: " + strings.Join(uniquePythonStrings(unsupported), ", ")
	}
	var extraPaths []string
	if encoded := options["extraPaths"]; encoded != "" {
		_ = json.Unmarshal([]byte(encoded), &extraPaths)
	}
	return reason, options, extraPaths
}

// parsePyrightFileSpecs accepts only relative FileSpec paths. Pyright 1.1.414
// remains the authority for wildcard matching; this check only prevents bad
// shapes and paths that could escape the materialized project root.
func parsePyrightFileSpecs(value json.RawMessage) ([]string, bool) {
	trimmed := strings.TrimSpace(string(value))
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var paths []string
	if err := json.Unmarshal(value, &paths); err != nil || paths == nil {
		return nil, false
	}
	for _, candidate := range paths {
		if !safeRelativePythonPath(candidate) {
			return nil, false
		}
	}
	return paths, true
}

func validPythonVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	for _, part := range parts {
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return false
			}
		}
	}
	return true
}

func validPythonPlatform(value string) bool {
	switch value {
	case "Windows", "Linux", "Darwin", "All":
		return true
	default:
		return false
	}
}

func safeRelativePythonPath(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || filepath.IsAbs(value) || strings.Contains(value, ":") {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(value))
	return clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func uniquePythonStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

// stripPythonJSONComments removes JSONC comments while preserving quoted text.
func stripPythonJSONComments(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	inString, escaped, lineComment, blockComment := false, false, false, false
	for i := 0; i < len(value); i++ {
		current := value[i]
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
			if current == '*' && i+1 < len(value) && value[i+1] == '/' {
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
		if current == '/' && i+1 < len(value) && value[i+1] == '/' {
			lineComment = true
			out.WriteString("  ")
			i++
			continue
		}
		if current == '/' && i+1 < len(value) && value[i+1] == '*' {
			blockComment = true
			out.WriteString("  ")
			i++
			continue
		}
		out.WriteByte(current)
	}
	return out.String()
}

func stripPythonJSONTrailingCommas(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	inString, escaped := false, false
	for i := 0; i < len(value); i++ {
		current := value[i]
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
		if current == ',' {
			next := i + 1
			for next < len(value) && (value[next] == ' ' || value[next] == '\t' || value[next] == '\r' || value[next] == '\n') {
				next++
			}
			if next < len(value) && (value[next] == '}' || value[next] == ']') {
				continue
			}
		}
		out.WriteByte(current)
	}
	return out.String()
}
