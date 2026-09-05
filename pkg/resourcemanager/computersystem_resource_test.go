package resourcemanager

import (
	"testing"

	"github.com/stretchr/testify/require"
	"kubevirt.io/kubevirtbmc/pkg/generated/redfish/server"
)

func TestNewComputerSystemAdvertisesResetAllowableValues(t *testing.T) {
	computerSystem := NewComputerSystem("1", "test-namespace/test-vm", server.RESOURCEPOWERSTATE_OFF).ComputerSystem()

	require.ElementsMatch(t, []server.ResourceResetType{
		server.RESOURCERESETTYPE_ON,
		server.RESOURCERESETTYPE_FORCE_OFF,
		server.RESOURCERESETTYPE_GRACEFUL_SHUTDOWN,
		server.RESOURCERESETTYPE_GRACEFUL_RESTART,
		server.RESOURCERESETTYPE_FORCE_RESTART,
	}, computerSystem.Actions.ComputerSystemReset.ResetTypeRedfishAllowableValues)
}

func TestBootDeviceToRedfishTarget(t *testing.T) {
	require.Equal(t, server.COMPUTERSYSTEMBOOTSOURCE_HDD, BootDeviceToRedfishTarget(BootDeviceHdd))
	require.Equal(t, server.COMPUTERSYSTEMBOOTSOURCE_PXE, BootDeviceToRedfishTarget(BootDevicePxe))
	require.Equal(t, server.COMPUTERSYSTEMBOOTSOURCE_CD, BootDeviceToRedfishTarget(BootDeviceCd))
	// "" (no device carries a bootOrder) must render as None, not the zero
	// value — the latter is omitempty-dropped from the JSON response.
	require.Equal(t, server.COMPUTERSYSTEMBOOTSOURCE_NONE, BootDeviceToRedfishTarget(""))
}
