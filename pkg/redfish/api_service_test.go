package redfish

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"go.uber.org/mock/gomock"
	"github.com/stretchr/testify/assert"

	"kubevirt.io/kubevirtbmc/pkg/generated/redfish/server"
	"kubevirt.io/kubevirtbmc/pkg/resourcemanager"
)

func ambiguousCdErr() error {
	return &resourcemanager.AmbiguousBootDeviceError{
		BootDevice: resourcemanager.BootDeviceCd,
		Candidates: []string{"cdrom-a", "cdrom-b"},
	}
}

// Ambiguity is a conflict with the target system's present device
// configuration (RFC 9110 §15.5.10), so clients get a deterministic 4xx
// rather than a retryable-looking 5xx.
func TestPatchComputerSystemAmbiguousBootDeviceReturns409(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRM := resourcemanager.NewMockResourceManager(ctrl)
	mockRM.EXPECT().SetBootDevice(resourcemanager.BootDeviceCd, gomock.Any()).Return(ambiguousCdErr())
	svc := NewAPIService(testUsername, testPassword, mockRM)

	resp, err := svc.RedfishV1SystemsComputerSystemIdPatch(context.Background(), "1",
		server.ComputerSystemV1220ComputerSystem{
			Boot: server.ComputerSystemV1220Boot{
				BootSourceOverrideEnabled: server.COMPUTERSYSTEMV1220BOOTSOURCEOVERRIDEENABLED_ONCE,
				BootSourceOverrideTarget:  server.COMPUTERSYSTEMBOOTSOURCE_CD,
			},
		})
	assert.NoError(t, err)
	assert.Equal(t, http.StatusConflict, resp.Code)
}

func TestSetDefaultBootOrderAmbiguousBootDeviceReturns409(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRM := resourcemanager.NewMockResourceManager(ctrl)
	mockRM.EXPECT().SetBootDevice(resourcemanager.BootDeviceCd, gomock.Any()).Return(ambiguousCdErr())
	svc := NewAPIService(testUsername, testPassword, mockRM)

	resp, err := svc.RedfishV1SystemsComputerSystemIdActionsComputerSystemSetDefaultBootOrderPost(
		context.Background(), "1", map[string]interface{}{"BootOrder": []interface{}{"Cd"}})
	assert.NoError(t, err)
	assert.Equal(t, http.StatusConflict, resp.Code)
}

func TestPatchComputerSystemGenericErrorStillReturns500(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockRM := resourcemanager.NewMockResourceManager(ctrl)
	mockRM.EXPECT().SetBootDevice(resourcemanager.BootDeviceCd, gomock.Any()).Return(fmt.Errorf("kubevirt api down"))
	svc := NewAPIService(testUsername, testPassword, mockRM)

	resp, err := svc.RedfishV1SystemsComputerSystemIdPatch(context.Background(), "1",
		server.ComputerSystemV1220ComputerSystem{
			Boot: server.ComputerSystemV1220Boot{
				BootSourceOverrideEnabled: server.COMPUTERSYSTEMV1220BOOTSOURCEOVERRIDEENABLED_ONCE,
				BootSourceOverrideTarget:  server.COMPUTERSYSTEMBOOTSOURCE_CD,
			},
		})
	assert.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, resp.Code)
}
