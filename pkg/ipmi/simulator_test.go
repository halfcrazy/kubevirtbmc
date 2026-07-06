package ipmi

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/bougou/go-ipmi/pkg/bmc"
	"github.com/bougou/go-ipmi/pkg/handlers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"kubevirt.io/kubevirtbmc/pkg/resourcemanager"
)

func TestBuildBMCRegistersUser(t *testing.T) {
	s := NewSimulator("127.0.0.1", 623, nil, "admin", "secret", "")

	b := s.buildBMC()

	user, err := b.Users.GetByName("admin")
	assert.NoError(t, err)
	assert.True(t, user.Enabled, "registered user must be enabled")
	assert.Equal(t, bmc.PrivilegeLevelAdministrator, user.ChannelAccess[lanChannel].MaxPrivilege)
	assert.True(t, user.ChannelAccess[lanChannel].Enabled)
	assert.True(t, user.VerifyPassword([]byte("secret")))
}

func TestBuildBMCNoUserWhenUsernameEmpty(t *testing.T) {
	s := NewSimulator("127.0.0.1", 623, nil, "", "", "")

	b := s.buildBMC()

	// Only the anonymous null user (ID 1) should exist.
	assert.Equal(t, 1, b.Users.Count())
	_, err := b.Users.GetByName("admin")
	assert.ErrorIs(t, err, bmc.ErrUserNotFound)
}

// TestBuildBMCHALExposesChassis ensures buildBMC wires the vmChassis adapter
// into the BMC HAL so go-ipmi's typed chassis handlers can dispatch through it.
// The PowerState call here only verifies the wiring (HAL.Chassis() returns a
// working adapter that forwards to the ResourceManager); the vmChassis business
// logic itself is exercised in handler_test.go.
func TestBuildBMCHALExposesChassis(t *testing.T) {
	rm := resourcemanager.NewMockResourceManager(gomock.NewController(t))
	rm.EXPECT().GetSystemUUID().Return("00000000-0000-0000-0000-000000000000", nil)
	rm.EXPECT().GetPowerStatus().Return(true, nil)
	s := NewSimulator("127.0.0.1", 623, rm, "admin", "secret", "")

	b := s.buildBMC()
	ch := b.HAL().Chassis()
	assert.NotNil(t, ch, "HAL must expose Chassis for typed chassis handlers")

	on, err := ch.PowerState(context.Background())
	assert.NoError(t, err)
	assert.True(t, on)
}

// TestRunDoesNotBlockCaller is a regression test: Simulator.Run must return
// after binding so the caller (VirtBMC.Run) can start sibling services such
// as Redfish. The blocking Serve loop runs in a background goroutine, and
// Stop must wait for that goroutine to exit.
func TestRunDoesNotBlockCaller(t *testing.T) {
	// Bind to an ephemeral port on loopback so concurrent test runs don't clash.
	s := NewSimulator("127.0.0.1", 0, nil, "admin", "secret", "")

	done := make(chan error, 1)
	go func() { done <- s.Run() }()

	select {
	case err := <-done:
		assert.NoError(t, err, "Run must return after bind, not block on Serve")
	case <-time.After(3 * time.Second):
		t.Fatal("Simulator.Run blocked the caller; Serve must run in a goroutine")
	}

	stopDone := make(chan struct{}, 1)
	go func() { s.Stop(); close(stopDone) }()
	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Simulator.Stop did not return; serve goroutine did not exit")
	}
}

// --- resolveGUID unit tests -------------------------------------------------

func TestResolveGUIDNilRM(t *testing.T) {
	s := NewSimulator("127.0.0.1", 623, nil, "admin", "secret", "")
	guid := s.resolveGUID()
	assert.Equal(t, [16]byte{}, guid, "nil ResourceManager must return zero GUID")
}

func TestResolveGUIDFormat(t *testing.T) {
	// Use a well-known UUID v4 and verify the wire-format bytes match
	// the ipmi_guid_t layout: node[6] (LSB first) || clk_lo || clk_hi
	// || time_hi_and_version (LE) || time_mid (LE) || time_low (LE).
	rm := resourcemanager.NewMockResourceManager(gomock.NewController(t))
	rm.EXPECT().GetSystemUUID().Return("6d3cf8a6-9297-4747-bb3d-6d363594bfd8", nil)

	s := NewSimulator("127.0.0.1", 623, rm, "admin", "secret", "")
	guid := s.resolveGUID()

	// Verify node bytes (reversed: RFC 4122 MSB → IPMI LSB first).
	assert.Equal(t, []byte{0xd8, 0xbf, 0x94, 0x35, 0x36, 0x6d}, guid[0:6], "node must be LSB first")

	// clock fields
	assert.Equal(t, uint8(0x3d), guid[6], "clock_seq_low")
	assert.Equal(t, uint8(0xbb), guid[7], "clock_seq_hi_and_rsvd")

	// time fields (LE)
	assert.Equal(t, []byte{0x47, 0x47}, guid[8:10], "time_hi_and_version LE")
	assert.Equal(t, []byte{0x97, 0x92}, guid[10:12], "time_mid LE")
	assert.Equal(t, []byte{0xa6, 0xf8, 0x3c, 0x6d}, guid[12:16], "time_low LE")

	// Version nibble must land at the high byte of time_hi_and_version (guid[9]).
	assert.Equal(t, uint8(0x47), guid[9], "version nibble at high byte of time_hi_and_version")

	// Reconstruct the original UUID from IPMI wire format.
	reconstructed := uuid.UUID{
		guid[15], guid[14], guid[13], guid[12], // time_low (BE from LE)
		guid[11], guid[10], // time_mid (BE from LE)
		guid[9], guid[8], // time_hi_and_version (BE from LE)
		guid[7],                                              // clock_seq_hi
		guid[6],                                              // clock_seq_low
		guid[5], guid[4], guid[3], guid[2], guid[1], guid[0], // node (MSB first from LSB first)
	}
	assert.Equal(t, "6d3cf8a6-9297-4747-bb3d-6d363594bfd8", reconstructed.String())
}

func TestResolveGUIDParseError(t *testing.T) {
	rm := resourcemanager.NewMockResourceManager(gomock.NewController(t))
	rm.EXPECT().GetSystemUUID().Return("not-a-valid-uuid", nil)

	s := NewSimulator("127.0.0.1", 623, rm, "admin", "secret", "")
	guid := s.resolveGUID()
	assert.Equal(t, [16]byte{}, guid, "invalid UUID string must return zero GUID")
}

// --- buildFRU unit tests -----------------------------------------------------

func TestBuildFRUNilRM(t *testing.T) {
	s := NewSimulator("127.0.0.1", 623, nil, "admin", "secret", "1.0")
	data := s.buildFRU()

	assert.GreaterOrEqual(t, len(data), 8, "FRU data includes at least the 8-byte common header")
	assert.Equal(t, uint8(0x01), data[0], "FRU format version must be 0x01")
	// With nil RM, serial defaults to all-zero UUID.
	assert.Contains(t, string(data), "00000000-0000")
}

func TestBuildFRUWithSerial(t *testing.T) {
	rm := resourcemanager.NewMockResourceManager(gomock.NewController(t))
	rm.EXPECT().GetSystemUUID().Return("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", nil)

	s := NewSimulator("127.0.0.1", 623, rm, "admin", "secret", "2.0")
	data := s.buildFRU()

	assert.Contains(t, string(data), "KubeVirt", "FRU must contain Manufacturer Name")
	assert.Contains(t, string(data), "KubeVirtBMC", "FRU must contain Product Name")
	assert.Contains(t, string(data), "2.0", "FRU must contain Product Version")
	assert.Contains(t, string(data), "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "FRU must contain Product Serial")
}

func TestBuildFRUFallbackVersion(t *testing.T) {
	s := NewSimulator("127.0.0.1", 623, nil, "admin", "secret", "")
	data := s.buildFRU()
	assert.Contains(t, string(data), "1.0", "empty appVersion must fall back to 1.0")
}

// --- handler unit tests ------------------------------------------------------

func TestHandleGetSDRRepoInfo(t *testing.T) {
	s := NewSimulator("127.0.0.1", 623, nil, "admin", "secret", "")
	resp, cc, err := s.handleGetSDRRepoInfo(context.Background(), nil, nil)
	assert.NoError(t, err)
	assert.Equal(t, handlers.CodeOK, cc)
	assert.Len(t, resp, 14)
	assert.Equal(t, uint8(0x51), resp[0], "SDR version must be 0x51 (IPMI 1.5)")
}

func TestHandleReserveSDR(t *testing.T) {
	s := NewSimulator("127.0.0.1", 623, nil, "admin", "secret", "")
	resp, cc, err := s.handleReserveSDR(context.Background(), nil, nil)
	assert.NoError(t, err)
	assert.Equal(t, handlers.CodeOK, cc)
	assert.Equal(t, []byte{0x01, 0x00}, resp, "reservation ID must be 1 (LE)")
}

func TestHandleGetSDR(t *testing.T) {
	s := NewSimulator("127.0.0.1", 623, nil, "admin", "secret", "")
	s.fruData = s.buildFRU()

	// Read the full record.  Pass less than 6 bytes so the handler uses
	// its defaults (offset=0, count=len(recData)).
	resp, cc, err := s.handleGetSDR(context.Background(), nil, nil)
	assert.NoError(t, err)
	assert.Equal(t, handlers.CodeOK, cc)

	// Response: 2 bytes nextRecordID + full recData (16 bytes).
	assert.Len(t, resp, 2+5+len(mcFRULocatorBody), "2 bytes nextRecordID + full record")
	assert.Equal(t, uint8(0xFF), resp[0], "nextRecordID high byte")
	assert.Equal(t, uint8(0xFF), resp[1], "nextRecordID low byte (0xFFFF = no more records)")

	// Verify record header within the returned SDR body.
	body := resp[2:]
	assert.Equal(t, uint8(0x51), body[2], "SDR version in record header")
	assert.Equal(t, uint8(0x12), body[3], "Record Type must be 0x12 (MC Device Locator)")
	assert.Equal(t, uint8(0x0b), body[4], "Record body length must be 11 (0x0b)")
}

func TestHandleGetSDRWithOffsetCount(t *testing.T) {
	s := NewSimulator("127.0.0.1", 623, nil, "admin", "secret", "")
	s.fruData = s.buildFRU()

	// Request only the first 3 bytes of the SDR body (offset=5, count=3).
	resp, cc, err := s.handleGetSDR(context.Background(), nil,
		[]byte{0x01, 0x00, 0x01, 0x00, 0x05, 0x03})
	assert.NoError(t, err)
	assert.Equal(t, handlers.CodeOK, cc)
	assert.Len(t, resp, 2+3, "2 bytes nextRecordID + 3 bytes requested data")
}

func TestHandleGetFRUInventoryAreaInfo(t *testing.T) {
	rm := resourcemanager.NewMockResourceManager(gomock.NewController(t))
	rm.EXPECT().GetSystemUUID().Return("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", nil)

	s := NewSimulator("127.0.0.1", 623, rm, "admin", "secret", "1.0")
	s.fruData = s.buildFRU()

	// Request area info for FRU device 0.
	resp, cc, err := s.handleGetFRUInventoryAreaInfo(context.Background(), nil,
		[]byte{0x00})
	assert.NoError(t, err)
	assert.Equal(t, handlers.CodeOK, cc)
	assert.Len(t, resp, 3)
	// Size is little-endian uint16 at resp[0:2].
	assert.Equal(t, uint16(len(s.fruData)),
		binary.LittleEndian.Uint16(resp[0:2]), "area size must match FRU data length")

	// Non-zero FRU Device ID must return error.
	_, cc, _ = s.handleGetFRUInventoryAreaInfo(context.Background(), nil,
		[]byte{0x01})
	assert.Equal(t, handlers.CompletionCode(0xFF), cc)
}

func TestHandleReadFRUData(t *testing.T) {
	rm := resourcemanager.NewMockResourceManager(gomock.NewController(t))
	rm.EXPECT().GetSystemUUID().Return("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", nil)

	s := NewSimulator("127.0.0.1", 623, rm, "admin", "secret", "1.0")
	s.fruData = s.buildFRU()

	// Read 16 bytes from offset 0 of FRU device 0.
	req := make([]byte, 4)
	req[0] = 0x00 // FRU Device ID 0
	req[3] = 16   // read count
	resp, cc, err := s.handleReadFRUData(context.Background(), nil, req)
	assert.NoError(t, err)
	assert.Equal(t, handlers.CodeOK, cc)
	assert.Equal(t, uint8(16), resp[0], "count returned must be 16")
	assert.Equal(t, s.fruData[:16], resp[1:17], "data must match first 16 bytes of FRU")

	// Verify FRU common header in the response.
	assert.Equal(t, uint8(0x01), resp[1], "FRU format version must be 0x01")
}
