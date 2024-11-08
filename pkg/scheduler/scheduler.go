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
	"context"
	"errors"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	configv1 "k8s.io/kube-scheduler/config/v1"
	"k8s.io/kubernetes/pkg/features"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/scheme"
	internalcache "k8s.io/kubernetes/pkg/scheduler/backend/cache"
	cachedebugger "k8s.io/kubernetes/pkg/scheduler/backend/cache/debugger"
	internalqueue "k8s.io/kubernetes/pkg/scheduler/backend/queue"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/parallelize"
	frameworkplugins "k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/dynamicresources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/scheduler/profile"
	"k8s.io/kubernetes/pkg/scheduler/util/assumecache"
)

// 调度器源码阅读https://zhuanlan.zhihu.com/p/557136318
const (
	//调度器在使假定的pod到期之前等待的时间。
	//有关此参数及其值的更多详细信息，请参阅问题#106361。
	// Duration the scheduler will wait before expiring an assumed pod.
	// See issue #106361 for more details about this parameter and its value.
	durationToExpireAssumedPod time.Duration = 0
)

// ErrNoNodesAvailable用于描述没有可用于调度Pod的节点的错误。
// ErrNoNodesAvailable is used to describe the error that no nodes available to schedule pods.
var ErrNoNodesAvailable = fmt.Errorf("no nodes available to schedule pods")

// 调度器监视新的未调度的Pod。它试图找到
// 它们所适应的节点，并将绑定写回api服务器。
// Scheduler watches for new unscheduled pods. It attempts to find
// nodes that they fit on and writes bindings back to the api server.
type Scheduler struct {
	//预计通过缓存所做的更改将被观察到
	//通过NodeLister和算法。
	// It is expected that changes made via Cache will be observed
	// by NodeLister and Algorithm.
	Cache internalcache.Cache

	Extenders []framework.Extender

	//NextPod应该是一个阻塞到下一个pod的函数
	//可用。我们不使用频道，因为日程安排
	//一个pod可能需要一些时间，我们不希望pod
	//当他们坐在一个通道里时，就会变得陈旧。
	// NextPod should be a function that blocks until the next pod
	// is available. We don't use a channel for this, because scheduling
	// a pod may take some amount of time and we don't want pods to get
	// stale while they sit in a channel.
	NextPod func(logger klog.Logger) (*framework.QueuedPodInfo, error)

	//FailureHandler在调度失败时被调用。
	// FailureHandler is called upon a scheduling failure.
	FailureHandler FailureHandlerFn

	//SchedulePod尝试将给定的pod调度到节点列表中的一个节点。
	//成功时返回一个ScheduleResult结构体，其中包含建议主机的名称，
	//否则将返回FitError并说明原因。
	// SchedulePod tries to schedule the given pod to one of the nodes in the node list.
	// Return a struct of ScheduleResult with the name of suggested host on success,
	// otherwise will return a FitError with reasons.
	SchedulePod func(ctx context.Context, fwk framework.Framework, state *framework.CycleState, pod *v1.Pod) (ScheduleResult, error)

	//关闭此选项可关闭调度程序。
	// Close this to shut down the scheduler.
	StopEverything <-chan struct{}

	//SchedulingQueue保存要调度的Pod
	// SchedulingQueue holds pods to be scheduled
	SchedulingQueue internalqueue.SchedulingQueue

	//配置文件是调度配置文件。
	// Profiles are the scheduling profiles.
	Profiles profile.Map

	client clientset.Interface

	nodeInfoSnapshot *internalcache.Snapshot

	percentageOfNodesToScore int32

	nextStartNodeIndex int

	//在创建调度器时，必须初始化记录器，
	//否则，日志记录功能将访问nil接收器
	//恐慌。
	// logger *must* be initialized when creating a Scheduler,
	// otherwise logging functions will access a nil sink and
	// panic.
	logger klog.Logger

	//registeredHandlers包含所有处理程序的注册。它用于检查在调度周期开始之前是否所有处理程序都已完成同步。
	// registeredHandlers contains the registrations of all handlers. It's used to check if all handlers have finished syncing before the scheduling cycles start.
	registeredHandlers []cache.ResourceEventHandlerRegistration
}

func (sched *Scheduler) applyDefaultHandlers() {
	sched.SchedulePod = sched.schedulePod
	sched.FailureHandler = sched.handleSchedulingFailure
}

type schedulerOptions struct {
	componentConfigVersion string
	kubeConfig             *restclient.Config
	//如果在v1中设置，则被NodesToScore的配置文件级别百分比覆盖。
	// Overridden by profile level percentageOfNodesToScore if set in v1.
	percentageOfNodesToScore          int32
	podInitialBackoffSeconds          int64
	podMaxBackoffSeconds              int64
	podMaxInUnschedulablePodsDuration time.Duration
	//包含要与树内注册表合并的树外插件。
	// Contains out-of-tree plugins to be merged with the in-tree registry.
	frameworkOutOfTreeRegistry frameworkruntime.Registry
	profiles                   []schedulerapi.KubeSchedulerProfile
	extenders                  []schedulerapi.Extender
	frameworkCapturer          FrameworkCapturer
	parallelism                int32
	applyDefaultProfile        bool
}

// 选项配置调度器
// Option configures a Scheduler
type Option func(*schedulerOptions)

// ScheduleResult表示调度pod的结果。
// ScheduleResult represents the result of scheduling a pod.
type ScheduleResult struct {
	//所选节点的名称。
	// Name of the selected node.
	SuggestedHost string
	//调度器在过滤中评估pod的节点数量
	//阶段及以后。
	// The number of nodes the scheduler evaluated the pod against in the filtering
	// phase and beyond.
	EvaluatedNodes int
	//在评估的节点中，适合pod的节点数量。
	// The number of nodes out of the evaluated ones that fit the pod.
	FeasibleNodes int
	//调度周期的提名信息。
	// The nominating info for scheduling cycle.
	nominatingInfo *framework.NominatingInfo
}

// WithComponentConfigVersion将组件配置版本设置为
// 使用的KubeScheduler配置版本。字符串应该是完整的
// 我们转换的外部类型的方案组/版本（例如
// “kubescheduler.config.k8s.io/v1”）
// WithComponentConfigVersion sets the component config version to the
// KubeSchedulerConfiguration version used. The string should be the full
// scheme group/version of the external type we converted from (for example
// "kubescheduler.config.k8s.io/v1")
func WithComponentConfigVersion(apiVersion string) Option {
	return func(o *schedulerOptions) {
		o.componentConfigVersion = apiVersion
	}
}

// WithKubeConfig为Scheduler设置kube-config。
// WithKubeConfig sets the kube config for Scheduler.
func WithKubeConfig(cfg *restclient.Config) Option {
	return func(o *schedulerOptions) {
		o.kubeConfig = cfg
	}
}

// WithProfiles为调度器设置配置文件。默认情况下，有一个配置文件
// 名为“默认调度程序”。
// WithProfiles sets profiles for Scheduler. By default, there is one profile
// with the name "default-scheduler".
func WithProfiles(p ...schedulerapi.KubeSchedulerProfile) Option {
	return func(o *schedulerOptions) {
		o.profiles = p
		o.applyDefaultProfile = false
	}
}

// WithParallelism为所有调度算法设置并行性。默认值为16。
// WithParallelism sets the parallelism for all scheduler algorithms. Default is 16.
func WithParallelism(threads int32) Option {
	return func(o *schedulerOptions) {
		o.parallelism = threads
	}
}

// WithPercentageOfNodesToScore为调度器设置NodesToSore的百分比。
// 默认值0将使用自适应百分比：50-（节点数）/125。
// WithPercentageOfNodesToScore sets percentageOfNodesToScore for Scheduler.
// The default value of 0 will use an adaptive percentage: 50 - (num of nodes)/125.
func WithPercentageOfNodesToScore(percentageOfNodesToScore *int32) Option {
	return func(o *schedulerOptions) {
		if percentageOfNodesToScore != nil {
			o.percentageOfNodesToScore = *percentageOfNodesToScore
		}
	}
}

// WithFrameworkOutOfTreeRegistry为树外插件设置注册表。那些插件
// 将附加到默认注册表。
// WithFrameworkOutOfTreeRegistry sets the registry for out-of-tree plugins. Those plugins
// will be appended to the default registry.
func WithFrameworkOutOfTreeRegistry(registry frameworkruntime.Registry) Option {
	return func(o *schedulerOptions) {
		o.frameworkOutOfTreeRegistry = registry
	}
}

// WithPodInitialBackoff Seconds为调度器设置podInitialBackoff秒数，默认值为1
// WithPodInitialBackoffSeconds sets podInitialBackoffSeconds for Scheduler, the default value is 1
func WithPodInitialBackoffSeconds(podInitialBackoffSeconds int64) Option {
	return func(o *schedulerOptions) {
		o.podInitialBackoffSeconds = podInitialBackoffSeconds
	}
}

// 使用PodMaxBackoff Seconds为调度器设置podMaxBackoff秒数，默认值为10
// WithPodMaxBackoffSeconds sets podMaxBackoffSeconds for Scheduler, the default value is 10
func WithPodMaxBackoffSeconds(podMaxBackoffSeconds int64) Option {
	return func(o *schedulerOptions) {
		o.podMaxBackoffSeconds = podMaxBackoffSeconds
	}
}

// 使用PodMaxInUnschedulePodsDuration为PriorityQueue设置podMaxInUnchedulePodsDuriation。
// WithPodMaxInUnschedulablePodsDuration sets podMaxInUnschedulablePodsDuration for PriorityQueue.
func WithPodMaxInUnschedulablePodsDuration(duration time.Duration) Option {
	return func(o *schedulerOptions) {
		o.podMaxInUnschedulablePodsDuration = duration
	}
}

// WithExtenders为调度器设置扩展器
// WithExtenders sets extenders for the Scheduler
func WithExtenders(e ...schedulerapi.Extender) Option {
	return func(o *schedulerOptions) {
		o.extenders = e
	}
}

// FrameworkCapturer用于在构建框架中注册通知函数。
// FrameworkCapturer is used for registering a notify function in building framework.
type FrameworkCapturer func(schedulerapi.KubeSchedulerProfile)

// WithBuildFrameworkCapturer设置了一个用于获取buildFramework详细信息的通知函数。
// WithBuildFrameworkCapturer sets a notify function for getting buildFramework details.
func WithBuildFrameworkCapturer(fc FrameworkCapturer) Option {
	return func(o *schedulerOptions) {
		o.frameworkCapturer = fc
	}
}

var defaultSchedulerOptions = schedulerOptions{
	percentageOfNodesToScore:          schedulerapi.DefaultPercentageOfNodesToScore,
	podInitialBackoffSeconds:          int64(internalqueue.DefaultPodInitialBackoffDuration.Seconds()),
	podMaxBackoffSeconds:              int64(internalqueue.DefaultPodMaxBackoffDuration.Seconds()),
	podMaxInUnschedulablePodsDuration: internalqueue.DefaultPodMaxInUnschedulablePodsDuration,
	parallelism:                       int32(parallelize.DefaultParallelism),
	//理想情况下，我们会在这里静态设置默认配置文件，但我们不能这样做，因为
	//创建默认配置文件可能需要测试特征门，这可能会导致
	//在测试中动态设置。因此，我们推迟创建它，直到New实际
	//调用。
	// Ideally we would statically set the default profile here, but we can't because
	// creating the default profile may require testing feature gates, which may get
	// set dynamically in tests. Therefore, we delay creating it until New is actually
	// invoked.
	applyDefaultProfile: true,
}

// 新返回一个调度器
// New returns a Scheduler
func New(ctx context.Context,
	client clientset.Interface,
	informerFactory informers.SharedInformerFactory,
	dynInformerFactory dynamicinformer.DynamicSharedInformerFactory,
	recorderFactory profile.RecorderFactory,
	opts ...Option) (*Scheduler, error) {

	logger := klog.FromContext(ctx)
	stopEverything := ctx.Done()
	// 参数配置
	options := defaultSchedulerOptions
	for _, opt := range opts {
		opt(&options)
	}
	// 应用默认参数
	if options.applyDefaultProfile {
		var versionedCfg configv1.KubeSchedulerConfiguration
		scheme.Scheme.Default(&versionedCfg)
		cfg := schedulerapi.KubeSchedulerConfiguration{}
		if err := scheme.Scheme.Convert(&versionedCfg, &cfg, nil); err != nil {
			return nil, err
		}
		options.profiles = cfg.Profiles
	}
	// 调度策略
	registry := frameworkplugins.NewInTreeRegistry()
	if err := registry.Merge(options.frameworkOutOfTreeRegistry); err != nil {
		return nil, err
	}

	metrics.Register()
	// 扩展插件
	extenders, err := buildExtenders(logger, options.extenders, options.profiles)
	if err != nil {
		return nil, fmt.Errorf("couldn't build extenders: %w", err)
	}

	podLister := informerFactory.Core().V1().Pods().Lister()
	nodeLister := informerFactory.Core().V1().Nodes().Lister()

	snapshot := internalcache.NewEmptySnapshot()
	metricsRecorder := metrics.NewMetricsAsyncRecorder(1000, time.Second, stopEverything)
	//waitingPods保存调度器中的所有Pod，并在许可阶段等待
	// waitingPods holds all the pods that are in the scheduler and waiting in the permit stage
	waitingPods := frameworkruntime.NewWaitingPodsMap()

	var resourceClaimCache *assumecache.AssumeCache
	var draManager framework.SharedDRAManager
	// 门控特性
	if utilfeature.DefaultFeatureGate.Enabled(features.DynamicResourceAllocation) {
		resourceClaimInformer := informerFactory.Resource().V1beta1().ResourceClaims().Informer()
		resourceClaimCache = assumecache.NewAssumeCache(logger, resourceClaimInformer, "ResourceClaim", "", nil)
		draManager = dynamicresources.NewDRAManager(ctx, resourceClaimCache, informerFactory)
	}
	// 调度插件
	profiles, err := profile.NewMap(ctx, options.profiles, registry, recorderFactory,
		frameworkruntime.WithComponentConfigVersion(options.componentConfigVersion),
		frameworkruntime.WithClientSet(client),
		frameworkruntime.WithKubeConfig(options.kubeConfig),
		frameworkruntime.WithInformerFactory(informerFactory),
		frameworkruntime.WithSharedDRAManager(draManager),
		frameworkruntime.WithSnapshotSharedLister(snapshot),
		frameworkruntime.WithCaptureProfile(frameworkruntime.CaptureProfile(options.frameworkCapturer)),
		frameworkruntime.WithParallelism(int(options.parallelism)),
		frameworkruntime.WithExtenders(extenders),
		frameworkruntime.WithMetricsRecorder(metricsRecorder),
		frameworkruntime.WithWaitingPods(waitingPods),
	)
	if err != nil {
		return nil, fmt.Errorf("initializing profiles: %v", err)
	}

	if len(profiles) == 0 {
		return nil, errors.New("at least one profile is required")
	}
	// 插件分类
	preEnqueuePluginMap := make(map[string][]framework.PreEnqueuePlugin)
	queueingHintsPerProfile := make(internalqueue.QueueingHintMapPerProfile)
	var returnErr error
	for profileName, profile := range profiles {
		preEnqueuePluginMap[profileName] = profile.PreEnqueuePlugins()
		queueingHintsPerProfile[profileName], err = buildQueueingHintMap(ctx, profile.EnqueueExtensions())
		if err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}

	if returnErr != nil {
		return nil, returnErr
	}
	// 正在准备被调度的pod列表，使用informerFactory实现（watch）
	podQueue := internalqueue.NewSchedulingQueue(
		profiles[options.profiles[0].SchedulerName].QueueSortFunc(),
		informerFactory,
		internalqueue.WithPodInitialBackoffDuration(time.Duration(options.podInitialBackoffSeconds)*time.Second),
		internalqueue.WithPodMaxBackoffDuration(time.Duration(options.podMaxBackoffSeconds)*time.Second),
		internalqueue.WithPodLister(podLister),
		internalqueue.WithPodMaxInUnschedulablePodsDuration(options.podMaxInUnschedulablePodsDuration),
		internalqueue.WithPreEnqueuePluginMap(preEnqueuePluginMap),
		internalqueue.WithQueueingHintMapPerProfile(queueingHintsPerProfile),
		internalqueue.WithPluginMetricsSamplePercent(pluginMetricsSamplePercent),
		internalqueue.WithMetricsRecorder(*metricsRecorder),
	)

	for _, fwk := range profiles {
		fwk.SetPodNominator(podQueue)
		fwk.SetPodActivator(podQueue)
	}
	//通过SchedulerCache做出的改变将被NodeLister和Algorithm观察到。
	schedulerCache := internalcache.New(ctx, durationToExpireAssumedPod)

	//安装缓存调试器。
	// Setup cache debugger.
	debugger := cachedebugger.New(nodeLister, podLister, schedulerCache, podQueue)
	debugger.ListenForSignal(ctx)

	sched := &Scheduler{
		Cache:                    schedulerCache,
		client:                   client,
		nodeInfoSnapshot:         snapshot,
		percentageOfNodesToScore: options.percentageOfNodesToScore,
		Extenders:                extenders,
		StopEverything:           stopEverything,
		SchedulingQueue:          podQueue,
		Profiles:                 profiles,
		logger:                   logger,
	}
	sched.NextPod = podQueue.Pop
	sched.applyDefaultHandlers()

	if err = addAllEventHandlers(sched, informerFactory, dynInformerFactory, resourceClaimCache, unionedGVKs(queueingHintsPerProfile)); err != nil {
		return nil, fmt.Errorf("adding event handlers: %w", err)
	}

	return sched, nil
}

// defaultQueueingHintFn是默认的排队提示函数。
// 它总是返回Queue作为排队提示。
// defaultQueueingHintFn is the default queueing hint function.
// It always returns Queue as the queueing hint.
var defaultQueueingHintFn = func(_ klog.Logger, _ *v1.Pod, _, _ interface{}) (framework.QueueingHint, error) {
	return framework.Queue, nil
}

func buildQueueingHintMap(ctx context.Context, es []framework.EnqueueExtensions) (internalqueue.QueueingHintMap, error) {
	queueingHintMap := make(internalqueue.QueueingHintMap)
	var returnErr error
	for _, e := range es {
		events, err := e.EventsToRegister(ctx)
		if err != nil {
			returnErr = errors.Join(returnErr, err)
		}

		//当插件注册空事件时会发生这种情况，通常是pod的情况
		//将仅可重新安排自我更新，例如schedulingGates插件、pod
		//将通过priorityQueue进入activeQ。Update（）。
		// This will happen when plugin registers with empty events, it's usually the case a pod
		// will become reschedulable only for self-update, e.g. schedulingGates plugin, the pod
		// will enter into the activeQ via priorityQueue.Update().
		if len(events) == 0 {
			continue
		}

		//注意：很少有插件实现EnqueueExtensions但返回nil。
		//我们将其视为：插件对任何事件都不感兴趣，因此该插件导致pod失败
		//不能被任何常规集群事件移动。
		//因此，我们可以忽略此类事件在此处注册。
		// Note: Rarely, a plugin implements EnqueueExtensions but returns nil.
		// We treat it as: the plugin is not interested in any event, and hence pod failed by that plugin
		// cannot be moved by any regular cluster event.
		// So, we can just ignore such EventsToRegister here.

		registerNodeAdded := false
		registerNodeTaintUpdated := false
		for _, event := range events {
			fn := event.QueueingHintFn
			if fn == nil || !utilfeature.DefaultFeatureGate.Enabled(features.SchedulerQueueingHints) {
				fn = defaultQueueingHintFn
			}

			if event.Event.Resource == framework.Node {
				if event.Event.ActionType&framework.Add != 0 {
					registerNodeAdded = true
				}
				if event.Event.ActionType&framework.UpdateNodeTaint != 0 {
					registerNodeTaintUpdated = true
				}
			}

			queueingHintMap[event.Event] = append(queueingHintMap[event.Event], &internalqueue.QueueingHintFunction{
				PluginName:     e.Name(),
				QueueingHintFn: fn,
			})
		}
		if registerNodeAdded && !registerNodeTaintUpdated {
			//暂时修复该问题https://github.com/kubernetes/kubernetes/issues/109437
			//由于preCheck，NodeAdded QueueingHint并不总是被调用。
			//这绝对不是插件开发人员所期望的，
			//注册UpdateNodeTaint事件是目前唯一的缓解措施。
			//
			//因此，这里为有NodeAdded事件但没有UpdateNodeTaint事件的插件注册UpdateNodeTait事件。
			//不过，这对重新使用效率有不良影响，比一些Pod被卡住要好得多
			//不可调度的吊舱池。
			//当我们删除preCheck功能时，此行为将被删除。
			//请参阅：https://github.com/kubernetes/kubernetes/issues/110175
			// Temporally fix for the issue https://github.com/kubernetes/kubernetes/issues/109437
			// NodeAdded QueueingHint isn't always called because of preCheck.
			// It's definitely not something expected for plugin developers,
			// and registering UpdateNodeTaint event is the only mitigation for now.
			//
			// So, here registers UpdateNodeTaint event for plugins that has NodeAdded event, but don't have UpdateNodeTaint event.
			// It has a bad impact for the requeuing efficiency though, a lot better than some Pods being stuch in the
			// unschedulable pod pool.
			// This behavior will be removed when we remove the preCheck feature.
			// See: https://github.com/kubernetes/kubernetes/issues/110175
			queueingHintMap[framework.ClusterEvent{Resource: framework.Node, ActionType: framework.UpdateNodeTaint}] =
				append(queueingHintMap[framework.ClusterEvent{Resource: framework.Node, ActionType: framework.UpdateNodeTaint}],
					&internalqueue.QueueingHintFunction{
						PluginName:     e.Name(),
						QueueingHintFn: defaultQueueingHintFn,
					},
				)
		}
	}
	if returnErr != nil {
		return nil, returnErr
	}
	return queueingHintMap, nil
}

// 跑步开始观看和安排。它开始调度并被阻止，直到上下文完成。
// Run begins watching and scheduling. It starts scheduling and blocked until the context is done.
func (sched *Scheduler) Run(ctx context.Context) {
	logger := klog.FromContext(ctx)
	sched.SchedulingQueue.Run(logger)

	//我们需要在专用的goroutine中启动scheduleOne循环，
	//因为scheduleOne函数挂起获取下一个项目
	//来自调度队列。
	//如果没有新的吊舱可供调度，它将挂在那里
	//如果在这个goroutine中完成，它将阻止关闭
	//SchedulingQueue，实际上在关机时导致死锁。
	// We need to start scheduleOne loop in a dedicated goroutine,
	// because scheduleOne function hangs on getting the next item
	// from the SchedulingQueue.
	// If there are no new pods to schedule, it will be hanging there
	// and if done in this goroutine it will be blocking closing
	// SchedulingQueue, in effect causing a deadlock on shutdown.
	go wait.UntilWithContext(ctx, sched.ScheduleOne, 0)

	<-ctx.Done()
	sched.SchedulingQueue.Close()

	// 如果插件满足io.Closer接口，则将其关闭。
	// If the plugins satisfy the io.Closer interface, they are closed.
	err := sched.Profiles.Close()
	if err != nil {
		logger.Error(err, "Failed to close plugins")
	}
}

// NewInformerFactory创建SharedInformerFFactory并初始化特定于调度程序的
// 到位podInformer。
// NewInformerFactory creates a SharedInformerFactory and initializes a scheduler specific
// in-place podInformer.
func NewInformerFactory(cs clientset.Interface, resyncPeriod time.Duration) informers.SharedInformerFactory {
	informerFactory := informers.NewSharedInformerFactory(cs, resyncPeriod)
	informerFactory.InformerFor(&v1.Pod{}, newPodInformer)
	return informerFactory
}

func buildExtenders(logger klog.Logger, extenders []schedulerapi.Extender, profiles []schedulerapi.KubeSchedulerProfile) ([]framework.Extender, error) {
	var fExtenders []framework.Extender
	if len(extenders) == 0 {
		return nil, nil
	}

	var ignoredExtendedResources []string
	var ignorableExtenders []framework.Extender
	for i := range extenders {
		logger.V(2).Info("Creating extender", "extender", extenders[i])
		extender, err := NewHTTPExtender(&extenders[i])
		if err != nil {
			return nil, err
		}
		if !extender.IsIgnorable() {
			fExtenders = append(fExtenders, extender)
		} else {
			ignorableExtenders = append(ignorableExtenders, extender)
		}
		for _, r := range extenders[i].ManagedResources {
			if r.IgnoredByScheduler {
				ignoredExtendedResources = append(ignoredExtendedResources, r.Name)
			}
		}
	}
	//将可忽略的延长器放置在延长器的尾部
	// place ignorable extenders to the tail of extenders
	fExtenders = append(fExtenders, ignorableExtenders...)

	//如果从扩展器中找到任何扩展资源，请将它们附加到每个配置文件的pluginConfig中。
	//这应该只对ComponentConfig有影响，在ComponentConfig中可以配置扩展器和
	//插件参数（在这种情况下，扩展器忽略的资源优先）。
	// If there are any extended resources found from the Extenders, append them to the pluginConfig for each profile.
	// This should only have an effect on ComponentConfig, where it is possible to configure Extenders and
	// plugin args (and in which case the extender ignored resources take precedence).
	if len(ignoredExtendedResources) == 0 {
		return fExtenders, nil
	}

	for i := range profiles {
		prof := &profiles[i]
		var found = false
		for k := range prof.PluginConfig {
			if prof.PluginConfig[k].Name == noderesources.Name {
				//更新现有参数
				// Update the existing args
				pc := &prof.PluginConfig[k]
				args, ok := pc.Args.(*schedulerapi.NodeResourcesFitArgs)
				if !ok {
					return nil, fmt.Errorf("want args to be of type NodeResourcesFitArgs, got %T", pc.Args)
				}
				args.IgnoredResources = ignoredExtendedResources
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("can't find NodeResourcesFitArgs in plugin config")
		}
	}
	return fExtenders, nil
}

type FailureHandlerFn func(ctx context.Context, fwk framework.Framework, podInfo *framework.QueuedPodInfo, status *framework.Status, nominatingInfo *framework.NominatingInfo, start time.Time)

func unionedGVKs(queueingHintsPerProfile internalqueue.QueueingHintMapPerProfile) map[framework.EventResource]framework.ActionType {
	gvkMap := make(map[framework.EventResource]framework.ActionType)
	for _, queueingHints := range queueingHintsPerProfile {
		for evt := range queueingHints {
			if _, ok := gvkMap[evt.Resource]; ok {
				gvkMap[evt.Resource] |= evt.ActionType
			} else {
				gvkMap[evt.Resource] = evt.ActionType
			}
		}
	}
	return gvkMap
}

// newPodInformer创建了一个仅返回非终端Pod的共享索引通知器。
// PodInformer允许添加索引器，但请注意，只允许添加非冲突索引器。
// newPodInformer creates a shared index informer that returns only non-terminal pods.
// The PodInformer allows indexers to be added, but note that only non-conflict indexers are allowed.
func newPodInformer(cs clientset.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	selector := fmt.Sprintf("status.phase!=%v,status.phase!=%v", v1.PodSucceeded, v1.PodFailed)
	tweakListOptions := func(options *metav1.ListOptions) {
		options.FieldSelector = selector
	}
	informer := coreinformers.NewFilteredPodInformer(cs, metav1.NamespaceAll, resyncPeriod, cache.Indexers{}, tweakListOptions)

	//删除“.media.managedFields”以提高内存使用率。
	//提取工作流（即“ExtractPod”）应不使用。
	// Dropping `.metadata.managedFields` to improve memory usage.
	// The Extract workflow (i.e. `ExtractPod`) should be unused.
	trim := func(obj interface{}) (interface{}, error) {
		if accessor, err := meta.Accessor(obj); err == nil {
			if accessor.GetManagedFields() != nil {
				accessor.SetManagedFields(nil)
			}
		}
		return obj, nil
	}
	informer.SetTransform(trim)
	return informer
}
