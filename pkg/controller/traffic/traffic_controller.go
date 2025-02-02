package traffic

import (
	"fmt"
	"reflect"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	shipper "github.com/bookingcom/shipper/pkg/apis/shipper/v1alpha1"
	shipperclient "github.com/bookingcom/shipper/pkg/client/clientset/versioned"
	informers "github.com/bookingcom/shipper/pkg/client/informers/externalversions"
	listers "github.com/bookingcom/shipper/pkg/client/listers/shipper/v1alpha1"
	"github.com/bookingcom/shipper/pkg/clusterclientstore"
	shippercontroller "github.com/bookingcom/shipper/pkg/controller"
	shippererrors "github.com/bookingcom/shipper/pkg/errors"
	diffutil "github.com/bookingcom/shipper/pkg/util/diff"
	"github.com/bookingcom/shipper/pkg/util/filters"
	trafficutil "github.com/bookingcom/shipper/pkg/util/traffic"
	shipperworkqueue "github.com/bookingcom/shipper/pkg/workqueue"
)

const (
	AgentName = "traffic-controller"
)

const (
	ServerError    = "ServerError"
	MissingService = "MissingService"
	InternalError  = "InternalError"
	UnknownError   = "UnknownError"
	PodsNotReady   = "PodsNotReady"
)

// Controller is the controller implementation for TrafficTarget resources.
type Controller struct {
	shipperclientset     shipperclient.Interface
	clusterClientStore   clusterclientstore.Interface
	trafficTargetsLister listers.TrafficTargetLister
	trafficTargetsSynced cache.InformerSynced
	workqueue            workqueue.RateLimitingInterface
	recorder             record.EventRecorder
}

// NewController returns a new TrafficTarget controller.
func NewController(
	shipperclientset shipperclient.Interface,
	shipperInformerFactory informers.SharedInformerFactory,
	store clusterclientstore.Interface,
	recorder record.EventRecorder,
) *Controller {

	// Obtain references to shared index informers for the TrafficTarget type.
	trafficTargetInformer := shipperInformerFactory.Shipper().V1alpha1().TrafficTargets()

	controller := &Controller{
		shipperclientset:   shipperclientset,
		clusterClientStore: store,

		trafficTargetsLister: trafficTargetInformer.Lister(),
		trafficTargetsSynced: trafficTargetInformer.Informer().HasSynced,
		workqueue:            workqueue.NewNamedRateLimitingQueue(shipperworkqueue.NewDefaultControllerRateLimiter(), "traffic_controller_traffictargets"),
		recorder:             recorder,
	}

	klog.Info("Setting up event handlers")
	// Set up an event handler for when TrafficTarget resources change.
	trafficTargetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueAllTrafficTargets,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueAllTrafficTargets(new)
		},
		// The sync handler needs to cope with the case where the object was deleted.
		DeleteFunc: controller.enqueueAllTrafficTargets,
	})

	store.AddSubscriptionCallback(controller.subscribeToAppClusterEvents)
	store.AddEventHandlerCallback(controller.registerAppClusterEventHandlers)

	return controller
}

func (c *Controller) registerAppClusterEventHandlers(informerFactory kubeinformers.SharedInformerFactory, clusterName string) {
	handler := cache.FilteringResourceEventHandler{
		FilterFunc: filters.BelongsToApp,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueueTrafficTargetsFromPod,
			DeleteFunc: c.enqueueTrafficTargetsFromPod,
			UpdateFunc: func(oldObj, newObj interface{}) {
				c.enqueueTrafficTargetsFromPod(newObj)
			},
		},
	}
	informerFactory.Core().V1().Pods().Informer().AddEventHandler(handler)
}

func (c *Controller) subscribeToAppClusterEvents(informerFactory kubeinformers.SharedInformerFactory) {
	informerFactory.Core().V1().Pods().Informer()
	informerFactory.Core().V1().Services().Informer()
}

// Run will set up the event handlers for types we are interested in, as well as
// syncing informer caches and starting workers. It will block until stopCh is
// closed, at which point it will shutdown the workqueue and wait for workers to
// finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	klog.V(2).Info("Starting Traffic controller")
	defer klog.V(2).Info("Shutting down Traffic controller")

	if ok := cache.WaitForCacheSync(stopCh, c.trafficTargetsSynced); !ok {
		runtime.HandleError(fmt.Errorf("failed to wait for caches to sync"))
		return
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	klog.V(4).Info("Started Traffic controller")

	<-stopCh
}

func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}

	defer c.workqueue.Done(obj)

	var (
		key string
		ok  bool
	)

	if key, ok = obj.(string); !ok {
		c.workqueue.Forget(obj)
		runtime.HandleError(fmt.Errorf("invalid object key (will retry: false): %#v", obj))
		return true
	}

	shouldRetry := false
	err := c.syncHandler(key)

	if err != nil {
		shouldRetry = shippererrors.ShouldRetry(err)
		runtime.HandleError(fmt.Errorf("error syncing TrafficTarget %q (will retry: %t): %s", key, shouldRetry, err.Error()))
	}

	if shouldRetry {
		c.workqueue.AddRateLimited(key)

		return true
	}

	c.workqueue.Forget(obj)
	klog.V(4).Infof("Successfully synced TrafficTarget %q", key)

	return true
}

func (c *Controller) syncHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return shippererrors.NewUnrecoverableError(err)
	}

	initialTT, err := c.trafficTargetsLister.TrafficTargets(namespace).Get(name)
	if err != nil {
		if kerrors.IsNotFound(err) {
			klog.V(3).Infof("TrafficTarget %q has been deleted", key)
			return nil
		}

		return shippererrors.NewKubeclientGetError(namespace, name, err).
			WithShipperKind("TrafficTarget")
	}

	tt, err := c.processTrafficTarget(initialTT.DeepCopy())

	if !reflect.DeepEqual(initialTT, tt) {
		if _, err := c.shipperclientset.ShipperV1alpha1().TrafficTargets(namespace).Update(tt); err != nil {
			return shippererrors.NewKubeclientUpdateError(tt, err).
				WithShipperKind("TrafficTarget")
		}
	}

	return err
}

func (c *Controller) processTrafficTarget(tt *shipper.TrafficTarget) (*shipper.TrafficTarget, error) {
	// TODO(jgreff): top-level errors on a TrafficTarget are not reported
	// to the user. We should introduce a condition to expose them.

	appName, ok := tt.Labels[shipper.AppLabel]
	if !ok {
		return tt, shippererrors.NewMissingShipperLabelError(tt, shipper.AppLabel)
	}

	namespace := tt.Namespace
	appSelector := labels.Set{shipper.AppLabel: appName}.AsSelector()
	list, err := c.trafficTargetsLister.TrafficTargets(namespace).List(appSelector)
	if err != nil {
		return tt, shippererrors.NewKubeclientListError(
			shipper.SchemeGroupVersion.WithKind("TrafficTarget"),
			namespace, appSelector, err)
	}

	shifter, err := newPodLabelShifter(appName, namespace, list)
	if err != nil {
		return tt, err
	}

	clusterErrors := shippererrors.NewMultiError()
	newClusterStatuses := make([]*shipper.ClusterTrafficStatus, 0, len(tt.Spec.Clusters))

	// This algorithm assumes cluster names are unique
	curClusterStatuses := make(map[string]*shipper.ClusterTrafficStatus)
	for _, clusterStatus := range tt.Status.Clusters {
		curClusterStatuses[clusterStatus.Name] = clusterStatus
	}

	for _, clusterSpec := range tt.Spec.Clusters {
		clusterStatus, ok := curClusterStatuses[clusterSpec.Name]
		if !ok {
			clusterStatus = &shipper.ClusterTrafficStatus{
				Name: clusterSpec.Name,
			}
			curClusterStatuses[clusterSpec.Name] = clusterStatus
		}

		err := c.processTrafficTargetOnCluster(tt, &clusterSpec, clusterStatus, shifter)
		if err != nil {
			clusterErrors.Append(err)
		}

		newClusterStatuses = append(newClusterStatuses, clusterStatus)
	}

	sort.Sort(byClusterName(newClusterStatuses))

	tt.Status.Clusters = newClusterStatuses

	return tt, clusterErrors.Flatten()
}

func (c *Controller) processTrafficTargetOnCluster(
	tt *shipper.TrafficTarget,
	spec *shipper.ClusterTrafficTarget,
	status *shipper.ClusterTrafficStatus,
	shifter *podLabelShifter,
) error {
	diff := diffutil.NewMultiDiff()
	defer func() {
		c.reportTrafficConditionChange(tt, diff)
	}()

	syncingReleaseName, ok := tt.Labels[shipper.ReleaseLabel]
	if !ok {
		err := shippererrors.NewMissingShipperLabelError(tt, shipper.ReleaseLabel)
		cond := trafficutil.NewClusterTrafficCondition(
			shipper.ClusterConditionTypeOperational,
			corev1.ConditionFalse,
			ServerError,
			err.Error(),
		)
		diff.Append(trafficutil.SetClusterTrafficCondition(status, *cond))
		status.Status = err.Error()

		return err
	}

	clientset, err := c.clusterClientStore.GetClient(spec.Name, AgentName)
	if err != nil {
		cond := trafficutil.NewClusterTrafficCondition(
			shipper.ClusterConditionTypeOperational,
			corev1.ConditionFalse,
			ServerError,
			err.Error(),
		)
		diff.Append(trafficutil.SetClusterTrafficCondition(status, *cond))
		status.Status = err.Error()

		return err
	}

	informerFactory, err := c.clusterClientStore.GetInformerFactory(spec.Name)
	if err != nil {
		cond := trafficutil.NewClusterTrafficCondition(
			shipper.ClusterConditionTypeOperational,
			corev1.ConditionFalse,
			ServerError,
			err.Error(),
		)
		diff.Append(trafficutil.SetClusterTrafficCondition(status, *cond))
		status.Status = err.Error()

		return err
	}

	cond := trafficutil.NewClusterTrafficCondition(
		shipper.ClusterConditionTypeOperational,
		corev1.ConditionTrue,
		"",
		"",
	)
	diff.Append(trafficutil.SetClusterTrafficCondition(status, *cond))

	achievedReleaseWeight, err := shifter.SyncCluster(spec.Name, syncingReleaseName, clientset, informerFactory)
	if err != nil {
		var reason string

		switch err.(type) {
		case shippererrors.UnexpectedObjectCountFromSelectorError:
			reason = MissingService
		case shippererrors.KubeclientError:
			reason = ServerError
		case shippererrors.TargetClusterMathError:
			reason = InternalError
		default:
			reason = UnknownError
		}

		cond := trafficutil.NewClusterTrafficCondition(
			shipper.ClusterConditionTypeReady,
			corev1.ConditionFalse,
			reason,
			err.Error(),
		)
		diff.Append(trafficutil.SetClusterTrafficCondition(status, *cond))

		return err
	}

	status.AchievedTraffic = achievedReleaseWeight
	cond = trafficutil.NewClusterTrafficCondition(
		shipper.ClusterConditionTypeReady,
		corev1.ConditionTrue,
		"",
		"",
	)
	diff.Append(trafficutil.SetClusterTrafficCondition(status, *cond))

	status.Status = "Synced"

	return nil
}

// enqueueTrafficTarget takes a TrafficTarget resource and converts it into a
// namespace/name string which is then put onto the work queue. This method
// should *not* be passed resources of any type other than TrafficTarget.
func (c *Controller) enqueueTrafficTarget(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	c.workqueue.Add(key)
}

func (c *Controller) enqueueAllTrafficTargets(obj interface{}) {
	trafficTarget, ok := obj.(*shipper.TrafficTarget)
	if !ok {
		runtime.HandleError(fmt.Errorf("not a shipper.TrafficTarget: %#v", obj))
		return
	}

	namespace := trafficTarget.Namespace

	appName, ok := trafficTarget.Labels[shipper.AppLabel]
	if !ok {
		runtime.HandleError(fmt.Errorf("TrafficTarget %s/%s is missing app label",
			namespace, trafficTarget.Name))
		return
	}

	selector := labels.Set{shipper.AppLabel: appName}.AsSelector()
	trafficTargets, err := c.trafficTargetsLister.TrafficTargets(namespace).List(selector)
	if err != nil {
		runtime.HandleError(fmt.Errorf(
			"cannot list traffic targets for app '%s/%s': %s",
			namespace, appName, err))
	}

	for _, tt := range trafficTargets {
		c.enqueueTrafficTarget(tt)
	}
}

func (c *Controller) enqueueTrafficTargetsFromPod(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		runtime.HandleError(fmt.Errorf("not a corev1.Pod: %#v", obj))
		return
	}

	appName, ok := pod.GetLabels()[shipper.AppLabel]
	if !ok {
		runtime.HandleError(fmt.Errorf(
			"object %q does not have label %s. FilterFunc not working?",
			shippercontroller.MetaKey(pod), shipper.AppLabel))
		return
	}

	namespace := pod.Namespace
	selector := labels.Set{shipper.AppLabel: appName}.AsSelector()
	trafficTargets, err := c.trafficTargetsLister.TrafficTargets(namespace).List(selector)
	if err != nil {
		err = shippererrors.NewKubeclientListError(
			shipper.SchemeGroupVersion.WithKind("TrafficTarget"),
			namespace, selector, err)
		runtime.HandleError(fmt.Errorf(
			"cannot list traffic targets for app '%s/%s': %s",
			namespace, appName, err))
	}

	for _, tt := range trafficTargets {
		c.enqueueTrafficTarget(tt)
	}
}

func (c *Controller) reportTrafficConditionChange(tt *shipper.TrafficTarget, diff diffutil.Diff) {
	if !diff.IsEmpty() {
		c.recorder.Event(tt, corev1.EventTypeNormal, "TrafficTargetConditionChanged", diff.String())
	}
}
