package virtbmc

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	cdiclient "kubevirt.io/client-go/containerizeddataimporter"
	"kubevirt.io/client-go/kubecli"
	kvclient "kubevirt.io/client-go/kubevirt"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bmcv1 "kubevirt.io/kubevirtbmc/api/bmc/v1beta1"
)

func restConfig(options Options) *rest.Config {
	config, err := clientcmd.BuildConfigFromFlags("", options.KubeconfigPath)
	if err != nil {
		// Fallback to in-cluster config
		config, err = rest.InClusterConfig()
		if err != nil {
			panic(err)
		}
	}
	return config
}

func NewVirtClient(options Options) kvclient.Interface {
	// KubeVirt client
	virtClient, err := kvclient.NewForConfig(restConfig(options))
	if err != nil {
		panic(err)
	}

	return virtClient
}

// NewKubecliClient returns the kubecli client, which (unlike the typed
// kubevirt client) implements the VMI subresource streams — SerialConsole,
// VNC — used by the console backend.
func NewKubecliClient(options Options) kubecli.KubevirtClient {
	c, err := kubecli.GetKubevirtClientFromRESTConfig(restConfig(options))
	if err != nil {
		panic(err)
	}
	return c
}

func NewCdiClient(options Options) *cdiclient.Clientset {
	// Build config
	config, err := clientcmd.BuildConfigFromFlags("", options.KubeconfigPath)
	if err != nil {
		// Fallback to in-cluster config
		config, err = rest.InClusterConfig()
		if err != nil {
			panic(err)
		}
	}

	// CDI client
	cdiClient, err := cdiclient.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	return cdiClient
}

// NewBMCClient returns a typed (controller-runtime) client for the
// VirtualMachineBMC CR and core Pods
func NewBMCClient(options Options) client.Client {
	config, err := clientcmd.BuildConfigFromFlags("", options.KubeconfigPath)
	if err != nil {
		config, err = rest.InClusterConfig()
		if err != nil {
			panic(err)
		}
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := bmcv1.AddToScheme(scheme); err != nil {
		panic(err)
	}

	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		panic(err)
	}

	return c
}
