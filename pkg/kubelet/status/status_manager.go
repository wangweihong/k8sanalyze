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

package status

import (
	"sort"
	"sync"
	"time"

	clientset "k8s.io/kubernetes/pkg/client/clientset_generated/clientset"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/v1"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	metav1 "k8s.io/kubernetes/pkg/apis/meta/v1"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/types"
	"k8s.io/kubernetes/pkg/util/diff"
	"k8s.io/kubernetes/pkg/util/wait"
)

// A wrapper around v1.PodStatus that includes a version to enforce that stale pod statuses are
// not sent to the API server.
//缓存的是api pod的状态
//为什么要缓存?猜想,因为所有的更新pod状态请求都是发送给manager.podStatusChannel,交由syncPod()去处理
//当同时由多个针对于某个pod的请求时,每次只有一个pod能被处理.因为状态更新请求都是有时间性的,
//这样就必须有一个方式,能够知道最新的状态更新请求.就可以知道哪些更新请求可以被忽略(因为已经过期了)
type versionedPodStatus struct {
	status v1.PodStatus //pod的状态
	// Monotonically increasing version number (per pod).
	version uint64 //pod的版本号,通过和manager.apiStatusVersions比较,就可以知道缓存的状态是否是最新的
	//这个可以理解为向syncPod()发起的pod的更新请求的版本号.
	//新的状态会在老的状态上加1.见SetPodStatus()
	// Pod name & namespace, for sending updates to API server.
	podName      string
	podNamespace string
}

//请求同步格式, 将会发送该格式的数据给manager.podStatusChannel,来同步相应的pod
type podStatusSyncRequest struct {
	podUID types.UID
	status versionedPodStatus
}

// Updates pod statuses in apiserver. Writes only when new status has changed.
// All methods are thread-safe.
//用来更新apiserver中pod的状态
type manager struct {
	kubeClient clientset.Interface // 访问api server的client,创建manager时必须配置,否则statusManager无法正常启动
	podManager kubepod.Manager     //kubelet的pod管理器
	// Map from pod UID to sync status of the corresponding pod.
	podStatuses map[types.UID]versionedPodStatus //pod状态的缓存列表,缓存的是pod的最新状态更新请求的版本号.
	//这个版本号只是记录最新的状态请求是哪个.新的请求会覆盖旧的.(因为旧的请求已经提交syncPod()等待处理,所以并不影响).
	//生成见updateStatusInternal()
	//syncBatch()将会根据这里缓存的版本号和apiStatusVersion做对比,发起更新请求
	podStatusesLock  sync.RWMutex
	podStatusChannel chan podStatusSyncRequest //发送请求给管理器来同步指定pod的状态到apiserver中
	//见Manager.SetPodStatus在对新的状态进行处理后将会发送该Channel中
	//Manager.Start()执行后,会起一个goroutine,无限等待该Channel上的值
	// Map from (mirror) pod UID to latest status version successfully sent to the API server.
	// apiStatusVersions must only be accessed from the sync thread.
	apiStatusVersions map[types.UID]uint64 //见syncPod(),用于缓存最新的同步到apiserver中Pod状态的版本..也就是最新一次syncPod()处理的pod状态更新请求中的版本号.
	//通过查询这个表,就可以知道pod同步的最新版本号.syncPod()就可以根据这个版本判定pod的某个状态更新请求是否过期.
	//见为什么要换存状态的猜想
}

// PodStatusProvider knows how to provide status for a pod.  It's intended to be used by other components
// that need to introspect status.
type PodStatusProvider interface {
	// GetPodStatus returns the cached status for the provided pod UID, as well as whether it
	// was a cache hit.
	GetPodStatus(uid types.UID) (v1.PodStatus, bool)
}

// Manager is the Source of truth for kubelet pod status, and should be kept up-to-date with
// the latest v1.PodStatus. It also syncs updates back to the API server.
// 状态管理器
type Manager interface {
	PodStatusProvider

	// Start the API server status sync loop.
	Start()

	// SetPodStatus caches updates the cached status for the given pod, and triggers a status update.
	//更新指定的api pod的pod status
	SetPodStatus(pod *v1.Pod, status v1.PodStatus) //只被kubelet的syncPod,rejectPod调用,status为Pod新的状态

	// SetContainerReadiness updates the cached container status with the given readiness, and
	// triggers a status update.
	SetContainerReadiness(podUID types.UID, containerID kubecontainer.ContainerID, ready bool) //被pkg/kubelet/prober/prober_manager.go UpdateReadiness调用

	// TerminatePod resets the container status for the provided pod to terminated and triggers
	// a status update.
	TerminatePod(pod *v1.Pod) //设置pod中所有容器的状态中的Terminated,然后更新
	//只被kubelet的dispatchWork()调用

	// RemoveOrphanedStatuses scans the status cache and removes any entries for pods not included in
	// the provided podUIDs.
	RemoveOrphanedStatuses(podUIDs map[types.UID]bool)
}

//状态同步周期
const syncPeriod = 10 * time.Second

//pod manager是kubelet的podManager
func NewManager(kubeClient clientset.Interface, podManager kubepod.Manager) Manager {
	return &manager{
		kubeClient:        kubeClient,
		podManager:        podManager,
		podStatuses:       make(map[types.UID]versionedPodStatus),
		podStatusChannel:  make(chan podStatusSyncRequest, 1000), // Buffer up to 1000 statuses
		apiStatusVersions: make(map[types.UID]uint64),
	}
}

// isStatusEqual returns true if the given pod statuses are equal, false otherwise.
// This method normalizes the status before comparing so as to make sure that meaningless
// changes will be ignored.
//检测两个pod的状态是否相同
func isStatusEqual(oldStatus, status *v1.PodStatus) bool {
	return api.Semantic.DeepEqual(status, oldStatus)
}

//每隔10s同步
func (m *manager) Start() {
	// Don't start the status manager if we don't have a client. This will happen
	// on the master, where the kubelet is responsible for bootstrapping the pods
	// of the master components.
	if m.kubeClient == nil {
		glog.Infof("Kubernetes client is nil, not starting status manager.")
		return
	}

	glog.Info("Starting to sync pod status with apiserver")
	syncTicker := time.Tick(syncPeriod) //创建一个10秒的滴答器
	//启动一个永久的goroutine,
	// syncPod and syncBatch share the same go routine to avoid sync races.
	go wait.Forever(func() {
		select {
		//如果接收到同步请求,
		case syncRequest := <-m.podStatusChannel:
			//更新api server中指定的Pod的状态
			m.syncPod(syncRequest.podUID, syncRequest.status)
			//如果10秒超时
		case <-syncTicker:
			m.syncBatch() //遍历所有缓存的pod状态,找到需要更新到apiserver中pod的状态,更新到apiserver中
		}
	}, 0)
}

//获得缓存中指定pod的状态
func (m *manager) GetPodStatus(uid types.UID) (v1.PodStatus, bool) {
	m.podStatusesLock.RLock()
	defer m.podStatusesLock.RUnlock()
	status, ok := m.podStatuses[m.podManager.TranslatePodUID(uid)]
	return status.status, ok
}

//更新apiserver中指定Pod的状态
func (m *manager) SetPodStatus(pod *v1.Pod, status v1.PodStatus) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()
	// Make sure we're caching a deep copy.
	//拷贝新的状态
	status, err := copyStatus(&status)
	if err != nil {
		return
	}
	// Force a status update if deletion timestamp is set. This is necessary
	// because if the pod is in the non-running state, the pod worker still
	// needs to be able to trigger an update and/or deletion.
	//
	m.updateStatusInternal(pod, status, pod.DeletionTimestamp != nil)
}

//设置指定Pod中指定容器的Ready,并生成新的PodReady condition,然后更新整个pod的Status
func (m *manager) SetContainerReadiness(podUID types.UID, containerID kubecontainer.ContainerID, ready bool) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	//获得指定的Pod
	pod, ok := m.podManager.GetPodByUID(podUID)
	if !ok {
		glog.V(4).Infof("Pod %q has been deleted, no need to update readiness", string(podUID))
		return
	}
	//如果缓存中存在该pod的状态
	oldStatus, found := m.podStatuses[pod.UID]
	if !found {
		glog.Warningf("Container readiness changed before pod has synced: %q - %q",
			format.Pod(pod), containerID.String())
		return
	}

	// Find the container to update.
	//找到指定的容器的状态
	containerStatus, _, ok := findContainerStatus(&oldStatus.status, containerID.String())
	if !ok {
		glog.Warningf("Container readiness changed for unknown container: %q - %q",
			format.Pod(pod), containerID.String())
		return
	}

	//容器的readiness探测结果等于预期的结果,则返回
	if containerStatus.Ready == ready {
		glog.V(4).Infof("Container readiness unchanged (%v): %q - %q", ready,
			format.Pod(pod), containerID.String())
		return
	}

	// Make sure we're not updating the cached version.
	// 拷贝缓存中的pod状态
	status, err := copyStatus(&oldStatus.status)
	if err != nil {
		return
	}
	//找到指定的容器,设置其Readiness probe状态
	containerStatus, _, _ = findContainerStatus(&status, containerID.String())
	//设置readiness probe状态
	containerStatus.Ready = ready

	// Update pod condition.
	readyConditionIndex := -1
	for i, condition := range status.Conditions {
		if condition.Type == v1.PodReady {
			readyConditionIndex = i
			break
		}
	}

	//根据Pod中容器的状态以及Pod的Phase,生成PodReady Condition
	readyCondition := GeneratePodReadyCondition(&pod.Spec, status.ContainerStatuses, status.Phase)

	if readyConditionIndex != -1 {
		status.Conditions[readyConditionIndex] = readyCondition
		//之前没有pod Ready Condition
	} else {
		glog.Warningf("PodStatus missing PodReady condition: %+v", status)
		status.Conditions = append(status.Conditions, readyCondition)
	}

	m.updateStatusInternal(pod, status, false)
}

//找到指定的容器的状态,如果是init容器也返回其状态
func findContainerStatus(status *v1.PodStatus, containerID string) (containerStatus *v1.ContainerStatus, init bool, ok bool) {
	// Find the container to update.

	for i, c := range status.ContainerStatuses {
		if c.ContainerID == containerID {
			return &status.ContainerStatuses[i], false, true
		}
	}

	for i, c := range status.InitContainerStatuses {
		if c.ContainerID == containerID {
			return &status.InitContainerStatuses[i], true, true
		}
	}

	return nil, false, false

}

//设置Pod中所有的容器的状态的Terminated
func (m *manager) TerminatePod(pod *v1.Pod) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	oldStatus := &pod.Status
	if cachedStatus, ok := m.podStatuses[pod.UID]; ok {
		oldStatus = &cachedStatus.status
	}
	//拷贝pod的状态
	status, err := copyStatus(oldStatus)
	if err != nil {
		return
	}
	//设置Pod中所有容器Status中的Terminated项
	for i := range status.ContainerStatuses {
		status.ContainerStatuses[i].State = v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{},
		}
	}
	//设置Pod中所有Init容器Status中的Terminated项
	for i := range status.InitContainerStatuses {
		status.InitContainerStatuses[i].State = v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{},
		}
	}
	m.updateStatusInternal(pod, pod.Status, true)
}

// updateStatusInternal updates the internal status cache, and queues an update to the api server if
// necessary. Returns whether an update was triggered.
// This method IS NOT THREAD SAFE and must be called from a locked function.
//forceUpdate:是否强制更新
//status为新的状态
func (m *manager) updateStatusInternal(pod *v1.Pod, status v1.PodStatus, forceUpdate bool) bool {
	var oldStatus v1.PodStatus
	//获取pod的旧状态,如果pod没有缓存状态,则当前状态为旧状态;
	cachedStatus, isCached := m.podStatuses[pod.UID]
	if isCached {
		oldStatus = cachedStatus.status
	} else if mirrorPod, ok := m.podManager.GetMirrorPodByPod(pod); ok {
		oldStatus = mirrorPod.Status
	} else {
		oldStatus = pod.Status
	}

	// Set ReadyCondition.LastTransitionTime.
	//对比新旧状态的ReadyCondition,决定readyCondition的上一次转换时间
	if _, readyCondition := v1.GetPodCondition(&status, v1.PodReady); readyCondition != nil {
		// Need to set LastTransitionTime.
		lastTransitionTime := metav1.Now()
		_, oldReadyCondition := v1.GetPodCondition(&oldStatus, v1.PodReady)
		if oldReadyCondition != nil && readyCondition.Status == oldReadyCondition.Status {
			lastTransitionTime = oldReadyCondition.LastTransitionTime
		}
		readyCondition.LastTransitionTime = lastTransitionTime
	}

	// Set InitializedCondition.LastTransitionTime.
	//对比新旧状态的InitCondition,决定InitCondition的上一次转换时间
	if _, initCondition := v1.GetPodCondition(&status, v1.PodInitialized); initCondition != nil {
		// Need to set LastTransitionTime.
		lastTransitionTime := metav1.Now()
		_, oldInitCondition := v1.GetPodCondition(&oldStatus, v1.PodInitialized)
		if oldInitCondition != nil && initCondition.Status == oldInitCondition.Status {
			lastTransitionTime = oldInitCondition.LastTransitionTime
		}
		initCondition.LastTransitionTime = lastTransitionTime
	}

	// ensure that the start time does not change across updates.
	//更新新状态的启动时间
	if oldStatus.StartTime != nil && !oldStatus.StartTime.IsZero() {
		status.StartTime = oldStatus.StartTime
	} else if status.StartTime.IsZero() {
		// if the status has no start time, we need to set an initial time
		now := metav1.Now()
		status.StartTime = &now
	}

	//转换容器状态中的各时间格式为rfc3339格式
	normalizeStatus(pod, &status)
	// The intent here is to prevent concurrent updates to a pod's status from
	// clobbering each other so the phase of a pod progresses monotonically.
	if isCached && isStatusEqual(&cachedStatus.status, &status) && !forceUpdate {
		glog.V(3).Infof("Ignoring same status for pod %q, status: %+v", format.Pod(pod), status)
		return false // No new status.
	}

	//将新版本的状态缓存
	newStatus := versionedPodStatus{
		status:       status,
		version:      cachedStatus.version + 1,
		podName:      pod.Name,
		podNamespace: pod.Namespace,
	}
	m.podStatuses[pod.UID] = newStatus

	select {
	//提交更新请求给syncPod()进行处理
	case m.podStatusChannel <- podStatusSyncRequest{pod.UID, newStatus}:
		return true
	default:
		// Let the periodic syncBatch handle the update if the channel is full.
		// We can't block, since we hold the mutex lock.
		glog.V(4).Infof("Skpping the status update for pod %q for now because the channel is full; status: %+v",
			format.Pod(pod), status)
		return false
	}
}

// deletePodStatus simply removes the given pod from the status cache.
func (m *manager) deletePodStatus(uid types.UID) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()
	delete(m.podStatuses, uid)
}

// TODO(filipg): It'd be cleaner if we can do this without signal from user.
//从状态管理器移除缓存中指定Pid的状态
func (m *manager) RemoveOrphanedStatuses(podUIDs map[types.UID]bool) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()
	for key := range m.podStatuses {
		if _, ok := podUIDs[key]; !ok {
			glog.V(5).Infof("Removing %q from status map.", key)
			delete(m.podStatuses, key)
		}
	}
}

// syncBatch syncs pods statuses with the apiserver.
//遍历所有缓存的pod状态,找到需要更新到apiserver中pod的状态,
//更新到apiserver中
func (m *manager) syncBatch() {
	//缓存需要更新的Pod状态
	var updatedStatuses []podStatusSyncRequest
	podToMirror, mirrorToPod := m.podManager.GetUIDTranslations()
	func() { // Critical section
		m.podStatusesLock.RLock()
		defer m.podStatusesLock.RUnlock()

		// Clean up orphaned versions.
		//如果指定uid不存在podversion map,或者??
		for uid := range m.apiStatusVersions {
			_, hasPod := m.podStatuses[uid]
			_, hasMirror := mirrorToPod[uid]
			if !hasPod && !hasMirror {
				delete(m.apiStatusVersions, uid)
			}
		}

		for uid, status := range m.podStatuses {
			syncedUID := uid
			if mirrorUID, ok := podToMirror[uid]; ok {
				if mirrorUID == "" {
					glog.V(5).Infof("Static pod %q (%s/%s) does not have a corresponding mirror pod; skipping", uid, status.podName, status.podNamespace)
					continue
				}
				syncedUID = mirrorUID
			}
			//如果指定的Pod状态需要更新
			if m.needsUpdate(syncedUID, status) {
				updatedStatuses = append(updatedStatuses, podStatusSyncRequest{uid, status})
			} else if m.needsReconcile(uid, status.status) {
				// Delete the apiStatusVersions here to force an update on the pod status
				// In most cases the deleted apiStatusVersions here should be filled
				// soon after the following syncPod() [If the syncPod() sync an update
				// successfully].
				delete(m.apiStatusVersions, syncedUID)
				updatedStatuses = append(updatedStatuses, podStatusSyncRequest{uid, status})
			}
		}
	}()

	for _, update := range updatedStatuses {
		m.syncPod(update.podUID, update.status)
	}
}

// syncPod syncs the given status with the API server. The caller must not hold the lock.
//将指定Pod的状态更新到api server对应的pod中
func (m *manager) syncPod(uid types.UID, status versionedPodStatus) {
	//检测指定pod是否需要更新,当pod状态版本缓存的版本你低于od状态缓存的最新版本号,则返回
	if !m.needsUpdate(uid, status) {
		glog.V(1).Infof("Status for pod %q is up-to-date; skipping", uid)
		return
	}

	// TODO: make me easier to express from client code
	//获取api pod
	pod, err := m.kubeClient.Core().Pods(status.podNamespace).Get(status.podName, metav1.GetOptions{})
	//??api pod不存在不报错???资源没有找到则报错,能够通过这种方式来判定client的出错吗?
	//可以
	if errors.IsNotFound(err) {
		glog.V(3).Infof("Pod %q (%s) does not exist on the server", status.podName, uid)
		// If the Pod is deleted the status will be cleared in
		// RemoveOrphanedStatuses, so we just ignore the update here.
		return
	}
	if err == nil {
		//返回Pod 真正的aid, 如果是mirror pod,返回statid pod的id
		translatedUID := m.podManager.TranslatePodUID(pod.UID)
		if len(translatedUID) > 0 && translatedUID != uid {
			glog.V(2).Infof("Pod %q was deleted and then recreated, skipping status update; old UID %q, new UID %q", format.Pod(pod), uid, translatedUID)
			m.deletePodStatus(uid)
			return
		}
		//更新Pod的状态
		pod.Status = status.status
		//如果pod的状态中有初始容器的状态,则压缩后保存在pod指定的annotation中
		if err := podutil.SetInitContainersStatusesAnnotations(pod); err != nil {
			glog.Error(err)
		}
		// TODO: handle conflict as a retry, make that easier too.
		//更新指定Pod的状态.
		pod, err = m.kubeClient.Core().Pods(pod.Namespace).UpdateStatus(pod)
		if err == nil {
			glog.V(3).Infof("Status for pod %q updated successfully: %+v", format.Pod(pod), status)
			m.apiStatusVersions[pod.UID] = status.version //更新Pod的版本
			if kubepod.IsMirrorPod(pod) {
				// We don't handle graceful deletion of mirror pods.
				return
			}
			//pod是否指定删除时间栈? api Pod被要求删除?这里不明白?
			if pod.DeletionTimestamp == nil {
				return
			}
			// 如果Pod不是镜像pod,而且指定删除时间栈
			if !notRunning(pod.Status.ContainerStatuses) {
				glog.V(3).Infof("Pod %q is terminated, but some containers are still running", format.Pod(pod))
				return
			}
			//pod不是镜像pod,而且指定了删除时间栈,而且仍存在运行状态的的容器
			//不指定优雅删除时间的
			deleteOptions := v1.NewDeleteOptions(0)
			// Use the pod UID as the precondition for deletion to prevent deleting a newly created pod with the same name and namespace.
			deleteOptions.Preconditions = v1.NewUIDPreconditions(string(pod.UID))
			//删除指定的Pod
			if err = m.kubeClient.Core().Pods(pod.Namespace).Delete(pod.Name, deleteOptions); err == nil {
				glog.V(3).Infof("Pod %q fully terminated and removed from etcd", format.Pod(pod))
				m.deletePodStatus(uid)
				return
			}
		}
	}

	// We failed to update status, wait for periodic sync to retry.
	glog.Warningf("Failed to update status for pod %q: %v", format.Pod(pod), err)
}

// needsUpdate returns whether the status is stale for the given pod UID.
// This method is not thread safe, and most only be accessed by the sync thread.
//检测pod的最新版本号是否低于缓存中pod状态的版本号
func (m *manager) needsUpdate(uid types.UID, status versionedPodStatus) bool {
	latest, ok := m.apiStatusVersions[uid] //
	return !ok || latest < status.version
}

// needsReconcile compares the given status with the status in the pod manager (which
// in fact comes from apiserver), returns whether the status needs to be reconciled with
// the apiserver. Now when pod status is inconsistent between apiserver and kubelet,
// kubelet should forcibly send an update to reconclie the inconsistence, because kubelet
// should be the source of truth of pod status.
// NOTE(random-liu): It's simpler to pass in mirror pod uid and get mirror pod by uid, but
// now the pod manager only supports getting mirror pod by static pod, so we have to pass
// static pod uid here.
// TODO(random-liu): Simplify the logic when mirror pod manager is added.
//状态需要调整?
// 查看apiserver中的PodStatus是否和kubelet缓存的一致,是否需要调整
func (m *manager) needsReconcile(uid types.UID, status v1.PodStatus) bool {
	// The pod could be a static pod, so we should translate first.
	pod, ok := m.podManager.GetPodByUID(uid)
	if !ok {
		glog.V(4).Infof("Pod %q has been deleted, no need to reconcile", string(uid))
		return false
	}
	// If the pod is a static pod, we should check its mirror pod, because only status in mirror pod is meaningful to us.
	if kubepod.IsStaticPod(pod) {
		mirrorPod, ok := m.podManager.GetMirrorPodByPod(pod)
		if !ok {
			glog.V(4).Infof("Static pod %q has no corresponding mirror pod, no need to reconcile", format.Pod(pod))
			return false
		}
		pod = mirrorPod
	}

	podStatus, err := copyStatus(&pod.Status)
	if err != nil {
		return false
	}
	//更新pod状态中的时间格式为rfc3339
	normalizeStatus(pod, &podStatus)

	//检测两个状态是否相同
	if isStatusEqual(&podStatus, &status) {
		// If the status from the source is the same with the cached status,
		// reconcile is not needed. Just return.
		return false
	}
	glog.V(3).Infof("Pod status is inconsistent with cached status for pod %q, a reconciliation should be triggered:\n %+v", format.Pod(pod),
		diff.ObjectDiff(podStatus, status))

	return true
}

// We add this function, because apiserver only supports *RFC3339* now, which means that the timestamp returned by
// apiserver has no nanosecond information. However, the timestamp returned by metav1.Now() contains nanosecond,
// so when we do comparison between status from apiserver and cached status, isStatusEqual() will always return false.
// There is related issue #15262 and PR #15263 about this.
// In fact, the best way to solve this is to do it on api side. However, for now, we normalize the status locally in
// kubelet temporarily.
// TODO(random-liu): Remove timestamp related logic after apiserver supports nanosecond or makes it consistent.
//转换容器状态中的各时间格式为rfc3339格式
func normalizeStatus(pod *v1.Pod, status *v1.PodStatus) *v1.PodStatus {
	//输出时间为2012-11-01 22:08:41 +0000 +0000形式
	normalizeTimeStamp := func(t *metav1.Time) {
		*t = t.Rfc3339Copy()
	}
	//将容器的启动时间,终止起始时间和结束时间转换成rfc3339格式
	normalizeContainerState := func(c *v1.ContainerState) {
		if c.Running != nil {
			normalizeTimeStamp(&c.Running.StartedAt)
		}
		if c.Terminated != nil {
			normalizeTimeStamp(&c.Terminated.StartedAt)
			normalizeTimeStamp(&c.Terminated.FinishedAt)
		}
	}

	if status.StartTime != nil {
		normalizeTimeStamp(status.StartTime)
	}
	//转换所有Condition的探测时间以及转换时间
	for i := range status.Conditions {
		condition := &status.Conditions[i]
		normalizeTimeStamp(&condition.LastProbeTime)
		normalizeTimeStamp(&condition.LastTransitionTime)
	}

	// update container statuses
	for i := range status.ContainerStatuses {
		cstatus := &status.ContainerStatuses[i]
		normalizeContainerState(&cstatus.State)
		normalizeContainerState(&cstatus.LastTerminationState)
	}
	// Sort the container statuses, so that the order won't affect the result of comparison
	//按容器状态名进行排序
	sort.Sort(kubetypes.SortedContainerStatuses(status.ContainerStatuses))

	// update init container statuses
	for i := range status.InitContainerStatuses {
		cstatus := &status.InitContainerStatuses[i]
		normalizeContainerState(&cstatus.State)
		normalizeContainerState(&cstatus.LastTerminationState)
	}
	// Sort the container statuses, so that the order won't affect the result of comparison
	kubetypes.SortInitContainerStatuses(pod, status.InitContainerStatuses)
	return status
}

// notRunning returns true if every status is terminated or waiting, or the status list
// is empty.
//检测是否存在容器处于非运行状态
func notRunning(statuses []v1.ContainerStatus) bool {
	for _, status := range statuses {
		if status.State.Terminated == nil && status.State.Waiting == nil {
			return false
		}
	}
	return true
}

//拷贝指定pod的状态
func copyStatus(source *v1.PodStatus) (v1.PodStatus, error) {
	clone, err := api.Scheme.DeepCopy(source)
	if err != nil {
		glog.Errorf("Failed to clone status %+v: %v", source, err)
		return v1.PodStatus{}, err
	}
	status := *clone.(*v1.PodStatus)
	return status, nil
}
