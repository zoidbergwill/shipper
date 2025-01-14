package installation

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"

	shipper "github.com/bookingcom/shipper/pkg/apis/shipper/v1alpha1"
	shipperfake "github.com/bookingcom/shipper/pkg/client/clientset/versioned/fake"
	shipperinformers "github.com/bookingcom/shipper/pkg/client/informers/externalversions"
	"github.com/bookingcom/shipper/pkg/clusterclientstore"
	shippertesting "github.com/bookingcom/shipper/pkg/testing"
)

func loadService(variant string) *corev1.Service {
	service := &corev1.Service{}
	serviceYamlPath := filepath.Join("testdata", fmt.Sprintf("service-%s.yaml", variant))

	if serviceRaw, err := ioutil.ReadFile(serviceYamlPath); err != nil {
		panic(err)
	} else if _, _, err = scheme.Codecs.UniversalDeserializer().Decode(serviceRaw, nil, service); err != nil {
		panic(err)
	}

	return service
}

func loadDeployment(variant string) *appsv1.Deployment {
	deployment := &appsv1.Deployment{}
	deploymentYamlPath := filepath.Join("testdata", fmt.Sprintf("deployment-%s.yaml", variant))

	if deploymentRaw, err := ioutil.ReadFile(deploymentYamlPath); err != nil {
		panic(err)
	} else if _, _, err = scheme.Codecs.UniversalDeserializer().Decode(deploymentRaw, nil, deployment); err != nil {
		panic(err)
	}

	return deployment
}

// buildCluster returns a cluster.
func buildCluster(name string) *shipper.Cluster {
	return &shipper.Cluster{
		ObjectMeta: v1.ObjectMeta{
			Name: name,
		},
		Status: shipper.ClusterStatus{
			InService: true,
		},
	}
}

// populateFakeDiscovery adds the apiResourceList into the given fake discovery.
func populateFakeDiscovery(discovery discovery.DiscoveryInterface, apiResourceList []*v1.APIResourceList) {
	fakeDiscovery := discovery.(*fakediscovery.FakeDiscovery)
	fakeDiscovery.Resources = apiResourceList
}

type objectsPerClusterMap map[string][]runtime.Object

// initializeClients returns some objects that are used in several tests,
// basically to reduce boilerplate.
func initializeClients(apiResourceList []*v1.APIResourceList, shipperObjects []runtime.Object, kubeObjectsPerCluster objectsPerClusterMap) (
	map[string]*shippertesting.FakeCluster,
	*shipperfake.Clientset,
	DynamicClientBuilderFunc,
	shipperinformers.SharedInformerFactory,
) {
	clientsPerCluster := make(map[string]*shippertesting.FakeCluster)

	for clusterName, objs := range kubeObjectsPerCluster {
		fakeClient := kubefake.NewSimpleClientset(objs...)
		populateFakeDiscovery(fakeClient.Discovery(), apiResourceList)
		fakeDynamicClient := fakedynamic.NewSimpleDynamicClient(scheme.Scheme, objs...)
		clientsPerCluster[clusterName] = shippertesting.NewFakeCluster(fakeClient, fakeDynamicClient)
	}

	fakeDynamicClientBuilder := func(kind *schema.GroupVersionKind, restConfig *rest.Config, cluster *shipper.Cluster) dynamic.Interface {
		if fdc, ok := clientsPerCluster[cluster.Name]; ok {
			return fdc.DynamicClient
		}
		panic(fmt.Sprintf(`couldn't find client for %q`, cluster.Name))
	}

	shipperclientset := shipperfake.NewSimpleClientset(shipperObjects...)
	shipperInformerFactory := shipperinformers.NewSharedInformerFactory(shipperclientset, time.Second*0)

	return clientsPerCluster, shipperclientset, fakeDynamicClientBuilder, shipperInformerFactory
}

// newController returns an installation.Controller after it has started and
// waited for informer caches sync and there is something on the controller's
// workqueue.
func newController(
	shipperclientset *shipperfake.Clientset,
	shipperInformerFactory shipperinformers.SharedInformerFactory,
	fakeClientProvider clusterclientstore.Interface,
	fakeDynamicClientBuilder DynamicClientBuilderFunc,
	fakeRecorder record.EventRecorder,
) *Controller {
	c := NewController(
		shipperclientset,
		shipperInformerFactory,
		fakeClientProvider,
		fakeDynamicClientBuilder,
		localFetchChart,
		fakeRecorder,
	)

	stopCh := make(chan struct{})
	defer close(stopCh)

	shipperInformerFactory.Start(stopCh)
	shipperInformerFactory.WaitForCacheSync(stopCh)

	wait.PollUntil(
		10*time.Millisecond,
		func() (bool, error) { return c.workqueue.Len() >= 1, nil },
		stopCh,
	)

	return c
}

func newInstaller(it *shipper.InstallationTarget) *Installer {
	return NewInstaller(localFetchChart, it)
}

func buildInstallationTarget(namespace, appName string, clusters []string, chart *shipper.Chart) *shipper.InstallationTarget {
	return &shipper.InstallationTarget{
		ObjectMeta: v1.ObjectMeta{
			Name:      appName,
			Namespace: namespace,
			Labels: map[string]string{
				shipper.AppLabel:            appName,
				shipper.HelmWorkaroundLabel: shipper.True,
			},
		},
		Spec: shipper.InstallationTargetSpec{
			Clusters:    clusters,
			Chart:       chart,
			Values:      nil,
			CanOverride: true,
		},
	}
}

func buildChart(appName, version, repoUrl string) shipper.Chart {
	return shipper.Chart{
		Name:    appName,
		Version: version,
		RepoURL: repoUrl,
	}
}
