package util

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	certManagerURLFmt      = "https://github.com/jetstack/cert-manager/releases/download/%s/cert-manager.yaml"
	defaultKubeVirtVersion = "v1.8.4"
	defaultCDIVersion      = "v1.65.0"
	kubeVirtOperatorURLFmt = "https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-operator.yaml"
	kubeVirtCRURLFmt       = "https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-cr.yaml"
)

var (
	certManagerVersion = os.Getenv("CERT_MANAGER_VERSION")
)

// kubeVirtInstallVersion prefers KUBEVIRT_VERSION from the Makefile / env.
func kubeVirtInstallVersion() string {
	if v := os.Getenv("KUBEVIRT_VERSION"); v != "" {
		return v
	}
	return defaultKubeVirtVersion
}

// cdiInstallVersion prefers CDI_VERSION from the Makefile / env.
func cdiInstallVersion() string {
	if v := os.Getenv("CDI_VERSION"); v != "" {
		return v
	}
	return defaultCDIVersion
}

func warnError(err error) {
	_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: %v\n", err)
}

func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := getProjectDir()
	cmd.Dir = dir

	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "Running command in %s: %s\n", cmd.Dir, command)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

func isKVMAvailable() bool {
	_, err := os.Stat("/dev/kvm")
	return err == nil
}

func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds", "--no-headers", "-o", "custom-columns=NAME:.metadata.name")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the cert-manager CRDs are present
	crdList := getNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if line == crd {
				return true
			}
		}
	}

	return false
}

// Virtual media mount requires CDI for DataVolumes.
func IsCDIInstalled() bool {
	cmd := exec.Command("kubectl", "get", "crd", "datavolumes.cdi.kubevirt.io", "--no-headers", "-o", "name")
	output, err := Run(cmd)
	if err != nil {
		return false
	}
	return strings.TrimSpace(output) == "customresourcedefinition.apiextensions.k8s.io/datavolumes.cdi.kubevirt.io"
}

// DeclarativeHotplugVolumes feature gate is enabled. Virtual media mount requires this gate.
func HasDeclarativeHotplugVolumesEnabled() bool {
	cmd := exec.Command("kubectl", "get", "kubevirt", "kubevirt", "-n", "kubevirt",
		"-o", "jsonpath={.spec.configuration.developerConfiguration.featureGates}", "--ignore-not-found")
	output, err := Run(cmd)
	if err != nil || output == "" {
		return false
	}
	return strings.Contains(output, "DeclarativeHotplugVolumes")
}

func VirtualMediaPrerequisitesMet() bool {
	return IsCDIInstalled() && HasDeclarativeHotplugVolumesEnabled()
}

func InstallCDI() error {
	version := cdiInstallVersion()
	if !strings.HasPrefix(version, "v") || strings.Contains(version, "<") {
		return fmt.Errorf("invalid CDI version %q", version)
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "Installing CDI %s\n", version)

	operatorURL := fmt.Sprintf("https://github.com/kubevirt/containerized-data-importer/releases/download/%s/cdi-operator.yaml", version)
	if _, err := Run(exec.Command("kubectl", "apply", "-f", operatorURL)); err != nil {
		return fmt.Errorf("apply CDI operator: %w", err)
	}
	crURL := fmt.Sprintf("https://github.com/kubevirt/containerized-data-importer/releases/download/%s/cdi-cr.yaml", version)
	if _, err := Run(exec.Command("kubectl", "apply", "-f", crURL)); err != nil {
		return fmt.Errorf("apply CDI CR: %w", err)
	}

	Eventually(func() (string, error) {
		cmd := exec.Command("kubectl", "get", "cdi", "cdi", "-n", "cdi",
			"-o", "jsonpath={.status.phase}", "--ignore-not-found")
		out, err := Run(cmd)
		return strings.TrimSpace(out), err
	}, "5m", "5s").Should(Equal("Deployed"), "CDI should reach Deployed phase")
	return nil
}

// EnsureDefaultStorageProfileAccessMode patches the default StorageProfile so CDI
// DataVolumes that only set storage.size (e.g. virtbmc InsertMedia) can bind on
// Kind local-path, whose provisioner is unrecognized and leaves claimPropertySets empty.
func EnsureDefaultStorageProfileAccessMode() error {
	patch := `{"spec":{"claimPropertySets":[{"accessModes":["ReadWriteOnce"],"volumeMode":"Filesystem"}]}}`
	Eventually(func() error {
		_, err := Run(exec.Command("kubectl", "get", "storageprofile", "standard"))
		return err
	}, "2m", "2s").Should(Succeed(), "StorageProfile standard should exist after CDI is Deployed")

	out, err := Run(exec.Command("kubectl", "patch", "storageprofile", "standard",
		"--type=merge", "-p", patch))
	if err != nil {
		return fmt.Errorf("patch StorageProfile standard: %w (%s)", err, out)
	}
	return nil
}

func EnsureDeclarativeHotplugVolumes() error {
	if HasDeclarativeHotplugVolumesEnabled() {
		return nil
	}
	cmd := exec.Command("kubectl", "patch", "kubevirt", "kubevirt", "-n", "kubevirt", "--type=json", "-p",
		`[{"op":"add","path":"/spec/configuration/developerConfiguration/featureGates/-","value":"DeclarativeHotplugVolumes"}]`)
	if _, err := Run(cmd); err != nil {
		// featureGates may be missing; merge-patch a full developerConfiguration
		cmd = exec.Command("kubectl", "patch", "kubevirt", "kubevirt", "-n", "kubevirt", "--type=merge", "-p",
			`{"spec":{"configuration":{"developerConfiguration":{"featureGates":["DeclarativeHotplugVolumes"]}}}}`)
		if _, err2 := Run(cmd); err2 != nil {
			return fmt.Errorf("enable DeclarativeHotplugVolumes: %w (merge fallback: %v)", err, err2)
		}
	}
	Eventually(HasDeclarativeHotplugVolumesEnabled, "2m", "5s").Should(BeTrue())
	// Patching the KubeVirt CR restarts virt-api; creating VMs before it is back
	// hits virtualmachines-mutator with connection refused.
	Eventually(func() error {
		_, err := Run(exec.Command("kubectl", "-n", "kubevirt", "rollout", "status", "deploy/virt-api", "--timeout=60s"))
		return err
	}, "5m", "5s").Should(Succeed(), "virt-api should be Available after feature-gate patch")
	Eventually(IsKubeVirtInstalled, "3m", "5s").Should(BeTrue(), "KubeVirt should return to Deployed")
	return nil
}

func IsKubeVirtInstalled() bool {
	cmd := exec.Command("kubectl", "get", "kubevirt", "kubevirt", "-n", "kubevirt",
		"-o", "jsonpath={.status.phase}", "--ignore-not-found")
	output, err := Run(cmd)
	if err != nil || output == "" {
		return false
	}
	return strings.TrimSpace(output) == "Deployed"
}

func InstallKubeVirt() error {
	version := kubeVirtInstallVersion()
	_, _ = fmt.Fprintf(GinkgoWriter, "Installing KubeVirt %s\n", version)
	operatorURL := fmt.Sprintf(kubeVirtOperatorURLFmt, version)
	cmd := exec.Command("kubectl", "apply", "-f", operatorURL)
	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("apply KubeVirt operator: %w", err)
	}

	crURL := fmt.Sprintf(kubeVirtCRURLFmt, version)
	cmd = exec.Command("kubectl", "apply", "-f", crURL)
	if _, err := Run(cmd); err != nil {
		return fmt.Errorf("apply KubeVirt CR: %w", err)
	}

	// Enable emulation only when KVM is not available.
	if !isKVMAvailable() {
		fmt.Println("KVM not available, enabling KubeVirt useEmulation")
		cmd = exec.Command("kubectl", "patch", "kubevirt", "kubevirt", "-n", "kubevirt", "--type=merge",
			"-p", `{"spec":{"configuration":{"developerConfiguration":{"useEmulation":true}}}}`)
		if _, err := Run(cmd); err != nil {
			return fmt.Errorf("patch KubeVirt CR for useEmulation: %w", err)
		}
	} else {
		fmt.Println("KVM is available, skipping useEmulation")
	}

	// RebootPolicy gate: guest-OS-initiated reboots recreate the VMI when
	// the VM uses rebootPolicy=Terminate (needed for oneshot consumption e2e).
	ensureRebootPolicyFeatureGate()

	Eventually(func() (string, error) {
		cmd := exec.Command("kubectl", "get", "kubevirt", "kubevirt", "-n", "kubevirt", "-o", "jsonpath={.status.phase}")
		out, err := Run(cmd)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(out), nil
	}, "5m", "5s").Should(Equal("Deployed"), "KubeVirt should reach Deployed phase")

	return nil
}

// ensureRebootPolicyFeatureGate adds the RebootPolicy feature gate to the
// KubeVirt CR if not already present.
func ensureRebootPolicyFeatureGate() {
	cmd := exec.Command("kubectl", "get", "kubevirt", "kubevirt", "-n", "kubevirt",
		"-o", "jsonpath={.spec.configuration.developerConfiguration.featureGates}", "--ignore-not-found")
	out, err := Run(cmd)
	if err != nil {
		fmt.Printf("Failed to get KubeVirt feature gates: %v\n", err)
		return
	}

	var gates []string
	raw := strings.TrimSpace(out)
	if raw != "" && raw != "[]" {
		if err := json.Unmarshal([]byte(raw), &gates); err != nil {
			fmt.Printf("Failed to parse KubeVirt feature gates: %v\n", err)
			return
		}
	}

	if slices.Contains(gates, "RebootPolicy") {
		return
	}

	gates = append(gates, "RebootPolicy")
	gatesJSON, _ := json.Marshal(gates)
	patch := fmt.Sprintf(`{"spec":{"configuration":{"developerConfiguration":{"featureGates":%s}}}}`, string(gatesJSON))
	cmd = exec.Command("kubectl", "patch", "kubevirt", "kubevirt", "-n", "kubevirt", "--type=merge", "-p", patch)
	if _, err := Run(cmd); err != nil {
		fmt.Printf("Failed to enable RebootPolicy feature gate: %v\n", err)
	}
}

func InstallCertManager() error {
	url := fmt.Sprintf(certManagerURLFmt, certManagerVersion)
	cmd := exec.Command("kubectl", "apply", "-f", url)
	if _, err := Run(cmd); err != nil {
		return err
	}

	cmd = exec.Command("kubectl", "wait", "deployment.apps/cert-manager-webhook",
		"--for", "condition=Available",
		"--namespace", "cert-manager",
		"--timeout", "5m",
	)
	_, err := Run(cmd)
	return err
}

func UninstallCertManager() {
	url := fmt.Sprintf(certManagerURLFmt, certManagerVersion)
	cmd := exec.Command("kubectl", "delete", "-f", url)
	if _, err := Run(cmd); err != nil {
		warnError(err)
	}
}

func getProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, err
	}
	for _, suffix := range []string{"/test/virtbmc-controller", "/test/virtbmc-agent", "/test/metal3-e2e"} {
		if idx := strings.Index(wd, suffix); idx != -1 {
			return wd[:idx], nil
		}
	}
	return wd, nil
}

func getNonEmptyLines(output string) []string {
	var res []string
	elements := strings.Split(output, "\n")
	for _, element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}
