package release

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	shipper "github.com/bookingcom/shipper/pkg/apis/shipper/v1alpha1"
	shipperrepo "github.com/bookingcom/shipper/pkg/chart/repo"
	shipperclient "github.com/bookingcom/shipper/pkg/client/clientset/versioned"
	shipperinformers "github.com/bookingcom/shipper/pkg/client/informers/externalversions"
	shipperlisters "github.com/bookingcom/shipper/pkg/client/listers/shipper/v1alpha1"
	"github.com/bookingcom/shipper/pkg/controller"
	shippercontroller "github.com/bookingcom/shipper/pkg/controller"
	shippererrors "github.com/bookingcom/shipper/pkg/errors"
	apputil "github.com/bookingcom/shipper/pkg/util/application"
	"github.com/bookingcom/shipper/pkg/util/conditions"
	diffutil "github.com/bookingcom/shipper/pkg/util/diff"
	releaseutil "github.com/bookingcom/shipper/pkg/util/release"
	rolloutblock "github.com/bookingcom/shipper/pkg/util/rolloutblock"
	shipperworkqueue "github.com/bookingcom/shipper/pkg/workqueue"
)

const (
	AgentName = "release-controller"
)

const (
	ClustersNotReady = "ClustersNotReady"
)

// Controller is a Kubernetes controller whose role is to pick up a newly created
// release and progress it forward by scheduling the release on a set of
// selected clusters, creating a set of associated objects and executing the
// strategy.
//
// Release Controller has 2 primary workqueues: releases and applications.
type Controller struct {
	clientset shipperclient.Interface

	applicationLister  shipperlisters.ApplicationLister
	applicationsSynced cache.InformerSynced

	releaseLister  shipperlisters.ReleaseLister
	releasesSynced cache.InformerSynced

	clusterLister  shipperlisters.ClusterLister
	clustersSynced cache.InformerSynced

	installationTargetLister  shipperlisters.InstallationTargetLister
	installationTargetsSynced cache.InformerSynced

	trafficTargetLister  shipperlisters.TrafficTargetLister
	trafficTargetsSynced cache.InformerSynced

	capacityTargetLister  shipperlisters.CapacityTargetLister
	capacityTargetsSynced cache.InformerSynced

	rolloutBlockLister shipperlisters.RolloutBlockLister
	rolloutBlockSynced cache.InformerSynced

	releaseWorkqueue workqueue.RateLimitingInterface

	chartFetcher shipperrepo.ChartFetcher

	recorder record.EventRecorder
}

type releaseInfo struct {
	release            *shipper.Release
	installationTarget *shipper.InstallationTarget
	trafficTarget      *shipper.TrafficTarget
	capacityTarget     *shipper.CapacityTarget
}

type ReleaseStrategyStateTransition struct {
	State    string
	Previous shipper.StrategyState
	New      shipper.StrategyState
}

func NewController(
	clientset shipperclient.Interface,
	informerFactory shipperinformers.SharedInformerFactory,
	chartFetcher shipperrepo.ChartFetcher,
	recorder record.EventRecorder,
) *Controller {

	applicationInformer := informerFactory.Shipper().V1alpha1().Applications()
	releaseInformer := informerFactory.Shipper().V1alpha1().Releases()
	clusterInformer := informerFactory.Shipper().V1alpha1().Clusters()
	installationTargetInformer := informerFactory.Shipper().V1alpha1().InstallationTargets()
	trafficTargetInformer := informerFactory.Shipper().V1alpha1().TrafficTargets()
	capacityTargetInformer := informerFactory.Shipper().V1alpha1().CapacityTargets()
	rolloutBlockInformer := informerFactory.Shipper().V1alpha1().RolloutBlocks()

	klog.Info("Building a release controller")

	controller := &Controller{
		clientset: clientset,

		applicationLister:  applicationInformer.Lister(),
		applicationsSynced: applicationInformer.Informer().HasSynced,

		releaseLister:  releaseInformer.Lister(),
		releasesSynced: releaseInformer.Informer().HasSynced,

		clusterLister:  clusterInformer.Lister(),
		clustersSynced: clusterInformer.Informer().HasSynced,

		installationTargetLister:  installationTargetInformer.Lister(),
		installationTargetsSynced: installationTargetInformer.Informer().HasSynced,

		trafficTargetLister:  trafficTargetInformer.Lister(),
		trafficTargetsSynced: trafficTargetInformer.Informer().HasSynced,

		capacityTargetLister:  capacityTargetInformer.Lister(),
		capacityTargetsSynced: capacityTargetInformer.Informer().HasSynced,

		rolloutBlockLister: rolloutBlockInformer.Lister(),
		rolloutBlockSynced: rolloutBlockInformer.Informer().HasSynced,

		releaseWorkqueue: workqueue.NewNamedRateLimitingQueue(
			shipperworkqueue.NewDefaultControllerRateLimiter(),
			"release_controller_releases",
		),

		chartFetcher: chartFetcher,

		recorder: recorder,
	}

	klog.Info("Setting up event handlers")

	releaseInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: controller.enqueueRelease,
			UpdateFunc: func(oldObj, newObj interface{}) {
				controller.enqueueRelease(newObj)
			},
			//TODO
			//DeleteFunc: controller.enqueueAppFromRelease,
		})

	eventHandler := cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueReleaseFromAssociatedObject,
		UpdateFunc: func(oldObj, newObj interface{}) {
			controller.enqueueReleaseFromAssociatedObject(newObj)
		},
		DeleteFunc: controller.enqueueReleaseFromAssociatedObject,
	}

	installationTargetInformer.Informer().AddEventHandler(eventHandler)
	capacityTargetInformer.Informer().AddEventHandler(eventHandler)
	trafficTargetInformer.Informer().AddEventHandler(eventHandler)

	return controller
}

// Run starts Release Controller workers and waits until stopCh is closed.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()
	defer c.releaseWorkqueue.ShutDown()

	klog.V(2).Info("Starting Release controller")
	defer klog.V(2).Info("Shutting down Release controller")

	if ok := cache.WaitForCacheSync(
		stopCh,
		c.applicationsSynced,
		c.releasesSynced,
		c.clustersSynced,
		c.installationTargetsSynced,
		c.trafficTargetsSynced,
		c.capacityTargetsSynced,
		c.rolloutBlockSynced,
	); !ok {
		runtime.HandleError(fmt.Errorf("failed to wait for caches to sync"))
		return
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runReleaseWorker, time.Second, stopCh)
	}

	klog.V(4).Info("Started Release controller")

	<-stopCh
}

func (c *Controller) runReleaseWorker() {
	for c.processNextReleaseWorkItem() {
	}
}

/*
func (c *Controller) runApplicationWorker() {
	for c.processNextAppWorkItem() {
	}
}
*/

// processNextReleaseWorkItem pops an element from the head of the workqueue and
// passes to the sync release handler. It returns bool indicating if the
// execution process should go on.
func (c *Controller) processNextReleaseWorkItem() bool {
	obj, shutdown := c.releaseWorkqueue.Get()
	if shutdown {
		return false
	}

	defer c.releaseWorkqueue.Done(obj)

	var (
		key string
		ok  bool
	)

	if key, ok = obj.(string); !ok {
		c.releaseWorkqueue.Forget(obj)
		runtime.HandleError(fmt.Errorf("invalid object key (will retry: false): %#v", obj))
		return true
	}

	shouldRetry := false
	err := c.syncOneReleaseHandler(key)

	if err != nil {
		shouldRetry = shippererrors.ShouldRetry(err)
		runtime.HandleError(fmt.Errorf("error syncing Release %q (will retry: %t): %s", key, shouldRetry, err.Error()))
	}

	if shouldRetry {
		c.releaseWorkqueue.AddRateLimited(key)

		return true
	}

	klog.V(4).Infof("Successfully synced Release %q", key)
	c.releaseWorkqueue.Forget(obj)

	return true
}

// syncOneReleaseHandler processes release keys one-by-one. This stage progresses
// the release through a scheduler: assigns a set of chosen clusters, creates
// required associated objects and marks the release as scheduled.
func (c *Controller) syncOneReleaseHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return shippererrors.NewUnrecoverableError(err)
	}

	initialRel, err := c.releaseLister.Releases(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			klog.V(3).Infof("Release %q not found", key)
			return nil
		}

		return shippererrors.NewKubeclientGetError(namespace, name, err).
			WithShipperKind("Release")
	}

	if releaseutil.HasEmptyEnvironment(initialRel) {
		return nil
	}

	rel, err := c.scheduleRelease(initialRel.DeepCopy())
	if err != nil {
		return err
	}

	rel, err = c.ensureReleaseState(rel.DeepCopy())
	if err != nil {
		return err
	}

	if !equality.Semantic.DeepEqual(initialRel, rel) {
		if _, updErr := c.clientset.ShipperV1alpha1().Releases(namespace).Update(rel); updErr != nil {
			return shippererrors.NewKubeclientUpdateError(rel, updErr).
				WithShipperKind("Release")
		}

		klog.V(4).Infof("Done scheduling Release %q", key)
		return nil
	}

	klog.V(4).Infof("Done processing Release %q", key)

	return nil
}

func (c *Controller) ensureReleaseState(curr *shipper.Release) (*shipper.Release, error) {
	if curr == nil {
		return nil, fmt.Errorf("non-empty release expected, nil provided")
	}
	appName, err := releaseutil.ApplicationNameForRelease(curr)
	if err != nil {
		return nil, err
	}
	// TODO: move this to a helper
	releases, err := c.releaseLister.Releases(curr.Namespace).ReleasesForApplication(appName)
	if err != nil {
		return nil, err
	}
	releases = releaseutil.SortByGenerationDescending(releases)
	var succ *shipper.Release
	found := false
	for ix, rel := range releases {
		// maybe call it shallow equal?
		if same, err := releaseEqual(rel, curr); err != nil {
			return nil, err
		} else if same {
			if ix > 0 {
				succ = releases[ix-1]
			}
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("release %q can not be found among application %q releases", controller.MetaKey(curr), appName)
	}

	relInfoCurr, err := c.buildReleaseInfo(curr)
	if err != nil {
		return nil, err
	}
	var relInfoSucc *releaseInfo
	if succ != nil {
		relInfoSucc, err = c.buildReleaseInfo(succ)
		if err != nil {
			return nil, err
		}
	}

	executor, err := NewStrategyExecutor(relInfoCurr, relInfoSucc, c.recorder, len(releases) > 1)
	if err != nil {
		return nil, err
	}

	patches, trans, err := executor.Execute()
	if err != nil {
		releaseSyncedCond := apputil.NewApplicationCondition(
			shipper.ApplicationConditionTypeReleaseSynced,
			corev1.ConditionFalse,
			conditions.StrategyExecutionFailed,
			fmt.Sprintf("failed to execute application strategy: %q", err),
		)
		diff := apputil.SetApplicationCondition(&app.Status, *releaseSyncedCond)
		_, err = c.clientset.ShipperV1alpha1().Applications(app.Namespace).Update(app)
		if err != nil {
			return nil, shippererrors.NewKubeclientUpdateError(app, err).
				WithShipperKind("Application")
		}
		if !diff.IsEmpty() {
			c.recorder.Eventf(app, corev1.EventTypeNormal, "ApplicationConditionChanged", diff.String())
		}
		return nil, err
	}

	for _, t := range trans {
		c.recorder.Eventf(
			executor.curr.release,
			corev1.EventTypeNormal,
			"ReleaseStateTransitioned",
			"Release %q had its state %q transitioned to %q",
			shippercontroller.MetaKey(executor.curr.release),
			t.State,
			t.New,
		)
	}

	if len(patches) == 0 {
		klog.V(4).Infof("Strategy verified, nothing to patch")
		return curr, nil
	}

	klog.V(4).Infof("Strategy has been executed, applying patches")
	for _, patch := range patches {
		c.applyPatch(curr.Namespace, curr.Name, patch)
	}

	return curr, nil
}

func (c *Controller) applyPatch(namespace, name string, patch ExecutorResult) error {
	name, gvk, b := patch.PatchSpec()

	var err error
	switch gvk.Kind {
	case "Release":
		_, err = c.clientset.ShipperV1alpha1().Releases(namespace).Patch(name, types.MergePatchType, b)
	case "InstallationTarget":
		_, err = c.clientset.ShipperV1alpha1().InstallationTargets(namespace).Patch(name, types.MergePatchType, b)
	case "CapacityTarget":
		_, err = c.clientset.ShipperV1alpha1().CapacityTargets(namespace).Patch(name, types.MergePatchType, b)
	case "TrafficTarget":
		_, err = c.clientset.ShipperV1alpha1().TrafficTargets(namespace).Patch(name, types.MergePatchType, b)
	default:
		return shippererrors.NewUnrecoverableError(fmt.Errorf("error syncing Release %q (will not retry): unknown GVK resource name: %s", name, gvk.Kind))
	}
	if err != nil {
		return shippererrors.NewKubeclientPatchError(namespace, name, err).WithKind(gvk)
	}

	return nil
}

func releaseEqual(r1, r2 *shipper.Release) (bool, error) {
	if r1 == nil || r2 == nil {
		return false, nil
	}
	g1, err := releaseutil.GetGeneration(r1)
	if err != nil {
		return false, err
	}
	g2, err := releaseutil.GetGeneration(r2)
	if err != nil {
		return false, err
	}
	return (r1.Name == r2.Name &&
		r1.Namespace == r2.Namespace &&
		g1 == g2), nil
}

func (c *Controller) scheduleRelease(rel *shipper.Release) (*shipper.Release, error) {
	scheduler := NewScheduler(
		c.clientset,
		c.clusterLister,
		c.installationTargetLister,
		c.capacityTargetLister,
		c.trafficTargetLister,
		c.rolloutBlockLister,
		c.chartFetcher,
		c.recorder,
	)

	initialRel := rel.DeepCopy()

	diff := diffutil.NewMultiDiff()
	defer func() {
		if !diff.IsEmpty() {
			c.recorder.Event(initialRel, corev1.EventTypeNormal, "ReleaseConditionChanged", diff.String())
		}
	}()

	rolloutBlocked, events, err := rolloutblock.BlocksRollout(c.rolloutBlockLister, rel)
	for _, ev := range events {
		c.recorder.Event(rel, ev.Type, ev.Reason, ev.Message)
	}
	if rolloutBlocked {
		var msg string
		if err != nil {
			msg = err.Error()
		}

		condition := releaseutil.NewReleaseCondition(
			shipper.ReleaseConditionTypeBlocked,
			corev1.ConditionTrue,
			shipper.RolloutBlockReason,
			msg,
		)
		diff.Append(releaseutil.SetReleaseCondition(&rel.Status, *condition))

		return rel, err
	}

	condition := releaseutil.NewReleaseCondition(
		shipper.ReleaseConditionTypeBlocked,
		corev1.ConditionFalse,
		"",
		"",
	)
	diff.Append(releaseutil.SetReleaseCondition(&rel.Status, *condition))

	scheduledRel, err := scheduler.ScheduleRelease(rel.DeepCopy())
	if err != nil {
		reason := reasonForReleaseCondition(err)
		condition := releaseutil.NewReleaseCondition(
			shipper.ReleaseConditionTypeScheduled,
			corev1.ConditionFalse,
			reason,
			err.Error(),
		)
		diff.Append(releaseutil.SetReleaseCondition(&initialRel.Status, *condition))

		return rel, err
	}

	rel = scheduledRel
	condition = releaseutil.NewReleaseCondition(
		shipper.ReleaseConditionTypeScheduled,
		corev1.ConditionTrue,
		"",
		"",
	)
	diff.Append(releaseutil.SetReleaseCondition(&rel.Status, *condition))

	klog.V(4).Infof("Release %q has been successfully scheduled", controller.MetaKey(rel))

	return rel, nil
}

// getAssociatedApplicationKey returns an application key in the format:
// <namespace>/<application name>
func (c *Controller) getAssociatedApplicationKey(rel *shipper.Release) (string, error) {
	appName, err := releaseutil.ApplicationNameForRelease(rel)
	if err != nil {
		return "", err
	}

	appKey := fmt.Sprintf("%s/%s", rel.Namespace, appName)

	return appKey, nil
}

// getAssociatedReleaseKey returns an owner reference release name for an
// associated object in the format:
// <namespace> / <release name>
func (c *Controller) getAssociatedReleaseKey(obj metav1.Object) (string, error) {
	references := obj.GetOwnerReferences()
	if n := len(references); n != 1 {
		return "", shippererrors.NewMultipleOwnerReferencesError(obj.GetName(), n)
	}

	owner := references[0]

	return fmt.Sprintf("%s/%s", obj.GetNamespace(), owner.Name), nil
}

// buildReleaseInfo returns a release and it's associated objects fetched from
// the lister interface. If some of them could not be found, it returns a
// corresponding error.
func (c *Controller) buildReleaseInfo(rel *shipper.Release) (*releaseInfo, error) {
	ns := rel.Namespace
	name := rel.Name

	installationTarget, err := c.installationTargetLister.InstallationTargets(ns).Get(name)
	if err != nil {
		return nil, shippererrors.NewKubeclientGetError(ns, name, err).
			WithShipperKind("InstallationTarget")
	}

	capacityTarget, err := c.capacityTargetLister.CapacityTargets(ns).Get(name)
	if err != nil {
		return nil, shippererrors.NewKubeclientGetError(ns, name, err).
			WithShipperKind("CapacityTarget")
	}

	trafficTarget, err := c.trafficTargetLister.TrafficTargets(ns).Get(name)
	if err != nil {
		return nil, shippererrors.NewKubeclientGetError(ns, name, err).
			WithShipperKind("TrafficTarget")
	}

	return &releaseInfo{
		release:            rel,
		installationTarget: installationTarget,
		trafficTarget:      trafficTarget,
		capacityTarget:     capacityTarget,
	}, nil
}

func (c *Controller) enqueueRelease(obj interface{}) {
	rel, ok := obj.(*shipper.Release)
	if !ok {
		runtime.HandleError(fmt.Errorf("not a shipper.Release: %#v", obj))
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(rel)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	c.releaseWorkqueue.Add(key)
}

func (c *Controller) enqueueReleaseRateLimited(obj interface{}) {
	rel, ok := obj.(*shipper.Release)
	if !ok {
		runtime.HandleError(fmt.Errorf("not a shipper.Release: %#v", obj))
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(rel)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	c.releaseWorkqueue.AddRateLimited(key)
}

/*
func (c *Controller) enqueueAppFromRelease(obj interface{}) {
	rel, ok := obj.(*shipper.Release)
	if !ok {
		runtime.HandleError(fmt.Errorf("not a shipper.Release: %#v", obj))
		return
	}

	appName, err := c.getAssociatedApplicationKey(rel)
	if err != nil {
		runtime.HandleError(fmt.Errorf("error fetching Application key for release %v: %s", rel, err))
		return
	}

	c.applicationWorkqueue.Add(appName)
}
*/

func (c *Controller) enqueueReleaseFromAssociatedObject(obj interface{}) {
	kubeobj, ok := obj.(metav1.Object)
	if !ok {
		runtime.HandleError(fmt.Errorf("not a metav1.Object: %#v", obj))
		return
	}

	releaseKey, err := c.getAssociatedReleaseKey(kubeobj)
	if err != nil {
		runtime.HandleError(err)
		return
	}

	c.releaseWorkqueue.Add(releaseKey)
}

func (c *Controller) reportReleaseConditionChange(rel *shipper.Release, diff diffutil.Diff) {

}

func reasonForReleaseCondition(err error) string {
	switch err.(type) {
	case shippererrors.NoRegionsSpecifiedError:
		return "NoRegionsSpecified"
	case shippererrors.NotEnoughClustersInRegionError:
		return "NotEnoughClustersInRegion"
	case shippererrors.NotEnoughCapableClustersInRegionError:
		return "NotEnoughCapableClustersInRegion"

	case shippererrors.DuplicateCapabilityRequirementError:
		return "DuplicateCapabilityRequirement"

	case shippererrors.ChartFetchFailureError:
		return "ChartFetchFailure"
	case shippererrors.BrokenChartSpecError:
		return "BrokenChartSpec"
	case shippererrors.WrongChartDeploymentsError:
		return "WrongChartDeployments"
	case shippererrors.RolloutBlockError:
		return "RolloutBlock"
	case shippererrors.ChartRepoInternalError:
		return "ChartRepoInternal"
	}

	if shippererrors.IsKubeclientError(err) {
		return "FailedAPICall"
	}

	return "unknown error! tell Shipper devs to classify it"
}
