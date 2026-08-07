package resourcemanager

import (
	"fmt"
)

// ErrRetryable marks a KubeVirt operation that failed only due to a
// transitional VM state (e.g. VMI still cleaning up) and may succeed on
// retry. Protocol layers detect it via errors.As and own the wire mapping:
// pkg/ipmi translates it to completion code 0xC0 Node Busy (IPMI spec §5.2
// Table 5-2), pkg/redfish to an HTTP iLO-style response. The domain layer
// deliberately carries no protocol semantics.
type ErrRetryable struct{ Err error }

func (e *ErrRetryable) Error() string {
	return fmt.Sprintf("retryable: %v", e.Err)
}

func (e *ErrRetryable) Unwrap() error {
	return e.Err
}
