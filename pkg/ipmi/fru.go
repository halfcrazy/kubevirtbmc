package ipmi

import (
	"context"
	"encoding/binary"

	"github.com/bougou/go-ipmi/pkg/handlers"
)

// --- FRU binary helpers ----------------------------------------------------

// encodeFRUString encodes a string in FRU type/length format
// (8-bit ASCII + Latin1). Length is capped at 63 bytes.
func encodeFRUString(s string) []byte {
	if len(s) > 63 {
		s = s[:63]
	}
	buf := []byte{0xC0 | byte(len(s))}
	buf = append(buf, []byte(s)...)
	return buf
}

// fruChecksum computes the FRU checksum byte (zero-sum).
func fruChecksum(data []byte) byte {
	var sum uint8
	for _, b := range data {
		sum += b
	}
	return byte(-sum)
}

// buildProductArea constructs the FRU Product Info Area per
// Platform Management FRU Information Storage Definition.
func buildProductArea(manufacturer, product, version, serial string) []byte {
	area := []byte{
		0x01, // format version
		0x00, // area length (filled in later, in multiples of 8 bytes)
		0x00, // language code (English)
	}

	// Fields in spec order:
	area = append(area, encodeFRUString(manufacturer)...) // 1. Manufacturer Name
	area = append(area, encodeFRUString(product)...)      // 2. Product Name
	area = append(area, 0xC0)                             // 3. Part/Model Number (empty)
	area = append(area, encodeFRUString(version)...)      // 4. Product Version
	area = append(area, encodeFRUString(serial)...)       // 5. Product Serial Number
	area = append(area, 0xC0)                             // 6. Asset Tag (empty)
	area = append(area, 0xC0)                             // 7. FRU File ID (empty)
	area = append(area, 0xC1)                             // end-of-fields sentinel

	// Pad to multiple of 8 bytes
	for (len(area)+1)%8 != 0 {
		area = append(area, 0)
	}
	area[1] = byte((len(area) + 1) / 8) // area length in 8-byte units
	area = append(area, fruChecksum(area))
	return area
}

// buildFRUData constructs a complete FRU binary blob with Common Header and
// Product Info Area, following the IPMI FRU specification.
func buildFRUData(manufacturer, product, version, serial string) []byte {
	productArea := buildProductArea(manufacturer, product, version, serial)

	header := make([]byte, 8)
	header[0] = 0x01 // format version
	header[4] = 0x01 // Product Info Area starts at offset 1 (byte 8)
	header[7] = fruChecksum(header[:7])

	return append(header, productArea...)
}

// --- FRU construction -------------------------------------------------------

// buildFRU constructs the FRU binary blob using the VM UUID as the serial.
// The serial is stored as a human-readable UUID string and is not affected
// by the IPMI wire-format byte order applied in resolveGUID.
func (s *Simulator) buildFRU() []byte {
	serial := "00000000-0000-0000-0000-000000000000"
	if s.rm != nil {
		if uidStr, err := s.rm.GetSystemUUID(); err == nil && uidStr != "" {
			serial = uidStr
		}
	}
	version := s.appVersion
	if version == "" {
		version = "1.0"
	}
	return buildFRUData("KubeVirt", "KubeVirtBMC", version, serial)
}

// --- Storage handlers -------------------------------------------------------

// handleGetFRUInventoryAreaInfo implements Get FRU Inventory Area Info (Storage 0x10).
func (s *Simulator) handleGetFRUInventoryAreaInfo(
	_ context.Context, _ *handlers.HandlerContext, data []byte,
) ([]byte, handlers.CompletionCode, error) {
	if len(data) < 1 {
		return nil, 0xFF, nil
	}
	fruID := data[0]
	if fruID != 0 {
		return nil, 0xFF, nil
	}
	size := uint16(len(s.fruData))
	resp := make([]byte, 3)
	binary.LittleEndian.PutUint16(resp[0:2], size)
	resp[2] = 0x00 // access type: byte
	return resp, handlers.CodeOK, nil
}

// handleReadFRUData implements Read FRU Data (Storage 0x11).
func (s *Simulator) handleReadFRUData(
	_ context.Context, _ *handlers.HandlerContext, data []byte,
) ([]byte, handlers.CompletionCode, error) {
	if len(data) < 4 {
		return nil, 0xFF, nil
	}
	fruID := data[0]
	if fruID != 0 {
		return nil, 0xFF, nil
	}
	offset := binary.LittleEndian.Uint16(data[1:3])
	count := int(data[3])

	if int(offset) >= len(s.fruData) {
		return nil, 0xFF, nil
	}
	end := int(offset) + count
	if end > len(s.fruData) {
		count = len(s.fruData) - int(offset)
	}

	resp := make([]byte, 1+count)
	resp[0] = byte(count)
	copy(resp[1:], s.fruData[offset:offset+uint16(count)])
	return resp, handlers.CodeOK, nil
}

// --- SDR helpers and handler ------------------------------------------------

// mcFRULocatorBody is the 11-byte body of a Management Controller FRU
// Locator Record (SDR type 0x12) declaring FRU device 0.  The layout
// matches ipmitool's sdr_record_mc_locator struct per IPMI v2.0 Table 43-8.
var mcFRULocatorBody = []byte{
	0x20,             // byte 0: dev_slave_addr (BMC)
	0x00,             // byte 1: channel_num:4 | reserved:4
	0x00,             // byte 2: global_init:4 | reserved:1 | pwr_state_notif:3
	0x08,             // byte 3: dev_support (FRU Inventory Device)
	0x00, 0x00, 0x00, // bytes 4-6: reserved[3]
	0x07, // byte 7: entity.id (Board)
	0x01, // byte 8: entity.instance
	0x00, // byte 9: oem
	0xC0, // byte 10: id_code (empty)
}

// handleGetSDRRepoInfo implements Get SDR Repository Info (Storage 0x20).
// Reports one SDR record so ipmitool discovers FRU device 0 through SDR
// rather than trying to rebuild an empty SDRR.
func (s *Simulator) handleGetSDRRepoInfo(
	_ context.Context, _ *handlers.HandlerContext, _ []byte,
) ([]byte, handlers.CompletionCode, error) {
	resp := make([]byte, 14)
	resp[0] = 0x51                              // SDR version 1.5
	binary.LittleEndian.PutUint16(resp[1:3], 1) // 1 record
	resp[3] = 0xff                              // free space: unspecified
	resp[4] = 0xff
	resp[13] = 0x02 // op support: Reserve SDR
	return resp, handlers.CodeOK, nil
}

// handleReserveSDR implements Reserve SDR Repository (Storage 0x22).
func (s *Simulator) handleReserveSDR(
	_ context.Context, _ *handlers.HandlerContext, _ []byte,
) ([]byte, handlers.CompletionCode, error) {
	return []byte{0x01, 0x00}, handlers.CodeOK, nil
}

// handleGetSDR implements Get SDR (Storage 0x23).  It returns the single
// MC FRU Locator record, respecting offset/count from the request.
func (s *Simulator) handleGetSDR(
	_ context.Context, _ *handlers.HandlerContext, data []byte,
) ([]byte, handlers.CompletionCode, error) {
	// Record data: header (5 bytes) + body (11 bytes).
	recData := make([]byte, 0, 5+len(mcFRULocatorBody))
	recData = append(recData, 0x01, 0x00) // Record ID = 1 (LS first)
	recData = append(recData, 0x51)       // SDR version
	recData = append(recData, 0x12)       // Record Type: MC Device Locator
	recData = append(recData, 0x0b)       // Body length: 11 bytes
	recData = append(recData, mcFRULocatorBody...)

	// Request: [0:2]reservationID [2:4]recordID [4]offset [5]count
	// count=0xFF means "read entire record" per IPMI spec §33.12.
	offset := 0
	count := len(recData)
	if len(data) >= 6 {
		offset = int(data[4])
		count = int(data[5])
		if count == 0xFF {
			count = len(recData) - offset
		}
	}
	if offset > len(recData) {
		return nil, 0xFF, nil
	}
	if offset+count > len(recData) {
		count = len(recData) - offset
	}

	resp := make([]byte, 0, 2+count)
	resp = append(resp, 0xFF, 0xFF) // nextRecordID: no more records
	resp = append(resp, recData[offset:offset+count]...)
	return resp, handlers.CodeOK, nil
}

// --- App handler ------------------------------------------------------------

// handleGetSystemGUID implements Get System GUID (App 0x37), returning the
// same BMC GUID used for RAKP.  go-ipmi registers Get Device GUID (0x08) but
// not Get System GUID (0x37), which is what ipmitool "mc guid" sends.
func (s *Simulator) handleGetSystemGUID(
	_ context.Context, hctx *handlers.HandlerContext, _ []byte,
) ([]byte, handlers.CompletionCode, error) {
	g := hctx.BMC.GUID
	resp := make([]byte, 16)
	copy(resp, g[:])
	return resp, handlers.CodeOK, nil
}
