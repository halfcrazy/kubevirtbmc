package ipmi

import (
	"context"
	"errors"

	"github.com/bougou/go-ipmi/pkg/hal"
	goipmihandlers "github.com/bougou/go-ipmi/pkg/handlers"

	"kubevirt.io/kubevirtbmc/pkg/resourcemanager"
)

// completionCodeError attaches an IPMI completion code to a domain error so
// go-ipmi's codeFromErr picks it up via errors.As.
type completionCodeError struct {
	code goipmihandlers.CompletionCode
	err  error
}

func (e *completionCodeError) Error() string { return e.err.Error() }
func (e *completionCodeError) Unwrap() error { return e.err }

// As answers only CompletionCode targets; returning false for anything else
// lets errors.As continue unwrapping, so domain error types stay detectable
// through the wrapper.
func (e *completionCodeError) As(target any) bool {
	if cc, ok := target.(*goipmihandlers.CompletionCode); ok {
		*cc = e.code
		return true
	}
	return false
}

// withCompletionCode is the single point where ResourceManager domain errors
// acquire IPMI wire semantics (spec §5.2 Table 5-2). Unmapped errors pass
// through unchanged and go-ipmi reports them as 0xFF Unspecified.
//
//   - ErrRetryable → 0xC0 Node Busy: "command processing resources are
//     temporarily unavailable" — a transitional VM state blocks the operation
//     and a retry may succeed.
func withCompletionCode(err error) error {
	if err == nil {
		return nil
	}
	var retryable *resourcemanager.ErrRetryable
	if errors.As(err, &retryable) {
		return &completionCodeError{code: goipmihandlers.CodeNodeBusy, err: err}
	}
	return err
}

// codedChassis injects the mapping above at the HAL boundary. It cannot live
// in a dispatch middleware: go-ipmi's typed handlers swallow the HAL error
// (returning only codeFromErr's byte), so the middleware chain never sees the
// domain error. Only methods whose rm calls can return wire-mappable typed
// errors — ErrRetryable from the power operations — are overridden; everything
// else is promoted unchanged, which is the answer to "which methods need the
// mapping": exactly these.
type codedChassis struct {
	hal.ChassisHAL
}

func (c codedChassis) SetPower(ctx context.Context, on bool) error {
	return withCompletionCode(c.ChassisHAL.SetPower(ctx, on))
}

func (c codedChassis) PowerCycle(ctx context.Context) error {
	return withCompletionCode(c.ChassisHAL.PowerCycle(ctx))
}

func (c codedChassis) ColdReset(ctx context.Context) error {
	return withCompletionCode(c.ChassisHAL.ColdReset(ctx))
}

func (c codedChassis) WarmReset(ctx context.Context) error {
	return withCompletionCode(c.ChassisHAL.WarmReset(ctx))
}
