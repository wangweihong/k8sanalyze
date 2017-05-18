/*
Copyright 2015 The Kubernetes Authors.

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

package pleg

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/types"
	"k8s.io/kubernetes/pkg/util/clock"
	"k8s.io/kubernetes/pkg/util/sets"
	"k8s.io/kubernetes/pkg/util/wait"
)

// GenericPLEG is an extremely simple generic PLEG that relies solely on
// periodic listing to discover container changes. It should be used
// as temporary replacement for container runtimes do not support a proper
// event generator yet.
//
// Note that GenericPLEG assumes that a container would not be created,
// terminated, and garbage collected within one relist period. If such an
// incident happens, GenenricPLEG would miss all events regarding this
// container. In the case of relisting failure, the window may become longer.
// Note that this assumption is not unique -- many kubelet internal components
// rely on terminated containers as tombstones for bookkeeping purposes. The
// garbage collector is implemented to work with such situations. However, to
// guarantee that kubelet can handle missing container events, it is
// recommended to set the relist period short and have an auxiliary, longer
// periodic sync in kubelet as the safety net.
//pleg，即PodLifecycleEventGenerator，pleg会记录Pod生命周期中的各种事件，如容器的启动、终止等，这些事件会写入etcd中，同时他检测到container异常退出时，他会通知kubelet，然后重启创建该container
//http://licyhust.com/%E5%AE%B9%E5%99%A8%E6%8A%80%E6%9C%AF/2016/10/27/kubelet-2/
type GenericPLEG struct {
	// The period for relisting.
	relistPeriod time.Duration //relist的作用是什么?默认的relist周期为1秒.注释中提到,如果网络稳定的话,可以使用更长的周期,见后面注释
	// The container runtime.
	runtime kubecontainer.Runtime //实际处理容器/Pod的接口,实际上就是docker/rkt/cri 容器管理器.
	// The channel from which the subscriber listens events.
	eventChannel chan *PodLifecycleEvent //用于传递Pod生命周期事件
	// The internal cache for pod/container information.
	podRecords podRecords //里面有两个表.old表保存着上一次relist时kubelet pods的数据. current则是当前relist获取到的pod的数据.
	// Time of the last relisting.
	relistTime atomic.Value //上一次relist时间
	// Cache for storing the runtime states required for syncing pods.
	cache kubecontainer.Cache //Pod status cache,缓存Pod的状态
	// For testability.
	clock clock.Clock
	// Pods that failed to have their status retrieved during a relist. These pods will be
	// retried during the next relisting.
	podsToReinspect map[types.UID]*kubecontainer.Pod //uuid映射的kubelet pod.这个表的作用.在relist时,出现获取指定Pod status失败(即使失败,也会将Pod status以及失败信息缓存到pod status cache中),
	//,则将该Pod记录到该表中.等待下一次relist,重新更新缓存中的状态
}

// plegContainerState has a one-to-one mapping to the
// kubecontainer.ContainerState except for the non-existent state. This state
// is introduced here to complete the state transition scenarios.
type plegContainerState string

const (
	plegContainerRunning     plegContainerState = "running"
	plegContainerExited      plegContainerState = "exited"
	plegContainerUnknown     plegContainerState = "unknown" //容器为Created状态或者Unkown时
	plegContainerNonExistent plegContainerState = "non-existent"
)

//将kubelet容器的状态转换成pleg状态
//
func convertState(state kubecontainer.ContainerState) plegContainerState {
	switch state {
	case kubecontainer.ContainerStateCreated:
		// kubelet doesn't use the "created" state yet, hence convert it to "unknown".
		return plegContainerUnknown
	case kubecontainer.ContainerStateRunning:
		return plegContainerRunning
	case kubecontainer.ContainerStateExited:
		return plegContainerExited
	case kubecontainer.ContainerStateUnknown:
		return plegContainerUnknown
	default:
		panic(fmt.Sprintf("unrecognized container state: %v", state))
	}
}

//新旧Pod怎么划分?两次relist时的pod
type podRecord struct {
	old     *kubecontainer.Pod //上一次relist时,kubelet上所有pod的数据
	current *kubecontainer.Pod //这一次relist时,kubelet上所有Pod的数据
}

type podRecords map[types.UID]*podRecord

//默认的relist周期为1秒
func NewGenericPLEG(runtime kubecontainer.Runtime, channelCapacity int,
	relistPeriod time.Duration, cache kubecontainer.Cache, clock clock.Clock) PodLifecycleEventGenerator {
	return &GenericPLEG{
		relistPeriod: relistPeriod,
		runtime:      runtime,
		eventChannel: make(chan *PodLifecycleEvent, channelCapacity),
		podRecords:   make(podRecords),
		cache:        cache, //pod 状态缓存
		clock:        clock,
	}
}

// Returns a channel from which the subscriber can receive PodLifecycleEvent
// events.
// TODO: support multiple subscribers.
//被kubelet.syncLoop()调用
func (g *GenericPLEG) Watch() chan *PodLifecycleEvent {
	return g.eventChannel
}

// Start spawns a goroutine to relist periodically.
//每隔1秒执行relist函数
func (g *GenericPLEG) Start() {
	go wait.Until(g.relist, g.relistPeriod, wait.NeverStop)
}

func (g *GenericPLEG) Healthy() (bool, error) {
	//获取上一次relist的时间
	relistTime := g.getRelistTime()
	// TODO: Evaluate if we can reduce this threshold.
	// The threshold needs to be greater than the relisting period + the
	// relisting time, which can vary significantly. Set a conservative
	// threshold so that we don't cause kubelet to be restarted unnecessarily.
	threshold := 2 * time.Minute
	//如果上一次relist时间到当前时间超过了两分钟,则超时报错
	if g.clock.Since(relistTime) > threshold {
		return false, fmt.Errorf("pleg was last seen active at %v", relistTime)
	}
	return true, nil
}

//根据Pod中容器cid的新旧两次状态,转换成pod生命周期事件
func generateEvents(podID types.UID, cid string, oldState, newState plegContainerState) []*PodLifecycleEvent {
	if newState == oldState {
		return nil
	}

	glog.V(4).Infof("GenericPLEG: %v/%v: %v -> %v", podID, cid, oldState, newState)
	switch newState {
	case plegContainerRunning:
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerStarted, Data: cid}}
	case plegContainerExited:
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerDied, Data: cid}}
	case plegContainerUnknown:
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerChanged, Data: cid}}
		//当pod中容器的状态为
	case plegContainerNonExistent:
		switch oldState {
		case plegContainerExited:
			// We already reported that the container died before.
			return []*PodLifecycleEvent{{ID: podID, Type: ContainerRemoved, Data: cid}}
		default:
			return []*PodLifecycleEvent{{ID: podID, Type: ContainerDied, Data: cid}, {ID: podID, Type: ContainerRemoved, Data: cid}}
		}
	default:
		panic(fmt.Sprintf("unrecognized container state: %v", newState))
	}
}

//获取上一次relist时间
func (g *GenericPLEG) getRelistTime() time.Time {
	val := g.relistTime.Load()
	if val == nil {
		return time.Time{}
	}
	return val.(time.Time)
}

//更新上一次relist时间
func (g *GenericPLEG) updateRelisTime(timestamp time.Time) {
	g.relistTime.Store(timestamp)
}

// relist queries the container runtime for list of pods/containers, compare
// with the internal pods/containers, and generats events accordingly.
// 获取kubelet上的Pod列表,结合记录podrecord上一次relist时pod的信息,创建pleg事件.
// 将pod最新的状态保存在pod status cache中(如果获取pod最新状态失败,仍然缓存状态以及错误信息,将失败的Pod保存到podsToReinspect表中),并发送pleg事件到eventChannel.最后更新pod status cache的全局时间栈
func (g *GenericPLEG) relist() {
	glog.V(5).Infof("GenericPLEG: Relisting")

	//如果之前进行过relist
	if lastRelistTime := g.getRelistTime(); !lastRelistTime.IsZero() {
		// 利用普罗米修斯采集数据?采集每次relist的时间?
		metrics.PLEGRelistInterval.Observe(metrics.SinceInMicroseconds(lastRelistTime))
	}

	//当前时间
	timestamp := g.clock.Now()
	// Update the relist time.
	//更新上一次Relist实际时间为当前时间
	g.updateRelisTime(timestamp)
	defer func() {
		//采集一次relist的延迟时间
		metrics.PLEGRelistLatency.Observe(metrics.SinceInMicroseconds(timestamp))
	}()

	// Get all the pods.
	//根据已存在的容器,创建Pod列表
	podList, err := g.runtime.GetPods(true)
	if err != nil {
		glog.Errorf("GenericPLEG: Unable to retrieve pods: %v", err)
		return
	}
	//转换成kubelet pod
	pods := kubecontainer.Pods(podList)
	//将所有Pod的信息记录到Pod记录表的current表(current表旧的数据已经被移除)
	// 这里有一个问题,old表的数据还存的是以前的数据.但current表的数据已经换成最新的数据了?
	//在进行事件处理时,会将podRecord中current表的数据覆盖old表
	//所以每一次relist时,到当前未知,pod record的表中current和old的数据是一样的(除非一些新的Pod)
	g.podRecords.setCurrent(pods)

	// Compare the old and the current pods, and generate events.
	eventsByPodID := map[types.UID][]*PodLifecycleEvent{}
	//
	for pid := range g.podRecords {
		oldPod := g.podRecords.getOld(pid)
		pod := g.podRecords.getCurrent(pid)
		// Get all containers in the old and the new pod.
		//根据Pod之前和当前容器的状态,生成PodLifecycleEvent
		allContainers := getContainersFromPods(oldPod, pod)
		///根据容器状态生成Pod生命周期事件
		for _, container := range allContainers {
			events := computeEvents(oldPod, pod, &container.ID)
			for _, e := range events {
				//记录Pod Lifecycle事件到eventsByPodID表中
				updateEvents(eventsByPodID, e)
			}
		}
	}

	var needsReinspection map[types.UID]*kubecontainer.Pod
	//如果pleg关联了Pod Status cache
	if g.cacheEnabled() {
		needsReinspection = make(map[types.UID]*kubecontainer.Pod)
	}

	// If there are events associated with a pod, we should update the
	// podCache.
	//遍历所有PLE事件
	for pid, events := range eventsByPodID {
		pod := g.podRecords.getCurrent(pid)
		if g.cacheEnabled() {
			// updateCache() will inspect the pod and update the cache. If an
			// error occurs during the inspection, we want PLEG to retry again
			// in the next relist. To achieve this, we do not update the
			// associated podRecord of the pod, so that the change will be
			// detect again in the next relist.
			// TODO: If many pods changed during the same relist period,
			// inspecting the pod and getting the PodStatus to update the cache
			// serially may take a while. We should be aware of this and
			// parallelize if needed.
			//获取Pod的状态,并缓存到pod status cache中,如果获取Pod状态出错
			if err := g.updateCache(pod, pid); err != nil {
				glog.Errorf("PLEG: Ignoring events for pod %s/%s: %v", pod.Name, pod.Namespace, err)

				// make sure we try to reinspect the pod during the next relisting
				//记录出错的Pod到表中
				needsReinspection[pid] = pod

				continue
				//缓存状态成功,如果pod存在于podsToReinspect表中,则移除
				//上一次relist时,该pod获取状态失败
			} else if _, found := g.podsToReinspect[pid]; found {
				// this pod was in the list to reinspect and we did so because it had events, so remove it
				// from the list (we don't want the reinspection code below to inspect it a second time in
				// this relist execution)
				delete(g.podsToReinspect, pid)
			}
		}
		// Update the internal storage and send out the events.
		//将pod record中current pod转换成old pod
		g.podRecords.update(pid)
		for i := range events {
			// Filter out events that are not reliable and no other components use yet.
			//忽略ContainerChanged事件
			if events[i].Type == ContainerChanged {
				continue
			}
			//发送事件
			g.eventChannel <- events[i]
		}
	}

	if g.cacheEnabled() {
		// reinspect any pods that failed inspection during the previous relist
		if len(g.podsToReinspect) > 0 {
			glog.V(5).Infof("GenericPLEG: Reinspecting pods that previously failed inspection")
			//之前relist 获取pod状态失败并缓存到Pod status Cache中d的Pod,再次获取pod状态并进行缓存
			for pid, pod := range g.podsToReinspect {
				if err := g.updateCache(pod, pid); err != nil {
					glog.Errorf("PLEG: pod %s/%s failed reinspection: %v", pod.Name, pod.Namespace, err)
					//仍然失败,
					needsReinspection[pid] = pod
				}
			}
		}

		// Update the cache timestamp.  This needs to happen *after*
		// all pods have been properly updated in the cache.
		//更新Pod Cache的全局TimeStamp
		g.cache.UpdateTime(timestamp)
	}

	// make sure we retain the list of pods that need reinspecting the next time relist is called
	//记录获取Pod Stataus失败的Pod的信息
	g.podsToReinspect = needsReinspection
}

//从Pod中获取容器(以及SandBox)列表
func getContainersFromPods(pods ...*kubecontainer.Pod) []*kubecontainer.Container {
	cidSet := sets.NewString()
	var containers []*kubecontainer.Container
	for _, p := range pods {
		if p == nil {
			continue
		}
		for _, c := range p.Containers {
			cid := string(c.ID.ID)
			if cidSet.Has(cid) {
				continue
			}
			cidSet.Insert(cid)
			containers = append(containers, c)
		}
		// Update sandboxes as containers
		// TODO: keep track of sandboxes explicitly.
		for _, c := range p.Sandboxes {
			cid := string(c.ID.ID)
			if cidSet.Has(cid) {
				continue
			}
			cidSet.Insert(cid)
			containers = append(containers, c)
		}

	}
	return containers
}

//根据容器生成Pod生命周期事件
func computeEvents(oldPod, newPod *kubecontainer.Pod, cid *kubecontainer.ContainerID) []*PodLifecycleEvent {
	var pid types.UID
	if oldPod != nil {
		pid = oldPod.ID
	} else if newPod != nil {
		pid = newPod.ID
	}
	oldState := getContainerState(oldPod, cid)
	newState := getContainerState(newPod, cid)
	return generateEvents(pid, cid.ID, oldState, newState)
}

func (g *GenericPLEG) cacheEnabled() bool {
	return g.cache != nil
}

//获取Pod的状态,并且缓存在PodCache中.注意获取Pod状态时可能会出错.即使出现错误相应状态,出错信息缓存到Pod status cache中
func (g *GenericPLEG) updateCache(pod *kubecontainer.Pod, pid types.UID) error {
	if pod == nil {
		// The pod is missing in the current relist. This means that
		// the pod has no visible (active or inactive) containers.
		glog.V(4).Infof("PLEG: Delete status for pod %q", string(pid))
		g.cache.Delete(pid)
		return nil
	}
	timestamp := g.clock.Now()
	// TODO: Consider adding a new runtime method
	// GetPodStatus(pod *kubecontainer.Pod) so that Docker can avoid listing
	// all containers again.
	//获取kubelet Pod的状态,实际上就是Container的状态
	status, err := g.runtime.GetPodStatus(pod.ID, pod.Name, pod.Namespace)
	glog.V(4).Infof("PLEG: Write status for %s/%s: %#v (err: %v)", pod.Name, pod.Namespace, status, err)
	g.cache.Set(pod.ID, status, err, timestamp) //更新到pod cache中,如果出错,包含错误
	return err
}

// 添加ple事件
func updateEvents(eventsByPodID map[types.UID][]*PodLifecycleEvent, e *PodLifecycleEvent) {
	if e == nil {
		return
	}
	eventsByPodID[e.ID] = append(eventsByPodID[e.ID], e)
}

//获取Pod中指定容器的pleg状态
func getContainerState(pod *kubecontainer.Pod, cid *kubecontainer.ContainerID) plegContainerState {
	// Default to the non-existent state.
	state := plegContainerNonExistent
	if pod == nil {
		return state
	}
	c := pod.FindContainerByID(*cid)
	if c != nil {
		return convertState(c.State)
	}
	// Search through sandboxes too.
	c = pod.FindSandboxByID(*cid)
	if c != nil {
		return convertState(c.State)
	}

	return state
}

//返回指定kubelet Pod旧的数据
func (pr podRecords) getOld(id types.UID) *kubecontainer.Pod {
	r, ok := pr[id]
	if !ok {
		return nil
	}
	return r.old
}

//返回指定kubelet Pod当前的数据
func (pr podRecords) getCurrent(id types.UID) *kubecontainer.Pod {
	r, ok := pr[id]
	if !ok {
		return nil
	}
	return r.current
}

//在Pod记录表中记录当前所有Pod的信息
func (pr podRecords) setCurrent(pods []*kubecontainer.Pod) {
	//??不先把旧的current表的pod移到old表中?
	for i := range pr {
		pr[i].current = nil
	}
	for _, pod := range pods {
		if r, ok := pr[pod.ID]; ok {
			r.current = pod
		} else {
			pr[pod.ID] = &podRecord{current: pod}
		}
	}
}

//将指定Pod的current pod record转换成old pod record
func (pr podRecords) update(id types.UID) {
	r, ok := pr[id]
	if !ok {
		return
	}
	pr.updateInternal(id, r)
}

//更新podRecord记录,current表的Pod转成old表
func (pr podRecords) updateInternal(id types.UID, r *podRecord) {
	if r.current == nil {
		// Pod no longer exists; delete the entry.
		delete(pr, id)
		return
	}
	r.old = r.current
	r.current = nil
}
