package redfish

import "testing"

func TestBaseRouteName(t *testing.T) {
	cases := map[string]string{
		"RedfishV1Get":                        "RedfishV1Get",
		"RedfishV1Get_0":                      "RedfishV1Get",
		"RedfishV1SystemsGet_12":              "RedfishV1SystemsGet",
		"RedfishV1SystemsComputerSystemId_":   "RedfishV1SystemsComputerSystemId_",
		"RedfishV1SystemsComputerSystemIdGet": "RedfishV1SystemsComputerSystemIdGet",
	}
	for in, want := range cases {
		if got := baseRouteName(in); got != want {
			t.Errorf("baseRouteName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The generated set must cover the routes the BMC clients actually use, and
// must stay clear of stub-only operations.
func TestImplementedMethods(t *testing.T) {
	for _, name := range []string{
		"RedfishV1Get",
		"RedfishV1SessionServiceSessionsPost",
		"RedfishV1SystemsComputerSystemIdGet",
		"RedfishV1SystemsComputerSystemIdActionsComputerSystemResetPost",
	} {
		if !implementedMethods[name] {
			t.Errorf("implementedMethods missing %q; run 'make generate-implemented-routes'", name)
		}
	}
	if implementedMethods["RedfishV1MetadataGet"] {
		t.Errorf("RedfishV1MetadataGet is a 501 stub and must not be registered")
	}
}
