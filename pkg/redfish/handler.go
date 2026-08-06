package redfish

import (
	"fmt"

	"github.com/google/uuid"

	bmcv1 "kubevirt.io/kubevirtbmc/api/bmc/v1beta1"
	"kubevirt.io/kubevirtbmc/pkg/generated/redfish/server"
	"kubevirt.io/kubevirtbmc/pkg/resourcemanager"
	"kubevirt.io/kubevirtbmc/pkg/session"
	"kubevirt.io/kubevirtbmc/pkg/util"
)

type handler struct {
	rm resourcemanager.ResourceManager

	bmcUser     string
	bmcPassword string
}

func NewHandler(bmcUser string, bmcPassword string, resourceManager resourcemanager.ResourceManager) *handler {
	return &handler{
		rm:          resourceManager,
		bmcUser:     bmcUser,
		bmcPassword: bmcPassword,
	}
}

func (h *handler) Authenticate(username, password *string) (string, string, error) {
	var id, token string
	if username == nil || password == nil {
		return id, token, fmt.Errorf("username and password must be provided")
	}

	if *username != h.bmcUser || *password != h.bmcPassword {
		return id, token, fmt.Errorf("invalid username or password")
	}

	id = uuid.New().String()
	tokenInfo := session.NewTokenInfo(id, *username)
	token = session.AddToken(tokenInfo)

	return id, token, nil
}

func (h *handler) GetSession(sessionID string) (string, string, error) {
	var id, username string
	tokenInfo, exists := session.GetTokenFromSessionID(sessionID)
	if !exists {
		return id, username, fmt.Errorf("session not found")
	}
	return tokenInfo.ID, tokenInfo.Username, nil
}

func (h *handler) DeleteSession(sessionID string) {
	session.RemoveToken(sessionID)
}

// GetServiceRoot builds the payload as a map rather than the generated
// ServiceRootV1161ServiceRoot struct: that struct's link fields are
// OdataV4IdRef values, and encoding/json's omitempty never drops zero-value
// structs, so unimplemented links would serialize as empty objects instead of
// disappearing. The payload only advertises links whose subtree has a real
// implementation (serviceRootLinks, generated) plus the schema-mandatory
// Links.Sessions — clients discover the resource tree through this payload,
// so every advertised link must be dereferenceable. ProtocolFeaturesSupported
// is omitted because the service supports none of the query parameters
// (DSP0266: include it only if query parameters are supported).
func (h *handler) GetServiceRoot() map[string]interface{} {
	root := map[string]interface{}{
		"@odata.context": "/redfish/v1/$metadata#ServiceRoot.ServiceRoot",
		"@odata.id":      "/redfish/v1",
		"@odata.type":    "#ServiceRoot.v1_16_1.ServiceRoot",
		"Description":    "ServiceRoot",
		"Id":             "RootService",
		"Name":           "ServiceRoot",
		"RedfishVersion": "1.16.1",
		"UUID":           "00000000-0000-0000-0000-000000000000",
		"Links": map[string]interface{}{
			"ManagerProvidingService": map[string]string{"@odata.id": "/redfish/v1/Managers/BMC"},
			"Sessions":                map[string]string{"@odata.id": "/redfish/v1/SessionService/Sessions"},
		},
	}
	for name, uri := range serviceRootLinks {
		root[name] = map[string]string{"@odata.id": uri}
	}
	return root
}

func (h *handler) GetSessionService() *server.SessionServiceV118SessionService {
	return &server.SessionServiceV118SessionService{
		OdataContext:   "/redfish/v1/$metadata#SessionService.SessionService",
		OdataId:        "/redfish/v1/SessionService",
		OdataType:      "#SessionService.v1_1_8.SessionService",
		Description:    "Session Service",
		Id:             "SessionService",
		Name:           "Session Service",
		ServiceEnabled: util.Ptr(true),
		Sessions: server.OdataV4IdRef{
			OdataId: "/redfish/v1/SessionService/Sessions",
		},
	}
}

func (h *handler) GetManagerCollection() *server.ManagerCollectionManagerCollection {
	return &server.ManagerCollectionManagerCollection{
		OdataContext: "/redfish/v1/$metadata#ManagerCollection.ManagerCollection",
		OdataId:      "/redfish/v1/Managers",
		OdataType:    "#ManagerCollection.ManagerCollection",
		Description:  "Manager Collection",
		Name:         "Manager Collection",
		Members: []server.OdataV4IdRef{
			{
				OdataId: "/redfish/v1/Managers/BMC",
			},
		},
	}
}

func (h *handler) GetManager() (*server.ManagerV1190Manager, error) {
	manager, err := h.rm.GetManager()
	if err != nil {
		return nil, err
	}

	adapter, ok := manager.(*resourcemanager.ManagerAdapter)
	if !ok {
		return nil, fmt.Errorf("manager is not a *resourcemanager.ManagerAdapter (got %T)", manager)
	}

	return adapter.Manager(), nil
}

func (h *handler) GetVirtualMediaCollection() *server.VirtualMediaCollectionVirtualMediaCollection {
	return &server.VirtualMediaCollectionVirtualMediaCollection{
		OdataContext: "/redfish/v1/$metadata#VirtualMediaCollection.VirtualMediaCollection",
		OdataId:      "/redfish/v1/Managers/BMC/VirtualMedia",
		OdataType:    "#VirtualMediaCollection.VirtualMediaCollection",
		Description:  "Virtual Media Collection",
		Name:         "Virtual Media Collection",
		Members: []server.OdataV4IdRef{
			{
				OdataId: "/redfish/v1/Managers/BMC/VirtualMedia/CD1",
			},
		},
		MembersodataCount: 1,
	}
}

func (h *handler) GetVirtualMedia() (*server.VirtualMediaV163VirtualMedia, error) {
	virtualMedia, err := h.rm.GetVirtualMedia()
	if err != nil {
		return nil, err
	}

	adapter, ok := virtualMedia.(*resourcemanager.VirtualMediaAdapter)
	if !ok {
		return nil, fmt.Errorf("virtualMedia is not a *resourcemanager.VirtualMediaAdapter (got %T)", virtualMedia)
	}

	return adapter.VirtualMedia(), nil
}

func (h *handler) VirtualMediaEject() error {
	return h.rm.EjectMedia()
}

func (h *handler) VirtualMediaInsert(image string) error {
	return h.rm.InsertMedia(image)
}

func (h *handler) GetComputerSystemCollection() *server.ComputerSystemCollectionComputerSystemCollection {
	return &server.ComputerSystemCollectionComputerSystemCollection{
		OdataContext: "/redfish/v1/$metadata#ComputerSystemCollection.ComputerSystemCollection",
		OdataId:      "/redfish/v1/Systems",
		OdataType:    "#ComputerSystemCollection.ComputerSystemCollection",
		Description:  "Computer System Collection",
		Name:         "Computer System Collection",
		Members: []server.OdataV4IdRef{
			{
				OdataId: "/redfish/v1/Systems/1",
			},
		},
	}
}

func (h *handler) GetComputerSystem() (*server.ComputerSystemV1220ComputerSystem, error) {
	computerSystem, err := h.rm.GetComputerSystem()
	if err != nil {
		return nil, err
	}

	adapter, ok := computerSystem.(*resourcemanager.ComputerSystemAdapter)
	if !ok {
		return nil, fmt.Errorf("computerSystem is not a *resourcemanager.ComputerSystemAdapter (got %T)", computerSystem)
	}

	cs := adapter.ComputerSystem()

	// GetBootFlags (VM spec + status.bootOverride) is authoritative; the
	// in-memory ComputerSystem model is lost on pod restart.
	if flags, err := h.rm.GetBootFlags(); err == nil && flags != nil {
		cs.Boot.BootSourceOverrideTarget = resourcemanager.BootDeviceToRedfishTarget(flags.BootDevice)
		cs.Boot.BootSourceOverrideMode = resourcemanager.EFIBootToRedfishMode(flags.EFIBoot)
		if flags.OverrideActive {
			if flags.Mode == resourcemanager.BootModeOneshot {
				cs.Boot.BootSourceOverrideEnabled = server.COMPUTERSYSTEMV1220BOOTSOURCEOVERRIDEENABLED_ONCE
			} else {
				cs.Boot.BootSourceOverrideEnabled = server.COMPUTERSYSTEMV1220BOOTSOURCEOVERRIDEENABLED_CONTINUOUS
			}
		} else {
			cs.Boot.BootSourceOverrideEnabled = server.COMPUTERSYSTEMV1220BOOTSOURCEOVERRIDEENABLED_DISABLED
		}
	}

	return cs, nil
}

func (h *handler) PatchComputerSystem(computerSystemPatch *server.ComputerSystemV1220ComputerSystem) error {
	boot := computerSystemPatch.Boot
	firmwareMode, hasFirmwareMode := redfishFirmwareMode(boot.BootSourceOverrideMode)

	var bootMode resourcemanager.BootMode
	hasBootOverride := true
	switch boot.BootSourceOverrideEnabled {
	case server.COMPUTERSYSTEMV1220BOOTSOURCEOVERRIDEENABLED_DISABLED:
		if err := h.rm.ClearBootOverrides(); err != nil {
			return err
		}
		if hasFirmwareMode {
			return h.rm.SetFirmwareMode(firmwareMode)
		}
		return nil
	case server.COMPUTERSYSTEMV1220BOOTSOURCEOVERRIDEENABLED_ONCE:
		bootMode = resourcemanager.BootModeOneshot
	case server.COMPUTERSYSTEMV1220BOOTSOURCEOVERRIDEENABLED_CONTINUOUS:
		bootMode = resourcemanager.BootModePersistent
	case "":
		// Enabled omitted: per DSP0266, PATCH leaves absent properties
		// unchanged, so the target applies under the current override mode.
		// (ironic sends target-only PATCHes when the desired enabled state
		// already matches the reported one.)
		override, err := h.rm.GetBootOverride()
		if err != nil {
			return err
		}
		if override == nil {
			hasBootOverride = false
		} else if override.Mode == bmcv1.BootOverrideModeOneshot {
			bootMode = resourcemanager.BootModeOneshot
		} else {
			bootMode = resourcemanager.BootModePersistent
		}
	default:
		hasBootOverride = false
	}

	if !hasBootOverride {
		if hasFirmwareMode {
			return h.rm.SetFirmwareMode(firmwareMode)
		}
		return nil
	}

	var bootDevice resourcemanager.BootDevice
	switch boot.BootSourceOverrideTarget {
	case server.COMPUTERSYSTEMBOOTSOURCE_PXE:
		bootDevice = resourcemanager.BootDevicePxe
	case server.COMPUTERSYSTEMBOOTSOURCE_HDD:
		bootDevice = resourcemanager.BootDeviceHdd
	case server.COMPUTERSYSTEMBOOTSOURCE_CD:
		bootDevice = resourcemanager.BootDeviceCd
	default:
		return nil
	}

	opts := &resourcemanager.BootOptions{Mode: bootMode}
	if hasFirmwareMode {
		efiBoot := firmwareMode == resourcemanager.FirmwareModeUEFI
		opts.EFIBoot = &efiBoot
	}
	if err := h.rm.SetBootDevice(bootDevice, opts); err != nil {
		return err
	}
	return nil
}

func redfishFirmwareMode(mode server.ComputerSystemV1220BootSourceOverrideMode) (resourcemanager.FirmwareMode, bool) {
	switch mode {
	case server.COMPUTERSYSTEMV1220BOOTSOURCEOVERRIDEMODE_UEFI:
		return resourcemanager.FirmwareModeUEFI, true
	case server.COMPUTERSYSTEMV1220BOOTSOURCEOVERRIDEMODE_LEGACY:
		return resourcemanager.FirmwareModeLegacy, true
	default:
		return "", false
	}
}

func (h *handler) ComputerSystemReset(resetType server.ResourceResetType) error {
	powerActionMap := map[server.ResourceResetType]func() error{
		server.RESOURCERESETTYPE_ON:                h.rm.PowerOn,
		server.RESOURCERESETTYPE_GRACEFUL_SHUTDOWN: h.rm.PowerOff,
		server.RESOURCERESETTYPE_FORCE_OFF:         h.rm.ForcePowerOff,
		server.RESOURCERESETTYPE_GRACEFUL_RESTART:  h.rm.PowerCycle,
		server.RESOURCERESETTYPE_FORCE_RESTART:     h.rm.ForcePowerCycle,
	}

	powerAction, ok := powerActionMap[resetType]
	if !ok {
		return fmt.Errorf("unsupported reset type: %s", resetType)
	}
	return powerAction()
}

// ComputerSystemSetDefaultBootOrder sets the boot order for the computer system back to default.
// TODO: Implement real default boot order setting. Right now we intentionally misuse the handler to set the first boot
// device.
func (h *handler) ComputerSystemSetDefaultBootOrder(bootDevices []string) error {
	var bootDevice resourcemanager.BootDevice
	if len(bootDevices) > 0 {
		bootDevice = resourcemanager.BootDevice(bootDevices[0])
	}
	return h.rm.SetBootDevice(bootDevice, &resourcemanager.BootOptions{Mode: resourcemanager.BootModePersistent})
}
