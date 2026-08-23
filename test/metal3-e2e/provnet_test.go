package metal3e2e

import (
	"context"
	"fmt"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bmcv1 "kubevirt.io/kubevirtbmc/api/bmc/v1beta1"
)

const (
	provMAC     = "02:00:00:00:00:11"
	provVMName  = "metal3-provnet-vm"
	provBMCName = "metal3-provnet-bmc"
	provSecret  = "metal3-provnet-secret"
	provNS      = "default"
)

var _ = Describe("provisioning network (br-prov + Multus)", Ordered, func() {
	ctx := context.Background()

	cleanupProvnet := func() {
		_ = k8sClient.Delete(ctx, &bmcv1.VirtualMachineBMC{
			ObjectMeta: metav1.ObjectMeta{Name: provBMCName, Namespace: provNS},
		})
		_ = k8sClient.Delete(ctx, &kubevirtv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{Name: provVMName, Namespace: provNS},
		})
		_ = k8sClient.Delete(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: provSecret, Namespace: provNS},
		})
		Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: provNS, Name: provVMName}, &kubevirtv1.VirtualMachine{})
			return apierrors.IsNotFound(err)
		}, testTimeout, testInterval).Should(BeTrue())
		Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: provNS, Name: provSecret}, &corev1.Secret{})
			return apierrors.IsNotFound(err)
		}, testTimeout, testInterval).Should(BeTrue())
	}

	// KEEP_ENV leaves resources for debugging; always wipe before create so re-runs don't hit AlreadyExists.
	BeforeAll(cleanupProvnet)

	AfterAll(func() {
		if os.Getenv("KEEP_ENV") == envTrueValue {
			_, _ = fmt.Fprintf(GinkgoWriter, "KEEP_ENV=true, skipping provnet cleanup\n")
			return
		}
		cleanupProvnet()
	})

	It("boots a VM on Multus/br-prov and exposes BMC ClusterIP", func() {
		By("creating BMC credentials")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: provSecret, Namespace: provNS},
			Type:       corev1.SecretTypeOpaque,
			StringData: map[string]string{"username": "admin", "password": "password"},
		})).To(Succeed())

		By("creating VM with Multus provisioning NIC (RunStrategy Always)")
		Expect(k8sClient.Create(ctx, minimalProvVM(provVMName, provNS, provMAC))).To(Succeed())

		By("creating VirtualMachineBMC with IPMI enabled")
		enabled := true
		Expect(k8sClient.Create(ctx, &bmcv1.VirtualMachineBMC{
			ObjectMeta: metav1.ObjectMeta{Name: provBMCName, Namespace: provNS},
			Spec: bmcv1.VirtualMachineBMCSpec{
				VirtualMachineRef: &corev1.LocalObjectReference{Name: provVMName},
				AuthSecretRef:     &corev1.LocalObjectReference{Name: provSecret},
				IPMI:              &bmcv1.IPMISpec{Enabled: &enabled},
			},
		})).To(Succeed())

		By("waiting for VirtualMachineBMC Ready + ClusterIP")
		Eventually(func(g Gomega) {
			var bmc bmcv1.VirtualMachineBMC
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: provNS, Name: provBMCName}, &bmc)).To(Succeed())
			g.Expect(bmc.Status.ClusterIP).NotTo(BeEmpty())
			ready := false
			for _, c := range bmc.Status.Conditions {
				if c.Type == bmcv1.ConditionReady && c.Status == metav1.ConditionTrue {
					ready = true
				}
			}
			g.Expect(ready).To(BeTrue())
		}, testTimeout, testInterval).Should(Succeed())

		By("waiting for VMI Running with provisioning MAC")
		Eventually(func(g Gomega) {
			var vmi kubevirtv1.VirtualMachineInstance
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: provNS, Name: provVMName}, &vmi)).To(Succeed())
			g.Expect(vmi.Status.Phase).To(Equal(kubevirtv1.Running))
			found := false
			for _, iface := range vmi.Status.Interfaces {
				if iface.MAC == provMAC || iface.Name == "provisioning" {
					found = true
					if iface.MAC != "" {
						g.Expect(iface.MAC).To(Equal(provMAC))
					}
				}
			}
			g.Expect(found).To(BeTrue(), "expected provisioning interface on VMI status")
		}, testTimeout, testInterval).Should(Succeed())

		By("waiting for virtbmc agent pod Ready")
		Eventually(func(g Gomega) {
			var deploy appsv1.Deployment
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{
				Namespace: provNS,
				Name:      provVMName + "-virtbmc",
			}, &deploy)).To(Succeed())
			g.Expect(deploy.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, testTimeout, testInterval).Should(Succeed())
	})
})

func minimalProvVM(name, namespace, mac string) *kubevirtv1.VirtualMachine {
	secureBoot := false
	runStrategy := kubevirtv1.RunStrategyAlways
	boot1, boot2 := uint(1), uint(2)
	return &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app.kubernetes.io/part-of": "kubevirtbmc-metal3-e2e"},
		},
		Spec: kubevirtv1.VirtualMachineSpec{
			RunStrategy: &runStrategy,
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app.kubernetes.io/part-of": "kubevirtbmc-metal3-e2e"},
				},
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						Resources: kubevirtv1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
						Firmware: &kubevirtv1.Firmware{
							Bootloader: &kubevirtv1.Bootloader{
								EFI: &kubevirtv1.EFI{SecureBoot: &secureBoot},
							},
						},
						Devices: kubevirtv1.Devices{
							Disks: []kubevirtv1.Disk{{
								Name: "rootdisk",
								DiskDevice: kubevirtv1.DiskDevice{
									Disk: &kubevirtv1.DiskTarget{Bus: kubevirtv1.DiskBusVirtio},
								},
								BootOrder: &boot1,
							}},
							Interfaces: []kubevirtv1.Interface{
								{
									Name: "default",
									InterfaceBindingMethod: kubevirtv1.InterfaceBindingMethod{
										Masquerade: &kubevirtv1.InterfaceMasquerade{},
									},
								},
								{
									Name:       "provisioning",
									MacAddress: mac,
									InterfaceBindingMethod: kubevirtv1.InterfaceBindingMethod{
										Bridge: &kubevirtv1.InterfaceBridge{},
									},
									BootOrder: &boot2,
								},
							},
						},
					},
					Networks: []kubevirtv1.Network{
						{Name: "default", NetworkSource: kubevirtv1.NetworkSource{Pod: &kubevirtv1.PodNetwork{}}},
						{Name: "provisioning", NetworkSource: kubevirtv1.NetworkSource{
							Multus: &kubevirtv1.MultusNetwork{NetworkName: "default/provisioning"},
						}},
					},
					Volumes: []kubevirtv1.Volume{{
						Name: "rootdisk",
						VolumeSource: kubevirtv1.VolumeSource{
							ContainerDisk: &kubevirtv1.ContainerDiskSource{
								Image: "quay.io/containerdisks/fedora:latest",
							},
						},
					}},
				},
			},
		},
	}
}
