package resourcemanager

import (
	"fmt"
	"strings"

	bmcv1 "kubevirt.io/kubevirtbmc/api/bmc/v1beta1"
)

type BootDevice string

const (
	BootDevicePxe  BootDevice = "Pxe"
	BootDeviceHdd  BootDevice = "Hdd"
	BootDeviceCd   BootDevice = "Cd"
	BootDeviceNone BootDevice = "None"
)

// AmbiguousBootDeviceError indicates that a boot override cannot select one
// device because multiple candidates exist and none participates in bootOrder.
type AmbiguousBootDeviceError struct {
	BootDevice BootDevice
	Candidates []string
}

func (e *AmbiguousBootDeviceError) Error() string {
	return fmt.Sprintf(
		"multiple candidates for %s boot have no bootOrder: %s",
		e.BootDevice,
		strings.Join(e.Candidates, ", "),
	)
}

// BootMode represents the persistence mode of a boot device override.
type BootMode string

const (
	BootModePersistent BootMode = "Persistent"
	BootModeOneshot    BootMode = "Oneshot"
)

// BootOptions carries optional parameters for SetBootDevice.
type BootOptions struct {
	Mode    BootMode
	EFIBoot *bool // nil = don't change firmware; true = EFI; false = Legacy BIOS
}

// OverrideMode represents the boot source override enablement state.
type OverrideMode string

const (
	OverrideModeDisabled   OverrideMode = "Disabled"
	OverrideModeOnce       OverrideMode = "Once"
	OverrideModeContinuous OverrideMode = "Continuous"
)

// FirmwareMode represents the boot source override firmware mode.
type FirmwareMode string

const (
	FirmwareModeLegacy FirmwareMode = "Legacy"
	FirmwareModeUEFI   FirmwareMode = "UEFI"
)

// BootFlagsState holds the current effective boot flags read from the VM template spec.
type BootFlagsState struct {
	BootDevice     BootDevice
	Mode           BootMode // Persist bit for IPMI; Oneshot/Persistent
	EFIBoot        bool
	OverrideActive bool // true when status.bootOverride exists on the VirtualMachineBMC CR
}

type ResourceManager interface {
	GetBootFlags() (*BootFlagsState, error)
	GetBootOverride() (*bmcv1.BootOverrideStatus, error)
	GetComputerSystem() (ComputerSystemInterface, error)
	GetManager() (ManagerInterface, error)
	GetVirtualMedia() (VirtualMediaInterface, error)

	EjectMedia() error
	InsertMedia(string) error
	GetPowerStatus() (bool, error)
	PowerOn() error
	PowerOff() error
	ForcePowerOff() error
	PowerCycle() error
	ForcePowerCycle() error
	SetBootDevice(BootDevice, *BootOptions) error
	GetSystemUUID() (string, error)
	SetFirmwareMode(FirmwareMode) error
	ClearBootOverrides() error
}
