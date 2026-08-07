package resourcemanager

import (
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
	bmcv1 "kubevirt.io/kubevirtbmc/api/bmc/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultTransitionalMaxWaitSeconds      int32 = 60
	defaultTransitionalPollIntervalSeconds int32 = 2
)

type transitionalStateConfig struct {
	Strategy            bmcv1.TransitionalStateStrategy
	MaxWaitSeconds      int32
	PollIntervalSeconds int32
}

func defaultTransitionalStateConfig() transitionalStateConfig {
	return transitionalStateConfig{
		Strategy:            bmcv1.TransitionalStateStrategyRetrySignal,
		MaxWaitSeconds:      defaultTransitionalMaxWaitSeconds,
		PollIntervalSeconds: defaultTransitionalPollIntervalSeconds,
	}
}

func (m *VirtualMachineResourceManager) loadTransitionalStateConfig() error {
	bmc := &bmcv1.VirtualMachineBMC{}
	if err := m.bmcClient.Get(m.ctx, client.ObjectKey{Namespace: m.namespace, Name: m.bmcName}, bmc); err != nil {
		return fmt.Errorf("failed to get VirtualMachineBMC: %w", err)
	}

	cfg := defaultTransitionalStateConfig()
	if spec := bmc.Spec.TransitionalState; spec != nil {
		if spec.Strategy != "" {
			cfg.Strategy = spec.Strategy
		}
		if spec.MaxWaitSeconds != nil {
			cfg.MaxWaitSeconds = *spec.MaxWaitSeconds
		}
		if spec.PollIntervalSeconds != nil {
			cfg.PollIntervalSeconds = *spec.PollIntervalSeconds
		}
	}
	m.transitionalState = cfg
	return nil
}

// tryThenVerify runs a power operation and, on failure, checks the real
// VM/VMI state instead of depending on KubeVirt error strings: an operation
// that failed because the state already converged is a success. Any other
// failure is handed to handleTransitionalState, with the same convergence
// check driving the wait polls.
func (m *VirtualMachineResourceManager) tryThenVerify(
	opName string,
	verb string,
	op func() error,
	check func(*kubevirtv1.VirtualMachine, *kubevirtv1.VirtualMachineInstance) bool,
) error {
	err := op()
	if err == nil {
		return nil
	}
	vm, vmi, getErr := m.fetchVMAndVMI()
	if getErr != nil {
		return fmt.Errorf("%s failed: %w; verify state also failed: %v", verb, err, getErr)
	}
	if check(vm, vmi) {
		return nil
	}
	return m.handleTransitionalState(opName, err, op, m.convergenceCheck(check))
}

func (m *VirtualMachineResourceManager) handleTransitionalState(
	opName string,
	opErr error,
	retryOp func() error,
	converged func() (bool, error),
) error {
	// Re-read the strategy on the transitional path so CR edits take effect
	// without a virtbmc pod restart. Best-effort: a failed read keeps the
	// previously loaded config rather than failing the power operation.
	if m.bmcClient != nil {
		if err := m.loadTransitionalStateConfig(); err != nil {
			logrus.WithError(err).Warn("failed to reload transitional state config, using cached value")
		}
	}
	if m.transitionalState.Strategy != bmcv1.TransitionalStateStrategyServerWait {
		return &ErrRetryable{Err: opErr}
	}
	return m.serverWait(opName, opErr, retryOp, converged)
}

func (m *VirtualMachineResourceManager) serverWait(
	opName string,
	opErr error,
	retryOp func() error,
	converged func() (bool, error),
) error {
	deadline := time.Now().Add(time.Duration(m.transitionalState.MaxWaitSeconds) * time.Second)
	interval := time.Duration(m.transitionalState.PollIntervalSeconds) * time.Second

	for {
		ok, err := converged()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%s: transitional state wait timed out after %ds: %w",
				opName, m.transitionalState.MaxWaitSeconds, opErr)
		}

		select {
		case <-m.ctx.Done():
			return fmt.Errorf("%s: %w", opName, m.ctx.Err())
		case <-time.After(interval):
		}

		if err := retryOp(); err == nil {
			return nil
		}
	}
}

// fetchVMAndVMI reads the current VM and VMI. The VMI read is best-effort:
// a VM that is down or mid-teardown has no (readable) VMI, and the
// convergence predicates treat a nil VMI as "no live VMI", so a VMI read
// error is not fatal — only a VM read error is.
func (m *VirtualMachineResourceManager) fetchVMAndVMI() (*kubevirtv1.VirtualMachine, *kubevirtv1.VirtualMachineInstance, error) {
	vm, err := m.virtClient.KubevirtV1().VirtualMachines(m.namespace).
		Get(m.ctx, m.name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	vmi, vmiErr := m.virtClient.KubevirtV1().VirtualMachineInstances(m.namespace).
		Get(m.ctx, m.name, metav1.GetOptions{})
	if vmiErr != nil {
		// Generated clientsets (real and fake alike, via client-go gentype)
		// return a non-nil empty object alongside the error, so the error is
		// the only failure signal; normalize to nil so vmi==nil keeps meaning
		// "no live VMI" for the predicates.
		vmi = nil
	}
	return vm, vmi, nil
}

// convergenceCheck adapts a VM/VMI convergence predicate into the polling
// closure handleTransitionalState/serverWait expect.
func (m *VirtualMachineResourceManager) convergenceCheck(
	check func(*kubevirtv1.VirtualMachine, *kubevirtv1.VirtualMachineInstance) bool,
) func() (bool, error) {
	return func() (bool, error) {
		vm, vmi, err := m.fetchVMAndVMI()
		if err != nil {
			return false, err
		}
		return check(vm, vmi), nil
	}
}

// isPowerOnConverged reports whether the power-on intent is observable in the
// VM/VMI state. vm.Status.Ready stays true while the VMI is gracefully
// terminating (until the QEMU process exits), and Manual/RerunOnFailure keep
// their run intent while a Stop is underway (Stop travels via
// StateChangeRequests, leaving runStrategy untouched), so "on" must be
// verified against the VMI's direction: only a VMI that is neither
// terminating nor final counts.
func isPowerOnConverged(vm *kubevirtv1.VirtualMachine, vmi *kubevirtv1.VirtualMachineInstance) bool {
	vmiAlive := vmi != nil && vmi.DeletionTimestamp.IsZero() && !vmi.IsFinal()

	if vm.Status.Ready && vmDesiresRunning(vm) &&
		!hasPendingStopRequest(vm.Status.StateChangeRequests) && vmiAlive {
		return true
	}
	if hasPendingStartRequest(vm.Status.StateChangeRequests) &&
		!hasPendingStopRequest(vm.Status.StateChangeRequests) {
		return true
	}
	rs, rsErr := vm.RunStrategy()
	// Always is safe without the VMI check: Stop flips the strategy to Halted
	// before the VMI is torn down, so observing Always means no stop is
	// underway and KubeVirt itself guarantees the run intent.
	if rsErr == nil && rs == kubevirtv1.RunStrategyAlways {
		return true
	}
	if rsErr == nil &&
		(rs == kubevirtv1.RunStrategyManual || rs == kubevirtv1.RunStrategyRerunOnFailure) &&
		!hasPendingStopRequest(vm.Status.StateChangeRequests) &&
		vmiAlive {
		return true
	}
	return false
}

func isPowerOffConverged(vm *kubevirtv1.VirtualMachine) bool {
	return !vm.Status.Ready && !hasPendingStartRequest(vm.Status.StateChangeRequests)
}

// powerOffConverged adapts isPowerOffConverged to the VM/VMI predicate shape
// tryThenVerify takes; power-off convergence does not consult the VMI.
func powerOffConverged(vm *kubevirtv1.VirtualMachine, _ *kubevirtv1.VirtualMachineInstance) bool {
	return isPowerOffConverged(vm)
}

func isRestartConverged(vm *kubevirtv1.VirtualMachine) bool {
	scrs := vm.Status.StateChangeRequests
	return hasPendingStopRequest(scrs) && hasPendingStartRequest(scrs)
}

// isPowerCycleConverged reports whether a power cycle intent is satisfied:
// either a full restart (stop+start) is queued, or the VM reached a genuinely
// running state. The wait path is only entered after a Restart failure, so
// observing a healthy running VMI implies the in-flight transition (whoever
// triggered it) completed — retrying Restart at that point would cycle the
// VM a second time.
func isPowerCycleConverged(vm *kubevirtv1.VirtualMachine, vmi *kubevirtv1.VirtualMachineInstance) bool {
	return isRestartConverged(vm) || isPowerOnConverged(vm, vmi)
}
