package resourcemanager

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestErrRetryableDetection asserts errors.As finds ErrRetryable by type,
// directly and through %w wrapping — the detection both protocol layers
// rely on for their wire mappings.
func TestErrRetryableDetection(t *testing.T) {
	original := &ErrRetryable{Err: fmt.Errorf("VM is not running")}

	var retryable *ErrRetryable
	assert.True(t, errors.As(original, &retryable))

	wrapped := fmt.Errorf("chassis control failed: %w", original)
	assert.True(t, errors.As(wrapped, &retryable))
	assert.Equal(t, "retryable: VM is not running", retryable.Error())
	assert.Equal(t, "VM is not running", original.Unwrap().Error())
}
