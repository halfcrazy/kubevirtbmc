package resourcemanager

import (
	"testing"

	"github.com/stretchr/testify/require"
	"kubevirt.io/kubevirtbmc/pkg/generated/redfish/server"
)

func TestNewComputerSystemSerialNumberMatchesName(t *testing.T) {
	name := "test-namespace/test-vm"
	cs := NewComputerSystem("1", name, server.RESOURCEPOWERSTATE_OFF).ComputerSystem()
	require.Equal(t, name, cs.Name)
	require.NotNil(t, cs.SerialNumber)
	require.Equal(t, name, *cs.SerialNumber)
}
