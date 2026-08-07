package resourcemanager

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	kubevirtv1 "kubevirt.io/api/core/v1"
	kubevirtfake "kubevirt.io/client-go/kubevirt/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

// terminatingVMI returns a Running VMI mid-graceful-shutdown: non-final but
// with DeletionTimestamp set, which must not read as "on".
func terminatingVMI() *kubevirtv1.VirtualMachineInstance {
	vmi := builder.NewVirtualMachineInstanceBuilder(testNamespace, testVMName).
		Phase(kubevirtv1.Running).
		Build()
	now := metav1.Now()
	vmi.DeletionTimestamp = &now
	return vmi
}

func TestIsPowerOnConverged_VMIDirection(t *testing.T) {
	runningVMI := builder.NewVirtualMachineInstanceBuilder(testNamespace, testVMName).
		Phase(kubevirtv1.Running).
		Build()

	t.Run("ready manual with healthy vmi", func(t *testing.T) {
		vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
			Ready(true).
			RunStrategy(kubevirtv1.RunStrategyManual).
			Build()
		require.True(t, isPowerOnConverged(vm, runningVMI))
	})

	t.Run("ready manual but vmi terminating", func(t *testing.T) {
		// The graceful-shutdown window: stop SCR already consumed, Ready is
		// still true, but the VMI is going down.
		vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
			Ready(true).
			RunStrategy(kubevirtv1.RunStrategyManual).
			Build()
		require.False(t, isPowerOnConverged(vm, terminatingVMI()))
	})

	t.Run("manual vmi starting", func(t *testing.T) {
		vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
			Ready(false).
			RunStrategy(kubevirtv1.RunStrategyManual).
			Build()
		vmi := builder.NewVirtualMachineInstanceBuilder(testNamespace, testVMName).
			Phase(kubevirtv1.Pending).
			Build()
		require.True(t, isPowerOnConverged(vm, vmi))
	})

	t.Run("manual vmi terminating", func(t *testing.T) {
		vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
			Ready(false).
			RunStrategy(kubevirtv1.RunStrategyManual).
			Build()
		require.False(t, isPowerOnConverged(vm, terminatingVMI()))
	})

	t.Run("legacy running converges via strategy clause", func(t *testing.T) {
		// spec.running=true maps to RunStrategyAlways; Stop flips it to
		// Halted before teardown, so a terminating VMI cannot race this
		// clause — it stays intentionally unguarded.
		vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
			Ready(true).
			Running(true).
			Build()
		require.True(t, isPowerOnConverged(vm, terminatingVMI()))
	})
}

func TestIsPowerCycleConverged(t *testing.T) {
	runningVMI := builder.NewVirtualMachineInstanceBuilder(testNamespace, testVMName).
		Phase(kubevirtv1.Running).
		Build()

	t.Run("restart queued", func(t *testing.T) {
		vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
			WithStateChangeRequests(
				kubevirtv1.VirtualMachineStateChangeRequest{Action: kubevirtv1.StopRequest},
				kubevirtv1.VirtualMachineStateChangeRequest{Action: kubevirtv1.StartRequest},
			).
			Build()
		require.True(t, isPowerCycleConverged(vm, nil))
	})

	t.Run("running with healthy vmi", func(t *testing.T) {
		vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
			Ready(true).
			RunStrategy(kubevirtv1.RunStrategyManual).
			Build()
		require.True(t, isPowerCycleConverged(vm, runningVMI))
	})

	t.Run("ready but vmi terminating", func(t *testing.T) {
		vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
			Ready(true).
			RunStrategy(kubevirtv1.RunStrategyManual).
			Build()
		require.False(t, isPowerCycleConverged(vm, terminatingVMI()))
	})

	t.Run("stop only in flight", func(t *testing.T) {
		vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
			Ready(true).
			RunStrategy(kubevirtv1.RunStrategyManual).
			WithStateChangeRequests(kubevirtv1.VirtualMachineStateChangeRequest{Action: kubevirtv1.StopRequest}).
			Build()
		require.False(t, isPowerCycleConverged(vm, terminatingVMI()))
	})
}

// TestServerWait_PowerOnWaitsForVMITeardown is the P1b regression test: a
// PowerOn landing in the graceful-shutdown window (Ready still true, VMI
// terminating) must wait for the teardown and retry Start, not report
// immediate success.
func TestServerWait_PowerOnWaitsForVMITeardown(t *testing.T) {
	vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
		Ready(true).
		RunStrategy(kubevirtv1.RunStrategyManual).
		Build()
	fakeVirtClient := kubevirtfake.NewSimpleClientset(vm)
	require.NoError(t, fakeVirtClient.Tracker().Add(terminatingVMI()))

	var startCalls atomic.Int32
	fakeVirtClient.PrependReactor("put", "virtualmachines", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "start" {
			return false, nil, nil
		}
		// Start is rejected while the old VMI exists, accepted once it is gone.
		if startCalls.Add(1) == 1 {
			return true, nil, errors.New("VM is already running")
		}
		return true, nil, nil
	})
	var vmiGets atomic.Int32
	fakeVirtClient.PrependReactor("get", "virtualmachineinstances", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if vmiGets.Add(1) > 2 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachineinstances"}, testVMName)
		}
		return false, nil, nil
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

	start := time.Now()
	err := vmrm.PowerOn()
	require.NoError(t, err)
	require.GreaterOrEqual(t, startCalls.Load(), int32(2), "Start must be retried after the teardown")
	require.GreaterOrEqual(t, time.Since(start), time.Second,
		"PowerOn must wait out the teardown instead of returning instant success")
}

// TestServerWait_PowerCycleCompletesViaStartAfterTeardown covers the P1a
// defect: a PowerCycle issued while a graceful shutdown is in flight (stop
// SCR pending) used to time out deterministically, because the wait only
// retried Restart — which can never succeed once the VMI is gone. The cycle
// must be completed via Start.
func TestServerWait_PowerCycleCompletesViaStartAfterTeardown(t *testing.T) {
	vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
		Ready(true).
		RunStrategy(kubevirtv1.RunStrategyManual).
		WithStateChangeRequests(kubevirtv1.VirtualMachineStateChangeRequest{Action: kubevirtv1.StopRequest}).
		Build()
	fakeVirtClient := kubevirtfake.NewSimpleClientset(vm)
	require.NoError(t, fakeVirtClient.Tracker().Add(terminatingVMI()))
	injectSubresourceError(t, fakeVirtClient, "restart", "stop/start already underway")

	var startCalls atomic.Int32
	fakeVirtClient.PrependReactor("put", "virtualmachines", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "start" {
			return false, nil, nil
		}
		startCalls.Add(1)
		return true, nil, nil
	})
	var vmiGets atomic.Int32
	fakeVirtClient.PrependReactor("get", "virtualmachineinstances", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if vmiGets.Add(1) > 2 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachineinstances"}, testVMName)
		}
		return false, nil, nil
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

	start := time.Now()
	err := vmrm.PowerCycle()
	require.NoError(t, err)
	require.Equal(t, int32(1), startCalls.Load(), "cycle must be completed by Start once the VMI is gone")
	require.GreaterOrEqual(t, time.Since(start), time.Second)
}

// TestServerWait_PowerCycleConvergesWhenInFlightRestartCompletes covers P2:
// while waiting, someone else's restart finishes (fresh VMI, SCRs consumed).
// The wait must observe the running VM and return — retrying Restart there
// would cycle the VM a second time.
func TestServerWait_PowerCycleConvergesWhenInFlightRestartCompletes(t *testing.T) {
	vm := builder.NewVirtualMachineBuilder(testNamespace, testVMName).
		Ready(true).
		RunStrategy(kubevirtv1.RunStrategyManual).
		Build()
	fakeVirtClient := kubevirtfake.NewSimpleClientset(vm)
	injectSubresourceError(t, fakeVirtClient, "restart", "stop/start already underway")

	freshVMI := builder.NewVirtualMachineInstanceBuilder(testNamespace, testVMName).
		Phase(kubevirtv1.Running).
		Build()
	var vmiGets atomic.Int32
	fakeVirtClient.PrependReactor("get", "virtualmachineinstances", func(action k8stesting.Action) (bool, runtime.Object, error) {
		// Teardown finishes mid-wait: a new VMI replaces the terminating one.
		if vmiGets.Add(1) >= 4 {
			return true, freshVMI, nil
		}
		return true, terminatingVMI(), nil
	})

	var startCalls atomic.Int32
	fakeVirtClient.PrependReactor("put", "virtualmachines", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "start" {
			startCalls.Add(1)
			return true, nil, nil
		}
		return false, nil, nil
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

	start := time.Now()
	err := vmrm.PowerCycle()
	require.NoError(t, err)
	require.Zero(t, startCalls.Load(), "no second power action once the in-flight restart completes")
	require.GreaterOrEqual(t, time.Since(start), time.Second,
		"must wait for the in-flight transition, not converge on the terminating VMI")
}

// TestHandleTransitionalState_ReloadsConfig covers P3: the strategy must be
// re-read from the CR on the transitional path, not only at Initialize time.
func TestHandleTransitionalState_ReloadsConfig(t *testing.T) {
	ctx := context.Background()
	bmc := newTestBMC() // no transitionalState spec → RetrySignal default
	bmcClient := newTestBMCClient(bmc)

	vmrm := &VirtualMachineResourceManager{
		ctx:       ctx,
		bmcClient: bmcClient,
		namespace: testNamespace,
		bmcName:   testBMCName,
		// Stale cache says ServerWait; the CR (RetrySignal) must win.
		transitionalState: transitionalStateConfig{
			Strategy:            bmcv1.TransitionalStateStrategyServerWait,
			MaxWaitSeconds:      5,
			PollIntervalSeconds: 1,
		},
	}

	opErr := errors.New("transitional")
	noop := func() error { return nil }
	converged := func() (bool, error) { return true, nil }

	err := vmrm.handleTransitionalState("test", opErr, noop, converged)
	var retryable *ErrRetryable
	require.True(t, errors.As(err, &retryable), "CR default RetrySignal must override the stale cache")

	// Flip the CR to ServerWait: the next call must wait, not signal.
	current := &bmcv1.VirtualMachineBMC{}
	require.NoError(t, bmcClient.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testBMCName}, current))
	maxWait, poll := int32(5), int32(1)
	current.Spec.TransitionalState = &bmcv1.TransitionalStateSpec{
		Strategy:            bmcv1.TransitionalStateStrategyServerWait,
		MaxWaitSeconds:      &maxWait,
		PollIntervalSeconds: &poll,
	}
	require.NoError(t, bmcClient.Update(ctx, current))

	require.NoError(t, vmrm.handleTransitionalState("test", opErr, noop, converged),
		"CR edit must take effect without re-Initialize")
}
