package resourcemanager

import (
	"fmt"
	"time"

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

func (m *VirtualMachineResourceManager) handleTransitionalState(
	opName string,
	opErr error,
	retryOp func() error,
	converged func() (bool, error),
) error {
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

func isPowerOnConverged(vm *kubevirtv1.VirtualMachine, vmi *kubevirtv1.VirtualMachineInstance) bool {
	if vm.Status.Ready && vmDesiresRunning(vm) && !hasPendingStopRequest(vm.Status.StateChangeRequests) {
		return true
	}
	if hasPendingStartRequest(vm.Status.StateChangeRequests) &&
		!hasPendingStopRequest(vm.Status.StateChangeRequests) {
		return true
	}
	rs, rsErr := vm.RunStrategy()
	if rsErr == nil && rs == kubevirtv1.RunStrategyAlways {
		return true
	}
	if rsErr == nil &&
		(rs == kubevirtv1.RunStrategyManual || rs == kubevirtv1.RunStrategyRerunOnFailure) &&
		!hasPendingStopRequest(vm.Status.StateChangeRequests) &&
		vmi != nil && !vmi.IsFinal() {
		return true
	}
	return false
}

func isPowerOffConverged(vm *kubevirtv1.VirtualMachine) bool {
	return !vm.Status.Ready && !hasPendingStartRequest(vm.Status.StateChangeRequests)
}

func isRestartConverged(vm *kubevirtv1.VirtualMachine) bool {
	scrs := vm.Status.StateChangeRequests
	return hasPendingStopRequest(scrs) && hasPendingStartRequest(scrs)
}
