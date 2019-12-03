package release

import (
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"

	shipper "github.com/bookingcom/shipper/pkg/apis/shipper/v1alpha1"
	"github.com/bookingcom/shipper/pkg/controller"
	"github.com/bookingcom/shipper/pkg/util/conditions"
	releaseutil "github.com/bookingcom/shipper/pkg/util/release"
)

type StrategyExecutor struct {
	curr, succ   *releaseInfo
	recorder     record.EventRecorder
	hasIncumbent bool
}

func NewStrategyExecutor(curr, succ *releaseInfo, recorder record.EventRecorder, hasIncumbent bool) (*StrategyExecutor, error) {
	return &StrategyExecutor{
		curr:         curr,
		succ:         succ,
		recorder:     recorder,
		hasIncumbent: hasIncumbent,
	}, nil
}

type PipelineStep func(conditions.StrategyConditionsMap) (bool, []ExecutorResult, []ReleaseStrategyStateTransition, error)

const (
	PipelineBreak    = false
	PipelineContinue = true
)

func (e *StrategyExecutor) EnsureInstallation(cond conditions.StrategyConditionsMap) (bool, []ExecutorResult, []ReleaseStrategyStateTransition, error) {
	strategy := e.curr.release.Spec.Environment.Strategy
	targetStep := e.curr.release.Spec.TargetStep
	isLastStep := int(targetStep) == len(strategy.Steps)-1

	if ready, clusters := checkInstallation(e.curr); !ready {
		if len(e.curr.installationTarget.Spec.Clusters) != len(e.curr.installationTarget.Status.Clusters) {
			cond.SetUnknown(
				shipper.StrategyConditionContenderAchievedInstallation,
				conditions.StrategyConditionsUpdate{
					Step:               targetStep,
					LastTransitionTime: time.Now(),
				},
			)
		} else {
			cond.SetFalse(
				shipper.StrategyConditionContenderAchievedInstallation,
				conditions.StrategyConditionsUpdate{
					Reason:             ClustersNotReady,
					Message:            fmt.Sprintf("clusters pending installation: %v. for more details try `kubectl describe it %s`", clusters, e.curr.installationTarget.Name),
					Step:               targetStep,
					LastTransitionTime: time.Now(),
				},
			)
		}

		return PipelineBreak, []ExecutorResult{
			e.buildContenderStrategyConditionsPatch(cond, targetStep, isLastStep, e.hasIncumbent),
		}, nil, nil
	}

	cond.SetTrue(
		shipper.StrategyConditionContenderAchievedInstallation,
		conditions.StrategyConditionsUpdate{
			LastTransitionTime: time.Now(),
			Step:               targetStep,
		},
	)

	return PipelineContinue, nil, nil, nil
}

/*
	3. For the head release, ensure capacity.
	  3.1. Ensure the capacity corresponds to the strategy contender.
*/
func (e *StrategyExecutor) EnsureCapacity(cond conditions.StrategyConditionsMap) (bool, []ExecutorResult, []ReleaseStrategyStateTransition, error) {

	// If succ is nil, the weight is solely defined by the target step and the strategy
	if e.succ == nil {
		targetStep := e.curr.release.Spec.TargetStep
		strategy := e.curr.release.Spec.Environment.Strategy
		strategyStep := strategy.Steps[targetStep]
		capacityWeight := strategyStep.Capacity.Contender
		isLastStep := int(targetStep) == len(strategy.Steps)-1

		if achieved, newSpec, clustersNotReady := checkCapacity(e.curr.capacityTarget, uint(capacityWeight)); !achieved {
			e.info("contender %q hasn't achieved capacity yet", e.curr.release.Name)

			var patches []ExecutorResult

			cond.SetFalse(
				shipper.StrategyConditionContenderAchievedCapacity,
				conditions.StrategyConditionsUpdate{
					Reason:             ClustersNotReady,
					Message:            fmt.Sprintf("clusters pending capacity adjustments: %v. for more details, try `kubectl describe ct %s`", clustersNotReady, e.curr.capacityTarget.Name),
					Step:               targetStep,
					LastTransitionTime: time.Now(),
				})

			if newSpec != nil {
				patches = append(patches, &CapacityTargetOutdatedResult{
					NewSpec: newSpec,
					Name:    e.curr.release.Name,
				})
			}

			patches = append(patches, e.buildContenderStrategyConditionsPatch(cond, targetStep, isLastStep, e.hasIncumbent))

			return PipelineBreak, patches, nil, nil
		} else {
			e.info("contender %q has achieved capacity", e.curr.release.Name)

			cond.SetTrue(
				shipper.StrategyConditionContenderAchievedCapacity,
				conditions.StrategyConditionsUpdate{
					Step:               targetStep,
					LastTransitionTime: time.Now(),
				})
		}
	} else {
		targetStep := e.succ.release.Spec.TargetStep
		strategy := e.succ.release.Spec.Environment.Strategy
		strategyStep := strategy.Steps[targetStep]
		capacityWeight := strategyStep.Capacity.Incumbent
		isLastStep := int(targetStep) == len(strategy.Steps)-1

		if achieved, newSpec, clustersNotReady := checkCapacity(e.curr.capacityTarget, uint(capacityWeight)); !achieved {
			//s.info("incumbent %q hasn't achieved capacity yet", s.incumbent.release.Name)

			var patches []ExecutorResult

			cond.SetFalse(
				shipper.StrategyConditionIncumbentAchievedCapacity,
				conditions.StrategyConditionsUpdate{
					Reason:             ClustersNotReady,
					Message:            fmt.Sprintf("incumbent capacity is unhealthy in clusters: %v. for more details, try `kubectl describe ct %s`", clustersNotReady, e.curr.capacityTarget.Name),
					Step:               targetStep,
					LastTransitionTime: time.Now(),
				})

			if newSpec != nil {
				patches = append(patches, &CapacityTargetOutdatedResult{
					NewSpec: newSpec,
					Name:    e.curr.release.Name,
				})
			}

			patches = append(patches, e.buildContenderStrategyConditionsPatch(cond, targetStep, isLastStep, e.hasIncumbent))

			return PipelineBreak, patches, nil, nil
		} else {
			//s.info("incumbent %q has achieved capacity", s.incumbent.release.Name)

			cond.SetTrue(
				shipper.StrategyConditionIncumbentAchievedCapacity,
				conditions.StrategyConditionsUpdate{
					Step:               targetStep,
					LastTransitionTime: time.Now(),
				})
		}
	}

	return PipelineContinue, nil, nil, nil
}

func (e *StrategyExecutor) EnsureTraffic(cond conditions.StrategyConditionsMap) (bool, []ExecutorResult, []ReleaseStrategyStateTransition, error) {
	if e.succ == nil {
		targetStep := e.curr.release.Spec.TargetStep
		strategy := e.curr.release.Spec.Environment.Strategy
		strategyStep := strategy.Steps[targetStep]
		trafficWeight := strategyStep.Traffic.Contender
		isLastStep := int(targetStep) == len(strategy.Steps)-1

		if achieved, newSpec, clustersNotReady := checkTraffic(e.curr.trafficTarget, uint32(trafficWeight), contenderTrafficComparison); !achieved {
			//s.info("contender %q hasn't achieved traffic yet", s.contender.release.Name)

			var patches []ExecutorResult

			cond.SetFalse(
				shipper.StrategyConditionContenderAchievedTraffic,
				conditions.StrategyConditionsUpdate{
					Reason:             ClustersNotReady,
					Message:            fmt.Sprintf("clusters pending traffic adjustments: %v. for more details, try `kubectl describe tt %s`", clustersNotReady, e.curr.trafficTarget.Name),
					Step:               targetStep,
					LastTransitionTime: time.Now(),
				})

			if newSpec != nil {
				patches = append(patches, &TrafficTargetOutdatedResult{
					NewSpec: newSpec,
					Name:    e.curr.release.Name,
				})
			}

			patches = append(patches, e.buildContenderStrategyConditionsPatch(cond, targetStep, isLastStep, e.hasIncumbent))

			return PipelineBreak, patches, nil, nil
		} else {
			//s.info("contender %q has achieved traffic", s.contender.release.Name)

			cond.SetTrue(
				shipper.StrategyConditionContenderAchievedTraffic,
				conditions.StrategyConditionsUpdate{
					Step:               targetStep,
					LastTransitionTime: time.Now(),
				})
		}
	} else {
		targetStep := e.succ.release.Spec.TargetStep
		strategy := e.succ.release.Spec.Environment.Strategy
		strategyStep := strategy.Steps[targetStep]
		trafficWeight := strategyStep.Traffic.Incumbent
		isLastStep := int(targetStep) == len(strategy.Steps)-1

		if achieved, newSpec, clustersNotReady := checkTraffic(e.curr.trafficTarget, uint32(trafficWeight), incumbentTrafficComparison); !achieved {
			//s.info("incumbent %q hasn't achieved traffic yet", s.incumbent.release.Name)

			var patches []ExecutorResult

			cond.SetFalse(
				shipper.StrategyConditionIncumbentAchievedTraffic,
				conditions.StrategyConditionsUpdate{
					Reason:             ClustersNotReady,
					Message:            fmt.Sprintf("incumbent traffic is unhealthy in clusters: %v. for more details, try `kubectl describe tt %s`", clustersNotReady, e.curr.trafficTarget.Name),
					Step:               targetStep,
					LastTransitionTime: time.Now(),
				})

			if newSpec != nil {
				patches = append(patches, &TrafficTargetOutdatedResult{
					NewSpec: newSpec,
					Name:    e.curr.release.Name,
				})
			}

			patches = append(patches, e.buildContenderStrategyConditionsPatch(cond, targetStep, isLastStep, e.hasIncumbent))

			return PipelineBreak, patches, nil, nil
		} else {
			//s.info("incumbent %q has achieved traffic", s.incumbent.release.Name)

			cond.SetTrue(
				shipper.StrategyConditionIncumbentAchievedTraffic,
				conditions.StrategyConditionsUpdate{
					Step:               targetStep,
					LastTransitionTime: time.Now(),
				})
		}
	}
	return PipelineContinue, nil, nil, nil
}

func (e *StrategyExecutor) EnsureReleaseStrategyState(cond conditions.StrategyConditionsMap) (bool, []ExecutorResult, []ReleaseStrategyStateTransition, error) {
	var releasePatches []ExecutorResult
	var releaseStrategyStateTransitions []ReleaseStrategyStateTransition

	targetStep := e.curr.release.Spec.TargetStep
	strategy := e.curr.release.Spec.Environment.Strategy
	isLastStep := int(targetStep) == len(strategy.Steps)-1
	relStatus := e.curr.release.Status.DeepCopy()

	newReleaseStrategyState := cond.AsReleaseStrategyState(
		e.curr.release.Spec.TargetStep,
		e.hasIncumbent,
		isLastStep)

	oldReleaseStrategyState := shipper.ReleaseStrategyState{}
	if relStatus.Strategy != nil {
		oldReleaseStrategyState = relStatus.Strategy.State
	}

	sort.Slice(relStatus.Conditions, func(i, j int) bool {
		return relStatus.Conditions[i].Type < relStatus.Conditions[j].Type
	})

	releaseStrategyStateTransitions =
		getReleaseStrategyStateTransitions(
			oldReleaseStrategyState,
			newReleaseStrategyState,
			releaseStrategyStateTransitions)

	relStatus.Strategy = &shipper.ReleaseStrategyStatus{
		Conditions: cond.AsReleaseStrategyConditions(),
		State:      newReleaseStrategyState,
	}

	previouslyAchievedStep := e.curr.release.Status.AchievedStep
	if previouslyAchievedStep == nil || targetStep != previouslyAchievedStep.Step {
		// we validate that it fits in the len() of Strategy.Steps early in the process
		targetStepName := e.curr.release.Spec.Environment.Strategy.Steps[targetStep].Name
		relStatus.AchievedStep = &shipper.AchievedStep{
			Step: targetStep,
			Name: targetStepName,
		}
		e.event(e.curr.release, "step %d finished", targetStep)
	}

	if isLastStep {
		condition := releaseutil.NewReleaseCondition(shipper.ReleaseConditionTypeComplete, corev1.ConditionTrue, "", "")
		if diff := releaseutil.SetReleaseCondition(relStatus, *condition); !diff.IsEmpty() {
			e.recorder.Eventf(
				e.curr.release,
				corev1.EventTypeNormal,
				"ReleaseConditionChanged",
				diff.String())
		}
	}

	releasePatches = append(releasePatches, &ReleaseUpdateResult{
		NewStatus: relStatus,
		Name:      e.curr.release.Name,
	})

	return PipelineBreak, releasePatches, releaseStrategyStateTransitions, nil
}

/*
	For each release object:
	0. Ensure release scheduled.
	  0.1. Choose clusters.
	  0.2. Ensure target objects exist.
	    0.2.1. Compare chosen clusters and if different, update the spec.
	1. Find it's ancestor.
	2. For the head release, ensure installation.
	  2.1. Simply check installation targets.
	3. For the head release, ensure capacity.
	  3.1. Ensure the capacity corresponds to the strategy contender.
	4. For the head release, ensure traffic.
	  4.1. Ensure the traffic corresponds to the strategy contender.
	5. For a tail release, ensure traffic.
	  5.1. Look at the leader and check it's target traffic.
	  5.2. Look at the strategy and figure out the target traffic.
	6. For a tail release, ensure capacity.
	  6.1. Look at the leader and check it's target capacity.
	  6.2 Look at the strategy and figure out the target capacity.
	7. Make necessary adjustments to the release object.
*/

func (e *StrategyExecutor) Execute() ([]ExecutorResult, []ReleaseStrategyStateTransition, error) {
	var pipeline []PipelineStep
	var res []ExecutorResult
	var trans []ReleaseStrategyStateTransition

	var releaseStrategyConditions []shipper.ReleaseStrategyCondition
	if e.curr.release.Status.Strategy != nil {
		releaseStrategyConditions = e.curr.release.Status.Strategy.Conditions
	}
	cond := conditions.NewStrategyConditions(releaseStrategyConditions...)

	// The latest release must be installed first
	if e.succ == nil {
		pipeline = []PipelineStep{
			e.EnsureInstallation,
			e.EnsureCapacity,
			e.EnsureTraffic,
			e.EnsureReleaseStrategyState,
		}
	} else {
		pipeline = []PipelineStep{
			e.EnsureTraffic,
			e.EnsureCapacity,
			//e.EnsureReleaseStrategyState,
		}
	}

	for _, step := range pipeline {
		brk, stepres, steptrans, err := step(cond)
		if err != nil {
			return nil, nil, err
		}
		res = append(res, stepres...)
		trans = append(trans, steptrans...)
		if brk == PipelineBreak {
			break
		}
	}

	return res, trans, nil
}

func (e *StrategyExecutor) buildContenderStrategyConditionsPatch(
	cond conditions.StrategyConditionsMap,
	step int32,
	isLastStep bool,
	hasIncumbent bool,
) *ReleaseUpdateResult {
	newStatus := e.curr.release.Status.DeepCopy()
	newStatus.Strategy = &shipper.ReleaseStrategyStatus{
		Conditions: cond.AsReleaseStrategyConditions(),
		State:      cond.AsReleaseStrategyState(step, hasIncumbent, isLastStep),
	}
	return &ReleaseUpdateResult{
		NewStatus: newStatus,
		Name:      e.curr.release.Name,
	}
}

func (e *StrategyExecutor) info(format string, args ...interface{}) {
	klog.Infof("Release %q: %s", controller.MetaKey(e.curr.release), fmt.Sprintf(format, args...))
}

func (e *StrategyExecutor) event(obj runtime.Object, format string, args ...interface{}) {
	e.recorder.Eventf(
		obj,
		corev1.EventTypeNormal,
		"StrategyApplied",
		format,
		args,
	)
}
