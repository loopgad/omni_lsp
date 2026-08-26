package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/conformance"
)

func TestRenderJSON(t *testing.T) {
	probes := []probe{
		{"version", statusPass, "omnilsp v" + version},
		{"config", statusWarn, "transport missing"},
		{"cache dir", statusFail, "not writable"},
		{"disk space", statusSkip, "platform probe pending"},
	}
	var buf bytes.Buffer
	if err := renderJSON(&buf, probes, nil); err != nil {
		t.Fatalf("renderJSON: %v", err)
	}

	var got struct {
		Schema      string                   `json:"schema"`
		Checks      []probe                  `json:"checks"`
		Conformance *conformance.CoreSummary `json:"conformance"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v\noutput:\n%s", err, buf.String())
	}
	if got.Schema != "omnilsp.doctor.v1" {
		t.Errorf("schema = %q, want %q", got.Schema, "omnilsp.doctor.v1")
	}
	if len(got.Checks) != len(probes) {
		t.Fatalf("got %d checks, want %d", len(got.Checks), len(probes))
	}
	for i, c := range got.Checks {
		if c != probes[i] {
			t.Errorf("check[%d] = %+v, want %+v", i, c, probes[i])
		}
	}
	for _, s := range []string{statusPass, statusWarn, statusFail, statusSkip} {
		if !strings.Contains(buf.String(), `"status": "`+s+`"`) {
			t.Errorf("output missing status %q:\n%s", s, buf.String())
		}
	}
}

func TestRenderJSONEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := renderJSON(&buf, nil, nil); err != nil {
		t.Fatalf("renderJSON(nil): %v", err)
	}
	var got doctorReport
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.Schema != "omnilsp.doctor.v1" {
		t.Errorf("schema = %q, want omnilsp.doctor.v1", got.Schema)
	}
}

func TestRenderText(t *testing.T) {
	probes := []probe{
		{"version", statusPass, "ok"},
		{"go toolchain", statusWarn, "missing"},
	}
	var buf bytes.Buffer
	renderText(&buf, probes)
	out := buf.String()
	for _, want := range []string{"PASS", "WARN", "version", "go toolchain"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
}
