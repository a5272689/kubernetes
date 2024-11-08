/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scheduler

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/parallelize"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/scheduler/util"
	utiltrace "k8s.io/utils/trace"
)

const (
	// Percentage of plugin metrics to be sampled.
	pluginMetricsSamplePercent = 10
	// minFeasibleNodesToFind is the minimum number of nodes that would be scored
	// in each scheduling cycle. This is a semi-arbitrary value to ensure that a
	// certain minimum of nodes are checked for feasibility. This in turn helps
	// ensure a minimum level of spreading.
	minFeasibleNodesToFind = 100
	// minFeasibleNodesPercentageToFind is the minimum percentage of nodes that
	// would be scored in each scheduling cycle. This is a semi-arbitrary value
	// to ensure that a certain minimum of nodes are checked for feasibility.
	// This in turn helps ensure a minimum level of spreading.
	minFeasibleNodesPercentageToFind = 5
	// numberOfHighestScoredNodesToReport is the number of node scores
	// to be included in ScheduleResult.
	numberOfHighestScoredNodesToReport = 3
)

// scheduleOne() 每次对一个 pod 进行调度，主要有以下步骤：

// 从 scheduler 调度队列中取出一个 pod，如果该 pod 处于删除状态则跳过
// 执行调度逻辑 sched.schedule() 返回通过预算及优选算法过滤后选出的最佳 node
// 如果过滤算法没有选出合适的 node，则返回 core.FitError
// 若没有合适的 node 会判断是否启用了抢占策略，若启用了则执行抢占机制
// 执行 reserve plugin
// pod 对应的 spec.NodeName 写上 scheduler 最终选择的 node，更新 scheduler cache
// 执行 permit plugin
// 执行 prebind plugin
// 进行绑定，请求 apiserver 异步处理最终的绑定操作，写入到 etcd
// 执行 postbind plugin
// ScheduleOne为单个pod完成整个调度工作流程。它在调度算法的主机适配上被序列化。
// ScheduleOne does the entire scheduling workflow for a single pod. It is serialized on the scheduling algorithm's host fitting.
func (sched *Scheduler) ScheduleOne(ctx context.Context) {
	logger := klog.FromContext(ctx)
	// podInfo就是从队列中获取到的Pod对象
	podInfo, err := sched.NextPod(logger)
	if err != nil {
		logger.Error(err, "Error while retrieving next pod from scheduling queue")
		return
	}
	// 检查pod的有效性，当 schedulerQueue 关闭时，pod 可能为nil
	// pod could be nil when schedulerQueue is closed
	if podInfo == nil || podInfo.Pod == nil {
		return
	}

	pod := podInfo.Pod
	// TODO(knelasevero): Remove duplicated keys from log entry calls
	// When contextualized logging hits GA
	// https://github.com/kubernetes/kubernetes/issues/111672
	logger = klog.LoggerWithValues(logger, "pod", klog.KObj(pod))
	ctx = klog.NewContext(ctx, logger)
	logger.V(4).Info("About to try and schedule pod", "pod", klog.KObj(pod))
	//根据定义的pod.Spec.SchedulerName查到对应的profile
	fwk, err := sched.frameworkForPod(pod)
	// 这不应该发生，因为我们只接受调度指定与配置文件之一匹配的调度程序名称的pod。
	if err != nil {
		//这不应该发生，因为我们只接受调度Pod
		//其指定与其中一个配置文件匹配的调度器名称。
		// This shouldn't happen, because we only accept for scheduling the pods
		// which specify a scheduler name that matches one of the profiles.
		logger.Error(err, "Error occurred")
		sched.SchedulingQueue.Done(pod.UID)
		return
	}
	// 可以跳过调度的情况，一般pod进不来
	if sched.skipPodSchedule(ctx, fwk, pod) {
		//我们不会把这个Pod放回队列，但我们必须清理飞行中的Pod/事件。
		// We don't put this Pod back to the queue, but we have to cleanup the in-flight pods/events.
		sched.SchedulingQueue.Done(pod.UID)
		return
	}

	logger.V(3).Info("Attempting to schedule pod", "pod", klog.KObj(pod))

	// 执行调度策略选择node
	//同时尝试为吊舱找到合适的位置。
	// Synchronously attempt to find a fit for the pod.
	start := time.Now()
	state := framework.NewCycleState()
	state.SetRecordPluginMetrics(rand.Intn(100) < pluginMetricsSamplePercent)

	//初始化一个空的podsToActivate结构，该结构将被插件填充或保持为空。
	// Initialize an empty podsToActivate struct, which will be filled up by plugins or stay empty.
	podsToActivate := framework.NewPodsToActivate()
	state.Write(framework.PodsToActivateKey, podsToActivate)

	schedulingCycleCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	scheduleResult, assumedPodInfo, status := sched.schedulingCycle(schedulingCycleCtx, state, fwk, podInfo, start, podsToActivate)
	// 出现调度失败的情况：
	// 这个时候可能会触发抢占preempt，抢占是一套复杂的逻辑,这里略去
	// 目前假设各类资源充足，能正常调度
	if !status.IsSuccess() {
		sched.FailureHandler(schedulingCycleCtx, fwk, assumedPodInfo, status, scheduleResult.nominatingInfo, start)
		return
	}

	//异步地将pod绑定到其主机（我们可以在上述假设步骤的b/c中执行此操作）。
	// bind the pod to its host asynchronously (we can do this b/c of the assumption step above).
	go func() {
		bindingCycleCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		metrics.Goroutines.WithLabelValues(metrics.Binding).Inc()
		defer metrics.Goroutines.WithLabelValues(metrics.Binding).Dec()

		status := sched.bindingCycle(bindingCycleCtx, state, fwk, scheduleResult, assumedPodInfo, start, podsToActivate)
		if !status.IsSuccess() {
			sched.handleBindingCycleError(bindingCycleCtx, state, fwk, assumedPodInfo, start, scheduleResult, status)
			return
		}
	}()
}

var clearNominatedNode = &framework.NominatingInfo{NominatingMode: framework.ModeOverride, NominatedNodeName: ""}

// schedulingCycle尝试调度单个Pod。
// schedulingCycle tries to schedule a single Pod.
func (sched *Scheduler) schedulingCycle(
	ctx context.Context,
	state *framework.CycleState,
	fwk framework.Framework,
	podInfo *framework.QueuedPodInfo,
	start time.Time,
	podsToActivate *framework.PodsToActivate,
) (ScheduleResult, *framework.QueuedPodInfo, *framework.Status) {
	logger := klog.FromContext(ctx)
	pod := podInfo.Pod
	// 调度pod
	scheduleResult, err := sched.SchedulePod(ctx, fwk, state, pod)
	if err != nil {
		defer func() {
			metrics.SchedulingAlgorithmLatency.Observe(metrics.SinceInSeconds(start))
		}()
		if err == ErrNoNodesAvailable {
			status := framework.NewStatus(framework.UnschedulableAndUnresolvable).WithError(err)
			return ScheduleResult{nominatingInfo: clearNominatedNode}, podInfo, status
		}

		fitError, ok := err.(*framework.FitError)
		if !ok {
			logger.Error(err, "Error selecting node for pod", "pod", klog.KObj(pod))
			return ScheduleResult{nominatingInfo: clearNominatedNode}, podInfo, framework.AsStatus(err)
		}

		//SchedulePod（）可能失败了，因为pod不适合任何主机，所以我们尝试
		//抢占，期望下次尝试调度pod时
		//由于优先购买权，将适合。也可能会安排不同的吊舱
		//进入被抢占的资源，但这是无害的。
		// SchedulePod() may have failed because the pod would not fit on any host, so we try to
		// preempt, with the expectation that the next time the pod is tried for scheduling it
		// will fit due to the preemption. It is also possible that a different pod will schedule
		// into the resources that were preempted, but this is harmless.

		if !fwk.HasPostFilterPlugins() {
			logger.V(3).Info("No PostFilter plugins are registered, so no preemption will be performed")
			return ScheduleResult{}, podInfo, framework.NewStatus(framework.Unschedulable).WithError(err)
		}

		//运行PostFilter插件，尝试使pod在未来的调度周期中可调度。
		// Run PostFilter plugins to attempt to make the pod schedulable in a future scheduling cycle.
		result, status := fwk.RunPostFilterPlugins(ctx, state, pod, fitError.Diagnosis.NodeToStatus)
		msg := status.Message()
		fitError.Diagnosis.PostFilterMsg = msg
		if status.Code() == framework.Error {
			logger.Error(nil, "Status after running PostFilter plugins for pod", "pod", klog.KObj(pod), "status", msg)
		} else {
			logger.V(5).Info("Status after running PostFilter plugins for pod", "pod", klog.KObj(pod), "status", msg)
		}

		var nominatingInfo *framework.NominatingInfo
		if result != nil {
			nominatingInfo = result.NominatingInfo
		}
		return ScheduleResult{nominatingInfo: nominatingInfo}, podInfo, framework.NewStatus(framework.Unschedulable).WithError(err)
	}

	metrics.SchedulingAlgorithmLatency.Observe(metrics.SinceInSeconds(start))
	//告诉缓存假设一个pod现在正在给定的节点上运行，即使它还没有被绑定。
	//这使我们能够继续调度，而无需等待绑定发生。
	// Tell the cache to assume that a pod now is running on a given node, even though it hasn't been bound yet.
	// This allows us to keep scheduling without waiting on binding to occur.
	assumedPodInfo := podInfo.DeepCopy()
	assumedPod := assumedPodInfo.Pod
	//take通过设置NodeName=scheduleResult来修改`assumedPod`。推荐主持人
	// assume modifies `assumedPod` by setting NodeName=scheduleResult.SuggestedHost
	err = sched.assume(logger, assumedPod, scheduleResult.SuggestedHost)
	if err != nil {
		//这很可能是重试逻辑时出现BUG的结果。
		//我们在这里报告了一个错误，以便可以重试pod调度。
		//这依赖于Error会检查pod是否已绑定的事实
		//如果是，则不会将其添加回未调度的Pod队列
		//（否则这将导致无限循环）。
		// This is most probably result of a BUG in retrying logic.
		// We report an error here so that pod scheduling can be retried.
		// This relies on the fact that Error will check if the pod has been bound
		// to a node and if so will not add it back to the unscheduled pods queue
		// (otherwise this would cause an infinite loop).
		return ScheduleResult{nominatingInfo: clearNominatedNode}, assumedPodInfo, framework.AsStatus(err)
	}
	//运行Reserve插件的Reserve方法。
	// Run the Reserve method of reserve plugins.
	if sts := fwk.RunReservePluginsReserve(ctx, state, assumedPod, scheduleResult.SuggestedHost); !sts.IsSuccess() {
		//触发取消保留以清理与保留Pod关联的状态
		// trigger un-reserve to clean up state associated with the reserved Pod
		fwk.RunReservePluginsUnreserve(ctx, state, assumedPod, scheduleResult.SuggestedHost)
		if forgetErr := sched.Cache.ForgetPod(logger, assumedPod); forgetErr != nil {
			logger.Error(forgetErr, "Scheduler cache ForgetPod failed")
		}

		if sts.IsRejected() {
			fitErr := &framework.FitError{
				NumAllNodes: 1,
				Pod:         pod,
				Diagnosis: framework.Diagnosis{
					NodeToStatus: framework.NewDefaultNodeToStatus(),
				},
			}
			fitErr.Diagnosis.NodeToStatus.Set(scheduleResult.SuggestedHost, sts)
			fitErr.Diagnosis.AddPluginStatus(sts)
			return ScheduleResult{nominatingInfo: clearNominatedNode}, assumedPodInfo, framework.NewStatus(sts.Code()).WithError(fitErr)
		}
		return ScheduleResult{nominatingInfo: clearNominatedNode}, assumedPodInfo, sts
	}

	//运行“permit”插件。
	// Run "permit" plugins.
	runPermitStatus := fwk.RunPermitPlugins(ctx, state, assumedPod, scheduleResult.SuggestedHost)
	if !runPermitStatus.IsWait() && !runPermitStatus.IsSuccess() {
		//触发取消保留以清理与保留Pod关联的状态
		// trigger un-reserve to clean up state associated with the reserved Pod
		fwk.RunReservePluginsUnreserve(ctx, state, assumedPod, scheduleResult.SuggestedHost)
		if forgetErr := sched.Cache.ForgetPod(logger, assumedPod); forgetErr != nil {
			logger.Error(forgetErr, "Scheduler cache ForgetPod failed")
		}

		if runPermitStatus.IsRejected() {
			fitErr := &framework.FitError{
				NumAllNodes: 1,
				Pod:         pod,
				Diagnosis: framework.Diagnosis{
					NodeToStatus: framework.NewDefaultNodeToStatus(),
				},
			}
			fitErr.Diagnosis.NodeToStatus.Set(scheduleResult.SuggestedHost, runPermitStatus)
			fitErr.Diagnosis.AddPluginStatus(runPermitStatus)
			return ScheduleResult{nominatingInfo: clearNominatedNode}, assumedPodInfo, framework.NewStatus(runPermitStatus.Code()).WithError(fitErr)
		}

		return ScheduleResult{nominatingInfo: clearNominatedNode}, assumedPodInfo, runPermitStatus
	}

	//在成功的调度周期结束时，如果需要，弹出并向上移动Pod。
	// At the end of a successful scheduling cycle, pop and move up Pods if needed.
	if len(podsToActivate.Map) != 0 {
		sched.SchedulingQueue.Activate(logger, podsToActivate.Map)
		//激活后清除条目。
		// Clear the entries after activation.
		podsToActivate.Map = make(map[string]*v1.Pod)
	}

	return scheduleResult, assumedPodInfo, nil
}

// bindingCycle尝试绑定一个假定的Pod。
// bindingCycle tries to bind an assumed Pod.
func (sched *Scheduler) bindingCycle(
	ctx context.Context,
	state *framework.CycleState,
	fwk framework.Framework,
	scheduleResult ScheduleResult,
	assumedPodInfo *framework.QueuedPodInfo,
	start time.Time,
	podsToActivate *framework.PodsToActivate) *framework.Status {
	logger := klog.FromContext(ctx)

	assumedPod := assumedPodInfo.Pod

	// Run "permit" plugins.
	if status := fwk.WaitOnPermit(ctx, assumedPod); !status.IsSuccess() {
		if status.IsRejected() {
			fitErr := &framework.FitError{
				NumAllNodes: 1,
				Pod:         assumedPodInfo.Pod,
				Diagnosis: framework.Diagnosis{
					NodeToStatus:         framework.NewDefaultNodeToStatus(),
					UnschedulablePlugins: sets.New(status.Plugin()),
				},
			}
			fitErr.Diagnosis.NodeToStatus.Set(scheduleResult.SuggestedHost, status)
			return framework.NewStatus(status.Code()).WithError(fitErr)
		}
		return status
	}

	// Run "prebind" plugins.
	if status := fwk.RunPreBindPlugins(ctx, state, assumedPod, scheduleResult.SuggestedHost); !status.IsSuccess() {
		if status.IsRejected() {
			fitErr := &framework.FitError{
				NumAllNodes: 1,
				Pod:         assumedPodInfo.Pod,
				Diagnosis: framework.Diagnosis{
					NodeToStatus:         framework.NewDefaultNodeToStatus(),
					UnschedulablePlugins: sets.New(status.Plugin()),
				},
			}
			fitErr.Diagnosis.NodeToStatus.Set(scheduleResult.SuggestedHost, status)
			return framework.NewStatus(status.Code()).WithError(fitErr)
		}
		return status
	}

	// Any failures after this point cannot lead to the Pod being considered unschedulable.
	// We define the Pod as "unschedulable" only when Pods are rejected at specific extension points, and PreBind is the last one in the scheduling/binding cycle.
	//
	// We can call Done() here because
	// we can free the cluster events stored in the scheduling queue sonner, which is worth for busy clusters memory consumption wise.
	sched.SchedulingQueue.Done(assumedPod.UID)

	// Run "bind" plugins.
	if status := sched.bind(ctx, fwk, assumedPod, scheduleResult.SuggestedHost, state); !status.IsSuccess() {
		return status
	}

	// Calculating nodeResourceString can be heavy. Avoid it if klog verbosity is below 2.
	logger.V(2).Info("Successfully bound pod to node", "pod", klog.KObj(assumedPod), "node", scheduleResult.SuggestedHost, "evaluatedNodes", scheduleResult.EvaluatedNodes, "feasibleNodes", scheduleResult.FeasibleNodes)
	metrics.PodScheduled(fwk.ProfileName(), metrics.SinceInSeconds(start))
	metrics.PodSchedulingAttempts.Observe(float64(assumedPodInfo.Attempts))
	if assumedPodInfo.InitialAttemptTimestamp != nil {
		metrics.PodSchedulingDuration.WithLabelValues(getAttemptsLabel(assumedPodInfo)).Observe(metrics.SinceInSeconds(*assumedPodInfo.InitialAttemptTimestamp))
		metrics.PodSchedulingSLIDuration.WithLabelValues(getAttemptsLabel(assumedPodInfo)).Observe(metrics.SinceInSeconds(*assumedPodInfo.InitialAttemptTimestamp))
	}
	// Run "postbind" plugins.
	fwk.RunPostBindPlugins(ctx, state, assumedPod, scheduleResult.SuggestedHost)

	// At the end of a successful binding cycle, move up Pods if needed.
	if len(podsToActivate.Map) != 0 {
		sched.SchedulingQueue.Activate(logger, podsToActivate.Map)
		// Unlike the logic in schedulingCycle(), we don't bother deleting the entries
		// as `podsToActivate.Map` is no longer consumed.
	}

	return nil
}

func (sched *Scheduler) handleBindingCycleError(
	ctx context.Context,
	state *framework.CycleState,
	fwk framework.Framework,
	podInfo *framework.QueuedPodInfo,
	start time.Time,
	scheduleResult ScheduleResult,
	status *framework.Status) {
	logger := klog.FromContext(ctx)

	assumedPod := podInfo.Pod
	// trigger un-reserve plugins to clean up state associated with the reserved Pod
	fwk.RunReservePluginsUnreserve(ctx, state, assumedPod, scheduleResult.SuggestedHost)
	if forgetErr := sched.Cache.ForgetPod(logger, assumedPod); forgetErr != nil {
		logger.Error(forgetErr, "scheduler cache ForgetPod failed")
	} else {
		// "Forget"ing an assumed Pod in binding cycle should be treated as a PodDelete event,
		// as the assumed Pod had occupied a certain amount of resources in scheduler cache.
		//
		// Avoid moving the assumed Pod itself as it's always Unschedulable.
		// It's intentional to "defer" this operation; otherwise MoveAllToActiveOrBackoffQueue() would
		// add this event to in-flight events and thus move the assumed pod to backoffQ anyways if the plugins don't have appropriate QueueingHint.
		if status.IsRejected() {
			defer sched.SchedulingQueue.MoveAllToActiveOrBackoffQueue(logger, framework.EventAssignedPodDelete, assumedPod, nil, func(pod *v1.Pod) bool {
				return assumedPod.UID != pod.UID
			})
		} else {
			sched.SchedulingQueue.MoveAllToActiveOrBackoffQueue(logger, framework.EventAssignedPodDelete, assumedPod, nil, nil)
		}
	}

	sched.FailureHandler(ctx, fwk, podInfo, status, clearNominatedNode, start)
}

func (sched *Scheduler) frameworkForPod(pod *v1.Pod) (framework.Framework, error) {
	fwk, ok := sched.Profiles[pod.Spec.SchedulerName]
	if !ok {
		return nil, fmt.Errorf("profile not found for scheduler name %q", pod.Spec.SchedulerName)
	}
	return fwk, nil
}

// skipPodSchedule returns true if we could skip scheduling the pod for specified cases.
func (sched *Scheduler) skipPodSchedule(ctx context.Context, fwk framework.Framework, pod *v1.Pod) bool {
	// Case 1: pod is being deleted.
	if pod.DeletionTimestamp != nil {
		fwk.EventRecorder().Eventf(pod, nil, v1.EventTypeWarning, "FailedScheduling", "Scheduling", "skip schedule deleting pod: %v/%v", pod.Namespace, pod.Name)
		klog.FromContext(ctx).V(3).Info("Skip schedule deleting pod", "pod", klog.KObj(pod))
		return true
	}

	// Case 2: pod that has been assumed could be skipped.
	// An assumed pod can be added again to the scheduling queue if it got an update event
	// during its previous scheduling cycle but before getting assumed.
	isAssumed, err := sched.Cache.IsAssumedPod(pod)
	if err != nil {
		// TODO(91633): pass ctx into a revised HandleError
		utilruntime.HandleError(fmt.Errorf("failed to check whether pod %s/%s is assumed: %v", pod.Namespace, pod.Name, err))
		return false
	}
	return isAssumed
}

// schedulePod尝试将给定的pod调度到节点列表中的一个节点。
// 如果成功，它将返回节点的名称。
// 如果失败，它将返回FitError并说明原因。
// schedulePod tries to schedule the given pod to one of the nodes in the node list.
// If it succeeds, it will return the name of the node.
// If it fails, it will return a FitError with reasons.
func (sched *Scheduler) schedulePod(ctx context.Context, fwk framework.Framework, state *framework.CycleState, pod *v1.Pod) (result ScheduleResult, err error) {
	trace := utiltrace.New("Scheduling", utiltrace.Field{Key: "namespace", Value: pod.Namespace}, utiltrace.Field{Key: "name", Value: pod.Name})
	defer trace.LogIfLong(100 * time.Millisecond)
	// 1.快照 node 信息，每次调度 pod 时都会获取一次快照
	if err := sched.Cache.UpdateSnapshot(klog.FromContext(ctx), sched.nodeInfoSnapshot); err != nil {
		return result, err
	}
	trace.Step("Snapshotting scheduler cache and node infos done")

	if sched.nodeInfoSnapshot.NumNodes() == 0 {
		return result, ErrNoNodesAvailable
	}

	// 2.Predict阶段：找到所有满足调度条件的节点feasibleNodes，不满足的就直接过滤
	feasibleNodes, diagnosis, err := sched.findNodesThatFitPod(ctx, fwk, state, pod)
	if err != nil {
		return result, err
	}
	trace.Step("Computing predicates done")

	// 3.预选后没有合适的 node 直接返回
	if len(feasibleNodes) == 0 {
		return result, &framework.FitError{
			Pod:         pod,
			NumAllNodes: sched.nodeInfoSnapshot.NumNodes(),
			Diagnosis:   diagnosis,
		}
	}

	// 4.当预选之后只剩下一个node，就使用它
	// When only one node after predicate, just use it.
	if len(feasibleNodes) == 1 {
		return ScheduleResult{
			SuggestedHost:  feasibleNodes[0].Node().Name,
			EvaluatedNodes: 1 + diagnosis.NodeToStatus.Len(),
			FeasibleNodes:  1,
		}, nil
	}

	// 5.Priority阶段：执行优选算法，获取打分之后的node列表
	priorityList, err := prioritizeNodes(ctx, sched.Extenders, fwk, state, pod, feasibleNodes)
	if err != nil {
		return result, err
	}

	// 6.根据打分选择分数最高的node
	host, _, err := selectHost(priorityList, numberOfHighestScoredNodesToReport)
	trace.Step("Prioritizing done")

	return ScheduleResult{
		SuggestedHost:  host,
		EvaluatedNodes: len(feasibleNodes) + diagnosis.NodeToStatus.Len(),
		FeasibleNodes:  len(feasibleNodes),
	}, err
}

// 预选
// 根据框架过滤节点以找到适合pod的节点
// 过滤器插件和过滤器扩展器。
// Filters the nodes to find the ones that fit the pod based on the framework
// filter plugins and filter extenders.
func (sched *Scheduler) findNodesThatFitPod(ctx context.Context, fwk framework.Framework, state *framework.CycleState, pod *v1.Pod) ([]*framework.NodeInfo, framework.Diagnosis, error) {
	logger := klog.FromContext(ctx)
	diagnosis := framework.Diagnosis{
		NodeToStatus: framework.NewDefaultNodeToStatus(),
	}

	allNodes, err := sched.nodeInfoSnapshot.NodeInfos().List()
	if err != nil {
		return nil, diagnosis, err
	}
	//运行“预过滤器”插件。
	// Run "prefilter" plugins.
	preRes, s, unscheduledPlugins := fwk.RunPreFilterPlugins(ctx, state, pod)
	diagnosis.UnschedulablePlugins = unscheduledPlugins
	if !s.IsSuccess() {
		if !s.IsRejected() {
			return nil, diagnosis, s.AsError()
		}
		//NodeToStatus中的所有节点将具有相同的状态，以便在抢占中处理它们。
		// All nodes in NodeToStatus will have the same status so that they can be handled in the preemption.
		diagnosis.NodeToStatus.SetAbsentNodesStatus(s)

		//在诊断中记录来自预过滤器的消息。预过滤器消息。
		// Record the messages from PreFilter in Diagnosis.PreFilterMsg.
		msg := s.Message()
		diagnosis.PreFilterMsg = msg
		logger.V(5).Info("Status after running PreFilter plugins for pod", "pod", klog.KObj(pod), "status", msg)
		diagnosis.AddPluginStatus(s)
		return nil, diagnosis, nil
	}

	//由于抢占，“NominatedNodeName”可能会在之前的调度周期中设置。
	//此节点可能是唯一适合pod的候选节点，因此我们在迭代所有节点之前先尝试它。
	// "NominatedNodeName" can potentially be set in a previous scheduling cycle as a result of preemption.
	// This node is likely the only candidate that will fit the pod, and hence we try it first before iterating over all nodes.
	if len(pod.Status.NominatedNodeName) > 0 {
		feasibleNodes, err := sched.evaluateNominatedNode(ctx, pod, fwk, state, diagnosis)
		if err != nil {
			logger.Error(err, "Evaluation failed on nominated node", "pod", klog.KObj(pod), "node", pod.Status.NominatedNodeName)
		}
		//指定节点通过所有过滤器，调度器可以将此节点分配给pod。
		// Nominated node passes all the filters, scheduler is good to assign this node to the pod.
		if len(feasibleNodes) != 0 {
			return feasibleNodes, diagnosis, nil
		}
	}

	nodes := allNodes
	if !preRes.AllNodes() {
		nodes = make([]*framework.NodeInfo, 0, len(preRes.NodeNames))
		for nodeName := range preRes.NodeNames {
			//PreRes可能会返回不存在的节点名；我们验证
			//节点存在于快照中。
			// PreRes may return nodeName(s) which do not exist; we verify
			// node exists in the Snapshot.
			if nodeInfo, err := sched.nodeInfoSnapshot.Get(nodeName); err == nil {
				nodes = append(nodes, nodeInfo)
			}
		}
		diagnosis.NodeToStatus.SetAbsentNodesStatus(framework.NewStatus(framework.UnschedulableAndUnresolvable, fmt.Sprintf("node(s) didn't satisfy plugin(s) %v", sets.List(unscheduledPlugins))))
	}
	feasibleNodes, err := sched.findNodesThatPassFilters(ctx, fwk, state, pod, &diagnosis, nodes)
	//始终尝试更新sched.nextStartNodeIndex，无论是否发生错误
	//这有助于确保所有节点都有机会被搜索
	// always try to update the sched.nextStartNodeIndex regardless of whether an error has occurred
	// this is helpful to make sure that all the nodes have a chance to be searched
	processedNodes := len(feasibleNodes) + diagnosis.NodeToStatus.Len()
	sched.nextStartNodeIndex = (sched.nextStartNodeIndex + processedNodes) % len(allNodes)
	if err != nil {
		return nil, diagnosis, err
	}

	feasibleNodesAfterExtender, err := findNodesThatPassExtenders(ctx, sched.Extenders, pod, feasibleNodes, diagnosis.NodeToStatus)
	if err != nil {
		return nil, diagnosis, err
	}
	if len(feasibleNodesAfterExtender) != len(feasibleNodes) {
		//扩展器过滤掉了一些节点。
		//
		//扩展器不支持调度框架中的任何类型的重新排队功能，如EnqueueExtensions。
		//当扩展器拒绝某些节点并且吊舱最终无法调度时，
		//我们将framework.ExtenderName设置为pInfo。非计划插件。
		//此Pod将从不可调度的Pod池重新排队到activeQ/backoffQ
		//任何类型的集群事件。
		// https://github.com/kubernetes/kubernetes/issues/122019
		// Extenders filtered out some nodes.
		//
		// Extender doesn't support any kind of requeueing feature like EnqueueExtensions in the scheduling framework.
		// When Extenders reject some Nodes and the pod ends up being unschedulable,
		// we put framework.ExtenderName to pInfo.UnschedulablePlugins.
		// This Pod will be requeued from unschedulable pod pool to activeQ/backoffQ
		// by any kind of cluster events.
		// https://github.com/kubernetes/kubernetes/issues/122019
		if diagnosis.UnschedulablePlugins == nil {
			diagnosis.UnschedulablePlugins = sets.New[string]()
		}
		diagnosis.UnschedulablePlugins.Insert(framework.ExtenderName)
	}

	return feasibleNodesAfterExtender, diagnosis, nil
}

func (sched *Scheduler) evaluateNominatedNode(ctx context.Context, pod *v1.Pod, fwk framework.Framework, state *framework.CycleState, diagnosis framework.Diagnosis) ([]*framework.NodeInfo, error) {
	nnn := pod.Status.NominatedNodeName
	nodeInfo, err := sched.nodeInfoSnapshot.Get(nnn)
	if err != nil {
		return nil, err
	}
	node := []*framework.NodeInfo{nodeInfo}
	feasibleNodes, err := sched.findNodesThatPassFilters(ctx, fwk, state, pod, &diagnosis, node)
	if err != nil {
		return nil, err
	}

	feasibleNodes, err = findNodesThatPassExtenders(ctx, sched.Extenders, pod, feasibleNodes, diagnosis.NodeToStatus)
	if err != nil {
		return nil, err
	}

	return feasibleNodes, nil
}

// hasScoring检查是否配置了评分节点。
// hasScoring checks if scoring nodes is configured.
func (sched *Scheduler) hasScoring(fwk framework.Framework) bool {
	if fwk.HasScorePlugins() {
		return true
	}
	for _, extender := range sched.Extenders {
		if extender.IsPrioritizer() {
			return true
		}
	}
	return false
}

// hasExtenderFilters检查是否有任何扩展器过滤节点。
// hasExtenderFilters checks if any extenders filter nodes.
func (sched *Scheduler) hasExtenderFilters() bool {
	for _, extender := range sched.Extenders {
		if extender.IsFilter() {
			return true
		}
	}
	return false
}

// findNodesThatPassFilters查找适合过滤器插件的节点。
// findNodesThatPassFilters finds the nodes that fit the filter plugins.
func (sched *Scheduler) findNodesThatPassFilters(
	ctx context.Context,
	fwk framework.Framework,
	state *framework.CycleState,
	pod *v1.Pod,
	diagnosis *framework.Diagnosis,
	nodes []*framework.NodeInfo) ([]*framework.NodeInfo, error) {
	numAllNodes := len(nodes)
	numNodesToFind := sched.numFeasibleNodesToFind(fwk.PercentageOfNodesToScore(), int32(numAllNodes))
	if !sched.hasExtenderFilters() && !sched.hasScoring(fwk) {
		numNodesToFind = 1
	}

	//创建具有足够空间的可行列表，以避免其增长
	//并允许分配。
	// Create feasible list with enough space to avoid growing it
	// and allow assigning.
	feasibleNodes := make([]*framework.NodeInfo, numNodesToFind)

	if !fwk.HasFilterPlugins() {
		for i := range feasibleNodes {
			feasibleNodes[i] = nodes[(sched.nextStartNodeIndex+i)%numAllNodes]
		}
		return feasibleNodes, nil
	}

	errCh := parallelize.NewErrorChannel()
	var feasibleNodesLen int32
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type nodeStatus struct {
		node   string
		status *framework.Status
	}
	result := make([]*nodeStatus, numAllNodes)
	checkNode := func(i int) {
		//我们从上一个调度周期中停止的地方开始检查节点，
		//这是为了确保所有节点在pod中都有相同的机会被检查。
		// We check the nodes starting from where we left off in the previous scheduling cycle,
		// this is to make sure all nodes have the same chance of being examined across pods.
		nodeInfo := nodes[(sched.nextStartNodeIndex+i)%numAllNodes]
		status := fwk.RunFilterPluginsWithNominatedPods(ctx, state, pod, nodeInfo)
		if status.Code() == framework.Error {
			errCh.SendErrorWithCancel(status.AsError(), cancel)
			return
		}
		if status.IsSuccess() {
			length := atomic.AddInt32(&feasibleNodesLen, 1)
			if length > numNodesToFind {
				cancel()
				atomic.AddInt32(&feasibleNodesLen, -1)
			} else {
				feasibleNodes[length-1] = nodeInfo
			}
		} else {
			result[i] = &nodeStatus{node: nodeInfo.Node().Name, status: status}
		}
	}

	beginCheckNode := time.Now()
	statusCode := framework.Success
	defer func() {
		//我们在这里而不是在framework.go中记录过滤器扩展点延迟，因为framework.RunFilterPlugins
		//函数是为每个节点调用的，而我们希望每个调度周期的所有节点都有一个总体延迟。
		//请注意，此延迟还包括“addNominatedPods”的延迟，它调用framework.RunPreFilterAddPod。
		// We record Filter extension point latency here instead of in framework.go because framework.RunFilterPlugins
		// function is called for each node, whereas we want to have an overall latency for all nodes per scheduling cycle.
		// Note that this latency also includes latency for `addNominatedPods`, which calls framework.RunPreFilterAddPod.
		metrics.FrameworkExtensionPointDuration.WithLabelValues(metrics.Filter, statusCode.String(), fwk.ProfileName()).Observe(metrics.SinceInSeconds(beginCheckNode))
	}()

	//一旦配置了可行节点的数量，就停止搜索更多节点
	//被发现。
	// Stops searching for more nodes once the configured number of feasible nodes
	// are found.
	fwk.Parallelizer().Until(ctx, numAllNodes, checkNode, metrics.Filter)
	feasibleNodes = feasibleNodes[:feasibleNodesLen]
	for _, item := range result {
		if item == nil {
			continue
		}
		diagnosis.NodeToStatus.Set(item.node, item.status)
		diagnosis.AddPluginStatus(item.status)
	}
	if err := errCh.ReceiveError(); err != nil {
		statusCode = framework.Error
		return feasibleNodes, err
	}
	return feasibleNodes, nil
}

// numFeasibleNodesToFind返回可行节点的数量，一旦找到，调度器就会停止
// 它搜索更可行的节点。
// numFeasibleNodesToFind returns the number of feasible nodes that once found, the scheduler stops
// its search for more feasible nodes.
func (sched *Scheduler) numFeasibleNodesToFind(percentageOfNodesToScore *int32, numAllNodes int32) (numNodes int32) {
	if numAllNodes < minFeasibleNodesToFind {
		return numAllNodes
	}

	//如果已设置，请使用NodesToScore的配置文件百分比。否则，请使用全局百分比OfNodesToScore。
	// Use profile percentageOfNodesToScore if it's set. Otherwise, use global percentageOfNodesToScore.
	var percentage int32
	if percentageOfNodesToScore != nil {
		percentage = *percentageOfNodesToScore
	} else {
		percentage = sched.percentageOfNodesToScore
	}

	if percentage == 0 {
		percentage = int32(50) - numAllNodes/125
		if percentage < minFeasibleNodesPercentageToFind {
			percentage = minFeasibleNodesPercentageToFind
		}
	}

	numNodes = numAllNodes * percentage / 100
	if numNodes < minFeasibleNodesToFind {
		return minFeasibleNodesToFind
	}

	return numNodes
}

func findNodesThatPassExtenders(ctx context.Context, extenders []framework.Extender, pod *v1.Pod, feasibleNodes []*framework.NodeInfo, statuses *framework.NodeToStatus) ([]*framework.NodeInfo, error) {
	logger := klog.FromContext(ctx)

	//扩展器按顺序调用。
	//原始可行节点中的节点可以在一个扩展器中排除，并传递给下一个扩展器
	//以递减的方式延长。
	// Extenders are called sequentially.
	// Nodes in original feasibleNodes can be excluded in one extender, and pass on to the next
	// extender in a decreasing manner.
	for _, extender := range extenders {
		if len(feasibleNodes) == 0 {
			break
		}
		if !extender.IsInterested(pod) {
			continue
		}

		//failedAndUnsolvableMap中故障节点的状态将添加到<states>中，
		//以便调度器框架能够尊重以下各项的UnscheduleandUnsolvable状态
		//这最终可以提高抢占效率。
		//注意：建议用户配置可能返回UnscheduleandUnsolvable的扩展器
		//地位领先于他人。
		// Status of failed nodes in failedAndUnresolvableMap will be added to <statuses>,
		// so that the scheduler framework can respect the UnschedulableAndUnresolvable status for
		// particular nodes, and this may eventually improve preemption efficiency.
		// Note: users are recommended to configure the extenders that may return UnschedulableAndUnresolvable
		// status ahead of others.
		feasibleList, failedMap, failedAndUnresolvableMap, err := extender.Filter(pod, feasibleNodes)
		if err != nil {
			if extender.IsIgnorable() {
				logger.Info("Skipping extender as it returned error and has ignorable flag set", "extender", extender, "err", err)
				continue
			}
			return nil, err
		}

		for failedNodeName, failedMsg := range failedAndUnresolvableMap {
			statuses.Set(failedNodeName, framework.NewStatus(framework.UnschedulableAndUnresolvable, failedMsg))
		}

		for failedNodeName, failedMsg := range failedMap {
			if _, found := failedAndUnresolvableMap[failedNodeName]; found {
				//failedAndUnsolvableMap优先于failedMap
				//请注意，只有当扩展器在两个映射中都返回节点时，才会发生这种情况
				// failedAndUnresolvableMap takes precedence over failedMap
				// note that this only happens if the extender returns the node in both maps
				continue
			}
			statuses.Set(failedNodeName, framework.NewStatus(framework.Unschedulable, failedMsg))
		}

		feasibleNodes = feasibleList
	}
	return feasibleNodes, nil
}

// prioritizeNodes通过运行分数插件对节点进行优先级排序，
// 其从调用RunScorePlugins（）返回每个节点的分数。
// 将每个插件的分数加在一起，得出该节点的分数，然后
// 任何扩展器也会运行。
// 最后将所有分数相加，得到所有节点的加权总分
// prioritizeNodes prioritizes the nodes by running the score plugins,
// which return a score for each node from the call to RunScorePlugins().
// The scores from each plugin are added together to make the score for that node, then
// any extenders are run as well.
// All scores are finally combined (added) to get the total weighted scores of all nodes
func prioritizeNodes(
	ctx context.Context,
	extenders []framework.Extender,
	fwk framework.Framework,
	state *framework.CycleState,
	pod *v1.Pod,
	nodes []*framework.NodeInfo,
) ([]framework.NodePluginScores, error) {
	logger := klog.FromContext(ctx)
	//如果没有提供优先级配置，则所有节点的得分都为1。
	//这是以所需格式生成优先级列表所必需的
	// If no priority configs are provided, then all nodes will have a score of one.
	// This is required to generate the priority list in the required format
	if len(extenders) == 0 && !fwk.HasScorePlugins() {
		result := make([]framework.NodePluginScores, 0, len(nodes))
		for i := range nodes {
			result = append(result, framework.NodePluginScores{
				Name:       nodes[i].Node().Name,
				TotalScore: 1,
			})
		}
		return result, nil
	}

	//运行PreScore插件。
	// Run PreScore plugins.
	preScoreStatus := fwk.RunPreScorePlugins(ctx, state, pod, nodes)
	if !preScoreStatus.IsSuccess() {
		return nil, preScoreStatus.AsError()
	}

	//运行Score插件。
	// Run the Score plugins.
	nodesScores, scoreStatus := fwk.RunScorePlugins(ctx, state, pod, nodes)
	if !scoreStatus.IsSuccess() {
		return nil, scoreStatus.AsError()
	}

	//如果启用，则在级别10记录其他详细信息。
	// Additional details logged at level 10 if enabled.
	loggerVTen := logger.V(10)
	if loggerVTen.Enabled() {
		for _, nodeScore := range nodesScores {
			for _, pluginScore := range nodeScore.Scores {
				loggerVTen.Info("Plugin scored node for pod", "pod", klog.KObj(pod), "plugin", pluginScore.Name, "node", nodeScore.Name, "score", pluginScore.Score)
			}
		}
	}

	if len(extenders) != 0 && nodes != nil {
		//allNodeExtendersScores具有所有节点的所有扩展器得分。
		//它以节点名称为关键字。
		// allNodeExtendersScores has all extenders scores for all nodes.
		// It is keyed with node name.
		allNodeExtendersScores := make(map[string]*framework.NodePluginScores, len(nodes))
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := range extenders {
			if !extenders[i].IsInterested(pod) {
				continue
			}
			wg.Add(1)
			go func(extIndex int) {
				metrics.Goroutines.WithLabelValues(metrics.PrioritizingExtender).Inc()
				defer func() {
					metrics.Goroutines.WithLabelValues(metrics.PrioritizingExtender).Dec()
					wg.Done()
				}()
				prioritizedList, weight, err := extenders[extIndex].Prioritize(pod, nodes)
				if err != nil {
					//扩展器的优先级错误可以忽略，让k8s/其他扩展器确定优先级
					// Prioritization errors from extender can be ignored, let k8s/other extenders determine the priorities
					logger.V(5).Info("Failed to run extender's priority function. No score given by this extender.", "error", err, "pod", klog.KObj(pod), "extender", extenders[extIndex].Name())
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for i := range *prioritizedList {
					nodename := (*prioritizedList)[i].Host
					score := (*prioritizedList)[i].Score
					if loggerVTen.Enabled() {
						loggerVTen.Info("Extender scored node for pod", "pod", klog.KObj(pod), "extender", extenders[extIndex].Name(), "node", nodename, "score", score)
					}

					//MaxExtenderPriority可能与调度器中使用的由MaxNodeScore定义的最大优先级不同，
					//因此，我们需要将扩展器返回的分数缩放到调度器使用的分数范围内。
					// MaxExtenderPriority may diverge from the max priority used in the scheduler and defined by MaxNodeScore,
					// therefore we need to scale the score returned by extenders to the score range used by the scheduler.
					finalscore := score * weight * (framework.MaxNodeScore / extenderv1.MaxExtenderPriority)

					if allNodeExtendersScores[nodename] == nil {
						allNodeExtendersScores[nodename] = &framework.NodePluginScores{
							Name:   nodename,
							Scores: make([]framework.PluginScore, 0, len(extenders)),
						}
					}
					allNodeExtendersScores[nodename].Scores = append(allNodeExtendersScores[nodename].Scores, framework.PluginScore{
						Name:  extenders[extIndex].Name(),
						Score: finalscore,
					})
					allNodeExtendersScores[nodename].TotalScore += finalscore
				}
			}(i)
		}

		//等待所有围棋套路结束
		// wait for all go routines to finish
		wg.Wait()
		for i := range nodesScores {
			if score, ok := allNodeExtendersScores[nodes[i].Node().Name]; ok {
				nodesScores[i].Scores = append(nodesScores[i].Scores, score.Scores...)
				nodesScores[i].TotalScore += score.TotalScore
			}
		}
	}

	if loggerVTen.Enabled() {
		for i := range nodesScores {
			loggerVTen.Info("Calculated node's final score for pod", "pod", klog.KObj(pod), "node", nodesScores[i].Name, "score", nodesScores[i].TotalScore)
		}
	}
	return nodesScores, nil
}

var errEmptyPriorityList = errors.New("empty priorityList")

// selectHost获取一个按优先级排列的节点列表，然后选择一个
// 以储层采样方式从得分最高的节点中提取。
// 它还返回前{count}个节点，
// 并且列表的顶部将始终是所选主机。
// selectHost takes a prioritized list of nodes and then picks one
// in a reservoir sampling manner from the nodes that had the highest score.
// It also returns the top {count} Nodes,
// and the top of the list will be always the selected host.
func selectHost(nodeScoreList []framework.NodePluginScores, count int) (string, []framework.NodePluginScores, error) {
	if len(nodeScoreList) == 0 {
		return "", nil, errEmptyPriorityList
	}

	var h nodeScoreHeap = nodeScoreList
	heap.Init(&h)
	cntOfMaxScore := 1
	selectedIndex := 0
	//堆的顶部是得分最高的NodeScoreResult。
	// The top of the heap is the NodeScoreResult with the highest score.
	sortedNodeScoreList := make([]framework.NodePluginScores, 0, count)
	sortedNodeScoreList = append(sortedNodeScoreList, heap.Pop(&h).(framework.NodePluginScores))

	//这个for循环将继续，直到检查了所有得分最高的节点进行储层采样，
	//sortedNodeScoreList获取（计数-1）个元素。
	// This for-loop will continue until all Nodes with the highest scores get checked for a reservoir sampling,
	// and sortedNodeScoreList gets (count - 1) elements.
	for ns := heap.Pop(&h).(framework.NodePluginScores); ; ns = heap.Pop(&h).(framework.NodePluginScores) {
		if ns.TotalScore != sortedNodeScoreList[0].TotalScore && len(sortedNodeScoreList) == count {
			break
		}

		if ns.TotalScore == sortedNodeScoreList[0].TotalScore {
			cntOfMaxScore++
			if rand.Intn(cntOfMaxScore) == 0 {
				//用1/cntOfMaxScore的概率替换候选者
				// Replace the candidate with probability of 1/cntOfMaxScore
				selectedIndex = cntOfMaxScore - 1
			}
		}

		sortedNodeScoreList = append(sortedNodeScoreList, ns)

		if h.Len() == 0 {
			break
		}
	}

	if selectedIndex != 0 {
		//将第一个替换为选定的一个
		// replace the first one with selected one
		previous := sortedNodeScoreList[0]
		sortedNodeScoreList[0] = sortedNodeScoreList[selectedIndex]
		sortedNodeScoreList[selectedIndex] = previous
	}

	if len(sortedNodeScoreList) > count {
		sortedNodeScoreList = sortedNodeScoreList[:count]
	}

	return sortedNodeScoreList[0].Name, sortedNodeScoreList, nil
}

// nodeScoreHeap是一堆框架.NodePluginScores。
// nodeScoreHeap is a heap of framework.NodePluginScores.
type nodeScoreHeap []framework.NodePluginScores

// nodeScoreHeap实现了堆。接口。
// nodeScoreHeap implements heap.Interface.
var _ heap.Interface = &nodeScoreHeap{}

func (h nodeScoreHeap) Len() int           { return len(h) }
func (h nodeScoreHeap) Less(i, j int) bool { return h[i].TotalScore > h[j].TotalScore }
func (h nodeScoreHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *nodeScoreHeap) Push(x interface{}) {
	*h = append(*h, x.(framework.NodePluginScores))
}

func (h *nodeScoreHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

// 假设向缓存发出信号，表明pod已经在缓存中，因此绑定可以是异步的。
// “假定”修改了“假定”。
// assume signals to the cache that a pod is already in the cache, so that binding can be asynchronous.
// assume modifies `assumed`.
func (sched *Scheduler) assume(logger klog.Logger, assumed *v1.Pod, host string) error {
	//乐观地假设绑定将成功并将其发送到apiserver
	//在背景中。
	//如果绑定失败，调度器将释放分配给假定pod的资源
	//立即。
	// Optimistically assume that the binding will succeed and send it to apiserver
	// in the background.
	// If the binding fails, scheduler will release resources allocated to assumed pod
	// immediately.
	assumed.Spec.NodeName = host

	if err := sched.Cache.AssumePod(logger, assumed); err != nil {
		logger.Error(err, "Scheduler cache AssumePod failed")
		return err
	}
	//如果“假定”是一个指定的pod，我们应该将其从内部缓存中删除
	// if "assumed" is a nominated pod, we should remove it from internal cache
	if sched.SchedulingQueue != nil {
		sched.SchedulingQueue.DeleteNominatedPodIfExists(assumed)
	}

	return nil
}

// bind将pod绑定到绑定对象中定义的给定节点。
// 绑定的优先级是：（1）扩展器和（2）框架插件。
// 我们希望它异步运行，因此我们在内部处理绑定指标。
// bind binds a pod to a given node defined in a binding object.
// The precedence for binding is: (1) extenders and (2) framework plugins.
// We expect this to run asynchronously, so we handle binding metrics internally.
func (sched *Scheduler) bind(ctx context.Context, fwk framework.Framework, assumed *v1.Pod, targetNode string, state *framework.CycleState) (status *framework.Status) {
	logger := klog.FromContext(ctx)
	defer func() {
		sched.finishBinding(logger, fwk, assumed, targetNode, status)
	}()

	bound, err := sched.extendersBinding(logger, assumed, targetNode)
	if bound {
		return framework.AsStatus(err)
	}
	return fwk.RunBindPlugins(ctx, state, assumed, targetNode)
}

// TODO（#87159）：将其移动到插件中。
// TODO(#87159): Move this to a Plugin.
func (sched *Scheduler) extendersBinding(logger klog.Logger, pod *v1.Pod, node string) (bool, error) {
	for _, extender := range sched.Extenders {
		if !extender.IsBinder() || !extender.IsInterested(pod) {
			continue
		}
		err := extender.Bind(&v1.Binding{
			ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID},
			Target:     v1.ObjectReference{Kind: "Node", Name: node},
		})
		if err != nil && extender.IsIgnorable() {
			logger.Info("Skipping extender in bind as it returned error and has ignorable flag set", "extender", extender, "err", err)
			continue
		}
		return true, err
	}
	return false, nil
}

func (sched *Scheduler) finishBinding(logger klog.Logger, fwk framework.Framework, assumed *v1.Pod, targetNode string, status *framework.Status) {
	if finErr := sched.Cache.FinishBinding(logger, assumed); finErr != nil {
		logger.Error(finErr, "Scheduler cache FinishBinding failed")
	}
	if !status.IsSuccess() {
		logger.V(1).Info("Failed to bind pod", "pod", klog.KObj(assumed))
		return
	}

	fwk.EventRecorder().Eventf(assumed, nil, v1.EventTypeNormal, "Scheduled", "Binding", "Successfully assigned %v/%v to %v", assumed.Namespace, assumed.Name, targetNode)
}

func getAttemptsLabel(p *framework.QueuedPodInfo) string {
	//我们通过限制尝试来分解pod调度持续时间
	//以避免最终得到高基数度量。
	// We breakdown the pod scheduling duration by attempts capped to a limit
	// to avoid ending up with a high cardinality metric.
	if p.Attempts >= 15 {
		return "15+"
	}
	return strconv.Itoa(p.Attempts)
}

// handleSchedulingFailure记录pod的一个事件，该事件指示
// pod未能安排时间。此外，更新pod条件和指定的节点名称（如果已设置）。
// handleSchedulingFailure records an event for the pod that indicates the
// pod has failed to schedule. Also, update the pod condition and nominated node name if set.
func (sched *Scheduler) handleSchedulingFailure(ctx context.Context, fwk framework.Framework, podInfo *framework.QueuedPodInfo, status *framework.Status, nominatingInfo *framework.NominatingInfo, start time.Time) {
	calledDone := false
	defer func() {
		if !calledDone {
			//基本上，AddUnscheduleIfNotPresent在内部调用DonePod。
			//但是，在某些极端情况下，AddUnscheduleIfNotPresent不会被调用。
			//在这里，我们显式地调用DonePod以避免pod泄漏。
			// Basically, AddUnschedulableIfNotPresent calls DonePod internally.
			// But, AddUnschedulableIfNotPresent isn't called in some corner cases.
			// Here, we call DonePod explicitly to avoid leaking the pod.
			sched.SchedulingQueue.Done(podInfo.Pod.UID)
		}
	}()

	logger := klog.FromContext(ctx)
	reason := v1.PodReasonSchedulerError
	if status.IsRejected() {
		reason = v1.PodReasonUnschedulable
	}

	switch reason {
	case v1.PodReasonUnschedulable:
		metrics.PodUnschedulable(fwk.ProfileName(), metrics.SinceInSeconds(start))
	case v1.PodReasonSchedulerError:
		metrics.PodScheduleError(fwk.ProfileName(), metrics.SinceInSeconds(start))
	}

	pod := podInfo.Pod
	err := status.AsError()
	errMsg := status.Message()

	if err == ErrNoNodesAvailable {
		logger.V(2).Info("Unable to schedule pod; no nodes are registered to the cluster; waiting", "pod", klog.KObj(pod))
	} else if fitError, ok := err.(*framework.FitError); ok { // Inject UnschedulablePlugins to PodInfo, which will be used later for moving Pods between queues efficiently.
		podInfo.UnschedulablePlugins = fitError.Diagnosis.UnschedulablePlugins
		podInfo.PendingPlugins = fitError.Diagnosis.PendingPlugins
		logger.V(2).Info("Unable to schedule pod; no fit; waiting", "pod", klog.KObj(pod), "err", errMsg)
	} else {
		logger.Error(err, "Error scheduling pod; retrying", "pod", klog.KObj(pod))
	}

	//检查Pod是否存在于informer缓存中。
	// Check if the Pod exists in informer cache.
	podLister := fwk.SharedInformerFactory().Core().V1().Pods().Lister()
	cachedPod, e := podLister.Pods(pod.Namespace).Get(pod.Name)
	if e != nil {
		logger.Info("Pod doesn't exist in informer cache", "pod", klog.KObj(pod), "err", e)
		//我们需要在这里调用DonePod，因为在这种情况下我们不调用AddUnscheduleIfNotPresent。
		// We need to call DonePod here because we don't call AddUnschedulableIfNotPresent in this case.
	} else {
		//在扩展器的情况下，pod可能已成功绑定，但向调度器返回响应时超时。
		//这可能会导致实时版本带有.spec.nodeName，这与内部排队版本不一致。
		// In the case of extender, the pod may have been bound successfully, but timed out returning its response to the scheduler.
		// It could result in the live version to carry .spec.nodeName, and that's inconsistent with the internal-queued version.
		if len(cachedPod.Spec.NodeName) != 0 {
			logger.Info("Pod has been assigned to node. Abort adding it back to queue.", "pod", klog.KObj(pod), "node", cachedPod.Spec.NodeName)
			//我们需要在这里调用DonePod，因为在这种情况下我们不会调用AddUnscheduleIfNotPresent
			// We need to call DonePod here because we don't call AddUnschedulableIfNotPresent in this case.
		} else {
			//由于<cachedPod>来自SharedInformer，我们需要在这里执行DeepCopy（）。
			//忽略此错误，因为apiserver没有正确验证关联项
			//我们无法修复向后兼容性的验证。
			// As <cachedPod> is from SharedInformer, we need to do a DeepCopy() here.
			// ignore this err since apiserver doesn't properly validate affinity terms
			// and we can't fix the validation for backwards compatibility.
			podInfo.PodInfo, _ = framework.NewPodInfo(cachedPod.DeepCopy())
			if err := sched.SchedulingQueue.AddUnschedulableIfNotPresent(logger, podInfo, sched.SchedulingQueue.SchedulingCycle()); err != nil {
				logger.Error(err, "Error occurred")
			}
			calledDone = true
		}
	}

	//用指定的pod信息更新调度队列。没有
	//这将在下一个调度周期之间出现竞争条件
	//以及调度器接收指定Pod的Pod Update的时间。
	//在这里，我们只检查测试中的nil。
	// Update the scheduling queue with the nominated pod information. Without
	// this, there would be a race condition between the next scheduling cycle
	// and the time the scheduler receives a Pod Update for the nominated pod.
	// Here we check for nil only for tests.
	if sched.SchedulingQueue != nil {
		sched.SchedulingQueue.AddNominatedPod(logger, podInfo.PodInfo, nominatingInfo)
	}

	if err == nil {
		//只有测试才能到达这里。
		// Only tests can reach here.
		return
	}

	msg := truncateMessage(errMsg)
	fwk.EventRecorder().Eventf(pod, nil, v1.EventTypeWarning, "FailedScheduling", "Scheduling", msg)
	if err := updatePod(ctx, sched.client, pod, &v1.PodCondition{
		Type:    v1.PodScheduled,
		Status:  v1.ConditionFalse,
		Reason:  reason,
		Message: errMsg,
	}, nominatingInfo); err != nil {
		logger.Error(err, "Error updating pod", "pod", klog.KObj(pod))
	}
}

// truncateMessage会截断达到NoteLengthLimit的消息。
// truncateMessage truncates a message if it hits the NoteLengthLimit.
func truncateMessage(message string) string {
	max := validation.NoteLengthLimit
	if len(message) <= max {
		return message
	}
	suffix := " ..."
	return message[:max-len(suffix)] + suffix
}

func updatePod(ctx context.Context, client clientset.Interface, pod *v1.Pod, condition *v1.PodCondition, nominatingInfo *framework.NominatingInfo) error {
	logger := klog.FromContext(ctx)
	logger.V(3).Info("Updating pod condition", "pod", klog.KObj(pod), "conditionType", condition.Type, "conditionStatus", condition.Status, "conditionReason", condition.Reason)
	podStatusCopy := pod.Status.DeepCopy()
	//只有当我们尝试设置NominatedNodeName时，它才会更新，值为
	//与现有的不同。
	// NominatedNodeName is updated only if we are trying to set it, and the value is
	// different from the existing one.
	nnnNeedsUpdate := nominatingInfo.Mode() == framework.ModeOverride && pod.Status.NominatedNodeName != nominatingInfo.NominatedNodeName
	if !podutil.UpdatePodCondition(podStatusCopy, condition) && !nnnNeedsUpdate {
		return nil
	}
	if nnnNeedsUpdate {
		podStatusCopy.NominatedNodeName = nominatingInfo.NominatedNodeName
	}
	return util.PatchPodStatus(ctx, client, pod, podStatusCopy)
}
