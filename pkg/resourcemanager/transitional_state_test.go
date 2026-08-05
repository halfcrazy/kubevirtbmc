package resourcemanager

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	kubevirtv1 "kubevirt.io/api/core/v1"
	kubevirtfake "kubevirt.io/client-go/kubevirt/fake"

	bmcv1 "kubevirt.io/kubevirtbmc/api/bmc/v1beta1"
	"kubevirt.io/kubevirtbmc/pkg/builder"
)

func TestServerWait_PowerOnRetriesUntilSuccess(t *testing.T) {
	vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).Ready(false).Build()
	fakeVirtClient := kubevirtfake.NewSimpleClientset(vm)

	var startCalls atomic.Int32
	fakeVirtClient.PrependReactor("put", "virtualmachines", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "start" {
			return false, nil, nil
		}
		if startCalls.Add(1) == 1 {
			return true, nil, errors.New("VM is already running")
		}
		return true, nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vmrm := &VirtualMachineResourceManager{
		ctx:        ctx,
		virtClient: fakeVirtClient,
		namespace:  testNamespace,
		name:       testVMName,
		transitionalState: transitionalStateConfig{
			Strategy:            bmcv1.TransitionalStateStrategyServerWait,
			MaxWaitSeconds:      5,
			PollIntervalSeconds: 1,
		},
	}

	err := vmrm.PowerOn()
	require.NoError(t, err)
	require.GreaterOrEqual(t, startCalls.Load(), int32(2))
}

func TestServerWait_PowerOnConvergesWithoutRetry(t *testing.T) {
	vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
		Ready(false).
		Running(true).
		Build()
	fakeVirtClient := kubevirtfake.NewSimpleClientset(vm)
	injectSubresourceError(t, fakeVirtClient, "start", "VM is already running")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vmrm := &VirtualMachineResourceManager{
		ctx:        ctx,
		virtClient: fakeVirtClient,
		namespace:  testNamespace,
		name:       testVMName,
		transitionalState: transitionalStateConfig{
			Strategy:            bmcv1.TransitionalStateStrategyServerWait,
			MaxWaitSeconds:      5,
			PollIntervalSeconds: 1,
		},
	}

	start := time.Now()
	err := vmrm.PowerOn()
	require.NoError(t, err)
	require.Less(t, time.Since(start), 2*time.Second, "should return immediately when already converged")
}

func TestServerWait_PowerOnTimesOut(t *testing.T) {
	vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
		Ready(false).
		RunStrategy(kubevirtv1.RunStrategyRerunOnFailure).
		Build()
	vmi := builder.NewVirtualMachineInstanceBuilder(testNamespace, testVMName).
		Phase(kubevirtv1.Failed).
		Build()

	fakeVirtClient := kubevirtfake.NewSimpleClientset(vm)
	require.NoError(t, fakeVirtClient.Tracker().Add(vmi))
	injectSubresourceError(t, fakeVirtClient, "start", "RerunOnFailure does not support starting VM from failed state")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	vmrm := &VirtualMachineResourceManager{
		ctx:        ctx,
		virtClient: fakeVirtClient,
		namespace:  testNamespace,
		name:       testVMName,
		transitionalState: transitionalStateConfig{
			Strategy:            bmcv1.TransitionalStateStrategyServerWait,
			MaxWaitSeconds:      2,
			PollIntervalSeconds: 1,
		},
	}

	start := time.Now()
	err := vmrm.PowerOn()
	require.Error(t, err)
	require.Contains(t, err.Error(), "transitional state wait timed out")
	require.GreaterOrEqual(t, time.Since(start), 2*time.Second)
	var retryable *ErrRetryable
	require.False(t, errors.As(err, &retryable))
}

func TestLoadTransitionalStateConfig_Defaults(t *testing.T) {
	bmc := newTestBMC()
	bmcClient := newTestBMCClient(bmc)

	ctx := context.Background()
	vmrm := &VirtualMachineResourceManager{
		ctx:       ctx,
		bmcClient: bmcClient,
		namespace: testNamespace,
		bmcName:   testBMCName,
	}

	require.NoError(t, vmrm.loadTransitionalStateConfig())
	require.Equal(t, bmcv1.TransitionalStateStrategyRetrySignal, vmrm.transitionalState.Strategy)
	require.Equal(t, int32(60), vmrm.transitionalState.MaxWaitSeconds)
	require.Equal(t, int32(2), vmrm.transitionalState.PollIntervalSeconds)
}

func TestLoadTransitionalStateConfig_ServerWait(t *testing.T) {
	maxWait := int32(30)
	poll := int32(3)
	bmc := newTestBMC()
	bmc.Spec.TransitionalState = &bmcv1.TransitionalStateSpec{
		Strategy:            bmcv1.TransitionalStateStrategyServerWait,
		MaxWaitSeconds:      &maxWait,
		PollIntervalSeconds: &poll,
	}
	bmcClient := newTestBMCClient(bmc)

	ctx := context.Background()
	vmrm := &VirtualMachineResourceManager{
		ctx:       ctx,
		bmcClient: bmcClient,
		namespace: testNamespace,
		bmcName:   testBMCName,
	}

	require.NoError(t, vmrm.loadTransitionalStateConfig())
	require.Equal(t, bmcv1.TransitionalStateStrategyServerWait, vmrm.transitionalState.Strategy)
	require.Equal(t, int32(30), vmrm.transitionalState.MaxWaitSeconds)
	require.Equal(t, int32(3), vmrm.transitionalState.PollIntervalSeconds)
}

func TestIsPowerOnConverged(t *testing.T) {
	t.Run("ready always running", func(t *testing.T) {
		vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
			Ready(true).
			RunStrategy(kubevirtv1.RunStrategyAlways).
			Build()
		require.True(t, isPowerOnConverged(vm, nil))
	})

	t.Run("pending start request", func(t *testing.T) {
		vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
			Ready(false).
			RunStrategy(kubevirtv1.RunStrategyManual).
			WithStateChangeRequests(kubevirtv1.VirtualMachineStateChangeRequest{Action: kubevirtv1.StartRequest}).
			Build()
		require.True(t, isPowerOnConverged(vm, nil))
	})

	t.Run("transitional not ready halted", func(t *testing.T) {
		vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
			Ready(false).
			RunStrategy(kubevirtv1.RunStrategyHalted).
			Build()
		require.False(t, isPowerOnConverged(vm, nil))
	})
}

func TestIsPowerOffConverged(t *testing.T) {
	vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).Ready(false).Build()
	require.True(t, isPowerOffConverged(vm))

	vm = builder.NewVirtualMachineBuilder(testNamespace, testVMName).
		Ready(false).
		WithStateChangeRequests(kubevirtv1.VirtualMachineStateChangeRequest{Action: kubevirtv1.StartRequest}).
		Build()
	require.False(t, isPowerOffConverged(vm))
}

func TestIsRestartConverged(t *testing.T) {
	vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
		WithStateChangeRequests(
			kubevirtv1.VirtualMachineStateChangeRequest{Action: kubevirtv1.StopRequest},
			kubevirtv1.VirtualMachineStateChangeRequest{Action: kubevirtv1.StartRequest},
		).
		Build()
	require.True(t, isRestartConverged(vm))

	vm = builder.NewVirtualMachineBuilder(testNamespace, testVMName).
		WithStateChangeRequests(kubevirtv1.VirtualMachineStateChangeRequest{Action: kubevirtv1.StopRequest}).
		Build()
	require.False(t, isRestartConverged(vm))
}
