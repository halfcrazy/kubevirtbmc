package metal3e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubevirtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bmcv1 "kubevirt.io/kubevirtbmc/api/bmc/v1beta1"
	"kubevirt.io/kubevirtbmc/test/util"
)

const (
	provisionNS = "default"

	// Fedora Cloud "Generic" is a hybrid BIOS/UEFI image; one qcow2 covers both
	// BMH bootModes. The bootMode↔firmware coupling is IPA media (legacy ISO is
	// BIOS-only; UEFI needs OVMF), not this user image.
	defaultUserImageURL = "https://download.fedoraproject.org/pub/archive/fedora/linux/releases/41/Cloud/x86_64/images/Fedora-Cloud-Base-Generic-41-1.4.x86_64.qcow2"
	defaultUserImageSum = "6205ae0c524b4d1816dbd3573ce29b5c44ed26c9fbc874fbe48c41c89dd0bac2"

	cachedImageName = "metal3-user.qcow2"
	provTimeout     = 60 * time.Minute

	bootModeLegacy = "legacy"
	bootModeUEFI   = "UEFI"
)

type provisionTarget struct {
	Name       string
	MAC        string
	SecretName string
	BMCName    string
	VMName     string
	RootDVName string
	BMHName    string
	Username   string
	Password   string
}

func newProvisionTarget(prefix, mac string) provisionTarget {
	// Root DV must not share the VM name: virtbmc InsertMedia creates the ISO
	// DataVolume as <vmName>, so a blank root named <vmName> would collide.
	return provisionTarget{
		Name:       prefix,
		MAC:        mac,
		SecretName: prefix + "-bmc-secret",
		BMCName:    prefix + "-bmc",
		VMName:     prefix,
		RootDVName: prefix + "-root",
		BMHName:    prefix + "-bmh",
		Username:   "admin",
		Password:   "password",
	}
}

func ensureCDIForProvision() {
	By("ensuring CDI + DeclarativeHotplugVolumes (Ironic VirtualMedia → CDI)")
	if !util.IsCDIInstalled() {
		By("installing CDI")
		Expect(util.InstallCDI()).To(Succeed())
	} else {
		By("CDI already installed")
	}
	By("patching Kind local-path StorageProfile with ReadWriteOnce (InsertMedia DV has no accessModes)")
	Expect(util.EnsureDefaultStorageProfileAccessMode()).To(Succeed())
	By("ensuring DeclarativeHotplugVolumes feature gate")
	Expect(util.EnsureDeclarativeHotplugVolumes()).To(Succeed())
}

func ironicHTTPDPod() string {
	out, err := exec.Command("kubectl", "-n", "baremetal-operator-system", "get", "pod",
		"-l", "ironic.metal3.io/app=ironic-service",
		"-o", "jsonpath={.items[0].metadata.name}").CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), string(out))
	pod := strings.TrimSpace(string(out))
	Expect(pod).NotTo(BeEmpty())
	return pod
}

func cacheUserImageOnIronicHTTPD() (imageURL, checksum string) {
	srcURL := os.Getenv("METAL3_USER_IMAGE_URL")
	checksum = os.Getenv("METAL3_USER_IMAGE_CHECKSUM")
	if srcURL == "" {
		srcURL = defaultUserImageURL
	}
	if checksum == "" {
		checksum = defaultUserImageSum
	}

	By("caching user qcow2 onto Ironic httpd (IPA on br-prov can reach BR_IP:6180)")
	pod := ironicHTTPDPod()
	check := exec.Command("kubectl", "-n", "baremetal-operator-system", "exec", pod, "-c", "httpd", "--",
		"test", "-s", "/shared/html/images/"+cachedImageName)
	if check.Run() != nil {
		tmp := filepath.Join(os.TempDir(), cachedImageName)
		if st, err := os.Stat(tmp); err != nil || st.Size() == 0 {
			By("downloading user image to " + tmp)
			cmd := exec.Command("curl", "-sfSL", "--retry", "3", "-o", tmp, srcURL)
			cmd.Stdout = GinkgoWriter
			cmd.Stderr = GinkgoWriter
			Expect(cmd.Run()).To(Succeed(), "download user image from %s", srcURL)
		} else {
			By(fmt.Sprintf("reusing local cached image %s (%d bytes)", tmp, st.Size()))
		}
		By("kubectl cp image into ironic httpd pod")
		Expect(exec.Command("kubectl", "-n", "baremetal-operator-system", "cp",
			tmp, pod+":/shared/html/images/"+cachedImageName, "-c", "httpd").Run()).
			To(Succeed(), "kubectl cp image into ironic httpd")
	} else {
		By("user image already present on ironic httpd")
	}

	brIP := os.Getenv("BR_IP")
	if brIP == "" {
		brIP = "172.22.0.1"
	}
	return fmt.Sprintf("http://%s:6180/images/%s", brIP, cachedImageName), checksum
}

func (t provisionTarget) cleanup(ctx context.Context) {
	// Delete BMH first and wait while VirtualMachineBMC is still up. Otherwise
	// Ironic's Redfish tear-down hits a dead ClusterIP, the node sticks in
	// error, and the next BMH with the same name reuses it and hangs in preparing.
	bmh := &unstructured.Unstructured{}
	bmh.SetGroupVersionKind(bmhGVK)
	bmh.SetName(t.BMHName)
	bmh.SetNamespace(provisionNS)
	_ = k8sClient.Delete(ctx, bmh)
	Eventually(func() bool {
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: provisionNS, Name: t.BMHName}, bmh)
		return apierrors.IsNotFound(err)
	}, testTimeout, testInterval).Should(BeTrue(), "BareMetalHost %s should be gone before deleting BMC", t.BMHName)

	_ = k8sClient.Delete(ctx, &bmcv1.VirtualMachineBMC{ObjectMeta: metav1.ObjectMeta{Name: t.BMCName, Namespace: provisionNS}})
	_ = k8sClient.Delete(ctx, &kubevirtv1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: t.VMName, Namespace: provisionNS}})
	_ = k8sClient.Delete(ctx, &cdiv1.DataVolume{ObjectMeta: metav1.ObjectMeta{Name: t.RootDVName, Namespace: provisionNS}})
	// InsertMedia names the ISO DataVolume after the VM; drop leftovers from failed runs.
	_ = k8sClient.Delete(ctx, &cdiv1.DataVolume{ObjectMeta: metav1.ObjectMeta{Name: t.VMName, Namespace: provisionNS}})
	_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: t.SecretName, Namespace: provisionNS}})
	Eventually(func() bool {
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: provisionNS, Name: t.VMName}, &kubevirtv1.VirtualMachine{})
		return apierrors.IsNotFound(err)
	}, testTimeout, testInterval).Should(BeTrue())
	for _, dvName := range []string{t.RootDVName, t.VMName} {
		name := dvName
		Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: provisionNS, Name: name}, &cdiv1.DataVolume{})
			return apierrors.IsNotFound(err)
		}, testTimeout, testInterval).Should(BeTrue(), "DataVolume %s should be gone", name)
	}
}

func (t provisionTarget) createStoppedVMWithBMC(ctx context.Context, enableIPMI bool, bootMode string) string {
	By(fmt.Sprintf("creating blank-disk VM (bootMode=%s) + VirtualMachineBMC for %s", bootMode, t.Name))
	t.cleanup(ctx)

	Expect(k8sClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: t.SecretName, Namespace: provisionNS},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"username": t.Username, "password": t.Password},
	})).To(Succeed())

	Expect(k8sClient.Create(ctx, &cdiv1.DataVolume{
		ObjectMeta: metav1.ObjectMeta{Name: t.RootDVName, Namespace: provisionNS},
		Spec: cdiv1.DataVolumeSpec{
			PVC: &corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					// Fedora Cloud Generic virtual size is 5Gi; keep a small margin.
					//$ qemu-img info Fedora-Cloud-Base-Generic-41-1.4.x86_64.qcow2
					//image: Fedora-Cloud-Base-Generic-41-1.4.x86_64.qcow2
					//file format: qcow2
					//virtual size: 5 GiB (5368709120 bytes)
					//disk size: 469 MiB
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("6Gi")},
				},
			},
			Source: &cdiv1.DataVolumeSource{Blank: &cdiv1.DataVolumeBlankImage{}},
		},
	})).To(Succeed())

	runStrategy := kubevirtv1.RunStrategyManual
	boot1, boot2 := uint(1), uint(2)
	// Match VM firmware to BMH bootMode: Ironic legacy virtualmedia ISO is
	// BIOS-only (OVMF → "DVD Not Found"); UEFI needs EFI with SecureBoot off.
	var firmware *kubevirtv1.Firmware
	if bootMode == bootModeUEFI {
		secureBoot := false
		firmware = &kubevirtv1.Firmware{
			Bootloader: &kubevirtv1.Bootloader{
				EFI: &kubevirtv1.EFI{SecureBoot: &secureBoot},
			},
		}
	}
	Expect(k8sClient.Create(ctx, &kubevirtv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      t.VMName,
			Namespace: provisionNS,
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
						CPU: &kubevirtv1.CPU{Cores: 2},
						Resources: kubevirtv1.ResourceRequirements{
							// IPA ramdisk needs headroom; 2Gi runs out of tmpfs
							// ("No space left on device" writing /etc/udev/hwdb.bin).
							Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
						},
						Firmware: firmware,
						Devices: kubevirtv1.Devices{
							Disks: []kubevirtv1.Disk{
								{
									Name: "rootdisk",
									DiskDevice: kubevirtv1.DiskDevice{
										Disk: &kubevirtv1.DiskTarget{Bus: kubevirtv1.DiskBusVirtio},
									},
									BootOrder: &boot1,
								},
								{
									Name: "cdrom",
									DiskDevice: kubevirtv1.DiskDevice{
										CDRom: &kubevirtv1.CDRomTarget{Bus: kubevirtv1.DiskBusSATA},
									},
								},
							},
							Interfaces: []kubevirtv1.Interface{
								{
									Name: "default",
									InterfaceBindingMethod: kubevirtv1.InterfaceBindingMethod{
										Masquerade: &kubevirtv1.InterfaceMasquerade{},
									},
								},
								{
									Name:       "provisioning",
									MacAddress: t.MAC,
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
					Volumes: []kubevirtv1.Volume{
						{Name: "rootdisk", VolumeSource: kubevirtv1.VolumeSource{
							DataVolume: &kubevirtv1.DataVolumeSource{Name: t.RootDVName},
						}},
						// No emptyDisk for cdrom: virtbmc InsertMedia appends a volume named
						// after the CDROM disk; a placeholder would duplicate the name and be
						// rejected by KubeVirt's virtualmachine-validator webhook.
					},
				},
			},
		},
	})).To(Succeed())

	bmc := &bmcv1.VirtualMachineBMC{
		ObjectMeta: metav1.ObjectMeta{Name: t.BMCName, Namespace: provisionNS},
		Spec: bmcv1.VirtualMachineBMCSpec{
			VirtualMachineRef: &corev1.LocalObjectReference{Name: t.VMName},
			AuthSecretRef:     &corev1.LocalObjectReference{Name: t.SecretName},
		},
	}
	if enableIPMI {
		enabled := true
		bmc.Spec.IPMI = &bmcv1.IPMISpec{Enabled: &enabled}
	}
	Expect(k8sClient.Create(ctx, bmc)).To(Succeed())

	var clusterIP string
	Eventually(func(g Gomega) {
		var cur bmcv1.VirtualMachineBMC
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: provisionNS, Name: t.BMCName}, &cur)).To(Succeed())
		g.Expect(cur.Status.ClusterIP).NotTo(BeEmpty())
		clusterIP = cur.Status.ClusterIP
	}, testTimeout, testInterval).Should(Succeed())
	return clusterIP
}

func (t provisionTarget) createBMH(ctx context.Context, bmcAddress, imageURL, checksum, bootMode string) {
	By(fmt.Sprintf("creating BareMetalHost %s (%s, bootMode=%s)", t.BMHName, bmcAddress, bootMode))
	bmh := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "metal3.io/v1alpha1",
		"kind":       "BareMetalHost",
		"metadata": map[string]interface{}{
			"name":      t.BMHName,
			"namespace": provisionNS,
			"annotations": map[string]interface{}{
				"inspect.metal3.io": "disabled",
			},
			"labels": map[string]interface{}{
				"kubevirtbmc.io/metal3-e2e": t.Name,
			},
		},
		"spec": map[string]interface{}{
			"online":                true,
			"bootMode":              bootMode,
			"bootMACAddress":        t.MAC,
			"automatedCleaningMode": "disabled",
			"bmc": map[string]interface{}{
				"address":                        bmcAddress,
				"credentialsName":                t.SecretName,
				"disableCertificateVerification": true,
			},
			"rootDeviceHints": map[string]interface{}{
				"deviceName": "/dev/vda",
			},
			"image": map[string]interface{}{
				"url":          imageURL,
				"checksum":     checksum,
				"checksumType": "sha256",
				"format":       "qcow2",
			},
		},
	}}
	bmh.SetGroupVersionKind(schema.GroupVersionKind{Group: "metal3.io", Version: "v1alpha1", Kind: "BareMetalHost"})
	Expect(k8sClient.Create(ctx, bmh)).To(Succeed())
}

func waitBMHProvisioningState(ctx context.Context, name, want string) {
	By("waiting for BareMetalHost " + name + " provisioning.state=" + want)
	Eventually(func(g Gomega) {
		bmh := &unstructured.Unstructured{}
		bmh.SetGroupVersionKind(bmhGVK)
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: provisionNS, Name: name}, bmh)).To(Succeed())
		state, _, err := unstructured.NestedString(bmh.Object, "status", "provisioning", "state")
		g.Expect(err).NotTo(HaveOccurred())
		if state != want {
			errType, _, _ := unstructured.NestedString(bmh.Object, "status", "errorType")
			errMsg, _, _ := unstructured.NestedString(bmh.Object, "status", "errorMessage")
			g.Expect(state).To(Equal(want), "errorType=%s errorMessage=%s", errType, errMsg)
		}
	}, provTimeout, 15*time.Second).Should(Succeed())
}

func waitGuestAgentConnected(ctx context.Context, vmName string) {
	By("waiting for QEMU guest agent on " + vmName)
	Eventually(func(g Gomega) {
		var vmi kubevirtv1.VirtualMachineInstance
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: provisionNS, Name: vmName}, &vmi)).To(Succeed())
		g.Expect(vmi.Status.Phase).To(Equal(kubevirtv1.Running))
		connected := false
		for _, c := range vmi.Status.Conditions {
			if c.Type == kubevirtv1.VirtualMachineInstanceAgentConnected && c.Status == corev1.ConditionTrue {
				connected = true
			}
		}
		g.Expect(connected).To(BeTrue())
	}, provTimeout, 15*time.Second).Should(Succeed())
}
