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
	"fmt"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/fieldpath"
)

// defaultPodLimitsForDownwardApi copies the input pod, and optional container,
// and applies default resource limits. it returns a copy of the input pod,
// and a copy of the input container (if specified) with default limits
// applied. if a container has no limit specified, it will default the limit to
// the node allocatable.
// TODO: if/when we have pod level resources, we need to update this function
// to use those limits instead of node allocatable.
//拷贝传入的pod和容器,将拷贝的pod和容器中所有没有设置limit的resource设置上限为kubelet的相应可分配资源上限
func (kl *Kubelet) defaultPodLimitsForDownwardApi(pod *v1.Pod, container *v1.Container) (*v1.Pod, *v1.Container, error) {
	if pod == nil {
		return nil, nil, fmt.Errorf("invalid input, pod cannot be nil")
	}

	//获取kublet的api node对象,
	node, err := kl.getNodeAnyWay()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to find node object, expected a node")
	}
	//获取节点的资源列表
	allocatable := node.Status.Allocatable

	podCopy, err := api.Scheme.Copy(pod)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to perform a deep copy of pod object: %v", err)
	}
	outputPod, ok := podCopy.(*v1.Pod)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected type returned from deep copy of pod object")
	}
	//遍历所有的容器,如果容器没有设置资源上限,则默认为节点上的可分配资源上限
	for idx := range outputPod.Spec.Containers {
		fieldpath.MergeContainerResourceLimits(&outputPod.Spec.Containers[idx], allocatable)
	}

	var outputContainer *v1.Container
	if container != nil {
		containerCopy, err := api.Scheme.DeepCopy(container)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to perform a deep copy of container object: %v", err)
		}
		outputContainer, ok = containerCopy.(*v1.Container)
		if !ok {
			return nil, nil, fmt.Errorf("unexpected type returned from deep copy of container object")
		}
		fieldpath.MergeContainerResourceLimits(outputContainer, allocatable)
	}

	return outputPod, outputContainer, nil
}
