package ipmi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/bougou/go-ipmi/pkg/bmc"
	"github.com/bougou/go-ipmi/pkg/handlers"
	"github.com/bougou/go-ipmi/pkg/server"
	udptransport "github.com/bougou/go-ipmi/pkg/transport/udp"
	ipmi "github.com/bougou/go-ipmi/pkg/types"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"kubevirt.io/kubevirtbmc/pkg/resourcemanager"
)

// lanChannel is the IPMI LAN channel number used by the simulator (channel 1,
// the default LAN channel in go-ipmi's ChannelStore).
const lanChannel uint8 = 1

// Simulator is an IPMI BMC simulator backed by a KubeVirt VirtualMachine.
//
// It wires github.com/bougou/go-ipmi's server stack (RMCP+ / IPMI v2.0 LANPLUS,
// plus minimal pre-session v1.0 LAN handling) to a ResourceManager. Chassis
// commands are routed to the ResourceManager through the typed hal.ChassisHAL
// implementation in handler.go (vmChassis); session establishment, RAKP,
// encryption and framing are handled by go-ipmi.
//
// IPMI command handler implementations live in fru.go (FRU/SDR) and
// handler.go (chassis HAL).  This file owns only the simulator lifecycle
// (bind/serve/stop) and BMC state construction.
type Simulator struct {
	ip       string
	port     int
	rm       resourcemanager.ResourceManager
	username string
	password string

	srv        *server.Server
	conn       *udptransport.Conn
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	fruData    []byte // pre-built FRU binary blob, set during Run()
	appVersion string // version string for FRU Product Version, set at construction
}

// NewSimulator creates a new IPMI simulator.
//
// The simulator does not bind the UDP socket until Run is called.
func NewSimulator(ip string, port int, resourceManager resourcemanager.ResourceManager, username, password, appVersion string) *Simulator {
	return &Simulator{
		ip:         ip,
		port:       port,
		rm:         resourceManager,
		username:   username,
		password:   password,
		appVersion: appVersion,
	}
}

// Run binds the UDP socket, builds the BMC state, and starts serving IPMI
// requests in a background goroutine. It returns as soon as the socket is
// bound so the caller (VirtBMC.Run) can proceed to start other services
// (e.g. Redfish). A bind failure is returned synchronously. Serve-time
// errors are logged, not returned. Use Stop to wait for the goroutine to exit.
func (s *Simulator) Run() error {
	listenIP := s.ip
	if listenIP == "" {
		listenIP = "0.0.0.0"
	}
	addr := net.JoinHostPort(listenIP, fmt.Sprintf("%d", s.port))
	conn, err := udptransport.Listen(addr)
	if err != nil {
		return fmt.Errorf("listen udp %q: %w", addr, err)
	}
	s.conn = conn

	b := s.buildBMC()

	reg := handlers.NewRegistry()
	handlers.RegisterAppHandlers(reg)
	handlers.RegisterSessionHandlers(reg)
	// RegisterChassisHandlers installs go-ipmi's typed codec handlers
	// (Chassis Control, Set/Get System Boot Options, Get Chassis Status, ...).
	// They dispatch through hal.ChassisHAL, which we back with vmChassis below
	// so each spec action maps to the corresponding KubeVirt ResourceManager API.
	handlers.RegisterChassisHandlers(reg)

	// Register custom handlers for FRU, SDR, and System GUID commands
	// that go-ipmi does not provide server-side implementations for.
	s.registerHandlers(reg)

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.srv = server.NewServer(b, conn, server.WithHandlerRegistry(reg))

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.srv.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logrus.WithError(err).Error("IPMI server exited with error")
		}
	}()

	logrus.Infof("IPMI service listening on %s", addr)
	return nil
}

// registerHandlers registers all custom IPMI command handlers that go-ipmi
// does not provide server-side implementations for.  These include:
//   - Get System GUID (App 0x37)
//   - FRU Inventory Area Info / Read FRU Data (Storage 0x10, 0x11)
//   - SDR Repository Info / Reserve SDR / Get SDR (Storage 0x20, 0x22, 0x23)
func (s *Simulator) registerHandlers(reg *handlers.Registry) {
	// Pre-build FRU data (needed by FRU and SDR handlers).
	s.fruData = s.buildFRU()

	// App commands
	reg.RegisterFunc(uint8(ipmi.CommandGetSystemGUID.NetFn), ipmi.CommandGetSystemGUID.ID, s.handleGetSystemGUID)

	// Storage commands — FRU
	reg.RegisterFunc(uint8(ipmi.CommandGetFRUInventoryAreaInfo.NetFn), ipmi.CommandGetFRUInventoryAreaInfo.ID, s.handleGetFRUInventoryAreaInfo)
	reg.RegisterFunc(uint8(ipmi.CommandReadFRUData.NetFn), ipmi.CommandReadFRUData.ID, s.handleReadFRUData)

	// Storage commands — SDR (for FRU device discovery)
	reg.RegisterFunc(uint8(ipmi.CommandGetSDRRepoInfo.NetFn), ipmi.CommandGetSDRRepoInfo.ID, s.handleGetSDRRepoInfo)
	reg.RegisterFunc(uint8(ipmi.CommandReserveSDRRepo.NetFn), ipmi.CommandReserveSDRRepo.ID, s.handleReserveSDR)
	reg.RegisterFunc(uint8(ipmi.CommandGetSDR.NetFn), ipmi.CommandGetSDR.ID, s.handleGetSDR)
}

// Stop gracefully shuts down the simulator: cancels the serve context, closes
// the UDP socket, and waits for the background serve goroutine to exit.
func (s *Simulator) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.srv != nil {
		_ = s.srv.Close()
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.wg.Wait()
	logrus.Info("IPMI simulator gracefully stopped")
}

// resolveGUID returns the VM's UUID as a 16-byte GUID in IPMI wire format,
// falling back to an all-zero GUID when the ResourceManager is nil (e.g. in
// unit tests).
//
// IPMI §20.8 defines the wire format as ipmi_guid_t: node[6] at bytes 0-5
// (LSB first), then clock_seq_low/clock_seq_hi_and_rsvd at bytes 6-7, then
// time_hi_and_version/time_mid/time_low at bytes 8-15 in little-endian
// order.  This matches the ipmitool struct sdr_record_mc_locator layout
// (node first, time fields last), NOT the RFC 4122 layout (time fields
// first, node last).
//
// K8s metadata.UID is always UUID v4 (random).  Storing the GUID in the
// correct ipmi_guid_t layout ensures ipmitool's auto-detection loop
// (RFC4122 → IPMI → SMBIOS) correctly identifies it as GUID_IPMI rather
// than falling through to GUID_SMBIOS.
func (s *Simulator) resolveGUID() [16]byte {
	if s.rm == nil {
		return [16]byte{}
	}
	uidStr, err := s.rm.GetSystemUUID()
	if err != nil {
		logrus.WithError(err).Warn("failed to get system UUID, falling back to zero GUID")
		return [16]byte{}
	}
	uid, err := uuid.Parse(uidStr)
	if err != nil {
		logrus.WithError(err).Warnf("failed to parse system UUID %q, falling back to zero GUID", uidStr)
		return [16]byte{}
	}

	var guid [16]byte
	// IPMI wire format per ipmi_guid_t (§20.8): node at bytes 0-5
	// (LSB first, reversed from RFC 4122 MSB order), then clock bytes, then time fields LE at bytes 8-15.
	guid[0], guid[1], guid[2], guid[3], guid[4], guid[5] =
		uid[15], uid[14], uid[13], uid[12], uid[11], uid[10] // node (6 bytes)
	guid[6] = uid[9]                                                        // clock_seq_low
	guid[7] = uid[8]                                                        // clock_seq_hi_and_rsvd
	guid[8], guid[9] = uid[7], uid[6]                                       // time_hi_and_version (LE)
	guid[10], guid[11] = uid[5], uid[4]                                     // time_mid (LE)
	guid[12], guid[13], guid[14], guid[15] = uid[3], uid[2], uid[1], uid[0] // time_low (LE)
	return guid
}

// buildBMC constructs the in-memory BMC state: device identity, GUID, the
// authenticated user account, and a HAL whose Chassis sub-interface is backed
// by vmChassis (the KubeVirt ResourceManager adapter). go-ipmi's typed chassis
// handlers dispatch through that HAL.
func (s *Simulator) buildBMC() *bmc.BMC {
	info := bmc.DeviceInfo{
		DeviceID:                0x20,
		DeviceRevision:          0x01,
		FirmwareMajor:           0x01,
		FirmwareMinor:           0x00,
		IPMIVersion:             0x20, // IPMI 2.0
		ManufacturerID:          0x000000,
		ProductID:               0x0000,
		AdditionalDeviceSupport: 0x08, // bit 3: FRU Inventory Device
	}

	guid := s.resolveGUID()

	b := bmc.New(info, guid, noopHAL{chassis: vmChassis{rm: s.rm}}, bmc.WithKG(nil))

	// Register the configured BMC user so RAKP username/password auth succeeds.
	if s.username != "" {
		user, err := b.Users.Add(2, s.username)
		if err != nil {
			logrus.WithError(err).Warn("failed to register IPMI user")
		} else {
			user.SetPassword([]byte(s.password))
			user.Enabled = true
			user.ChannelAccess = map[uint8]bmc.UserChannelAccess{
				lanChannel: {
					MaxPrivilege: bmc.PrivilegeLevelAdministrator,
					Enabled:      true,
				},
			}
		}
	}

	return b
}
