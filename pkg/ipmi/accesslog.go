package ipmi

import (
	"context"
	"fmt"
	"strings"

	"github.com/bougou/go-ipmi/pkg/hal"
	"github.com/bougou/go-ipmi/pkg/handlers"
	"github.com/bougou/go-ipmi/pkg/types"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"kubevirt.io/kubevirtbmc/pkg/accesslog"
)

// cmdField holds the ipmitool-style rendering of the command being served
// ("chassis power on"). Its presence marks the request as one that reached a
// HAL operation, which is what ipmiLevel keys on.
const cmdField = "cmd"

// specCmdField holds the spec name of the dispatched command ("Chassis
// Control"). It names the command, not the action: one spec command covers
// on, off, cycle and reset alike, so it complements rather than replaces cmdField.
const specCmdField = "ipmi_cmd"

// accessLogMiddleware emits one access-log line per dispatched IPMI command.
// hctx.Command is filled by Registry.Dispatch before the chain runs (zero for
// unregistered commands); the ipmitool-style action lands one layer down, in
// loggingChassis, where the decoded HAL call is visible. user and remote are
// absent on pre-session traffic (Get Channel Authentication Capabilities,
// RAKP), which is the exchange that establishes them.
func accessLogMiddleware(next handlers.Handler) handlers.Handler {
	return handlers.HandlerFunc(func(ctx context.Context, hctx *handlers.HandlerContext, data []byte) ([]byte, types.CompletionCode, error) {
		ctx = accesslog.Start(ctx, uuid.NewString())

		response, completionCode, err := next.Handle(ctx, hctx, data)

		fields := logrus.Fields{"completion_code": fmt.Sprintf("0x%02x", uint8(completionCode))}
		var cmd types.Command
		if hctx != nil {
			cmd = hctx.Command
			if hctx.Command.Name != "" {
				fields[specCmdField] = hctx.Command.Name
			}
			if hctx.User != nil && hctx.User.Name != "" {
				fields["user"] = hctx.User.Name
			}
			// Only RMCP+ sessions track the console address; v1.5 sessions
			// carry none, so the field is simply left out for them.
			if hctx.Session != nil {
				if addr := hctx.Session.GetAddr(); addr != nil {
					fields["remote"] = addr.String()
				}
			}
		}
		if completionCode != types.CodeOK {
			fields["completion"] = types.StrCC(cmd, uint8(completionCode))
		}
		accesslog.Emit(ctx, ipmiLevel(ctx, completionCode, err), "ipmi request", err, fields)

		return response, completionCode, err
	})
}

// ipmiLevel grades a completion code by the ranges of IPMI 2.0 §5.2 Table 5-2:
// 00h is success; 80h-BEh are command-specific codes, i.e. normal negative
// answers ("parameter not supported") rather than faults, so they warn; C0h-FFh
// are generic errors, except Node Busy and the state/privilege rejections an
// initiator is expected to hit and retry, which also warn.
//
// Requests without cmdField stay at debug whatever the outcome. They never
// reached a HAL operation: session setup, RAKP, Get Device ID, and negative
// answers issued before any HAL call — probes for things the BMC does not
// implement (ipmitool writes set-in-progress before every `chassis bootdev`
// and tolerates 80h by design; `chassis power diag` gets C9h; `chassis
// identify` FFh). One ipmitool invocation drives several of these and at
// info/warn they would bury the line that says what the client asked for.
// Nothing actionable is lost: loggingChassis records cmdField before the HAL
// call, so every operation that can genuinely fault is labelled.
//
// Invalid Command (C1h) is debug for the same reason: go-ipmi runs the chain
// for unregistered commands too, which is how probes for commands the BMC
// lacks (ipmitool's DCMI probes, for one) show up.
func ipmiLevel(ctx context.Context, cc types.CompletionCode, err error) logrus.Level {
	labelled := accesslog.Has(ctx, cmdField)
	switch {
	case cc == types.CodeInvalidCommand:
		return logrus.DebugLevel
	case err != nil:
		return logrus.ErrorLevel
	case cc == types.CodeOK:
		if labelled {
			return logrus.InfoLevel
		}
		return logrus.DebugLevel
	case !labelled:
		return logrus.DebugLevel
	case cc < 0xC0:
		return logrus.WarnLevel
	case cc == types.CodeNodeBusy,
		cc == types.CodeInsufficientPrivilege,
		cc == types.CodeNotSupported:
		return logrus.WarnLevel
	default:
		return logrus.ErrorLevel
	}
}

// loggingChassis records the ipmitool command each HAL call corresponds to (and
// the result, where one is worth reading back). As a decorator it derives the
// label from the request as it arrived rather than the translated arguments,
// and leaves vmChassis as pure spec-to-KubeVirt mapping.
type loggingChassis struct {
	hal.ChassisHAL
}

func (c loggingChassis) PowerState(ctx context.Context) (bool, error) {
	on, err := c.ChassisHAL.PowerState(ctx)
	fields := logrus.Fields{cmdField: "chassis power status"}
	if err == nil {
		fields["power"] = powerWord(on)
	}
	accesslog.Record(ctx, fields)
	return on, err
}

func (c loggingChassis) SetPower(ctx context.Context, on bool) error {
	accesslog.Record(ctx, logrus.Fields{cmdField: "chassis power " + powerWord(on)})
	return c.ChassisHAL.SetPower(ctx, on)
}

func (c loggingChassis) PowerCycle(ctx context.Context) error {
	accesslog.Record(ctx, logrus.Fields{cmdField: "chassis power cycle"})
	return c.ChassisHAL.PowerCycle(ctx)
}

func (c loggingChassis) ColdReset(ctx context.Context) error {
	accesslog.Record(ctx, logrus.Fields{cmdField: "chassis power reset"})
	return c.ChassisHAL.ColdReset(ctx)
}

func (c loggingChassis) WarmReset(ctx context.Context) error {
	accesslog.Record(ctx, logrus.Fields{cmdField: "chassis power soft"})
	return c.ChassisHAL.WarmReset(ctx)
}

func (c loggingChassis) SetBootFlags(ctx context.Context, flags *types.BootOptionParam_BootFlags) error {
	accesslog.Record(ctx, logrus.Fields{cmdField: bootdevCmd(flags)})
	return c.ChassisHAL.SetBootFlags(ctx, flags)
}

func (c loggingChassis) GetBootFlags(ctx context.Context) (*types.BootOptionParam_BootFlags, error) {
	accesslog.Record(ctx, logrus.Fields{cmdField: "chassis bootparam get 5"})
	return c.ChassisHAL.GetBootFlags(ctx)
}

func powerWord(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// bootdevCmd renders Set System Boot Options boot flags (spec Table 28-6) as
// the ipmitool command line that produces them, so an operator can correlate
// the log with what they ran.
func bootdevCmd(flags *types.BootOptionParam_BootFlags) string {
	if flags == nil {
		return "chassis bootdev"
	}

	if flags.BootDeviceSelector == types.BootDeviceSelectorNoOverride {
		// ipmitool has no "clear the override" verb; `bootdev none` is how
		// callers express selector 0000b.
		return "chassis bootdev none"
	}

	cmd := "chassis bootdev " + bootdevName(flags.BootDeviceSelector)
	var options []string
	if flags.Persist {
		options = append(options, "persistent")
	}
	if flags.BIOSBootType {
		options = append(options, "efiboot")
	}
	if len(options) > 0 {
		cmd += " options=" + strings.Join(options, ",")
	}
	return cmd
}

func bootdevName(selector types.BootDeviceSelector) string {
	switch selector {
	case types.BootDeviceSelectorForcePXE:
		return "pxe"
	case types.BootDeviceSelectorForceHardDrive, types.BootDeviceSelectorForceHardDriveSafe:
		return "disk"
	case types.BootDeviceSelectorForceCDROM:
		return "cdrom"
	case types.BootDeviceSelectorForceBIOSSetup:
		return "bios"
	case types.BootDeviceSelectorForceDiagnosticPartition:
		return "diag"
	case types.BootDeviceSelectorForceFloppy:
		return "floppy"
	default:
		return fmt.Sprintf("0x%02x", uint8(selector))
	}
}
