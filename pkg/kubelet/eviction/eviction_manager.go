/*
Copyright 2016 The Kubernetes Authors.

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

package eviction

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api/resource"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/client/record"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/qos"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/util/clock"
	"k8s.io/kubernetes/pkg/util/wait"
)

// managerImpl implements Manager
//驱逐的管理器
type managerImpl struct {
	//  used to track time
	clock clock.Clock
	// config is how the manager is configured
	//其中包含触发eviction的阈值
	config Config
	// the function to invoke to kill a pod
	killPodFunc KillPodFunc //驱逐pod的函数
	// the interface that knows how to do image gc
	imageGC ImageGC
	// protects access to internal state
	sync.RWMutex
	// node conditions are the set of conditions present
	//用来决策Pod是否接受,这个节点条件是谁配置的
	nodeConditions []v1.NodeConditionType //节点条件类型映射特定资源类型,以及映射特定阈值信号.
	// captures when a node condition was last observed based on a threshold being met
	nodeConditionsLastObservedAt nodeConditionsObservedAt
	// nodeRef is a reference to the node
	nodeRef *v1.ObjectReference
	// used to record events about the node
	recorder record.EventRecorder
	// used to measure usage stats on system
	summaryProvider stats.SummaryProvider //获取Pod统计,以及各驱逐信号相关资源统计
	// records when a threshold was first observed
	thresholdsFirstObservedAt thresholdsObservedAt //记录阈值信号的观察时间,在每次evict管理器同步时,如果有新阈值信号,则更新该表,记录新信号观察时间;如果是已记录的阈值信号,则仍记录上一次观察时间
	// records the set of thresholds that have been met (including graceperiod) but not yet resolved
	thresholdsMet []Threshold //在每一次evict同步周期中,如果thresholdsFirstObjectedAt表中的信号第一次观察时间到同步时时间已经超过该信号的宽限期,则记录该信号到当前表中
	// resourceToRankFunc maps a resource to ranking function for that resource.
	resourceToRankFunc map[v1.ResourceName]rankFunc //见synchronize()方法 每项资源的得分排序函数,1)首先基于pod的Qos,2)QOS相同时,根据相应资源统计进行排序
	// resourceToNodeReclaimFuncs maps a resource to an ordered list of functions that know how to reclaim that resource.
	resourceToNodeReclaimFuncs map[v1.ResourceName]nodeReclaimFuncs //见synchronize()
	// last observations from synchronize
	lastObservations signalObservations //上一次阈值信号对应的资源统计
	// notifiersInitialized indicates if the threshold notifiers have been initialized (i.e. synchronize() has been called once)
	notifiersInitialized bool
}

// ensure it implements the required interface
var _ Manager = &managerImpl{}

// NewManager returns a configured Manager and an associated admission handler to enforce eviction configuration.
//这个只被Kubelet对象在创建时调用,Config中的阈值是kubelet运行时,配置的--evict-*解析得来
func NewManager(
	summaryProvider stats.SummaryProvider, //资源分析器
	config Config, //阈值配置
	killPodFunc KillPodFunc,
	imageGC ImageGC, //镜像回收
	recorder record.EventRecorder,
	nodeRef *v1.ObjectReference,
	clock clock.Clock) (Manager, lifecycle.PodAdmitHandler, error) {
	manager := &managerImpl{
		clock:           clock,
		killPodFunc:     killPodFunc,
		imageGC:         imageGC,
		config:          config,
		recorder:        recorder,
		summaryProvider: summaryProvider,
		nodeRef:         nodeRef,
		nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
		thresholdsFirstObservedAt:    thresholdsObservedAt{},
	}
	return manager, manager, nil
}

// Admit rejects a pod if its not safe to admit for node stability.
//attrs包含需要判定的pod,以及Kubelet已有的pod?
//返回值是判定决策结果
func (m *managerImpl) Admit(attrs *lifecycle.PodAdmitAttributes) lifecycle.PodAdmitResult {
	m.RLock()
	defer m.RUnlock()
	//如果节点的有效条件为空,则允许pod进入
	if len(m.nodeConditions) == 0 {
		return lifecycle.PodAdmitResult{Admit: true}
	}

	// the node has memory pressure, admit if not best-effort
	//如果kubelet正处于内存压力,判定pod的Qos是否是BestEffort,如果是则直接判定失败(OOM情况下,BestEffort的Pod会被杀死以释放内存)
	//如果pod是重要Pod或者不是BestEffort的话,决策成功
	if hasNodeCondition(m.nodeConditions, v1.NodeMemoryPressure) {
		notBestEffort := qos.BestEffort != qos.GetPodQOS(attrs.Pod)
		if notBestEffort || kubetypes.IsCriticalPod(attrs.Pod) {
			return lifecycle.PodAdmitResult{Admit: true}
		}
	}

	// reject pods when under memory pressure (if pod is best effort), or if under disk pressure.
	glog.Warningf("Failed to admit pod %v - %s", format.Pod(attrs.Pod), "node has conditions: %v", m.nodeConditions)
	return lifecycle.PodAdmitResult{
		Admit:   false,
		Reason:  reason,
		Message: fmt.Sprintf(message, m.nodeConditions),
	}
}

// Start starts the control loop to observe and response to low compute resources.
//周期执行, podFunc是获取kubelet绑定的pod.status.phase没有处于successed和failed的所有Pod
func (m *managerImpl) Start(diskInfoProvider DiskInfoProvider, podFunc ActivePodsFunc, monitoringInterval time.Duration) {
	// start the eviction manager monitoring
	go wait.Until(func() { m.synchronize(diskInfoProvider, podFunc) }, monitoringInterval, wait.NeverStop)
}

// IsUnderMemoryPressure returns true if the node is under memory pressure.
//是否存在内存压力
func (m *managerImpl) IsUnderMemoryPressure() bool {
	m.RLock()
	defer m.RUnlock()
	return hasNodeCondition(m.nodeConditions, v1.NodeMemoryPressure)
}

// IsUnderDiskPressure returns true if the node is under disk pressure.
//是否存在硬盘压力
func (m *managerImpl) IsUnderDiskPressure() bool {
	m.RLock()
	defer m.RUnlock()
	return hasNodeCondition(m.nodeConditions, v1.NodeDiskPressure)
}

func startMemoryThresholdNotifier(thresholds []Threshold, observations signalObservations, hard bool, handler thresholdNotifierHandlerFunc) error {
	//如果阈值的触发信号是可用内存
	for _, threshold := range thresholds {
		//当触发信号不是可用内存,或者阈值不符合所指定程度(软/硬)时
		if threshold.Signal != SignalMemoryAvailable || hard != isHardEvictionThreshold(threshold) {
			continue
		}

		//指定信号的资源统计?
		observed, found := observations[SignalMemoryAvailable]
		if !found {
			continue
		}
		cgroups, err := cm.GetCgroupSubsystems()
		if err != nil {
			return err
		}
		// TODO add support for eviction from --cgroup-root
		cgpath, found := cgroups.MountPoints["memory"]
		if !found || len(cgpath) == 0 {
			return fmt.Errorf("memory cgroup mount point not found")
		}
		attribute := "memory.usage_in_bytes"
		quantity := getThresholdQuantity(threshold.Value, observed.capacity)
		usageThreshold := resource.NewQuantity(observed.capacity.Value(), resource.DecimalSI)
		usageThreshold.Sub(*quantity)
		description := fmt.Sprintf("<%s available", formatThresholdValue(threshold.Value))
		memcgThresholdNotifier, err := NewMemCGThresholdNotifier(cgpath, attribute, usageThreshold.String(), description, handler)
		if err != nil {
			return err
		}
		go memcgThresholdNotifier.Start(wait.NeverStop)
		return nil
	}
	return nil
}

// synchronize is the main control loop that enforces eviction thresholds.
// 注意:如果需要驱逐pod,该函数每次调用只会驱逐一个pod
func (m *managerImpl) synchronize(diskInfoProvider DiskInfoProvider, podFunc ActivePodsFunc) {
	// if we have nothing to do, just return
	//阈值是触发evict的条件, kubelet设置了哪些阈值驱逐信号
	thresholds := m.config.Thresholds
	if len(thresholds) == 0 {
		return
	}

	// build the ranking functions (if not yet known)
	// TODO: have a function in cadvisor that lets us know if global housekeeping has completed
	//如果管理器的资源评分函数和资源节点回收函数为空,则初始化这些函数
	if len(m.resourceToRankFunc) == 0 || len(m.resourceToNodeReclaimFuncs) == 0 {
		// this may error if cadvisor has yet to complete housekeeping, so we will just try again in next pass.
		//判断系统是否使用镜像文件系统
		hasDedicatedImageFs, err := diskInfoProvider.HasDedicatedImageFs()
		if err != nil {
			return
		}
		m.resourceToRankFunc = buildResourceToRankFunc(hasDedicatedImageFs)
		m.resourceToNodeReclaimFuncs = buildResourceToNodeReclaimFuncs(m.imageGC, hasDedicatedImageFs)
	}

	// make observations and get a function to derive pod usage stats relative to those observations.
	//通过资源统计接口,获取所有Signal对应资源结果,以及Pod统计的获取函数. observations是各驱逐信号对应资源的资源统计,statsFunc根据传递的ID返回一个Pod的资源统计
	observations, statsFunc, err := makeSignalObservations(m.summaryProvider)
	if err != nil {
		glog.Errorf("eviction manager: unexpected err: %v", err)
		return
	}

	// attempt to create a threshold notifier to improve eviction response time
	if m.config.KernelMemcgNotification && !m.notifiersInitialized {
		glog.Infof("eviction manager attempting to integrate with kernel memcg notification api")
		m.notifiersInitialized = true
		// start soft memory notification
		err = startMemoryThresholdNotifier(m.config.Thresholds, observations, false, func(desc string) {
			glog.Infof("soft memory eviction threshold crossed at %s", desc)
			// TODO wait grace period for soft memory limit
			m.synchronize(diskInfoProvider, podFunc)
		})
		if err != nil {
			glog.Warningf("eviction manager: failed to create hard memory threshold notifier: %v", err)
		}
		// start hard memory notification
		err = startMemoryThresholdNotifier(m.config.Thresholds, observations, true, func(desc string) {
			glog.Infof("hard memory eviction threshold crossed at %s", desc)
			m.synchronize(diskInfoProvider, podFunc)
		})
		if err != nil {
			glog.Warningf("eviction manager: failed to create soft memory threshold notifier: %v", err)
		}
	}

	// determine the set of thresholds met independent of grace period
	//检测驱逐信号对应的哪些资源已经达到了阈值
	thresholds = thresholdsMet(thresholds, observations, false)

	// determine the set of thresholds previously met that have not yet satisfied the associated min-reclaim
	//
	if len(m.thresholdsMet) > 0 {
		//检测未处理的阈值中,有哪些在加上资源可用量+最小回收量后, 仍达到阈值
		thresholdsNotYetResolved := thresholdsMet(m.thresholdsMet, observations, true)
		thresholds = mergeThresholds(thresholds, thresholdsNotYetResolved)
	}

	// determine the set of thresholds whose stats have been updated since the last sync
	//记录哪些达到阈值的驱逐信号对应的资源, 当前资源统计时间比上一次更新的
	thresholds = thresholdsUpdatedStats(thresholds, observations, m.lastObservations)

	// track when a threshold was first observed
	now := m.clock.Now()
	// 返回阈值信号表中的观察时间,如果是第一次观察到,则设置观察时间为now; 如果上次已经观察到,返回上一次观察时间
	thresholdsFirstObservedAt := thresholdsFirstObservedAt(thresholds, m.thresholdsFirstObservedAt, now)

	// the set of node conditions that are triggered by currently observed thresholds
	//根据已经达到驱逐信号阈值的,转换成相应节点状况
	nodeConditions := nodeConditions(thresholds)

	// track when a node condition was last observed
	//设置节点状况表的观察时间为now, 如果上次节点观察表中存在不存在于当前节点状况表的状况,将该状况和观察时间进行记录
	nodeConditionsLastObservedAt := nodeConditionsLastObservedAt(nodeConditions, m.nodeConditionsLastObservedAt, now)

	// node conditions report true if it has been observed within the transition period window
	//返回所有被观察时间距当前时间 小于指定周期的节点状况
	nodeConditions = nodeConditionsObservedSince(nodeConditionsLastObservedAt, m.config.PressureTransitionPeriod, now)

	// determine the set of thresholds we need to drive eviction behavior (i.e. all grace periods are met)
	// 返回观察的阈值信号表中,信号观察时间距离当前时间大于阈值宽限期的阈值信号
	thresholds = thresholdsMetGracePeriod(thresholdsFirstObservedAt, now)

	// update internal state
	m.Lock()
	m.nodeConditions = nodeConditions
	m.thresholdsFirstObservedAt = thresholdsFirstObservedAt
	m.nodeConditionsLastObservedAt = nodeConditionsLastObservedAt
	m.thresholdsMet = thresholds
	m.lastObservations = observations
	m.Unlock()

	// determine the set of resources under starvation
	//获取触发阈值信号的资源的列表
	starvedResources := getStarvedResources(thresholds)
	if len(starvedResources) == 0 {
		glog.V(3).Infof("eviction manager: no resources are starved")
		return
	}

	// rank the resources to reclaim by eviction priority
	//按资源类型优先级进行排序,memory类型优先级最高
	sort.Sort(byEvictionPriority(starvedResources))
	resourceToReclaim := starvedResources[0]
	glog.Warningf("eviction manager: attempting to reclaim %v", resourceToReclaim)

	// determine if this is a soft or hard eviction associated with the resource
	softEviction := isSoftEvictionThresholds(thresholds, resourceToReclaim)

	// record an event about the resources we are now attempting to reclaim via eviction
	m.recorder.Eventf(m.nodeRef, v1.EventTypeWarning, "EvictionThresholdMet", "Attempting to reclaim %s", resourceToReclaim)

	// check if there are node-level resources we can reclaim to reduce pressure before evicting end-user pods.
	if m.reclaimNodeLevelResources(resourceToReclaim, observations) {
		glog.Infof("eviction manager: able to reduce %v pressure without evicting pods.", resourceToReclaim)
		return
	}

	glog.Infof("eviction manager: must evict pod(s) to reclaim %v", resourceToReclaim)

	// rank the pods for eviction
	//获取指定资源的排序函数
	rank, ok := m.resourceToRankFunc[resourceToReclaim]
	if !ok {
		glog.Errorf("eviction manager: no ranking function for resource %s", resourceToReclaim)
		return
	}

	// the only candidates viable for eviction are those pods that had anything running.
	//podFunc是获取kubelet绑定的pod.status.phase没有处于successed和failed的所有Pod
	activePods := podFunc()
	if len(activePods) == 0 {
		glog.Errorf("eviction manager: eviction thresholds have been met, but no pods are active to evict")
		return
	}

	// rank the running pods for eviction for the specified resource
	//对	pod进行资源统计,并排序
	rank(activePods, statsFunc)

	glog.Infof("eviction manager: pods ranked for eviction: %s", format.Pods(activePods))

	// we kill at most a single pod during each eviction interval
	//遍历激活状态的Pod,驱逐一个pod 成功则退出
	for i := range activePods {
		pod := activePods[i]
		if kubepod.IsStaticPod(pod) {
			// The eviction manager doesn't evict static pods. To stop a static
			// pod, the admin needs to remove the manifest from kubelet's
			// --config directory.
			// TODO(39124): This is a short term fix, we can't assume static pods
			// are always well behaved.
			glog.Infof("eviction manager: NOT evicting static pod %v", pod.Name)
			continue
		}
		status := v1.PodStatus{
			Phase:   v1.PodFailed,
			Message: fmt.Sprintf(message, resourceToReclaim),
			Reason:  reason,
		}
		// record that we are evicting the pod
		m.recorder.Eventf(pod, v1.EventTypeWarning, reason, fmt.Sprintf(message, resourceToReclaim))
		gracePeriodOverride := int64(0)
		if softEviction {
			gracePeriodOverride = m.config.MaxPodGracePeriodSeconds
		}
		// this is a blocking call and should only return when the pod and its containers are killed.
		err := m.killPodFunc(pod, status, &gracePeriodOverride)
		if err != nil {
			glog.Infof("eviction manager: pod %s failed to evict %v", format.Pod(pod), err)
			continue
		}
		// success, so we return until the next housekeeping interval
		glog.Infof("eviction manager: pod %s evicted successfully", format.Pod(pod))
		return
	}
	glog.Infof("eviction manager: unable to evict any pods from the node")
}

// reclaimNodeLevelResources attempts to reclaim node level resources.  returns true if thresholds were satisfied and no pod eviction is required.
func (m *managerImpl) reclaimNodeLevelResources(resourceToReclaim v1.ResourceName, observations signalObservations) bool {
	//
	nodeReclaimFuncs := m.resourceToNodeReclaimFuncs[resourceToReclaim]
	for _, nodeReclaimFunc := range nodeReclaimFuncs {
		// attempt to reclaim the pressured resource.
		reclaimed, err := nodeReclaimFunc()
		if err == nil {
			// update our local observations based on the amount reported to have been reclaimed.
			// note: this is optimistic, other things could have been still consuming the pressured resource in the interim.
			signal := resourceToSignal[resourceToReclaim]
			value, ok := observations[signal]
			if !ok {
				glog.Errorf("eviction manager: unable to find value associated with signal %v", signal)
				continue
			}
			value.available.Add(*reclaimed)

			// evaluate all current thresholds to see if with adjusted observations, we think we have met min reclaim goals
			if len(thresholdsMet(m.thresholdsMet, observations, true)) == 0 {
				return true
			}
		} else {
			glog.Errorf("eviction manager: unexpected error when attempting to reduce %v pressure: %v", resourceToReclaim, err)
		}
	}
	return false
}
