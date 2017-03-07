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

package qos

// QOSClass defines the supported qos classes of Pods/Containers.
type QOSClass string

//所有的pod属于这三个级别中的一种.BestEffort是最低优先级,一旦系统OOM时,BestEffort的Pod将会被杀死以获取内存
//Burstable第二, 一旦OOM,BestEffort被杀完了,Burstable会被杀死
//参考https://github.com/kubernetes/community/blob/master/contributors/design-proposals/resource-qos.md
const (
	// Guaranteed is the Guaranteed qos class.
	Guaranteed QOSClass = "Guaranteed"
	// Burstable is the Burstable qos class.
	Burstable QOSClass = "Burstable"
	// BestEffort is the BestEffort qos class.
	BestEffort QOSClass = "BestEffort"
)
