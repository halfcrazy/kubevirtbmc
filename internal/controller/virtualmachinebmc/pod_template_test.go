package virtualmachinebmc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bmcv1 "kubevirt.io/kubevirtbmc/api/bmc/v1beta1"
)

func TestStampPodTemplateHash(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      map[string]string{"app": "virtbmc"},
			Annotations: map[string]string{EnableIPMIAnnotation: "false"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: virtBMCContainerName, Image: "virtbmc:v1"}},
		},
	}

	require.NoError(t, stampPodTemplateHash(pod))
	firstHash := pod.Annotations[PodTemplateHashAnnotation]
	require.NotEmpty(t, firstHash)

	require.NoError(t, stampPodTemplateHash(pod))
	require.Equal(t, firstHash, pod.Annotations[PodTemplateHashAnnotation])

	pod.Spec.Containers[0].Image = "virtbmc:v2"
	require.NoError(t, stampPodTemplateHash(pod))
	require.NotEqual(t, firstHash, pod.Annotations[PodTemplateHashAnnotation])
}

func TestReconcilePodTemplateChangeDeletesStalePod(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	virtualMachineBMC := &bmcv1.VirtualMachineBMC{
		ObjectMeta: metav1.ObjectMeta{Name: "test-bmc", Namespace: "default"},
		Spec: bmcv1.VirtualMachineBMCSpec{
			VirtualMachineRef: &corev1.LocalObjectReference{Name: "test-vm"},
			AuthSecretRef:     &corev1.LocalObjectReference{Name: "test-secret"},
		},
	}

	reconciler := &VirtualMachineBMCReconciler{
		Scheme:         scheme,
		AgentImageName: "virtbmc",
		AgentImageTag:  "v1",
	}
	existingPod := reconciler.constructPodFromVirtualMachineBMC(virtualMachineBMC)
	require.NoError(t, stampPodTemplateHash(existingPod))

	reconciler.Client = fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existingPod).
		Build()
	reconciler.AgentImageTag = "v2"
	desiredPod := reconciler.constructPodFromVirtualMachineBMC(virtualMachineBMC)
	require.NoError(t, stampPodTemplateHash(desiredPod))

	requeue, err := reconciler.reconcilePodTemplateChange(ctx, virtualMachineBMC, desiredPod)
	require.NoError(t, err)
	require.True(t, requeue)

	err = reconciler.Get(ctx, types.NamespacedName{
		Name:      existingPod.Name,
		Namespace: existingPod.Namespace,
	}, &corev1.Pod{})
	require.True(t, apierrors.IsNotFound(err))
}
