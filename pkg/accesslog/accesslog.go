// Package accesslog emits one structured log line per BMC request, shared by
// the Redfish (HTTP) and IPMI (RMCP) front ends.
//
// A front end calls Start at the edge of a request and Emit once it is done;
// Emit grades the line's level from the outcome (HTTP status, IPMI completion
// code). Code running in between has two ways to log, and the choice is about
// whether the message has a severity of its own:
//
//   - Record defers. Use it for anything that describes the request or its
//     outcome: what was asked (reset type, boot device, ipmitool command), what
//     resulted (power state), which path was taken (a power cycle that fell
//     back to PowerOn), and the error behind a failure status. These carry no
//     severity of their own — a fallback is only interesting when the request
//     as a whole failed — so they wait for Emit and inherit its level, landing
//     on the same line as status and duration.
//   - Logger writes immediately. Use it when the severity is independent of
//     the outcome: typically an error that is swallowed so the request still
//     succeeds and the access line will be Info, which would hide it from a
//     warn/error filter. The line is tagged with the request id so it can be
//     correlated with the access-log entry that follows.
//
// Rule of thumb: if a reader only needs it while looking at that request,
// Record it; if it should surface on its own in a warn/error filter, Logger it.
package accesslog

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"kubevirt.io/kubevirtbmc/pkg/requestid"
)

type ctxKey struct{}

type entry struct {
	start time.Time

	mu     sync.Mutex
	fields logrus.Fields
}

// Start opens the access-log scope for a request and stores id under the
// requestid key so every log line written downstream can be correlated.
func Start(ctx context.Context, id string) context.Context {
	return context.WithValue(requestid.With(ctx, id), ctxKey{}, &entry{
		start:  time.Now(),
		fields: logrus.Fields{},
	})
}

// Record attaches fields to the access-log line for ctx; later values win on
// conflict. It is a no-op outside a Start'ed request, so callers need not
// distinguish request from non-request code paths.
func Record(ctx context.Context, fields logrus.Fields) {
	e, ok := ctx.Value(ctxKey{}).(*entry)
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, v := range fields {
		e.fields[k] = v
	}
}

// Has reports whether key has been recorded for ctx.
func Has(ctx context.Context, key string) bool {
	e, ok := ctx.Value(ctxKey{}).(*entry)
	if !ok {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok = e.fields[key]
	return ok
}

// Logger returns a logrus entry tagged with the request id from ctx, for lines
// that must be written immediately at their own level (see the package doc).
func Logger(ctx context.Context) *logrus.Entry {
	if id := requestid.From(ctx); id != "" {
		return logrus.WithField("request_id", id)
	}
	return logrus.NewEntry(logrus.StandardLogger())
}

// Emit writes the access-log line for ctx: recorded fields, then the caller's
// fields (which win on conflict), plus request_id, duration and err if non-nil.
func Emit(ctx context.Context, level logrus.Level, msg string, err error, fields logrus.Fields) {
	e, _ := ctx.Value(ctxKey{}).(*entry)

	merged := logrus.Fields{}
	if e != nil {
		e.mu.Lock()
		for k, v := range e.fields {
			merged[k] = v
		}
		e.mu.Unlock()
		merged["duration"] = time.Since(e.start).String()
	}
	for k, v := range fields {
		merged[k] = v
	}
	if id := requestid.From(ctx); id != "" {
		merged["request_id"] = id
	}

	log := logrus.WithFields(merged)
	if err != nil {
		log = log.WithError(err)
	}
	log.Log(level, msg)
}
