package plugin

import "testing"

func TestO5_APIVersionRangeNegotiation(t *testing.T) {
	if !APIVersionSupported(SupportedAPIVersion) {
		t.Fatal("preferred version must always be supported")
	}
	if APIVersionSupported("omnilsp.plugin.v99") {
		t.Fatal("unknown version must be refused")
	}
	// Simulate a transition window: host lists v1 and v2.
	old := SupportedAPIVersions
	SupportedAPIVersions = append(SupportedAPIVersions, "omnilsp.plugin.v2")
	defer func() { SupportedAPIVersions = old }()
	if !APIVersionSupported("omnilsp.plugin.v2") {
		t.Fatal("transition-window version should negotiate")
	}
}
