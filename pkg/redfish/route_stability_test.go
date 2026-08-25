package redfish

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Misses must be answered deterministically regardless of route registration
// order (the generated NewRouter ranges a map, fresh random order per
// construction): 405 with an Allow header when the path exists under another
// method, 404 when the path is absent entirely.
func TestMissStatusIsDeterministic(t *testing.T) {
	probes := []struct {
		method, path string
		wantCode     int
		wantAllow    string
	}{
		// Sessions collection GET is stripped; only POST exists.
		{http.MethodGet, "/redfish/v1/SessionService/Sessions", http.StatusMethodNotAllowed, "POST"},
		// Systems collection POST is stripped; only GET exists.
		{http.MethodPost, "/redfish/v1/Systems", http.StatusMethodNotAllowed, "GET"},
		// Chassis is not in the route table at all.
		{http.MethodGet, "/redfish/v1/Chassis", http.StatusNotFound, ""},
	}

	for i := 0; i < 50; i++ {
		router := newRouter(testUsername, testPassword, nil)
		for _, probe := range probes {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(probe.method, probe.path, nil))
			if rec.Code != probe.wantCode {
				t.Fatalf("construction %d: %s %s = %d, want %d",
					i, probe.method, probe.path, rec.Code, probe.wantCode)
			}
			if rec.Header().Get("Allow") != probe.wantAllow {
				t.Fatalf("construction %d: %s %s Allow = %q, want %q",
					i, probe.method, probe.path, rec.Header().Get("Allow"), probe.wantAllow)
			}
		}
	}
}
