package redfish

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"kubevirt.io/kubevirtbmc/pkg/accesslog"
	"kubevirt.io/kubevirtbmc/pkg/generated/redfish/server"
	"kubevirt.io/kubevirtbmc/pkg/requestid"
)

func TestRedactJSON(t *testing.T) {
	in := []byte(`{"UserName":"admin","Password":"secret","Nested":{"Token":"abc"}}`)
	out := redactJSON(in)
	require.Contains(t, out, `"Password":"[redacted]"`)
	require.Contains(t, out, `"Token":"[redacted]"`)
	require.Contains(t, out, `"UserName":"admin"`)
}

func TestAccessLog_InjectsRequestID(t *testing.T) {
	var sawID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawID = requestid.From(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset",
		bytes.NewReader([]byte(`{"ResetType":"On"}`)))
	rr := httptest.NewRecorder()

	prev := logrus.GetLevel()
	logrus.SetLevel(logrus.ErrorLevel)
	defer logrus.SetLevel(prev)

	accessLog(inner).ServeHTTP(rr, req)
	require.NotEmpty(t, sawID)
	require.Equal(t, sawID, rr.Header().Get(requestid.Header))
	require.Equal(t, http.StatusNoContent, rr.Code)
}

func TestAccessLog_AdoptsAndSanitizesInboundRequestID(t *testing.T) {
	prev := logrus.GetLevel()
	logrus.SetLevel(logrus.ErrorLevel)
	defer logrus.SetLevel(prev)

	tests := []struct {
		name    string
		header  string
		adopted bool
	}{
		{name: "well formed id is adopted", header: "ironic-req-42", adopted: true},
		{name: "empty header gets a fresh id", header: ""},
		{name: "over-long header is rejected", header: strings.Repeat("a", maxRequestIDLen+1)},
		{name: "control characters are rejected", header: "abc\ndef"},
		{name: "spaces are rejected", header: "abc def"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sawID string
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sawID = requestid.From(r.Context())
				w.WriteHeader(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tt.header != "" {
				req.Header.Set(requestid.Header, tt.header)
			}
			rr := httptest.NewRecorder()

			accessLog(inner).ServeHTTP(rr, req)

			require.NotEmpty(t, sawID)
			require.Equal(t, sawID, rr.Header().Get(requestid.Header))
			if tt.adopted {
				require.Equal(t, tt.header, sawID)
			} else {
				require.NotEqual(t, tt.header, sawID)
			}
		})
	}
}

func TestAccessLog_PreservesBody(t *testing.T) {
	var got string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		got = string(b)
		w.WriteHeader(http.StatusOK)
	})
	body := `{"Padding":"` + strings.Repeat("x", maxBodyLog) + `","ResetType":"On"}`

	// Capture is only wired up at debug level, so exercise both.
	for _, level := range []logrus.Level{logrus.InfoLevel, logrus.DebugLevel} {
		got = ""
		prev := logrus.GetLevel()
		logrus.SetLevel(level)

		req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader([]byte(body)))
		accessLog(inner).ServeHTTP(httptest.NewRecorder(), req)
		require.Equal(t, body, got)

		logrus.SetLevel(prev)
	}
}

func TestAccessLog_LogsRedactedBodyAtDebug(t *testing.T) {
	previousLevel := logrus.GetLevel()
	logrus.SetLevel(logrus.DebugLevel)
	defer logrus.SetLevel(previousLevel)

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/redfish/v1/SessionService/Sessions",
		bytes.NewReader([]byte(`{"UserName":"admin","Password":"secret"}`)))

	accessLog(inner).ServeHTTP(httptest.NewRecorder(), req)

	entry := hook.LastEntry()
	require.NotNil(t, entry)
	require.Contains(t, entry.Data["body"], `"Password":"[redacted]"`)
	require.NotContains(t, entry.Data["body"], "secret")
}

func TestAccessLog_ErrorHandlerExplains500(t *testing.T) {
	previousLevel := logrus.GetLevel()
	logrus.SetLevel(logrus.InfoLevel)
	defer logrus.SetLevel(previousLevel)

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordingErrorHandler(w, r, errors.New("virtualmachines.kubevirt.io \"vm\" not found"),
			&server.ImplResponse{Code: http.StatusInternalServerError})
	})
	accessLog(inner).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", nil))

	entry := hook.LastEntry()
	require.NotNil(t, entry)
	require.Equal(t, logrus.ErrorLevel, entry.Level)
	require.Contains(t, entry.Data[logrus.ErrorKey], "not found")
}

func TestAccessLog_HandlerFieldsLandOnTheSameLine(t *testing.T) {
	previousLevel := logrus.GetLevel()
	logrus.SetLevel(logrus.InfoLevel)
	defer logrus.SetLevel(previousLevel)

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accesslog.Record(r.Context(), logrus.Fields{"reset_type": "ForceOff"})
		w.WriteHeader(http.StatusNoContent)
	})
	accessLog(inner).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", nil))

	entry := hook.LastEntry()
	require.NotNil(t, entry)
	require.Equal(t, "ForceOff", entry.Data["reset_type"])
	require.Equal(t, http.StatusNoContent, entry.Data["status"])
	require.Equal(t, "192.0.2.1:1234", entry.Data["remote"], "httptest.NewRequest's fixed RemoteAddr")
	require.NotEmpty(t, entry.Data["request_id"])
	require.NotEmpty(t, entry.Data["duration"])
}

func TestAccessLog_UsesResponseStatusLevel(t *testing.T) {
	previousLevel := logrus.GetLevel()
	logrus.SetLevel(logrus.DebugLevel)
	defer logrus.SetLevel(previousLevel)

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	tests := []struct {
		name   string
		status int
		level  logrus.Level
	}{
		{name: "success", status: http.StatusNoContent, level: logrus.InfoLevel},
		{name: "client error", status: http.StatusBadRequest, level: logrus.WarnLevel},
		{name: "server error", status: http.StatusInternalServerError, level: logrus.ErrorLevel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook.Reset()
			handler := accessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))

			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

			entry := hook.LastEntry()
			require.NotNil(t, entry)
			require.Equal(t, tt.level, entry.Level)
		})
	}
}
