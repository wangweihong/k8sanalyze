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

package pod

import (
	"encoding/json"
	"fmt"

	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/util/intstr"
)

const (
	// TODO: to be de!eted after v1.3 is released. PodSpec has a dedicated Hostname field.
	// The annotation value is a string specifying the hostname to be used for the pod e.g 'my-webserver-1'
	PodHostnameAnnotation = "pod.beta.kubernetes.io/hostname"

	// TODO: to be de!eted after v1.3 is released. PodSpec has a dedicated Subdomain field.
	// The annotation value is a string specifying the subdomain e.g. "my-web-service"
	// If specified, on the pod itself, "<hostname>.my-web-service.<namespace>.svc.<cluster domain>" would resolve to
	// the pod's IP.
	// If there is a headless service named "my-web-service" in the same namespace as the pod, then,
	// <hostname>.my-web-service.<namespace>.svc.<cluster domain>" would be resolved by the cluster DNS Server.
	PodSubdomainAnnotation = "pod.beta.kubernetes.io/subdomain"
)

// FindPort locates the container port for the given pod and portName.  If the
// targetPort is a number, use that.  If the targetPort is a string, look that
// string up in all named ports in all containers in the target pod.  If no
// match is found, fail.
func FindPort(pod *v1.Pod, svcPort *v1.ServicePort) (int, error) {
	portName := svcPort.TargetPort
	switch portName.Type {
	case intstr.String:
		name := portName.StrVal
		for _, container := range pod.Spec.Containers {
			for _, port := range container.Ports {
				if port.Name == name && port.Protocol == svcPort.Protocol {
					return int(port.ContainerPort), nil
				}
			}
		}
	case intstr.Int:
		return portName.IntValue(), nil
	}

	return 0, fmt.Errorf("no suitable port for manifest: %s", pod.UID)
}

// TODO: remove this function when init containers becomes a stable feature
func SetInitContainersAndStatuses(pod *v1.Pod) error {
	var initContainersAnnotation string
	initContainersAnnotation = pod.Annotations[v1.PodInitContainersAnnotationKey]
	initContainersAnnotation = pod.Annotations[v1.PodInitContainersBetaAnnotationKey]
	if len(initContainersAnnotation) > 0 {
		var values []v1.Container
		if err := json.Unmarshal([]byte(initContainersAnnotation), &values); err != nil {
			return err
		}
		pod.Spec.InitContainers = values
	}

	var initContainerStatusesAnnotation string
	initContainerStatusesAnnotation = pod.Annotations[v1.PodInitContainerStatusesAnnotationKey]
	initContainerStatusesAnnotation = pod.Annotations[v1.PodInitContainerStatusesBetaAnnotationKey]
	if len(initContainerStatusesAnnotation) > 0 {
		var values []v1.ContainerStatus
		if err := json.Unmarshal([]byte(initContainerStatusesAnnotation), &values); err != nil {
			return err
		}
		pod.Status.InitContainerStatuses = values
	}
	return nil
}

// TODO: remove this function when init containers becomes a stable feature
func SetInitContainersAnnotations(pod *v1.Pod) error {
	if len(pod.Spec.InitContainers) > 0 {
		value, err := json.Marshal(pod.Spec.InitContainers)
		if err != nil {
			return err
		}
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		pod.Annotations[v1.PodInitContainersAnnotationKey] = string(value)
		pod.Annotations[v1.PodInitContainersBetaAnnotationKey] = string(value)
	}
	return nil
}

// TODO: remove this function when init containers becomes a stable feature
//如果pod状态中存在初始容器状态,则将init容器状态的值压缩后保存在指定的annotation中
func SetInitContainersStatusesAnnotations(pod *v1.Pod) error {
	if len(pod.Status.InitContainerStatuses) > 0 {
		value, err := json.Marshal(pod.Status.InitContainerStatuses)
		if err != nil {
			return err
		}
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		pod.Annotations[v1.PodInitContainerStatusesAnnotationKey] = string(value)
		pod.Annotations[v1.PodInitContainerStatusesBetaAnnotationKey] = string(value)
	}
	return nil
}
