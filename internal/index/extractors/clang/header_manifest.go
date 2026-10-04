package clang

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const headerManifestOption = "omnilsp.clang.helperHeaders"

type helperHeaderIdentity struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func helperDependencyContentDigest(paths []string) (string, error) {
	_, digest, err := helperDependencyContentManifest(context.Background(), paths)
	return digest, err
}

func helperDependencyContentManifest(ctx context.Context, paths []string) ([]helperHeaderIdentity, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(paths) > maxHelperHeaders {
		return nil, "", errors.New("clang helper header count exceeded its bounded limit")
	}
	canonicalPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		abs, err := filepath.Abs(filepath.Clean(path))
		if err != nil {
			return nil, "", fmt.Errorf("resolve clang helper header path %q: %w", path, err)
		}
		canonicalPaths = append(canonicalPaths, abs)
	}
	sort.Slice(canonicalPaths, func(i, j int) bool { return canonicalPath(canonicalPaths[i]) < canonicalPath(canonicalPaths[j]) })

	manifest := make([]helperHeaderIdentity, 0, len(canonicalPaths))
	seen := make(map[string]struct{}, len(canonicalPaths))
	remaining := int64(maxHelperHeaderBytes)
	for _, path := range canonicalPaths {
		key := canonicalPath(path)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		identity, size, err := hashRegularFile(ctx, path, remaining)
		if err != nil {
			return nil, "", fmt.Errorf("hash clang helper header %q: %w", path, err)
		}
		remaining -= size
		manifest = append(manifest, helperHeaderIdentity{Path: path, SHA256: identity})
	}
	digest, err := digestHelperHeaderManifest(manifest)
	if err != nil {
		return nil, "", err
	}
	return manifest, digest, nil
}

func digestHelperHeaderManifest(manifest []helperHeaderIdentity) (string, error) {
	if len(manifest) == 0 || len(manifest) > maxHelperHeaders {
		return "", errors.New("clang helper header manifest is empty or exceeds its bounded limit")
	}
	hash := sha256.New()
	previous := ""
	for _, header := range manifest {
		if !filepath.IsAbs(header.Path) || filepath.Clean(header.Path) != header.Path || len(header.SHA256) != sha256.Size*2 {
			return "", errors.New("clang helper header manifest contains an invalid path or SHA-256")
		}
		if _, err := hex.DecodeString(header.SHA256); err != nil || strings.ToLower(header.SHA256) != header.SHA256 {
			return "", errors.New("clang helper header manifest contains an invalid SHA-256")
		}
		key := canonicalPath(header.Path)
		if previous != "" && key <= previous {
			return "", errors.New("clang helper header manifest is not uniquely sorted")
		}
		previous = key
		_, _ = io.WriteString(hash, key+"\x00"+header.SHA256+"\n")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func encodeHelperHeaderManifest(manifest []helperHeaderIdentity) (string, error) {
	if _, err := digestHelperHeaderManifest(manifest); err != nil {
		return "", err
	}
	data, err := json.Marshal(manifest)
	return string(data), err
}

func decodeHelperHeaderManifest(raw string) ([]helperHeaderIdentity, string, error) {
	var manifest []helperHeaderIdentity
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, "", err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, "", errors.New("clang helper header manifest contains trailing JSON")
	}
	digest, err := digestHelperHeaderManifest(manifest)
	if err != nil {
		return nil, "", err
	}
	canonical, err := json.Marshal(manifest)
	if err != nil {
		return nil, "", err
	}
	if string(canonical) != raw {
		return nil, "", errors.New("clang helper header manifest is not canonical JSON")
	}
	return manifest, digest, nil
}

func verifyHelperHeaderManifest(ctx context.Context, manifest []helperHeaderIdentity) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := digestHelperHeaderManifest(manifest); err != nil {
		return err
	}
	remaining := int64(maxHelperHeaderBytes)
	for _, expected := range manifest {
		if err := ctx.Err(); err != nil {
			return err
		}
		actual, size, err := hashRegularFile(ctx, expected.Path, remaining)
		if err != nil {
			return fmt.Errorf("verify clang helper header %q: %w", expected.Path, err)
		}
		remaining -= size
		if actual != expected.SHA256 {
			return fmt.Errorf("clang helper header SHA-256 changed: %s", expected.Path)
		}
	}
	return nil
}

func hashRegularFile(ctx context.Context, path string, limit int64) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return "", 0, statErr
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
		_ = file.Close()
		return "", 0, errors.New("file is non-regular or exceeds the header byte limit")
	}
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return "", 0, err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			total += int64(read)
			if total > limit || total > info.Size() {
				_ = file.Close()
				return "", 0, errors.New("file grew beyond its captured or allowed size")
			}
			_, _ = hash.Write(buffer[:read])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = file.Close()
			return "", 0, readErr
		}
	}
	closeErr := file.Close()
	if total != info.Size() {
		return "", total, errors.New("file size changed while hashing")
	}
	if closeErr != nil {
		return "", total, closeErr
	}
	return hex.EncodeToString(hash.Sum(nil)), total, nil
}
