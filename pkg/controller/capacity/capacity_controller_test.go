package capacity

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	shipper "github.com/bookingcom/shipper/pkg/apis/shipper/v1alpha1"
	shipperfake "github.com/bookingcom/shipper/pkg/client/clientset/versioned/fake"
	shipperinformers "github.com/bookingcom/shipper/pkg/client/informers/externalversions"
	"github.com/bookingcom/shipper/pkg/controller/capacity/builder"
	shippertesting "github.com/bookingcom/shipper/pkg/testing"
	capacityutil "github.com/bookingcom/shipper/pkg/util/capacity"
)

var (
	numClus int32
)

func init() {
	rand.Seed(time.Now().UTC().UnixNano())
	numClus = 1 + rand.Int31n(5)
	capacityutil.CapacityConditionsShouldDiscardTimestamps = true
}

func TestUpdatingCapacityTargetUpdatesDeployment(t *testing.T) {
	f := NewFixture(t)

	capacityTarget := newCapacityTarget(10, 50, numClus)
	f.managementObjects = append(f.managementObjects, capacityTarget.DeepCopy())

	deployment := newDeployment(0, 0)
	f.targetClusterObjects = append(f.targetClusterObjects, deployment)

	expectedClusterConditions := []shipper.ClusterCapacityCondition{
		{
			Type:   shipper.ClusterConditionTypeOperational,
			Status: corev1.ConditionTrue,
		},
		{
			Type:   shipper.ClusterConditionTypeReady,
			Status: corev1.ConditionTrue,
		},
	}

	f.expectCapacityTargetStatusUpdate(capacityTarget, 0, 0, expectedClusterConditions, []shipper.ClusterCapacityReport{*builder.NewReport("nginx").Build()})

	f.runCapacityTargetSyncHandler()
	f.checkDeploymentPatchWithReplicas(deployment, 5)
}

func TestCapacityTargetStatusReturnsCorrectFleetReportWithSinglePod(t *testing.T) {
	f := NewFixture(t)

	capacityTarget := newCapacityTarget(1, 100, numClus)

	deployment := newDeployment(1, 1)
	podLabels, _ := metav1.LabelSelectorAsMap(deployment.Spec.Selector)

	podA := newPodBuilder("pod-a", deployment.GetNamespace(), podLabels).
		AddContainerStatus("app", corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, 0, nil).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodInitialized, Status: corev1.ConditionTrue}).
		AddPodCondition(corev1.PodCondition{Type: "ContainersReady", Status: corev1.ConditionTrue}).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue}).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}).
		Build()

	f.targetClusterObjects = append(f.targetClusterObjects, deployment, podA)

	c := builder.NewReport("nginx").
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(1, "ContainersReady", "True", "").
				AddOrIncrementContainerState("app", "pod-a", "Running", "", "")).
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(1, "Initialized", "True", "").
				AddOrIncrementContainerState("app", "pod-a", "Running", "", "")).
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(1, "PodScheduled", "True", "").
				AddOrIncrementContainerState("app", "pod-a", "Running", "", "")).
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(1, "Ready", "True", "").
				AddOrIncrementContainerState("app", "pod-a", "Running", "", ""))

	f.managementObjects = append(f.managementObjects, capacityTarget.DeepCopy())

	for i := int32(0); i < numClus; i++ {
		capacityTarget.Status.Clusters = append(capacityTarget.Status.Clusters, shipper.ClusterCapacityStatus{
			Name:              fmt.Sprintf("cluster_%d", i),
			Reports:           []shipper.ClusterCapacityReport{*c.Build()},
			AchievedPercent:   100,
			AvailableReplicas: 1,
			Conditions: []shipper.ClusterCapacityCondition{
				{Type: shipper.ClusterConditionTypeOperational, Status: corev1.ConditionTrue},
				{Type: shipper.ClusterConditionTypeReady, Status: corev1.ConditionTrue},
			},
		})
	}

	updateAction := kubetesting.NewUpdateAction(
		schema.GroupVersionResource{
			Group:    shipper.SchemeGroupVersion.Group,
			Version:  shipper.SchemeGroupVersion.Version,
			Resource: "capacitytargets",
		},
		capacityTarget.GetNamespace(),
		capacityTarget,
	)

	f.managementClusterActions = append(f.managementClusterActions, updateAction)
	f.runCapacityTargetSyncHandler()

	// Calling the sync handler again with the updated capacity target object should yield the same results.
	f.managementObjects = []runtime.Object{capacityTarget.DeepCopy()}
	f.managementClusterActions = []kubetesting.Action{}
	f.runCapacityTargetSyncHandler()
}

func TestCapacityTargetStatusReturnsCorrectFleetReportWithSinglePodCompletedContainer(t *testing.T) {
	f := NewFixture(t)

	capacityTarget := newCapacityTarget(1, 100, numClus)

	deployment := newDeployment(1, 1)
	podLabels, _ := metav1.LabelSelectorAsMap(deployment.Spec.Selector)

	podA := newPodBuilder("pod-a", deployment.GetNamespace(), podLabels).
		AddContainerStatus("app", corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 1}}, 0, nil).
		AddPodCondition(corev1.PodCondition{Type: "ContainersReady", Status: corev1.ConditionTrue}).
		Build()

	f.targetClusterObjects = append(f.targetClusterObjects, deployment, podA)

	c := builder.NewReport("nginx").
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(1, "ContainersReady", "True", "").
				AddOrIncrementContainerState("app", "pod-a", "Terminated", "Completed", "Terminated with exit code 1"))

	f.managementObjects = append(f.managementObjects, capacityTarget.DeepCopy())

	for i := int32(0); i < numClus; i++ {
		capacityTarget.Status.Clusters = append(capacityTarget.Status.Clusters, shipper.ClusterCapacityStatus{
			Name:              fmt.Sprintf("cluster_%d", i),
			Reports:           []shipper.ClusterCapacityReport{*c.Build()},
			AchievedPercent:   100,
			AvailableReplicas: 1,
			Conditions: []shipper.ClusterCapacityCondition{
				{Type: shipper.ClusterConditionTypeOperational, Status: corev1.ConditionTrue},
				{Type: shipper.ClusterConditionTypeReady, Status: corev1.ConditionTrue},
			},
		})
	}

	updateAction := kubetesting.NewUpdateAction(
		schema.GroupVersionResource{
			Group:    shipper.SchemeGroupVersion.Group,
			Version:  shipper.SchemeGroupVersion.Version,
			Resource: "capacitytargets",
		},
		capacityTarget.GetNamespace(),
		capacityTarget,
	)

	f.managementClusterActions = append(f.managementClusterActions, updateAction)
	f.runCapacityTargetSyncHandler()

	// Calling the sync handler again with the updated capacity target object should yield the same results.
	f.managementObjects = []runtime.Object{capacityTarget.DeepCopy()}
	f.managementClusterActions = []kubetesting.Action{}
	f.runCapacityTargetSyncHandler()
}

func TestCapacityTargetStatusReturnsCorrectFleetReportWithSinglePodTerminatedContainer(t *testing.T) {
	f := NewFixture(t)

	capacityTarget := newCapacityTarget(1, 100, numClus)

	deployment := newDeployment(1, 1)
	podLabels, _ := metav1.LabelSelectorAsMap(deployment.Spec.Selector)

	podA := newPodBuilder("pod-a", deployment.GetNamespace(), podLabels).
		AddContainerStatus("app", corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Terminated", Signal: 9}}, 0, nil).
		AddPodCondition(corev1.PodCondition{Type: "ContainersReady", Status: corev1.ConditionTrue}).
		Build()

	f.targetClusterObjects = append(f.targetClusterObjects, deployment, podA)

	c := builder.NewReport("nginx").
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(1, "ContainersReady", "True", "").
				AddOrIncrementContainerState("app", "pod-a", "Terminated", "Terminated", "Terminated with signal 9"))

	f.managementObjects = append(f.managementObjects, capacityTarget.DeepCopy())

	for i := int32(0); i < numClus; i++ {
		capacityTarget.Status.Clusters = append(capacityTarget.Status.Clusters, shipper.ClusterCapacityStatus{
			Name:              fmt.Sprintf("cluster_%d", i),
			Reports:           []shipper.ClusterCapacityReport{*c.Build()},
			AchievedPercent:   100,
			AvailableReplicas: 1,
			Conditions: []shipper.ClusterCapacityCondition{
				{Type: shipper.ClusterConditionTypeOperational, Status: corev1.ConditionTrue},
				{Type: shipper.ClusterConditionTypeReady, Status: corev1.ConditionTrue},
			},
		})
	}

	updateAction := kubetesting.NewUpdateAction(
		schema.GroupVersionResource{
			Group:    shipper.SchemeGroupVersion.Group,
			Version:  shipper.SchemeGroupVersion.Version,
			Resource: "capacitytargets",
		},
		capacityTarget.GetNamespace(),
		capacityTarget,
	)

	f.managementClusterActions = append(f.managementClusterActions, updateAction)
	f.runCapacityTargetSyncHandler()

	// Calling the sync handler again with the updated capacity target object should yield the same results.
	f.managementObjects = []runtime.Object{capacityTarget.DeepCopy()}
	f.managementClusterActions = []kubetesting.Action{}
	f.runCapacityTargetSyncHandler()
}

func TestCapacityTargetStatusReturnsCorrectFleetReportWithSinglePodRestartedContainer(t *testing.T) {
	f := NewFixture(t)

	capacityTarget := newCapacityTarget(1, 100, numClus)

	deployment := newDeployment(1, 1)
	podLabels, _ := metav1.LabelSelectorAsMap(deployment.Spec.Selector)

	podA := newPodBuilder("pod-a", deployment.GetNamespace(), podLabels).
		AddContainerStatus("app", corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Terminated", Signal: 9}}, 0, nil).
		AddPodCondition(corev1.PodCondition{Type: "ContainersReady", Status: corev1.ConditionTrue}).
		Build()

	f.targetClusterObjects = append(f.targetClusterObjects, deployment, podA)

	c := builder.NewReport("nginx").
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(1, "ContainersReady", "True", "").
				AddOrIncrementContainerState("app", "pod-a", "Terminated", "Terminated", "Terminated with signal 9"))

	f.managementObjects = append(f.managementObjects, capacityTarget.DeepCopy())

	for i := int32(0); i < numClus; i++ {
		capacityTarget.Status.Clusters = append(capacityTarget.Status.Clusters, shipper.ClusterCapacityStatus{
			Name:              fmt.Sprintf("cluster_%d", i),
			Reports:           []shipper.ClusterCapacityReport{*c.Build()},
			AchievedPercent:   100,
			AvailableReplicas: 1,
			Conditions: []shipper.ClusterCapacityCondition{
				{Type: shipper.ClusterConditionTypeOperational, Status: corev1.ConditionTrue},
				{Type: shipper.ClusterConditionTypeReady, Status: corev1.ConditionTrue},
			},
		})
	}

	updateAction := kubetesting.NewUpdateAction(
		schema.GroupVersionResource{
			Group:    shipper.SchemeGroupVersion.Group,
			Version:  shipper.SchemeGroupVersion.Version,
			Resource: "capacitytargets",
		},
		capacityTarget.GetNamespace(),
		capacityTarget,
	)

	f.managementClusterActions = append(f.managementClusterActions, updateAction)
	f.runCapacityTargetSyncHandler()

	// Calling the sync handler again with the updated capacity target object should yield the same results.
	f.managementObjects = []runtime.Object{capacityTarget.DeepCopy()}
	f.managementClusterActions = []kubetesting.Action{}
	f.runCapacityTargetSyncHandler()
}

func TestCapacityTargetStatusReturnsCorrectFleetReportWithSinglePodRestartedContainerWithTerminationMessage(t *testing.T) {
	f := NewFixture(t)

	capacityTarget := newCapacityTarget(1, 100, numClus)

	deployment := newDeployment(1, 1)
	podLabels, _ := metav1.LabelSelectorAsMap(deployment.Spec.Selector)

	podA := newPodBuilder("pod-a", deployment.GetNamespace(), podLabels).
		AddContainerStatus("app", corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Terminated", Signal: 9}}, 1, &corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "termination message"}}).
		AddPodCondition(corev1.PodCondition{Type: "ContainersReady", Status: corev1.ConditionTrue}).
		Build()

	f.targetClusterObjects = append(f.targetClusterObjects, deployment, podA)

	c := builder.NewReport("nginx").
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(1, "ContainersReady", "True", "").
				AddOrIncrementContainerState("app", "pod-a", "Terminated", "Terminated", "termination message"))

	f.managementObjects = append(f.managementObjects, capacityTarget.DeepCopy())

	for i := int32(0); i < numClus; i++ {
		capacityTarget.Status.Clusters = append(capacityTarget.Status.Clusters, shipper.ClusterCapacityStatus{
			Name:              fmt.Sprintf("cluster_%d", i),
			Reports:           []shipper.ClusterCapacityReport{*c.Build()},
			AchievedPercent:   100,
			AvailableReplicas: 1,
			Conditions: []shipper.ClusterCapacityCondition{
				{Type: shipper.ClusterConditionTypeOperational, Status: corev1.ConditionTrue},
				{Type: shipper.ClusterConditionTypeReady, Status: corev1.ConditionTrue},
			},
		})
	}

	updateAction := kubetesting.NewUpdateAction(
		schema.GroupVersionResource{
			Group:    shipper.SchemeGroupVersion.Group,
			Version:  shipper.SchemeGroupVersion.Version,
			Resource: "capacitytargets",
		},
		capacityTarget.GetNamespace(),
		capacityTarget,
	)

	f.managementClusterActions = append(f.managementClusterActions, updateAction)
	f.runCapacityTargetSyncHandler()

	// Calling the sync handler again with the updated capacity target object should yield the same results.
	f.managementObjects = []runtime.Object{capacityTarget.DeepCopy()}
	f.managementClusterActions = []kubetesting.Action{}
	f.runCapacityTargetSyncHandler()
}

func TestCapacityTargetStatusReturnsCorrectFleetReportWithMultiplePods(t *testing.T) {
	f := NewFixture(t)

	capacityTarget := newCapacityTarget(2, 100, numClus)

	deployment := newDeployment(2, 2)
	podLabels, _ := metav1.LabelSelectorAsMap(deployment.Spec.Selector)

	podA := newPodBuilder("pod-a", deployment.GetNamespace(), podLabels).
		AddContainerStatus("app", corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, 0, nil).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodInitialized, Status: corev1.ConditionTrue}).
		AddPodCondition(corev1.PodCondition{Type: "ContainersReady", Status: corev1.ConditionTrue}).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue}).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}).
		Build()

	podB := newPodBuilder("pod-b", deployment.GetNamespace(), podLabels).
		AddContainerStatus("app", corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, 0, nil).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodInitialized, Status: corev1.ConditionTrue}).
		AddPodCondition(corev1.PodCondition{Type: "ContainersReady", Status: corev1.ConditionTrue}).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue}).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}).
		Build()

	f.targetClusterObjects = append(f.targetClusterObjects, deployment, podA, podB)

	c := builder.NewReport("nginx").
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(2, "ContainersReady", "True", "").
				AddOrIncrementContainerState("app", "pod-a", "Running", "", "").
				AddOrIncrementContainerState("app", "pod-a", "Running", "", "")).
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(2, "Initialized", "True", "").
				AddOrIncrementContainerState("app", "pod-a", "Running", "", "").
				AddOrIncrementContainerState("app", "pod-a", "Running", "", "")).
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(2, "PodScheduled", "True", "").
				AddOrIncrementContainerState("app", "pod-a", "Running", "", "").
				AddOrIncrementContainerState("app", "pod-a", "Running", "", "")).
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(2, "Ready", "True", "").
				AddOrIncrementContainerState("app", "pod-a", "Running", "", "").
				AddOrIncrementContainerState("app", "pod-a", "Running", "", ""))

	f.managementObjects = append(f.managementObjects, capacityTarget.DeepCopy())

	for i := int32(0); i < numClus; i++ {
		capacityTarget.Status.Clusters = append(capacityTarget.Status.Clusters, shipper.ClusterCapacityStatus{
			Name:              fmt.Sprintf("cluster_%d", i),
			Reports:           []shipper.ClusterCapacityReport{*c.Build()},
			AchievedPercent:   100,
			AvailableReplicas: 2,
			Conditions: []shipper.ClusterCapacityCondition{
				{Type: shipper.ClusterConditionTypeOperational, Status: corev1.ConditionTrue},
				{Type: shipper.ClusterConditionTypeReady, Status: corev1.ConditionTrue},
			},
		})
	}

	updateAction := kubetesting.NewUpdateAction(
		schema.GroupVersionResource{
			Group:    shipper.SchemeGroupVersion.Group,
			Version:  shipper.SchemeGroupVersion.Version,
			Resource: "capacitytargets",
		},
		capacityTarget.GetNamespace(),
		capacityTarget,
	)

	f.managementClusterActions = append(f.managementClusterActions, updateAction)
	f.runCapacityTargetSyncHandler()

	// Calling the sync handler again with the updated capacity target object should yield the same results.
	f.managementObjects = []runtime.Object{capacityTarget.DeepCopy()}
	f.managementClusterActions = []kubetesting.Action{}
	f.runCapacityTargetSyncHandler()
}

func TestCapacityTargetStatusReturnsCorrectFleetReportWithMultiplePodsWithDifferentConditions(t *testing.T) {
	f := NewFixture(t)

	capacityTarget := newCapacityTarget(3, 100, numClus)

	deployment := newDeployment(3, 3)
	podLabels, _ := metav1.LabelSelectorAsMap(deployment.Spec.Selector)

	podA := newPodBuilder("pod-a", deployment.GetNamespace(), podLabels).
		AddContainerStatus("app", corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}, 0, nil).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodInitialized, Status: corev1.ConditionTrue}).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"}).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}).
		Build()

	podB := newPodBuilder("pod-b", deployment.GetNamespace(), podLabels).
		AddContainerStatus("app", corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}, 0, nil).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodInitialized, Status: corev1.ConditionTrue}).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"}).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}).
		Build()

	podC := newPodBuilder("pod-c", deployment.GetNamespace(), podLabels).
		AddContainerStatus("app", corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}, 0, nil).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodInitialized, Status: corev1.ConditionTrue}).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"}).
		AddPodCondition(corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}).
		Build()

	f.targetClusterObjects = append(f.targetClusterObjects, deployment, podA, podB, podC)

	c := builder.NewReport("nginx").
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(3, string(corev1.PodInitialized), string(corev1.ConditionTrue), "").
				AddOrIncrementContainerState("app", "pod-a", "Waiting", "ContainerCreating", "").
				AddOrIncrementContainerState("app", "pod-a", "Waiting", "ContainerCreating", "").
				AddOrIncrementContainerState("app", "pod-c", "Terminated", "Completed", "Terminated with exit code 0")).
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(3, string(corev1.PodScheduled), string(corev1.ConditionTrue), "").
				AddOrIncrementContainerState("app", "pod-a", "Waiting", "ContainerCreating", "").
				AddOrIncrementContainerState("app", "pod-a", "Waiting", "ContainerCreating", "").
				AddOrIncrementContainerState("app", "pod-c", "Terminated", "Completed", "Terminated with exit code 0")).
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(3, string(corev1.PodReady), string(corev1.ConditionFalse), "ContainersNotReady").
				AddOrIncrementContainerState("app", "pod-a", "Waiting", "ContainerCreating", "").
				AddOrIncrementContainerState("app", "pod-a", "Waiting", "ContainerCreating", "").
				AddOrIncrementContainerState("app", "pod-c", "Terminated", "Completed", "Terminated with exit code 0"))

	f.managementObjects = append(f.managementObjects, capacityTarget.DeepCopy())

	sadPodsStatuses := []shipper.PodStatus{
		{
			Condition: corev1.PodCondition{
				Type:   corev1.PodReady,
				Status: corev1.ConditionFalse,
				Reason: "ContainersNotReady",
			},
			Containers: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ContainerCreating",
						},
					},
				},
			},
			Name: "pod-a",
		},
		{
			Condition: corev1.PodCondition{
				Type:   corev1.PodReady,
				Status: corev1.ConditionFalse,
				Reason: "ContainersNotReady",
			},
			Containers: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason: "ContainerCreating",
						},
					},
				},
			},
			Name: "pod-b",
		},
		{
			Condition: corev1.PodCondition{
				Type:   corev1.PodReady,
				Status: corev1.ConditionFalse,
				Reason: "ContainersNotReady",
			},
			Containers: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason: "Completed",
						},
					},
				},
			},
			Name: "pod-c",
		},
	}

	sort.Slice(sadPodsStatuses, func(i, j int) bool {
		return sadPodsStatuses[i].Name < sadPodsStatuses[j].Name
	})

	for i := int32(0); i < numClus; i++ {
		capacityTarget.Status.Clusters = append(capacityTarget.Status.Clusters, shipper.ClusterCapacityStatus{
			Name:              fmt.Sprintf("cluster_%d", i),
			Reports:           []shipper.ClusterCapacityReport{*c.Build()},
			AchievedPercent:   100,
			AvailableReplicas: 3,
			Conditions: []shipper.ClusterCapacityCondition{
				{Type: shipper.ClusterConditionTypeReady, Status: corev1.ConditionFalse, Reason: PodsNotReady, Message: "there are 3 sad pods"},
			},
			SadPods: sadPodsStatuses,
		})
	}

	sort.Slice(capacityTarget.Status.Clusters[0].SadPods, func(i, j int) bool {
		return capacityTarget.Status.Clusters[0].SadPods[i].Name < capacityTarget.Status.Clusters[0].SadPods[j].Name
	})

	updateAction := kubetesting.NewUpdateAction(
		schema.GroupVersionResource{
			Group:    shipper.SchemeGroupVersion.Group,
			Version:  shipper.SchemeGroupVersion.Version,
			Resource: "capacitytargets",
		},
		capacityTarget.GetNamespace(),
		capacityTarget,
	)

	f.managementClusterActions = append(f.managementClusterActions, updateAction)
	f.runCapacityTargetSyncHandler()

	// Calling the sync handler again with the updated capacity target object should yield the same results.
	f.managementObjects = []runtime.Object{capacityTarget.DeepCopy()}
	f.managementClusterActions = []kubetesting.Action{}
	f.runCapacityTargetSyncHandler()
}

func TestUpdatingDeploymentsUpdatesTheCapacityTargetStatus(t *testing.T) {
	f := NewFixture(t)

	capacityTarget := newCapacityTarget(10, 50, numClus)
	f.managementObjects = append(f.managementObjects, capacityTarget.DeepCopy())

	deployment := newDeployment(5, 5)
	f.targetClusterObjects = append(f.targetClusterObjects, deployment)

	clusterConditions := []shipper.ClusterCapacityCondition{
		{
			Type:    shipper.ClusterConditionTypeReady,
			Status:  corev1.ConditionFalse,
			Reason:  WrongPodCount,
			Message: "expected 5 replicas but have 0",
		},
	}
	f.expectCapacityTargetStatusUpdate(capacityTarget, 5, 50, clusterConditions, []shipper.ClusterCapacityReport{*builder.NewReport("nginx").Build()})

	f.runCapacityTargetSyncHandler()
}

// TestSadPodsAreReflectedInCapacityTargetStatus tests a case where
// the deployment should have 5 available pods, but it has 4 happy
// pods and 1 sad pod.
func TestSadPodsAreReflectedInCapacityTargetStatus(t *testing.T) {
	f := NewFixture(t)

	capacityTarget := newCapacityTarget(2, 100, numClus)
	f.managementObjects = append(f.managementObjects, capacityTarget.DeepCopy())

	deployment := newDeployment(2, 1)
	happyPod := createHappyPodForDeployment(deployment)
	sadPod := createSadPodForDeployment(deployment)
	f.targetClusterObjects = append(f.targetClusterObjects, deployment, happyPod, sadPod)

	clusterConditions := []shipper.ClusterCapacityCondition{
		{
			Type:    shipper.ClusterConditionTypeReady,
			Status:  corev1.ConditionFalse,
			Reason:  PodsNotReady,
			Message: "there are 1 sad pods",
		},
	}

	c := builder.NewReport("nginx").
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(1, string(corev1.PodReady), string(corev1.ConditionFalse), "ExpectedFail")).
		AddPodConditionBreakdownBuilder(
			builder.NewPodConditionBreakdown(1, string(corev1.PodReady), string(corev1.ConditionTrue), ""))

	f.expectCapacityTargetStatusUpdate(capacityTarget, 1, 50, clusterConditions, []shipper.ClusterCapacityReport{*c.Build()}, createSadPodConditionFromPod(sadPod))

	f.runCapacityTargetSyncHandler()
}

func NewFixture(t *testing.T) *fixture {
	return &fixture{
		t: t,
	}
}

type fixture struct {
	t *testing.T

	targetClusterObjects []runtime.Object

	managementClientset       *shipperfake.Clientset
	managementInformerFactory shipperinformers.SharedInformerFactory
	managementObjects         []runtime.Object

	store             *shippertesting.FakeClusterClientStore
	clientsPerCluster map[string]*shippertesting.FakeCluster

	managementClusterActions []kubetesting.Action
}

func (f *fixture) initializeFixture() {
	f.managementClientset = shipperfake.NewSimpleClientset(f.managementObjects...)
	f.managementInformerFactory = shipperinformers.NewSharedInformerFactory(f.managementClientset, shippertesting.NoResyncPeriod)

	f.clientsPerCluster = make(map[string]*shippertesting.FakeCluster)
	for i := int32(0); i < numClus; i++ {
		clusterName := fmt.Sprintf("cluster_%d", i)
		f.clientsPerCluster[clusterName] = shippertesting.NewFakeCluster(
			kubefake.NewSimpleClientset(f.targetClusterObjects...), nil)
	}
	f.store = shippertesting.NewFakeClusterClientStore(f.clientsPerCluster)
}

func (f *fixture) newController() *Controller {
	controller := NewController(
		f.managementClientset,
		f.managementInformerFactory,
		f.store,
		record.NewFakeRecorder(10),
	)

	return controller
}

func (f *fixture) runInternal() *Controller {
	f.initializeFixture()

	controller := f.newController()

	stopCh := make(chan struct{})
	defer close(stopCh)

	f.store.Run(stopCh)

	f.managementInformerFactory.Start(stopCh)
	f.managementInformerFactory.WaitForCacheSync(stopCh)

	return controller
}

func (f *fixture) runCapacityTargetSyncHandler() {
	controller := f.runInternal()
	if err := controller.capacityTargetSyncHandler("reviewsapi/capacity-v0.0.1"); err != nil {
		f.t.Errorf("sync handler unexpectedly returned error: %v", err)
	}

	managementClusterActual := shippertesting.FilterActions(f.managementClientset.Actions())
	shippertesting.CheckActions(f.managementClusterActions, managementClusterActual, f.t)
}

func (f *fixture) checkDeploymentPatchWithReplicas(deployment *appsv1.Deployment, replicas int32) {
	patchAction := kubetesting.NewPatchSubresourceAction(
		schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		deployment.GetNamespace(),
		deployment.GetName(),
		types.StrategicMergePatchType,
		[]byte(fmt.Sprintf(`{"spec": {"replicas": %d}}`, replicas)),
	)

	for _, fakeCluster := range f.clientsPerCluster {
		actualActions := shippertesting.FilterActions(fakeCluster.Client.Actions())
		shippertesting.CheckActions([]kubetesting.Action{patchAction}, actualActions, f.t)
	}
}

func (f *fixture) expectCapacityTargetStatusUpdate(capacityTarget *shipper.CapacityTarget, availableReplicas, achievedPercent int32, clusterConditions []shipper.ClusterCapacityCondition, reports []shipper.ClusterCapacityReport, sadPods ...shipper.PodStatus) {

	for _, clusterSpec := range capacityTarget.Spec.Clusters {
		clusterStatus := shipper.ClusterCapacityStatus{
			Name:              clusterSpec.Name,
			AvailableReplicas: availableReplicas,
			AchievedPercent:   achievedPercent,
			Conditions:        clusterConditions,
			SadPods:           sadPods,
			Reports:           reports,
		}

		capacityTarget.Status.Clusters = append(capacityTarget.Status.Clusters, clusterStatus)
	}

	updateAction := kubetesting.NewUpdateAction(
		schema.GroupVersionResource{
			Group:    shipper.SchemeGroupVersion.Group,
			Version:  shipper.SchemeGroupVersion.Version,
			Resource: "capacitytargets",
		},
		capacityTarget.GetNamespace(),
		capacityTarget,
	)

	f.managementClusterActions = append(f.managementClusterActions, updateAction)
}

func newCapacityTarget(totalReplicaCount, percent int32, numClus int32) *shipper.CapacityTarget {
	name := "capacity-v0.0.1"
	namespace := "reviewsapi"

	clusters := make([]shipper.ClusterCapacityTarget, 0, numClus)

	for i := int32(0); i < numClus; i++ {
		clus := shipper.ClusterCapacityTarget{
			Name:              fmt.Sprintf("cluster_%d", i),
			Percent:           percent,
			TotalReplicaCount: totalReplicaCount,
		}
		clusters = append(clusters, clus)
	}

	metaLabels := map[string]string{
		shipper.ReleaseLabel: "0.0.1",
	}

	return &shipper.CapacityTarget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    metaLabels,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: shipper.SchemeGroupVersion.String(),
					Kind:       "Release",
					Name:       "0.0.1",
				},
			},
		},
		Spec: shipper.CapacityTargetSpec{
			Clusters: clusters,
		},
	}
}

func newDeployment(replicas int32, availableReplicas int32) *appsv1.Deployment {
	name := "nginx"
	namespace := "reviewsapi"
	status := appsv1.DeploymentStatus{
		AvailableReplicas: availableReplicas,
	}

	metaLabels := map[string]string{
		shipper.ReleaseLabel: "0.0.1",
	}

	specSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			shipper.ReleaseLabel: "0.0.1",
		},
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    metaLabels,
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: specSelector,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: metaLabels,
				},
			},
		},
		Status: status,
	}
}

func createSadPodForDeployment(deployment *appsv1.Deployment) *corev1.Pod {
	sadCondition := corev1.PodCondition{
		Type:    corev1.PodReady,
		Status:  corev1.ConditionFalse,
		Reason:  "ExpectedFail",
		Message: "This failure is meant to happen!",
	}

	status := corev1.PodStatus{
		Phase:      corev1.PodFailed,
		Conditions: []corev1.PodCondition{sadCondition},
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nginx-1a93Y2-sad",
			Namespace: "reviewsapi",
			Labels: map[string]string{
				shipper.ReleaseLabel: deployment.Labels[shipper.ReleaseLabel],
			},
		},
		Status: status,
	}
}

func createHappyPodForDeployment(deployment *appsv1.Deployment) *corev1.Pod {
	sadCondition := corev1.PodCondition{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	}

	status := corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{sadCondition},
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nginx-1a93Y2-happy",
			Namespace: "reviewsapi",
			Labels: map[string]string{
				shipper.ReleaseLabel: deployment.Labels[shipper.ReleaseLabel],
			},
		},
		Status: status,
	}
}

func createSadPodConditionFromPod(sadPod *corev1.Pod) shipper.PodStatus {
	return shipper.PodStatus{
		Name:      sadPod.Name,
		Condition: sadPod.Status.Conditions[0],
	}
}
