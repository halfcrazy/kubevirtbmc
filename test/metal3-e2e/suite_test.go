package metal3e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	kubevirtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	bmcv1 "kubevirt.io/kubevirtbmc/api/bmc/v1beta1"
	"kubevirt.io/kubevirtbmc/test/util"
)

const (
	metal3ClusterName = "kvbmc-metal3-e2e"
	testTimeout       = 5 * time.Minute
	testInterval      = 2 * time.Second
	envTrueValue      = "true"
)

var (
	nadGVK = schema.GroupVersionKind{
		Group:   "k8s.cni.cncf.io",
		Version: "v1",
		Kind:    "NetworkAttachmentDefinition",
	}

	skipKubeVirtInstall    = os.Getenv("KUBEVIRT_INSTALL_SKIP") == "true"
	skipCertManagerInstall = os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true"

	repo = func() string {
		if r := os.Getenv("REPO"); r != "" {
			return r
		}
		return "kubevirtbmc"
	}()
	tag = func() string {
		if t := os.Getenv("TAG"); t != "" {
			return t
		}
		return "dev"
	}()
	controllerManagerImage = fmt.Sprintf("%s/virtbmc-controller:%s", repo, tag)
	agentImage             = fmt.Sprintf("%s/virtbmc:%s", repo, tag)

	config    *rest.Config
	k8sClient client.Client
)

func TestMetal3E2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Metal3/Ironic integration E2E Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	if os.Getenv("KIND_CLUSTER") == "" {
		Expect(os.Setenv("KIND_CLUSTER", metal3ClusterName)).To(Succeed())
	}

	By("loading controller/agent images into Kind")
	Expect(util.LoadImageToKindClusterWithName(controllerManagerImage, agentImage)).To(Succeed())

	By("creating clients")
	var err error
	config, err = clientConfig()
	Expect(err).NotTo(HaveOccurred())
	Expect(kubevirtv1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(bmcv1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(cdiv1.AddToScheme(scheme.Scheme)).To(Succeed())
	k8sClient, err = client.New(config, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	if !skipKubeVirtInstall {
		By("installing KubeVirt if needed")
		if !util.IsKubeVirtInstalled() {
			Expect(util.InstallKubeVirt()).To(Succeed())
		}
	}

	if !skipCertManagerInstall {
		By("installing cert-manager if needed")
		if !util.IsCertManagerCRDsInstalled() {
			Expect(util.InstallCertManager()).To(Succeed())
		}
	}

	// CDI + the DeclarativeHotplugVolumes gate back Ironic VirtualMedia → CDI;
	// same "cluster prerequisite" tier as KubeVirt/cert-manager above.
	ensureCDIForProvision()

	By("deploying kubevirtbmc controller")
	cmd := exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", controllerManagerImage))
	_, err = util.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	ctx := context.Background()
	util.WaitForControllerManagerReady(ctx, k8sClient, testTimeout, testInterval)
	util.WaitForWebhookServiceReady(ctx, k8sClient, testTimeout, testInterval)
	util.WaitForWebhookEndpointSlicesReady(ctx, k8sClient, testTimeout, testInterval)
	time.Sleep(util.WebhookRegistrationDelay)

	By("requiring Multus NetworkAttachmentDefinition from metal3-e2e-setup")
	Eventually(func() error {
		nad := &unstructured.Unstructured{}
		nad.SetGroupVersionKind(nadGVK)
		return k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "provisioning"}, nad)
	}, testTimeout, testInterval).Should(Succeed(),
		"NAD default/provisioning missing; run: make metal3-e2e-setup")

	// Same tier as the NAD above: Ironic httpd comes from metal3-e2e-setup.
	// Idempotent (cached on the httpd pod and in os.TempDir), so rerunning a
	// suite against a warm cluster costs nothing here.
	cachedImageURL, cachedImageSum = cacheUserImageOnIronicHTTPD()
})

var _ = AfterSuite(func() {
	if os.Getenv("KEEP_ENV") == envTrueValue {
		_, _ = fmt.Fprintf(GinkgoWriter, "KEEP_ENV=true, skipping teardown\n")
		return
	}
	By("undeploying kubevirtbmc controller")
	cmd := exec.Command("make", "undeploy")
	if _, err := util.Run(cmd); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: undeploy failed: %v\n", err)
	}
	// Keep cert-manager / KubeVirt / Metal3 across runs; use make metal3-e2e-teardown to wipe.
})

func clientConfig() (*rest.Config, error) {
	if kubeconfig := os.Getenv("KUBECONFIG"); kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
}
