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

package kubelet

import (
	"fmt"
	"sync"
	"time"
)

//运行时状态列表?
//Kubelet运行时将会启动不同的goroutine,每隔几秒会更新这里的cidr地址,遇到网络错误则记录
type runtimeState struct {
	sync.RWMutex
	lastBaseRuntimeSync      time.Time //由updateRuntimeUp()进行更新
	baseRuntimeSyncThreshold time.Duration
	networkError             error  //网络错误,当没有网络错误时,则为nil;goroutine会周期调用updateRuntimeUp()或者syncNetworkStatus来更新该项.syncPod()将会根据该项决定是否同步pod
	internalError            error  //内部错误
	cidr                     string //cidr地址,在创建runtimeState就调用updatePodCIDR()进行更新,而不会停止的gotouine会调用syncNetworkStatus周期性调用updatePodCIDR()来更新.
	initError                error
}

func (s *runtimeState) setRuntimeSync(t time.Time) {
	s.Lock()
	defer s.Unlock()
	s.lastBaseRuntimeSync = t
}

func (s *runtimeState) setInternalError(err error) {
	s.Lock()
	defer s.Unlock()
	s.internalError = err
}

//更新网络错误,在kubelet.go中由updateRuntimeUp()或者syncNetworkStatus函数设置
func (s *runtimeState) setNetworkState(err error) {
	s.Lock()
	defer s.Unlock()
	s.networkError = err
}

//设置cidr地址
func (s *runtimeState) setPodCIDR(cidr string) {
	s.Lock()
	defer s.Unlock()
	s.cidr = cidr
}

func (s *runtimeState) podCIDR() string {
	s.RLock()
	defer s.RUnlock()
	return s.cidr
}

func (s *runtimeState) setInitError(err error) {
	s.Lock()
	defer s.Unlock()
	s.initError = err
}

//获取所有的运行时错误
func (s *runtimeState) runtimeErrors() []string {
	s.RLock()
	defer s.RUnlock()
	var ret []string
	if s.initError != nil {
		ret = append(ret, s.initError.Error())
	}
	if !s.lastBaseRuntimeSync.Add(s.baseRuntimeSyncThreshold).After(time.Now()) {
		ret = append(ret, "container runtime is down")
	}
	if s.internalError != nil {
		ret = append(ret, s.internalError.Error())
	}
	return ret
}

func (s *runtimeState) networkErrors() []string {
	s.RLock()
	defer s.RUnlock()
	var ret []string
	if s.networkError != nil {
		ret = append(ret, s.networkError.Error())
	}
	return ret
}

func newRuntimeState(
	runtimeSyncThreshold time.Duration,
) *runtimeState {
	return &runtimeState{
		lastBaseRuntimeSync:      time.Time{},
		baseRuntimeSyncThreshold: runtimeSyncThreshold,
		networkError:             fmt.Errorf("network state unknown"),
		internalError:            nil,
	}
}
