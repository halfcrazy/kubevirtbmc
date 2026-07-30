package ipmi

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/bougou/go-ipmi/pkg/bmc"
	"github.com/bougou/go-ipmi/pkg/hal"
	"github.com/bougou/go-ipmi/pkg/handlers"
	"github.com/bougou/go-ipmi/pkg/types"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"kubevirt.io/kubevirtbmc/pkg/accesslog"
	"kubevirt.io/kubevirtbmc/pkg/resourcemanager"
)

func TestAccessLogMiddlewareLevels(t *testing.T) {
	previousLevel := logrus.GetLevel()
	logrus.SetLevel(logrus.DebugLevel)
	defer logrus.SetLevel(previousLevel)

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	tests := []struct {
		name           string
		labelled       bool
		completionCode types.CompletionCode
		err            error
		level          logrus.Level
	}{
		{name: "labelled success", labelled: true, completionCode: types.CodeOK, level: logrus.InfoLevel},
		{name: "unlabelled success stays quiet", completionCode: types.CodeOK, level: logrus.DebugLevel},
		{name: "node busy", labelled: true, completionCode: types.CodeNodeBusy, level: logrus.WarnLevel},
		{name: "command specific code", labelled: true, completionCode: types.CodeParameterNotSupported, level: logrus.WarnLevel},
		{name: "generic error", labelled: true, completionCode: types.CodeRequestDataFieldInvalid, level: logrus.ErrorLevel},
		{name: "handler error", labelled: true, completionCode: types.CodeOK, err: errors.New("backend failed"), level: logrus.ErrorLevel},
		{name: "unimplemented command probe", completionCode: types.CodeInvalidCommand, err: errors.New("no handler for netFn=0x2c cmd=0x3e"), level: logrus.DebugLevel},
		{name: "unimplemented boot parameter probe", completionCode: types.CodeParameterNotSupported, level: logrus.DebugLevel},
		{name: "unsupported chassis action probe", completionCode: types.CodeParameterOutOfRange, level: logrus.DebugLevel},
		{name: "unimplemented feature probe", completionCode: types.CodeUnspecifiedError, level: logrus.DebugLevel},
		{name: "labelled unspecified error is a real fault", labelled: true, completionCode: types.CodeUnspecifiedError, level: logrus.ErrorLevel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook.Reset()
			next := handlers.HandlerFunc(func(ctx context.Context, _ *handlers.HandlerContext, _ []byte) ([]byte, types.CompletionCode, error) {
				if tt.labelled {
					accesslog.Record(ctx, logrus.Fields{cmdField: "chassis power status"})
				}
				return nil, tt.completionCode, tt.err
			})

			_, completionCode, err := accessLogMiddleware(next).Handle(context.Background(), &handlers.HandlerContext{}, nil)
			require.Equal(t, tt.completionCode, completionCode)
			require.ErrorIs(t, err, tt.err)

			entry := hook.LastEntry()
			require.NotNil(t, entry)
			require.Equal(t, tt.level, entry.Level)
			require.NotEmpty(t, entry.Data["request_id"])
			require.NotEmpty(t, entry.Data["duration"])
			require.NotEmpty(t, entry.Data["completion_code"])
		})
	}
}

func TestAccessLogMiddlewareFormatsCompletionCode(t *testing.T) {
	previousLevel := logrus.GetLevel()
	logrus.SetLevel(logrus.DebugLevel)
	defer logrus.SetLevel(previousLevel)

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	next := handlers.HandlerFunc(func(_ context.Context, _ *handlers.HandlerContext, _ []byte) ([]byte, types.CompletionCode, error) {
		return nil, types.CodeInsufficientPrivilege, nil
	})
	_, _, _ = accessLogMiddleware(next).Handle(context.Background(), &handlers.HandlerContext{}, nil)

	entry := hook.LastEntry()
	require.NotNil(t, entry)
	require.Equal(t, "0xd4", entry.Data["completion_code"])
	require.Equal(t, types.CodeInsufficientPrivilege.String(), entry.Data["completion"])
}

// Command-specific codes (80h-BEh) only have a name relative to the dispatched
// command; without one the field would just duplicate completion_code in hex.
func TestAccessLogMiddlewareNamesCommandSpecificCompletionCode(t *testing.T) {
	previousLevel := logrus.GetLevel()
	logrus.SetLevel(logrus.DebugLevel)
	defer logrus.SetLevel(previousLevel)

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	next := handlers.HandlerFunc(func(_ context.Context, _ *handlers.HandlerContext, _ []byte) ([]byte, types.CompletionCode, error) {
		return nil, types.CodeParameterNotSupported, nil
	})

	for _, cmd := range []types.Command{types.CommandSetSystemBootOptions, types.CommandGetSystemBootOptions} {
		hook.Reset()
		hctx := &handlers.HandlerContext{Command: cmd}
		_, _, _ = accessLogMiddleware(next).Handle(context.Background(), hctx, nil)
		require.Equal(t, "Parameter not supported", hook.LastEntry().Data["completion"], "command %q", cmd.Name)
	}

	hook.Reset()
	hctx := &handlers.HandlerContext{Command: types.CommandChassisControl}
	_, _, _ = accessLogMiddleware(next).Handle(context.Background(), hctx, nil)
	require.Equal(t, "0x80", hook.LastEntry().Data["completion"],
		"commands without a command-specific name keep the hex fallback")
}

func TestAccessLogMiddlewareNamesDispatchedCommand(t *testing.T) {
	previousLevel := logrus.GetLevel()
	logrus.SetLevel(logrus.DebugLevel)
	defer logrus.SetLevel(previousLevel)

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	next := handlers.HandlerFunc(func(_ context.Context, _ *handlers.HandlerContext, _ []byte) ([]byte, types.CompletionCode, error) {
		return nil, types.CodeOK, nil
	})

	hctx := &handlers.HandlerContext{Command: types.CommandChassisControl}
	_, _, _ = accessLogMiddleware(next).Handle(context.Background(), hctx, nil)
	require.Equal(t, "Chassis Control", hook.LastEntry().Data[specCmdField])

	hook.Reset()
	_, _, _ = accessLogMiddleware(next).Handle(context.Background(), &handlers.HandlerContext{}, nil)
	_, ok := hook.LastEntry().Data[specCmdField]
	require.False(t, ok, "unnamed commands should not carry %s", specCmdField)
}

func TestAccessLogMiddlewareNamesUserAndRemote(t *testing.T) {
	previousLevel := logrus.GetLevel()
	logrus.SetLevel(logrus.DebugLevel)
	defer logrus.SetLevel(previousLevel)

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	next := handlers.HandlerFunc(func(_ context.Context, _ *handlers.HandlerContext, _ []byte) ([]byte, types.CompletionCode, error) {
		return nil, types.CodeOK, nil
	})

	sess := &bmc.Session{}
	sess.SetAddr(&net.UDPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 40123})
	hctx := &handlers.HandlerContext{Session: sess, User: &bmc.User{Name: "admin"}}
	_, _, _ = accessLogMiddleware(next).Handle(context.Background(), hctx, nil)
	entry := hook.LastEntry()
	require.Equal(t, "admin", entry.Data["user"])
	require.Equal(t, "10.0.0.7:40123", entry.Data["remote"])

	hook.Reset()
	_, _, _ = accessLogMiddleware(next).Handle(context.Background(), &handlers.HandlerContext{}, nil)
	entry = hook.LastEntry()
	_, ok := entry.Data["user"]
	require.False(t, ok, "pre-session traffic has no user")
	_, ok = entry.Data["remote"]
	require.False(t, ok, "pre-session traffic has no session address")
}

// Drives a real Registry to pin the two go-ipmi Dispatch behaviours the
// middleware relies on: hctx.Command is filled before the chain runs, and
// unregistered commands still reach the chain, with CodeInvalidCommand.
func TestAccessLogMiddlewareThroughRegistryDispatch(t *testing.T) {
	previousLevel := logrus.GetLevel()
	logrus.SetLevel(logrus.DebugLevel)
	defer logrus.SetLevel(previousLevel)

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	reg := handlers.NewRegistry()
	reg.Use(accessLogMiddleware)
	reg.Register(types.CommandChassisControl, handlers.HandlerFunc(
		func(context.Context, *handlers.HandlerContext, []byte) ([]byte, types.CompletionCode, error) {
			return nil, types.CodeOK, nil
		}))

	// Chassis Control needs an authenticated session: go-ipmi rejects
	// session-less LAN requests with D4h before the chain sees them.
	hctx := &handlers.HandlerContext{Session: &bmc.Session{PrivilegeLevel: bmc.PrivilegeLevelAdministrator}}
	_, cc, err := reg.Dispatch(context.Background(), hctx,
		uint8(types.CommandChassisControl.NetFn), types.CommandChassisControl.ID, nil)
	require.NoError(t, err)
	require.Equal(t, types.CodeOK, cc)

	entry := hook.LastEntry()
	require.NotNil(t, entry)
	require.Equal(t, "Chassis Control", entry.Data[specCmdField])

	hook.Reset()
	_, cc, err = reg.Dispatch(context.Background(), hctx, 0x2c, 0x3e, nil)
	require.Error(t, err)
	require.Equal(t, types.CodeInvalidCommand, cc)

	entry = hook.LastEntry()
	require.NotNil(t, entry)
	require.Equal(t, logrus.DebugLevel, entry.Level)
	require.Equal(t, "0xc1", entry.Data["completion_code"])
	_, ok := entry.Data[specCmdField]
	require.False(t, ok, "probes for unregistered commands should not carry %s", specCmdField)
	require.Contains(t, entry.Data[logrus.ErrorKey].(error).Error(), "no handler")
}

func TestLoggingChassisLabelsCommands(t *testing.T) {
	previousLevel := logrus.GetLevel()
	logrus.SetLevel(logrus.DebugLevel)
	defer logrus.SetLevel(previousLevel)

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	efi := true
	tests := []struct {
		name       string
		expect     logrus.Fields
		expectCall func(*resourcemanager.MockResourceManager)
		invoke     func(context.Context, hal.ChassisHAL) error
	}{
		{
			name:   "power status",
			expect: logrus.Fields{cmdField: "chassis power status", "power": "on"},
			expectCall: func(rm *resourcemanager.MockResourceManager) {
				rm.EXPECT().GetPowerStatus(gomock.Any()).Return(true, nil)
			},
			invoke: func(ctx context.Context, c hal.ChassisHAL) error {
				_, err := c.PowerState(ctx)
				return err
			},
		},
		{
			name:   "power on",
			expect: logrus.Fields{cmdField: "chassis power on"},
			expectCall: func(rm *resourcemanager.MockResourceManager) {
				rm.EXPECT().PowerOn(gomock.Any()).Return(nil)
			},
			invoke: func(ctx context.Context, c hal.ChassisHAL) error { return c.SetPower(ctx, true) },
		},
		{
			name:   "power off",
			expect: logrus.Fields{cmdField: "chassis power off"},
			expectCall: func(rm *resourcemanager.MockResourceManager) {
				rm.EXPECT().ForcePowerOff(gomock.Any()).Return(nil)
			},
			invoke: func(ctx context.Context, c hal.ChassisHAL) error { return c.SetPower(ctx, false) },
		},
		{
			name:   "power cycle",
			expect: logrus.Fields{cmdField: "chassis power cycle"},
			expectCall: func(rm *resourcemanager.MockResourceManager) {
				rm.EXPECT().ForcePowerCycle(gomock.Any()).Return(nil)
			},
			invoke: func(ctx context.Context, c hal.ChassisHAL) error { return c.PowerCycle(ctx) },
		},
		{
			name:   "hard reset",
			expect: logrus.Fields{cmdField: "chassis power reset"},
			expectCall: func(rm *resourcemanager.MockResourceManager) {
				rm.EXPECT().PowerCycle(gomock.Any()).Return(nil)
			},
			invoke: func(ctx context.Context, c hal.ChassisHAL) error { return c.ColdReset(ctx) },
		},
		{
			name:   "soft shutdown",
			expect: logrus.Fields{cmdField: "chassis power soft"},
			expectCall: func(rm *resourcemanager.MockResourceManager) {
				rm.EXPECT().PowerOff(gomock.Any()).Return(nil)
			},
			invoke: func(ctx context.Context, c hal.ChassisHAL) error { return c.WarmReset(ctx) },
		},
		{
			name:   "set boot flags",
			expect: logrus.Fields{cmdField: "chassis bootdev pxe options=persistent,efiboot"},
			expectCall: func(rm *resourcemanager.MockResourceManager) {
				rm.EXPECT().SetBootDevice(gomock.Any(), resourcemanager.BootDevicePxe, gomock.Any()).Return(nil)
			},
			invoke: func(ctx context.Context, c hal.ChassisHAL) error {
				return c.SetBootFlags(ctx, &types.BootOptionParam_BootFlags{
					BootDeviceSelector: types.BootDeviceSelectorForcePXE,
					Persist:            true,
					BIOSBootType:       true,
				})
			},
		},
		{
			name:   "get boot flags",
			expect: logrus.Fields{cmdField: "chassis bootparam get 5"},
			expectCall: func(rm *resourcemanager.MockResourceManager) {
				rm.EXPECT().GetBootFlags(gomock.Any()).Return(&resourcemanager.BootFlagsState{
					BootDevice: resourcemanager.BootDevicePxe,
					EFIBoot:    efi,
				}, nil)
			},
			invoke: func(ctx context.Context, c hal.ChassisHAL) error {
				_, err := c.GetBootFlags(ctx)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook.Reset()
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockRM := resourcemanager.NewMockResourceManager(ctrl)
			tt.expectCall(mockRM)

			chassis := loggingChassis{ChassisHAL: vmChassis{rm: mockRM}}
			next := handlers.HandlerFunc(func(ctx context.Context, _ *handlers.HandlerContext, _ []byte) ([]byte, types.CompletionCode, error) {
				return nil, types.CodeOK, tt.invoke(ctx, chassis)
			})
			_, _, err := accessLogMiddleware(next).Handle(context.Background(), &handlers.HandlerContext{}, nil)
			require.NoError(t, err)

			entry := hook.LastEntry()
			require.NotNil(t, entry)
			for key, want := range tt.expect {
				require.Equal(t, want, entry.Data[key], "field %q", key)
			}
		})
	}
}

func TestBootdevCmd(t *testing.T) {
	tests := []struct {
		flags *types.BootOptionParam_BootFlags
		want  string
	}{
		{flags: nil, want: "chassis bootdev"},
		{
			flags: &types.BootOptionParam_BootFlags{BootDeviceSelector: types.BootDeviceSelectorNoOverride},
			want:  "chassis bootdev none",
		},
		{
			flags: &types.BootOptionParam_BootFlags{BootDeviceSelector: types.BootDeviceSelectorForcePXE},
			want:  "chassis bootdev pxe",
		},
		{
			flags: &types.BootOptionParam_BootFlags{BootDeviceSelector: types.BootDeviceSelectorForceHardDrive, Persist: true},
			want:  "chassis bootdev disk options=persistent",
		},
		{
			flags: &types.BootOptionParam_BootFlags{BootDeviceSelector: types.BootDeviceSelectorForceCDROM, BIOSBootType: true},
			want:  "chassis bootdev cdrom options=efiboot",
		},
		{
			flags: &types.BootOptionParam_BootFlags{
				BootDeviceSelector: types.BootDeviceSelectorForcePXE,
				Persist:            true,
				BIOSBootType:       true,
			},
			want: "chassis bootdev pxe options=persistent,efiboot",
		},
		{
			flags: &types.BootOptionParam_BootFlags{BootDeviceSelector: types.BootDeviceSelectorForceRemoteMedia},
			want:  "chassis bootdev 0x09",
		},
	}

	for _, tt := range tests {
		require.Equal(t, tt.want, bootdevCmd(tt.flags))
	}
}
