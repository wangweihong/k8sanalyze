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
	"fmt"
	"strings"

	"k8s.io/kubernetes/pkg/api/v1"
)

// GeneratePodReadyCondition returns ready condition if all containers in a pod are ready, else it
// returns an unready condition.
//检测Pod中容器是否服务就绪,返回相应的ready Condition
func GeneratePodReadyCondition(spec *v1.PodSpec, containerStatuses []v1.ContainerStatus, podPhase v1.PodPhase) v1.PodCondition {
	// Find if all containers are ready or not.
	//获取不到容器状态列表
	if containerStatuses == nil {
		return v1.PodCondition{
			Type:   v1.PodReady,
			Status: v1.ConditionFalse,
			Reason: "UnknownContainerStatuses",
		}
	}
	unknownContainers := []string{}
	unreadyContainers := []string{}
	//遍历所有的容器
	for _, container := range spec.Containers {
		//获取指定容器的就绪状态,如果容器状态没有设置Ready,添加容器到unreadyContainers列表中
		if containerStatus, ok := v1.GetContainerStatus(containerStatuses, container.Name); ok {
			if !containerStatus.Ready {
				unreadyContainers = append(unreadyContainers, container.Name)
			}
			//如果有有容器没有容器状态,则添加到未知容器列表
		} else {
			unknownContainers = append(unknownContainers, container.Name)
		}
	}

	// If all containers are known and succeeded, just return PodCompleted.
	//如果容器处于已成功阶段且不存在未知的容器
	//即Pod中的容器自动停止
	if podPhase == v1.PodSucceeded && len(unknownContainers) == 0 {
		return v1.PodCondition{
			Type:   v1.PodReady,
			Status: v1.ConditionFalse, ///???为什么返回这个? 因为这里查询的Ready Condition,即服务就绪状态.Pod处于已完成(容器处于Terminated状态),就无法提供服务
			Reason: "PodCompleted",
		}
	}

	unreadyMessages := []string{}
	if len(unknownContainers) > 0 {
		unreadyMessages = append(unreadyMessages, fmt.Sprintf("containers with unknown status: %s", unknownContainers))
	}
	if len(unreadyContainers) > 0 {
		unreadyMessages = append(unreadyMessages, fmt.Sprintf("containers with unready status: %s", unreadyContainers))
	}
	unreadyMessage := strings.Join(unreadyMessages, ", ")
	if unreadyMessage != "" {
		return v1.PodCondition{
			Type:    v1.PodReady,
			Status:  v1.ConditionFalse,
			Reason:  "ContainersNotReady",
			Message: unreadyMessage,
		}
	}

	return v1.PodCondition{
		Type:   v1.PodReady,
		Status: v1.ConditionTrue,
	}
}

// GeneratePodInitializedCondition returns initialized condition if all init containers in a pod are ready, else it
// returns an uninitialized condition.
//生成pod init容器的状态中的Initialized Condition
//但注意https://kubernetes.io/docs/concepts/workloads/pods/init-containers/提到.Init Containers do not support readiness probes because they must run to completion before the Pod can be ready.
func GeneratePodInitializedCondition(spec *v1.PodSpec, containerStatuses []v1.ContainerStatus, podPhase v1.PodPhase) v1.PodCondition {
	// Find if all containers are ready or not.
	if containerStatuses == nil && len(spec.InitContainers) > 0 {
		return v1.PodCondition{
			Type:   v1.PodInitialized,
			Status: v1.ConditionFalse,
			Reason: "UnknownContainerStatuses",
		}
	}
	unknownContainers := []string{}
	unreadyContainers := []string{}
	for _, container := range spec.InitContainers {
		if containerStatus, ok := v1.GetContainerStatus(containerStatuses, container.Name); ok {
			if !containerStatus.Ready {
				unreadyContainers = append(unreadyContainers, container.Name)
			}
		} else {
			unknownContainers = append(unknownContainers, container.Name)
		}
	}

	// If all init containers are known and succeeded, just return PodCompleted.
	//如果Pod处于Sucess状态(已终止),
	if podPhase == v1.PodSucceeded && len(unknownContainers) == 0 {
		return v1.PodCondition{
			Type:   v1.PodInitialized,
			Status: v1.ConditionTrue,
			Reason: "PodCompleted",
		}
	}

	unreadyMessages := []string{}
	if len(unknownContainers) > 0 {
		unreadyMessages = append(unreadyMessages, fmt.Sprintf("containers with unknown status: %s", unknownContainers))
	}
	if len(unreadyContainers) > 0 {
		unreadyMessages = append(unreadyMessages, fmt.Sprintf("containers with incomplete status: %s", unreadyContainers))
	}
	unreadyMessage := strings.Join(unreadyMessages, ", ")
	if unreadyMessage != "" {
		return v1.PodCondition{
			Type:    v1.PodInitialized,
			Status:  v1.ConditionFalse,
			Reason:  "ContainersNotInitialized",
			Message: unreadyMessage,
		}
	}

	return v1.PodCondition{
		Type:   v1.PodInitialized,
		Status: v1.ConditionTrue,
	}
}
