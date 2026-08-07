package ipmi

import (
	"errors"
	"fmt"
	"testing"

	goipmihandlers "github.com/bougou/go-ipmi/pkg/handlers"
	"github.com/stretchr/testify/assert"

	"kubevirt.io/kubevirtbmc/pkg/resourcemanager"
)

func completionCodeOf(t *testing.T, err error) (goipmihandlers.CompletionCode, bool) {
	t.Helper()
	var cc goipmihandlers.CompletionCode
	ok := errors.As(err, &cc)
	return cc, ok
}

// TestWithCompletionCode pins the domain-error → completion-code mapping
// (IPMI spec §5.2 Table 5-2) that go-ipmi's codeFromErr extracts via
// errors.As from the errors vmChassis returns.
func TestWithCompletionCode(t *testing.T) {
	t.Run("nil passes through", func(t *testing.T) {
		assert.NoError(t, withCompletionCode(nil))
	})

	t.Run("ErrRetryable maps to 0xC0 Node Busy", func(t *testing.T) {
		err := withCompletionCode(&resourcemanager.ErrRetryable{Err: fmt.Errorf("VMI cleaning up")})
		cc, ok := completionCodeOf(t, err)
		assert.True(t, ok)
		assert.Equal(t, goipmihandlers.CodeNodeBusy, cc)
	})

	t.Run("ErrRetryable survives %w wrapping", func(t *testing.T) {
		inner := &resourcemanager.ErrRetryable{Err: fmt.Errorf("VMI cleaning up")}
		err := withCompletionCode(fmt.Errorf("power on: %w", inner))
		cc, ok := completionCodeOf(t, err)
		assert.True(t, ok)
		assert.Equal(t, goipmihandlers.CodeNodeBusy, cc)
	})

	t.Run("unmapped errors pass through unchanged", func(t *testing.T) {
		plain := fmt.Errorf("kubevirt api down")
		err := withCompletionCode(plain)
		assert.Equal(t, plain, err)
		_, ok := completionCodeOf(t, err)
		assert.False(t, ok, "go-ipmi should fall back to 0xFF Unspecified")
	})
}
