package virtualmachinebmc

import (
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	bmcv1 "kubevirt.io/kubevirtbmc/api/bmc/v1beta1"
)

// The agent takes the StorageClass from its --storage-class flag; the CR field
// only reaches the agent through the rendered Deployment args.
func TestCreateVirtBMCDeploymentStorageClassArg(t *testing.T) {
	r := &VirtualMachineBMCReconciler{}
	newBMC := func(sc *string) *bmcv1.VirtualMachineBMC {
		return &bmcv1.VirtualMachineBMC{
			ObjectMeta: metav1.ObjectMeta{Name: "test-bmc", Namespace: "default"},
			Spec: bmcv1.VirtualMachineBMCSpec{
				VirtualMachineRef: &corev1.LocalObjectReference{Name: "testvm"},
				StorageClassName:  sc,
			},
		}
	}

	args := r.createVirtBMCDeployment(newBMC(ptr.To("fast-sc")), "").Spec.Template.Spec.Containers[0].Args
	if idx := slices.Index(args, "--storage-class"); idx < 0 || args[idx+1] != "fast-sc" {
		t.Errorf("args %v should contain --storage-class fast-sc", args)
	}

	for _, sc := range []*string{nil, ptr.To("")} {
		args := r.createVirtBMCDeployment(newBMC(sc), "").Spec.Template.Spec.Containers[0].Args
		if slices.Contains(args, "--storage-class") {
			t.Errorf("args %v should omit --storage-class when unset or empty", args)
		}
	}
}

func TestCreateVirtBMCDeploymentUsesSingleWriterStrategy(t *testing.T) {
	r := &VirtualMachineBMCReconciler{}
	bmc := &bmcv1.VirtualMachineBMC{
		ObjectMeta: metav1.ObjectMeta{Name: "test-bmc", Namespace: "default"},
		Spec: bmcv1.VirtualMachineBMCSpec{
			VirtualMachineRef: &corev1.LocalObjectReference{Name: "testvm"},
		},
	}

	deployment := r.createVirtBMCDeployment(bmc, "")
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		t.Fatalf("virtbmc agent must have exactly one replica")
	}
	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("virtbmc agent must use Recreate strategy, got %q", deployment.Spec.Strategy.Type)
	}
}
