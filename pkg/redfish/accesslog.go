package redfish

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"kubevirt.io/kubevirtbmc/pkg/accesslog"
	"kubevirt.io/kubevirtbmc/pkg/generated/redfish/server"
	"kubevirt.io/kubevirtbmc/pkg/requestid"
)

const (
	maxBodyLog = 4096
	// maxRequestIDLen bounds an id echoed back from the client so a caller
	// cannot inflate every log line and response header at will.
	maxRequestIDLen = 64
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.Len(); remaining > 0 {
		if remaining < len(p) {
			_, _ = b.Buffer.Write(p[:remaining])
		} else {
			_, _ = b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// accessLog emits one line per Redfish request; handlers add request-specific
// detail to it through accesslog.Record.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := inboundRequestID(r)
		ctx := accesslog.Start(r.Context(), id)
		r = r.WithContext(ctx)
		w.Header().Set(requestid.Header, id)

		// The body is tee'd rather than read up front: the handler's own read
		// fills the buffer, so capture costs nothing when the body is unused.
		var bodyCapture *cappedBuffer
		if r.Body != nil && logrus.IsLevelEnabled(logrus.DebugLevel) &&
			r.Method != http.MethodGet && r.Method != http.MethodHead {
			bodyCapture = &cappedBuffer{limit: maxBodyLog + 1}
			r.Body = &readCloser{
				Reader: io.TeeReader(r.Body, bodyCapture),
				Closer: r.Body,
			}
		}

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		fields := logrus.Fields{
			"method": r.Method,
			"path":   r.URL.Path,
			"status": rec.status,
			"remote": r.RemoteAddr,
		}
		if bodyCapture != nil && bodyCapture.Len() > 0 {
			body := bodyCapture.Bytes()
			if len(body) > maxBodyLog {
				body = body[:maxBodyLog]
				fields["body_truncated"] = true
			}
			fields["body"] = redactJSON(body)
		}
		accesslog.Emit(ctx, statusLevel(rec.status), "redfish request", nil, fields)
	})
}

// recordingErrorHandler records the failure onto the access-log line; the
// generated handler only writes it into the response body, which would leave a
// 500 in the log with no reason attached.
func recordingErrorHandler(w http.ResponseWriter, r *http.Request, err error, result *server.ImplResponse) {
	if err != nil {
		accesslog.Record(r.Context(), logrus.Fields{logrus.ErrorKey: err.Error()})
	}
	server.DefaultErrorHandler(w, r, err, result)
}

// statusLevel: 5xx is our fault, 4xx is the client's (including the 401s a
// probing initiator produces before it authenticates), the rest is routine.
func statusLevel(status int) logrus.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return logrus.ErrorLevel
	case status >= http.StatusBadRequest:
		return logrus.WarnLevel
	default:
		return logrus.InfoLevel
	}
}

// inboundRequestID adopts the caller's X-Request-ID so a trace started in
// ironic carries through, but only if it is a bounded printable token; anything
// else is replaced rather than echoed into logs and response headers.
func inboundRequestID(r *http.Request) string {
	id := r.Header.Get(requestid.Header)
	if id == "" || len(id) > maxRequestIDLen {
		return uuid.NewString()
	}
	for _, c := range []byte(id) {
		if c < 0x21 || c > 0x7e {
			return uuid.NewString()
		}
	}
	return id
}

func redactJSON(body []byte) string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return string(body)
	}
	redactMap(m)
	out, err := json.Marshal(m)
	if err != nil {
		return string(body)
	}
	return string(out)
}

func redactMap(m map[string]any) {
	for k, v := range m {
		if isSensitiveKey(k) {
			m[k] = "[redacted]"
			continue
		}
		switch t := v.(type) {
		case map[string]any:
			redactMap(t)
		case []any:
			for _, item := range t {
				if child, ok := item.(map[string]any); ok {
					redactMap(child)
				}
			}
		}
	}
}

func isSensitiveKey(k string) bool {
	switch strings.ToLower(k) {
	case "password", "token", "x-auth-token", "authorization":
		return true
	default:
		return false
	}
}
