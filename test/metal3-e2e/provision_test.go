package metal3e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ironicGVK = schema.GroupVersionKind{
		Group:   "ironic.metal3.io",
		Version: "v1alpha1",
		Kind:    "Ironic",
	}
	bmhGVK = schema.GroupVersionKind{
		Group:   "metal3.io",
		Version: "v1alpha1",
		Kind:    "BareMetalHost",
	}
)

type provisionCase struct {
	target     provisionTarget
	enableIPMI bool
	bootMode   string
	bmcAddress func(bmcIP string) string
}

// redfishCase builds a BMH that talks Redfish. scheme is the Metal3 address
// prefix and selects Ironic's boot interface, e.g.:
//   - "redfish-virtualmedia" → InsertMedia ISO
//   - "redfish"              → BootSourceOverrideTarget=Pxe (ipxe)
//   - "redfish-uefihttp"     → UEFI HTTP Boot (future)
func redfishCase(prefix, mac, bootMode, scheme string) provisionCase {
	return provisionCase{
		target:     newProvisionTarget(prefix, mac),
		enableIPMI: false,
		bootMode:   bootMode,
		bmcAddress: func(ip string) string {
			return fmt.Sprintf("%s+http://%s/redfish/v1/Systems/1", scheme, ip)
		},
	}
}

func ipmiProvisionCase(prefix, mac, bootMode string) provisionCase {
	return provisionCase{
		target:     newProvisionTarget(prefix, mac),
		enableIPMI: true,
		bootMode:   bootMode,
		bmcAddress: func(ip string) string {
			return fmt.Sprintf("ipmi://%s:623", ip)
		},
	}
}

var _ = Describe("Metal3 stack (IrSO + BMO)", func() {
	It("has Ironic Ready and BareMetalHost CRD installed", func() {
		ctx := context.Background()

		By("waiting for Ironic CR Ready")
		Eventually(func(g Gomega) {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(ironicGVK)
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{
				Namespace: "baremetal-operator-system",
				Name:      "ironic",
			}, obj)).To(Succeed())
			conds, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			ready := false
			for _, c := range conds {
				m, ok := c.(map[string]interface{})
				if !ok {
					continue
				}
				if m["type"] == "Ready" && m["status"] == "True" {
					ready = true
				}
			}
			g.Expect(ready).To(BeTrue())
		}, 10*time.Minute, testInterval).Should(Succeed())

		By("checking BareMetalHost CRD exists")
		cmd := exec.Command("kubectl", "get", "crd", "baremetalhosts.metal3.io", "-o", "name")
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(out))
		Expect(string(out)).To(ContainSubstring("baremetalhosts.metal3.io"))

		By("checking BMO deployment Available")
		Eventually(func() string {
			cmd := exec.Command("kubectl", "-n", "baremetal-operator-system",
				"get", "deploy", "baremetal-operator-controller-manager",
				"-o", "jsonpath={.status.conditions[?(@.type=='Available')].status}")
			out, err := cmd.CombinedOutput()
			if err != nil {
				return ""
			}
			return string(out)
		}, testTimeout, testInterval).Should(Equal("True"))
	})
})

// Environment prep (CDI, user image on Ironic httpd) lives in BeforeSuite
// (suite_test.go); these are set there so every spec reports pure case time.
var (
	cachedImageURL string
	cachedImageSum string
)

var _ = Describe("Metal3 BareMetalHost provision", Ordered, Label("metal3", "slow"), func() {
	ctx := context.Background()

	// protocol × bootMode. One Fedora Generic qcow2 covers both bootModes;
	// VM firmware is matched to BMH bootMode (see createStoppedVMWithBMC).
	// Cases run one-at-a-time and clean up after each so disk/RAM stay fit for
	// GitHub-hosted runners (~14Gi free disk, ~7Gi RAM).
	DescribeTable("provisions and verifies guest agent",
		func(c provisionCase) {
			DeferCleanup(func() {
				if os.Getenv("KEEP_ENV") == envTrueValue {
					_, _ = fmt.Fprintf(GinkgoWriter, "KEEP_ENV=true, skipping cleanup of %s\n", c.target.Name)
					return
				}
				c.target.cleanup(ctx)
			})
			bmcIP := c.target.createStoppedVMWithBMC(ctx, c.enableIPMI, c.bootMode)
			c.target.createBMH(ctx, c.bmcAddress(bmcIP), cachedImageURL, cachedImageSum, c.bootMode)
			waitBMHProvisioningState(ctx, c.target.BMHName, "provisioned")
			waitGuestAgentConnected(ctx, c.target.VMName)
		},
		Entry("redfish-virtualmedia / legacy", redfishCase("node-rfv-leg", "02:00:00:00:00:21", bootModeLegacy, "redfish-virtualmedia")),
		Entry("redfish-virtualmedia / UEFI", redfishCase("node-rfv-uefi", "02:00:00:00:00:23", bootModeUEFI, "redfish-virtualmedia")),
		Entry("redfish+PXE / legacy", redfishCase("node-rfp-leg", "02:00:00:00:00:25", bootModeLegacy, "redfish")),
		Entry("redfish+PXE / UEFI", redfishCase("node-rfp-uefi", "02:00:00:00:00:26", bootModeUEFI, "redfish")),
		Entry("ipmi+PXE / legacy", ipmiProvisionCase("node-ipmi-leg", "02:00:00:00:00:22", bootModeLegacy)),
		Entry("ipmi+PXE / UEFI", ipmiProvisionCase("node-ipmi-uefi", "02:00:00:00:00:24", bootModeUEFI)),
	)
})
