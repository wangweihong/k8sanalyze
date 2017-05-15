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

package kubelet

import (
	"fmt"
	"sync"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/client/record"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
	"k8s.io/kubernetes/pkg/types"
	"k8s.io/kubernetes/pkg/util/runtime"
	"k8s.io/kubernetes/pkg/util/wait"
)

// OnCompleteFunc is a function that is invoked when an operation completes.
// If err is non-nil, the operation did not complete successfully.
type OnCompleteFunc func(err error)

// PodStatusFunc is a function that is invoked to generate a pod status.
type PodStatusFunc func(pod *v1.Pod, podStatus *kubecontainer.PodStatus) v1.PodStatus

// KillPodOptions are options when performing a pod update whose update type is kill.
type KillPodOptions struct {
	// PodStatusFunc is the function to invoke to set pod status in response to a kill request.
	PodStatusFunc PodStatusFunc
	// PodTerminationGracePeriodSecondsOverride is optional override to use if a pod is being killed as part of kill operation.
	PodTerminationGracePeriodSecondsOverride *int64
}

// UpdatePodOptions is an options struct to pass to a UpdatePod operation.
//Pod的更新参数,这是传递给Podworker的更新参数,将会转换成syncPodOptions.
type UpdatePodOptions struct {
	// pod to update
	Pod *v1.Pod //要更新的Pod
	// the mirror pod for the pod to update, if it is a static pod
	MirrorPod *v1.Pod
	// the type of update (create, update, sync, kill)
	UpdateType kubetypes.SyncPodType //更新类型
	// optional callback function when operation completes
	// this callback is not guaranteed to be completed since a pod worker may
	// drop update requests if it was fulfilling a previous request.  this is
	// only guaranteed to be invoked in response to a kill pod request which is
	// always delivered.
	OnCompleteFunc OnCompleteFunc // 更新完成后的回调函数
	// if update type is kill, use the specified options to kill the pod.
	KillPodOptions *KillPodOptions
}

// PodWorkers is an abstract interface for testability.
type PodWorkers interface {
	UpdatePod(options *UpdatePodOptions) //对指定的Pod进行更新(创建/更新,同步,杀死)
	ForgetNonExistingPodWorkers(desiredPods map[types.UID]empty)
	ForgetWorker(uid types.UID) //pod删除时调用,参考 kubelet.deletePOd.关闭指定Pod的goroutine
}

// syncPodOptions provides the arguments to a SyncPod operation.
//pod同步选项, pod worker将会将获取的UpdatePodOPtions转换成syncPodOptions,传递给podworkers.syncPodFn
type syncPodOptions struct {
	// the mirror pod for the pod to sync, if it is a static pod
	mirrorPod *v1.Pod
	// pod to sync
	pod *v1.Pod //这是api pod不是kubelet pod
	// the type of update (create, update, sync)
	updateType kubetypes.SyncPodType
	// the current status
	podStatus *kubecontainer.PodStatus //从pod cache(container/cache.go)中获取的Pod的最新的状态
	// if update type is kill, use the specified options to kill the pod.
	killPodOptions *KillPodOptions
}

// the function to invoke to perform a sync.
type syncPodFnType func(options syncPodOptions) error

//pod出栈的延迟时间
const (
	// jitter factor for resyncInterval
	workerResyncIntervalJitterFactor = 0.5

	// jitter factor for backOffPeriod
	workerBackOffPeriodJitterFactor = 0.5
)

//
type podWorkers struct {
	// Protects all per worker fields.
	podLock sync.Mutex

	// Tracks all running per-pod goroutines - per-pod goroutine will be
	// processing updates received through its corresponding channel.
	//见UpdatePod()
	podUpdates map[types.UID]chan UpdatePodOptions //每一个pod,都启动一个goroutine.当pod状态有更新时,将pod的状态写到对应的Pod的podUdates channel, 然后goroutine进行更新
	// Track the current state of per-pod goroutines.
	// Currently all update request for a given pod coming when another
	// update of this pod is being processed are ignored.
	isWorking map[types.UID]bool //用于保存per-groutine是否在工作.
	// Tracks the last undelivered work item for this pod - a work item is
	// undelivered if it comes in while the worker is working.
	lastUndeliveredWorkUpdate map[types.UID]UpdatePodOptions //在尝试更新新的状态,per-goroutine可能仍然在进行上一次的状态更新操作(isWorking==true).
	//因此将新的更新请求参数进行保存

	//更新Pod时,会将Pod加入工作对列中
	//注意这个队列不是传统意义上的队列, 每个pod一个元素.已存在的Pod重新入队,会更新出队时间
	workQueue queue.WorkQueue //工作队列, 加入了这个工作队列有什么用

	// This function is run to sync the desired stated of pod.
	// NOTE: This function has to be thread-safe - it can be called for
	// different pods at the same time.
	syncPodFn syncPodFnType // ??同步pod的状态?和apiserver同步?
	//实际进行Pod更新. kubelet.syncPod()实现.这是真正进行Pod更新的动作
	// The EventRecorder to use
	recorder record.EventRecorder

	// backOffPeriod is the duration to back off when there is a sync error.
	backOffPeriod time.Duration //重试周期

	// resyncInterval is the duration to wait until the next sync.
	resyncInterval time.Duration //重新同步周期

	// podCache stores kubecontainer.PodStatus for all pods.
	podCache kubecontainer.Cache //缓存Pode状态,获取状态时,需要指定哪个时间的状态
}

func newPodWorkers(syncPodFn syncPodFnType, recorder record.EventRecorder, workQueue queue.WorkQueue,
	resyncInterval, backOffPeriod time.Duration, podCache kubecontainer.Cache) *podWorkers {
	return &podWorkers{
		podUpdates:                map[types.UID]chan UpdatePodOptions{},
		isWorking:                 map[types.UID]bool{},
		lastUndeliveredWorkUpdate: map[types.UID]UpdatePodOptions{},
		syncPodFn:                 syncPodFn,
		recorder:                  recorder,
		workQueue:                 workQueue,
		resyncInterval:            resyncInterval,
		backOffPeriod:             backOffPeriod,
		podCache:                  podCache,
	}
}

//接收通过channel传递过来的Pod的状态,并进行同步
//见updatePdds
func (p *podWorkers) managePodLoop(podUpdates <-chan UpdatePodOptions) {
	var lastSyncTime time.Time
	//接收podUpdates Chan中的数据,一旦podUpdates chan被发送端关闭,则退出
	//根据缓存中Pod的状态,同步指定的Pod?
	for update := range podUpdates {
		err := func() error {
			//获取要更新的Pod的UID
			podUID := update.Pod.UID
			// This is a blocking call that would return only if the cache
			// has an entry for the pod that is newer than minRuntimeCache
			// Time. This ensures the worker doesn't start syncing until
			// after the cache is at least newer than the finished time of
			// the previous sync.
			//获得缓存中Pod的状态
			status, err := p.podCache.GetNewerThan(podUID, lastSyncTime)
			if err != nil {
				return err
			}
			//调用kubelet.syncPod进行更新
			err = p.syncPodFn(syncPodOptions{
				mirrorPod:      update.MirrorPod,
				pod:            update.Pod,
				podStatus:      status, //通过podCache获取pod的状态
				killPodOptions: update.KillPodOptions,
				updateType:     update.UpdateType,
			})
			lastSyncTime = time.Now() //更新时间
			return err
		}()
		// notify the call-back function if the operation succeeded or not
		//指定更新完成后的回调函数
		if update.OnCompleteFunc != nil {
			update.OnCompleteFunc(err)
		}
		//同步失败报错
		// 这是pod出错时describe中的打印
		if err != nil {
			glog.Errorf("Error syncing pod %s, skipping: %v", update.Pod.UID, err)
			p.recorder.Eventf(update.Pod, v1.EventTypeWarning, events.FailedSync, "Error syncing pod, skipping: %v", err)
		}
		//将po加入工作队列,并处理未传递的更新请求;如果没有未处理的,则更新worker为空闲状态
		p.wrapUp(update.Pod.UID, err)
	}
}

// Apply the new setting to the specified pod.
// If the options provide an OnCompleteFunc, the function is invoked if the update is accepted.
// Update requests are ignored if a kill pod request is pending.
//如果更新Pod请求相关的Pod没有启动pod worker,则启动pod worker,提交更新请求给Podworker.如果pod worker正在工作,
//则保存请求在未处理请求队列中
func (p *podWorkers) UpdatePod(options *UpdatePodOptions) {
	pod := options.Pod
	uid := pod.UID
	var podUpdates chan UpdatePodOptions
	var exists bool

	p.podLock.Lock()
	defer p.podLock.Unlock()
	//指定Pod的goroutine已经不存在,则创建pod worker
	if podUpdates, exists = p.podUpdates[uid]; !exists {
		// We need to have a buffer here, because checkForUpdates() method that
		// puts an update into channel is called from the same goroutine where
		// the channel is consumed. However, it is guaranteed that in such case
		// the channel is empty, so buffer of size 1 is enough.
		podUpdates = make(chan UpdatePodOptions, 1)
		p.podUpdates[uid] = podUpdates

		// Creating a new pod worker either means this is a new pod, or that the
		// kubelet just restarted. In either case the kubelet is willing to believe
		// the status of the pod for the first pod worker sync. See corresponding
		// comment in syncPod.
		//启动一个goroutine(podWorker),接收podUpdates Channel传递的状态.
		go func() {
			defer runtime.HandleCrash()
			p.managePodLoop(podUpdates)
		}()
	}
	//如果go routine没有在工作,则同步状态
	if !p.isWorking[pod.UID] {
		p.isWorking[pod.UID] = true
		podUpdates <- *options
	} else {
		// if a request to kill a pod is pending, we do not let anything overwrite that request.
		//如果per-goroutine仍然在工作, 则检测是否仍然存在未传递的更新事件
		//如果没有未传递的更新事件或者其更新类型不是杀死Pod,则覆盖旧的更新的参数
		update, found := p.lastUndeliveredWorkUpdate[pod.UID]
		if !found || update.UpdateType != kubetypes.SyncPodKill {
			p.lastUndeliveredWorkUpdate[pod.UID] = *options //记录未处理的更新请求
		}
	}
}

//关闭per-pod goroutine,清除指定的Pod
func (p *podWorkers) removeWorker(uid types.UID) {
	if ch, ok := p.podUpdates[uid]; ok {
		//关闭指定的channel,会导致per-pod goroutine退出
		close(ch)
		delete(p.podUpdates, uid)
		// If there is an undelivered work update for this pod we need to remove it
		// since per-pod goroutine won't be able to put it to the already closed
		// channel when it finish processing the current work update.
		if _, cached := p.lastUndeliveredWorkUpdate[uid]; cached {
			delete(p.lastUndeliveredWorkUpdate, uid)
		}
	}
}

//关闭指定的Pod的per-pod goroutine
func (p *podWorkers) ForgetWorker(uid types.UID) {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	p.removeWorker(uid)
}

//关闭不存在于desirePods中的per-pod gorourine
func (p *podWorkers) ForgetNonExistingPodWorkers(desiredPods map[types.UID]empty) {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	for key := range p.podUpdates {
		if _, exists := desiredPods[key]; !exists {
			p.removeWorker(key)
		}
	}
}

//将po加入工作队列,并处理未传递的更新请求;如果没有未处理的,则更新worker为空闲状态
func (p *podWorkers) wrapUp(uid types.UID, syncErr error) {
	// Requeue the last update if the last sync returned error.
	switch {
	case syncErr == nil:
		// No error; requeue at the regular resync interval.
		//将指定的Pod入栈加入队列,并设置出栈时间
		p.workQueue.Enqueue(uid, wait.Jitter(p.resyncInterval, workerResyncIntervalJitterFactor))
	default:
		// Error occurred during the sync; back off and then retry.
		//将指定的Pod入栈加入队列,并设置出栈时间(和上面时间不同)
		p.workQueue.Enqueue(uid, wait.Jitter(p.backOffPeriod, workerBackOffPeriodJitterFactor))
	}
	p.checkForUpdates(uid)
}

//处理pod的未传递更新请求;如果没有,则更新per-podworke的工作状态
func (p *podWorkers) checkForUpdates(uid types.UID) {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	//如果Pod有未传递的更新请求
	if workUpdate, exists := p.lastUndeliveredWorkUpdate[uid]; exists {
		//更新未处理的更新请求
		p.podUpdates[uid] <- workUpdate
		delete(p.lastUndeliveredWorkUpdate, uid)
	} else {
		p.isWorking[uid] = false
	}
}

// killPodNow returns a KillPodFunc that can be used to kill a pod.
// It is intended to be injected into other modules that need to kill a pod.
//返回Pod清理函数
func killPodNow(podWorkers PodWorkers, recorder record.EventRecorder) eviction.KillPodFunc {
	return func(pod *v1.Pod, status v1.PodStatus, gracePeriodOverride *int64) error {
		// determine the grace period to use when killing the pod
		gracePeriod := int64(0)
		//如果指定了强制优雅结束时间, 则覆盖pod默认的优雅结束时间
		if gracePeriodOverride != nil {
			gracePeriod = *gracePeriodOverride
		} else if pod.Spec.TerminationGracePeriodSeconds != nil {
			gracePeriod = *pod.Spec.TerminationGracePeriodSeconds
		}

		// we timeout and return an error if we don't get a callback within a reasonable time.
		// the default timeout is relative to the grace period (we settle on 2s to wait for kubelet->runtime traffic to complete in sigkill)
		timeout := int64(gracePeriod + (gracePeriod / 2))
		minTimeout := int64(2)
		if timeout < minTimeout {
			timeout = minTimeout
		}
		timeoutDuration := time.Duration(timeout) * time.Second

		// open a channel we block against until we get a result
		type response struct {
			err error
		}
		ch := make(chan response)
		podWorkers.UpdatePod(&UpdatePodOptions{
			Pod:        pod,
			UpdateType: kubetypes.SyncPodKill,
			OnCompleteFunc: func(err error) {
				ch <- response{err: err}
			},
			KillPodOptions: &KillPodOptions{
				PodStatusFunc: func(p *v1.Pod, podStatus *kubecontainer.PodStatus) v1.PodStatus {
					return status
				},
				PodTerminationGracePeriodSecondsOverride: gracePeriodOverride,
			},
		})

		// wait for either a response, or a timeout
		select {
		case r := <-ch:
			return r.err
		case <-time.After(timeoutDuration):
			recorder.Eventf(pod, v1.EventTypeWarning, events.ExceededGracePeriod, "Container runtime did not kill the pod within specified grace period.")
			return fmt.Errorf("timeout waiting to kill pod")
		}
	}
}
