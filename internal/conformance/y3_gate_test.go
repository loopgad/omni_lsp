package conformance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestY39_SBOMRegeneratesFromGoSum executes the Y3-9 supply-chain gate
// (registry.go: `go run scripts/gen-sbom.go -o sbom.cdx.json .`) against a
// scratch output so the check never dirties the tree, then pins the CycloneDX
// 1.5 contract the N15 evidence depends on: format/spec/serial present, at
// least one component carrying a pkg:golang purl, and the generator identity
// in metadata. A green run here is the AUTO-probe form of the gate; the
// generated artifact itself stays per-run evidence (sbom.cdx.json is
// intentionally not committed — it embeds a wall-clock timestamp).
func TestY39_SBOMRegeneratesFromGoSum(t *testing.T) {
	root := moduleRoot()
	out := filepath.Join(t.TempDir(), "sbom.cdx.json")
	cmd := exec.Command("go", "run", "scripts/gen-sbom.go", "-o", out, ".")
	cmd.Dir = root
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gen-sbom gate failed: %v\n%s", err, truncate(string(outBytes), 400))
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read generated SBOM: %v", err)
	}
	var bom struct {
		BOMFormat   string `json:"bomFormat"`
		SpecVersion string `json:"specVersion"`
		Serial      string `json:"serialNumber"`
		Version     int    `json:"version"`
		Metadata    struct {
			Timestamp string `json:"timestamp"`
			Tools     []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"metadata"`
		Components []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			PURL  string `json:"purl"`
			Scope string `json:"scope"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &bom); err != nil {
		t.Fatalf("decode generated SBOM: %v", err)
	}
	if bom.BOMFormat != "CycloneDX" || bom.SpecVersion != "1.5" || bom.Version != 1 {
		t.Fatalf("SBOM header mismatch: format=%q spec=%q version=%d", bom.BOMFormat, bom.SpecVersion, bom.Version)
	}
	if !strings.HasPrefix(bom.Serial, "urn:uuid:") {
		t.Errorf("serialNumber %q is not a urn:uuid", bom.Serial)
	}
	if bom.Metadata.Timestamp == "" {
		t.Error("metadata.timestamp missing")
	}
	if len(bom.Metadata.Tools) == 0 || bom.Metadata.Tools[0].Name != "omnilsp-gen-sbom" {
		t.Errorf("metadata.tools does not identify the generator: %+v", bom.Metadata.Tools)
	}
	if len(bom.Components) == 0 {
		t.Fatal("SBOM carries zero components; go.sum derivation is broken")
	}
	purlOK := 0
	for _, c := range bom.Components {
		if c.Type != "library" || !strings.HasPrefix(c.PURL, "pkg:golang/") {
			t.Errorf("component %+v is not a library with a pkg:golang purl", c)
			continue
		}
		if c.Scope != "required" && c.Scope != "excluded" {
			t.Errorf("component %s has unknown scope %q", c.Name, c.Scope)
		}
		purlOK++
	}
	if purlOK == 0 {
		t.Error("no component carries a pkg:golang purl")
	}
}
