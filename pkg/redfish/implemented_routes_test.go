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

// The service root must advertise every implemented top-level link and stay
// clear of links whose backing collection GET is a stub.
func TestServiceRootLinks(t *testing.T) {
	for _, field := range []string{"Systems", "Managers", "SessionService"} {
		if _, ok := serviceRootLinks[field]; !ok {
			t.Errorf("serviceRootLinks missing %q; run 'make generate-implemented-routes'", field)
		}
	}
	for _, field := range []string{"Chassis", "Tasks", "AccountService", "UpdateService"} {
		if _, ok := serviceRootLinks[field]; ok {
			t.Errorf("serviceRootLinks advertises %q but its collection GET is not implemented", field)
		}
	}
}

// The service root payload must carry exactly the advertised links plus the
// schema-mandatory Links.Sessions, and must not resurrect pruned links as
// empty objects.
func TestGetServiceRootPayload(t *testing.T) {
	root := NewHandler("user", "pass", nil).GetServiceRoot()
	for field := range serviceRootLinks {
		ref, ok := root[field].(map[string]string)
		if !ok || ref["@odata.id"] == "" {
			t.Errorf("service root link %q missing or malformed: %v", field, root[field])
		}
	}
	for _, field := range []string{"Chassis", "Registries", "CompositionService", "ProtocolFeaturesSupported"} {
		if _, ok := root[field]; ok {
			t.Errorf("service root must not contain %q", field)
		}
	}
	links, ok := root["Links"].(map[string]interface{})
	if !ok || links["Sessions"] == nil {
		t.Errorf("service root must contain the schema-required Links.Sessions: %v", root["Links"])
	}
	if root["Id"] == "" {
		t.Errorf("service root must contain the schema-required Id")
	}
}
