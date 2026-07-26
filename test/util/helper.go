package util

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	certManagerURLFmt      = "https://github.com/jetstack/cert-manager/releases/download/%s/cert-manager.yaml"
	kubeVirtStableVersion  = "https://storage.googleapis.com/kubevirt-prow/release/kubevirt/kubevirt/stable.txt"
	kubeVirtOperatorURLFmt = "https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-operator.yaml"
	kubeVirtCRURLFmt       = "https://github.com/kubevirt/kubevirt/releases/download/%s/kubevirt-cr.yaml"

	// multusDaemonSetURL bundles the NAD CRD and auto-generates
	// 00-multus.conf delegating to the primary CNI (kindnet).
	multusDaemonSetURL = "https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/v4.2.2/deployments/multus-daemonset-thick.yml"
	// Kind node images may not ship the bridge binary the test NAD needs.
	cniPluginsVersion = "v1.6.2"
	cniPluginsURLFmt  = "https://github.com/containernetworking/plugins/releases/download/%[1]s/cni-plugins-linux-%[2]s-%[1]s.tgz"
)

var (
	certManagerVersion = os.Getenv("CERT_MANAGER_VERSION")
)

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

// IsMultusInstalled checks whether the Multus daemonset is present.
func IsMultusInstalled() bool {
	cmd := exec.Command("kubectl", "get", "daemonset", "kube-multus-ds",
		"-n", "kube-system", "--no-headers", "-o", "name", "--ignore-not-found")
	output, err := Run(cmd)
	if err != nil {
		return false
	}
	return strings.TrimSpace(output) == "daemonset.apps/kube-multus-ds"
}

// InstallMultus installs Multus (thick plugin) and the bridge CNI binary on
// every Kind node.
func InstallMultus() error {
	if err := ensureBridgeCNIPlugin(); err != nil {
		return err
	}

	cmd := exec.Command("kubectl", "apply", "-f", multusDaemonSetURL)
	if out, err := Run(cmd); err != nil {
		return fmt.Errorf("apply Multus thick daemonset: %w\noutput: %s", err, out)
	}

	cmd = exec.Command("kubectl", "rollout", "status", "daemonset/kube-multus-ds",
		"-n", "kube-system", "--timeout=180s")
	if out, err := Run(cmd); err != nil {
		return fmt.Errorf("wait for Multus daemonset rollout: %w\noutput: %s", err, out)
	}
	return nil
}

// ensureBridgeCNIPlugin copies the bridge CNI binary into /opt/cni/bin on
// Kind nodes that lack it. The plugin creates the host bridge itself.
func ensureBridgeCNIPlugin() error {
	cmd := exec.Command(resolveKindBinary(), "get", "nodes", "--name", kindClusterName())
	out, err := Run(cmd)
	if err != nil {
		return fmt.Errorf("kind get nodes: %w\noutput: %s", err, out)
	}

	bridgeByArch := map[string]string{}
	for _, node := range getNonEmptyLines(out) {
		if _, err := Run(exec.Command("docker", "exec", node, "test", "-x", "/opt/cni/bin/bridge")); err == nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "bridge CNI plugin already present on %s\n", node)
			continue
		}

		archOut, err := Run(exec.Command("docker", "exec", node, "uname", "-m"))
		if err != nil {
			return fmt.Errorf("get architecture of node %s: %w", node, err)
		}
		arch := strings.TrimSpace(archOut)
		switch arch {
		case "x86_64":
			arch = "amd64"
		case "aarch64", "arm64":
			arch = "arm64"
		default:
			return fmt.Errorf("unsupported node architecture %q on %s", arch, node)
		}

		bridge, ok := bridgeByArch[arch]
		if !ok {
			tmp, err := os.MkdirTemp("", "cni-plugins")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmp) //nolint:errcheck

			tgz := filepath.Join(tmp, "cni-plugins.tgz")
			url := fmt.Sprintf(cniPluginsURLFmt, cniPluginsVersion, arch)
			if out, err := Run(exec.Command("curl", "-fsSL", "-o", tgz, url)); err != nil {
				return fmt.Errorf("download CNI plugins from %s: %w\noutput: %s", url, err, out)
			}
			if out, err := Run(exec.Command("tar", "-xzf", tgz, "-C", tmp, "./bridge")); err != nil {
				return fmt.Errorf("extract bridge plugin: %w\noutput: %s", err, out)
			}
			bridge = filepath.Join(tmp, "bridge")
			bridgeByArch[arch] = bridge
		}

		if out, err := Run(exec.Command("docker", "cp", bridge, node+":/opt/cni/bin/bridge")); err != nil {
			return fmt.Errorf("copy bridge plugin to node %s: %w\noutput: %s", node, err, out)
		}
		if out, err := Run(exec.Command("docker", "exec", node, "chmod", "+x", "/opt/cni/bin/bridge")); err != nil {
			return fmt.Errorf("chmod bridge plugin on node %s: %w\noutput: %s", node, err, out)
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "installed bridge CNI plugin on %s\n", node)
	}
	return nil
}

// CreateTestNetworkAttachmentDefinition creates a bridge NAD for NetworkRef
// tests. Empty ipam means L2-only: attach the interface, no IP allocation.
func CreateTestNetworkAttachmentDefinition(namespace, name string) error {
	nadYAML := fmt.Sprintf(`apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: %s
  namespace: %s
spec:
  config: '{"cniVersion":"0.3.1","name":"%[1]s","type":"bridge","bridge":"br-test","ipam":{}}'
`, name, namespace)

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(nadYAML)
	dir, _ := getProjectDir()
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("create NetworkAttachmentDefinition: %w\noutput: %s", err, string(out))
	}
	_, _ = fmt.Fprintf(GinkgoWriter, "Created NetworkAttachmentDefinition: %s\n", string(out))
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
	cmd := exec.Command("curl", "-sL", kubeVirtStableVersion)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("get KubeVirt version: %w", err)
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return fmt.Errorf("empty KubeVirt version from %s", kubeVirtStableVersion)
	}

	operatorURL := fmt.Sprintf(kubeVirtOperatorURLFmt, version)
	cmd = exec.Command("kubectl", "apply", "-f", operatorURL)
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
	for _, suffix := range []string{"/test/virtbmc-controller", "/test/virtbmc-agent"} {
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
