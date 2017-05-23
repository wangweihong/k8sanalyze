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

package kubelet

import (
	"sort"

	"github.com/golang/glog"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/util/wait"
)

const (
	// The limit on the number of buffered container deletion requests
	// This number is a bit arbitrary and may be adjusted in the future.
	containerDeletorBufferLimit = 50 //缓存50个容器删除请求
)

//容器状态列表
type containerStatusbyCreatedList []*kubecontainer.ContainerStatus

//
type podContainerDeletor struct {
	worker           chan<- kubecontainer.ContainerID //用于传递将要删除的容器ID
	containersToKeep int                              //保留多少退出状态的容器不删除.
}

//根据创建时间进行排序,创建时间越玩排越前
func (a containerStatusbyCreatedList) Len() int           { return len(a) }
func (a containerStatusbyCreatedList) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a containerStatusbyCreatedList) Less(i, j int) bool { return a[i].CreatedAt.After(a[j].CreatedAt) }

//创建一个Pod容器删除管理器,执行一个不会睡眠的,永不停止的goroutine,负责删除指定容器
func newPodContainerDeletor(runtime kubecontainer.Runtime, containersToKeep int) *podContainerDeletor {
	buffer := make(chan kubecontainer.ContainerID, containerDeletorBufferLimit)
	go wait.Until(func() {
		for {
			select {
			case id := <-buffer:
				runtime.DeleteContainer(id) //调用容器管理器删除指定的容器,
			}
		}
	}, 0, wait.NeverStop)

	return &podContainerDeletor{
		worker:           buffer,
		containersToKeep: containersToKeep,
	}
}

// getContainersToDeleteInPod returns the exited containers in a pod whose name matches the name inferred from filterContainerId (if not empty), ordered by the creation time from the latest to the earliest.
// If filterContainerId is empty, all dead containers in the pod are returned.
//如果指定了容器ID,找到pod状态中指定容器ID的容器状态,r如果该容器为退出状态,添加到候选容器列表中;如果没有指定容器,找到所有处于退出状态的容器状态,添加到候选容器状态列表.
//按照创建时间的后先顺序进行排序,返回超过containerToKepp后的容器状态列表
func getContainersToDeleteInPod(filterContainerId string, podStatus *kubecontainer.PodStatus, containersToKeep int) containerStatusbyCreatedList {
	//找到指定容器ID在Pod中的状态
	matchedContainer := func(filterContainerId string, podStatus *kubecontainer.PodStatus) *kubecontainer.ContainerStatus {
		if filterContainerId == "" {
			return nil
		}
		//遍历Pod中所有容器状态,如果容器状态相关容器的ID为指定的ID,则返回该容器状态
		for _, containerStatus := range podStatus.ContainerStatuses {
			if containerStatus.ID.ID == filterContainerId {
				return containerStatus
			}
		}
		return nil
	}(filterContainerId, podStatus)

	//如果找不到指定的容器状态,返回空容器状态列表
	if filterContainerId != "" && matchedContainer == nil {
		glog.Warningf("Container %q not found in pod's containers", filterContainerId)
		return containerStatusbyCreatedList{}
	}

	// Find the exited containers whose name matches the name of the container with id being filterContainerId
	var candidates containerStatusbyCreatedList
	//遍历所有的容器状态,找到处于退出状态的容器,添加该容器状态到候选人列表中
	for _, containerStatus := range podStatus.ContainerStatuses {
		if containerStatus.State != kubecontainer.ContainerStateExited {
			continue
		}
		if matchedContainer == nil || matchedContainer.Name == containerStatus.Name {
			candidates = append(candidates, containerStatus)
		}
	}

	//如果退出的容器的数量低于保留容器数量,返回空状态列表
	if len(candidates) <= containersToKeep {
		return containerStatusbyCreatedList{}
	}
	//对候选人进行创建时间逆序排序,找到超过保留容器数量的容器状态列表
	sort.Sort(candidates)
	return candidates[containersToKeep:]
}

// deleteContainersInPod issues container deletion requests for containers selected by getContainersToDeleteInPod.
//通知容器移除器删除退出状态的容器,容器移除器会保留未超过containersTokeep数量的容器,通过指定removeAll可以移除所有退出状态容器.
//通过fileterContainerId可以指定移除指定ID的容器
func (p *podContainerDeletor) deleteContainersInPod(filterContainerId string, podStatus *kubecontainer.PodStatus, removeAll bool) {
	//获取容器保留数量
	containersToKeep := p.containersToKeep
	//如果指定了移除所有,设置为0
	if removeAll {
		containersToKeep = 0
	}

	//获取所有的a处于退出状态的所有容器状态或者指定容器状态,将其PID传递给容器移除器进行移除
	for _, candidate := range getContainersToDeleteInPod(filterContainerId, podStatus, containersToKeep) {
		select {
		case p.worker <- candidate.ID:
		default:
			glog.Warningf("Failed to issue the request to remove container %v", candidate.ID)
		}
	}
}
