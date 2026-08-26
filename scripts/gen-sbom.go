//go:build ignore

// gen-sbom emits a CycloneDX 1.5 JSON SBOM derived from go.mod + go.sum.
// Zero-dependency by design (N15): run with
//
//	go run scripts/gen-sbom.go -o sbom.cdx.json [module-root]
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type cdxBOM struct {
	BOMFormat   string    `json:"bomFormat"`
	SpecVersion string    `json:"specVersion"`
	Serial      string    `json:"serialNumber"`
	Version     int       `json:"version"`
	Metadata    cdxMeta   `json:"metadata"`
	Components  []cdxComp `json:"components"`
}

type cdxMeta struct {
	Timestamp string    `json:"timestamp"`
	Tools     []cdxComp `json:"tools"`
}

type cdxComp struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	PURL    string `json:"purl,omitempty"`
	Scope   string `json:"scope,omitempty"`
	Hashes  []struct {
		Alg   string `json:"alg"`
		Value string `json:"content"`
	} `json:"hashes,omitempty"`
}

func main() {
	out := flag.String("o", "sbom.cdx.json", "output path")
	root := flag.String("root", ".", "module root")
	flag.Parse()

	gomod, err := os.ReadFile(filepath.Join(*root, "go.mod"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "read go.mod:", err)
		os.Exit(1)
	}
	module, requires := parseGoMod(string(gomod))

	sums := readSums(filepath.Join(*root, "go.sum"))

	comps := make([]cdxComp, 0, len(requires))
	for _, req := range requires {
		scope := "required"
		if req.indirect {
			scope = "excluded" // transitive chain, documented per U5 whitelist
		}
		c := cdxComp{
			Type:    "library",
			Name:    req.path,
			Version: req.version,
			PURL:    fmt.Sprintf("pkg:golang/%s@%s", req.path, req.version),
			Scope:   scope,
		}
		if h, ok := sums[req.path+"@"+req.version]; ok {
			c.Hashes = []struct {
				Alg   string `json:"alg"`
				Value string `json:"content"`
			}{{Alg: "SHA-256", Value: h.content}}
		}
		comps = append(comps, c)
	}

	bom := cdxBOM{
		BOMFormat:   "CycloneDX",
		SpecVersion: "1.5",
		Serial:      serialFor(module.path, module.version),
		Version:     1,
		Metadata: cdxMeta{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Tools: []cdxComp{{
				Type: "application", Name: "omnilsp-gen-sbom", Version: "v1",
			}},
		},
		Components: comps,
	}
	data, err := json.MarshalIndent(bom, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if dbg := os.Getenv("SBOM_DEBUG"); dbg != "" {
		sample := ""
		for k, v := range sums {
			sample = fmt.Sprintf("%s -> %s...", k, v.content[:12])
			break
		}
		fmt.Fprintf(os.Stderr, "debug: module=%q requires=%d sums=%d sample=%s\n",
			module.path, len(requires), len(sums), sample)
	}
	fmt.Printf("SBOM written: %s (%d components)\n", *out, len(comps))
}

func serialFor(name, ver string) string {
	h := sha256.Sum256([]byte(name + "@" + ver + time.Now().UTC().Format("20060102")))
	return "urn:uuid:" + hex.EncodeToString(h[:16])
}

type require struct {
	path, version string
	indirect      bool
}

type moduleInfo struct{ path, version string }

func parseGoMod(text string) (moduleInfo, []require) {
	var mod moduleInfo
	var reqs []require
	inBlock := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "module "):
			mod.path = strings.TrimSpace(strings.TrimPrefix(line, "module "))
		case strings.HasPrefix(line, "require ("):
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case strings.HasPrefix(line, "require "):
			f := strings.Fields(strings.TrimPrefix(line, "require "))
			if len(f) >= 2 {
				reqs = append(reqs, require{path: f[0], version: f[1], indirect: strings.Contains(line, "// indirect")})
			}
		case inBlock && line != "" && !strings.HasPrefix(line, "//"):
			f := strings.Fields(line)
			if len(f) >= 2 {
				reqs = append(reqs, require{path: f[0], version: f[1], indirect: strings.Contains(line, "// indirect")})
			}
		}
	}
	return mod, reqs
}

type sumEntry struct {
	alg, content string
}

func readSums(path string) map[string]sumEntry {
	out := map[string]sumEntry{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// go.sum line: <module-path> <version> h1:<base64> — hash in field 3,
		// map key reassembled as path@version to match require entries.
		fields := strings.Fields(sc.Text())
		if len(fields) == 3 && strings.HasPrefix(fields[2], "h1:") {
			raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(fields[2], "h1:"))
			if err != nil {
				continue
			}
			out[fields[0]+"@"+fields[1]] = sumEntry{alg: "SHA-256", content: hex.EncodeToString(raw)}
		}
	}
	return out
}
