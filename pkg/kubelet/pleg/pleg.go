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
	"k8s.io/kubernetes/pkg/types"
)

type PodLifeCycleEventType string

//generateEvent将根据pleg容器状态(其实就是容器状态)转换成容器事件
const (
	ContainerStarted PodLifeCycleEventType = "ContainerStarted" //容器正在运行
	ContainerDied    PodLifeCycleEventType = "ContainerDied"    //容器退出
	ContainerRemoved PodLifeCycleEventType = "ContainerRemoved"
	// PodSync is used to trigger syncing of a pod when the observed change of
	// the state of the pod cannot be captured by any single event above.
	PodSync PodLifeCycleEventType = "PodSync"
	// Do not use the events below because they are disabled in GenericPLEG.
	//见pleg/generic,generateEvent/convertState,当容器容器状态unknown/或者容器状态为created,则Pleg事件为ContainerChanged
	ContainerChanged PodLifeCycleEventType = "ContainerChanged"
)

// PodLifecycleEvent is an event that reflects the change of the pod state.
type PodLifecycleEvent struct {
	// The pod ID.
	ID types.UID
	// The type of the event.
	Type PodLifeCycleEventType //Pod生命周期事件
	// The accompanied data which varies based on the event type.
	//   - ContainerStarted/ContainerStopped: the container name (string).
	//   - All other event types: unused.
	Data interface{}
}

type PodLifecycleEventGenerator interface {
	Start()                         //启动一个死循环,周期执行relist ,被kubelet.Run()调用
	Watch() chan *PodLifecycleEvent //提供一个channel用来接收pleg事件. 被kubelet.syncLoop()调用
	Healthy() (bool, error)         //检测pleg是否健康.检测上一次relist距当前时间是否超过阈值时间(默认为2分钟)
}
