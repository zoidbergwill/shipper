package testing

import (
	"fmt"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/bookingcom/shipper/pkg/clusterclientstore"
	shippererrors "github.com/bookingcom/shipper/pkg/errors"
)

// FakeClusterClientStore is a fake implementation of a ClusterClientStore,
// allowing you to provide your own clientsets.
type FakeClusterClientStore struct {
	clusters map[string]*FakeCluster

	subscriptionCallbacks []clusterclientstore.SubscriptionRegisterFunc
	eventHandlerCallbacks []clusterclientstore.EventHandlerRegisterFunc
}

func NewFakeClusterClientStore(clusters map[string]*FakeCluster) *FakeClusterClientStore {
	return &FakeClusterClientStore{clusters: clusters}
}

func (s *FakeClusterClientStore) AddSubscriptionCallback(c clusterclientstore.SubscriptionRegisterFunc) {
	s.subscriptionCallbacks = append(s.subscriptionCallbacks, c)
}

func (s *FakeClusterClientStore) AddEventHandlerCallback(c clusterclientstore.EventHandlerRegisterFunc) {
	s.eventHandlerCallbacks = append(s.eventHandlerCallbacks, c)
}

func (s *FakeClusterClientStore) Run(stopCh <-chan struct{}) {
	for name, cluster := range s.clusters {
		informerFactory := cluster.InformerFactory

		for _, subscriptionCallback := range s.subscriptionCallbacks {
			subscriptionCallback(informerFactory)
		}

		for _, eventHandlerCallback := range s.eventHandlerCallbacks {
			eventHandlerCallback(informerFactory, name)
		}

		informerFactory.Start(stopCh)
		informerFactory.WaitForCacheSync(stopCh)
	}
}

func (s *FakeClusterClientStore) GetClient(clusterName string, ua string) (kubernetes.Interface, error) {
	cluster, ok := s.clusters[clusterName]
	if !ok {
		return nil, fmt.Errorf("no client for cluster %q", clusterName)
	}

	return cluster.Client, nil
}

func (s *FakeClusterClientStore) GetConfig(clusterName string) (*rest.Config, error) {
	return &rest.Config{}, nil
}

func (s *FakeClusterClientStore) GetInformerFactory(clusterName string) (informers.SharedInformerFactory, error) {
	return s.clusters[clusterName].InformerFactory, nil
}

func NewFailingFakeClusterClientStore() clusterclientstore.Interface {
	return &FailingFakeClusterClientStore{}
}

// FailingFakeClusterClientStore is an implementation of
// clusterclientstore.Interface that always fails when trying to get clients
// for any cluster.
type FailingFakeClusterClientStore struct {
	FakeClusterClientStore
}

func (s *FailingFakeClusterClientStore) GetClient(clusterName string, ua string) (kubernetes.Interface, error) {
	return nil, shippererrors.NewClusterNotReadyError(clusterName)
}
