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
	"sync"

	"github.com/golang/groupcache/lru"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/types"
)

// ReasonCache stores the failure reason of the latest container start
// in a string, keyed by <pod_UID>_<container_name>. The goal is to
// propagate this reason to the container status. This endeavor is
// "best-effort" for two reasons:
//   1. The cache is not persisted.
//   2. We use an LRU cache to avoid extra garbage collection work. This
//      means that some entries may be recycled before a pod has been
//      deleted.
// TODO(random-liu): Use more reliable cache which could collect garbage of failed pod.
// TODO(random-liu): Move reason cache to somewhere better.
//缓存Pod同步时启动容器失败的原因和信息
type ReasonCache struct {
	lock  sync.Mutex
	cache *lru.Cache //last-recent update
	//key只是: uuid+<容器名/网络名>
	//value是reasonInfo
}

// reasonInfo is the cached item in ReasonCache
//Reason Cache中缓存的数据.key值是Pod的ID,value是启动容器失败的原因和信息
type reasonInfo struct {
	reason  error  //失败原因
	message string //失败信息
}

// maxReasonCacheEntries is the cache entry number in lru cache. 1000 is a proper number
// for our 100 pods per node target. If we support more pods per node in the future, we
// may want to increase the number.
const maxReasonCacheEntries = 1000

func NewReasonCache() *ReasonCache {
	return &ReasonCache{cache: lru.New(maxReasonCacheEntries)}
}

//这个name是kubecontainer.PodSyncResult的对象名(容器名或者网络名(目前似乎不支持))
func (c *ReasonCache) composeKey(uid types.UID, name string) string {
	return fmt.Sprintf("%s_%s", uid, name)
}

// add adds error reason into the cache
func (c *ReasonCache) add(uid types.UID, name string, reason error, message string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.cache.Add(c.composeKey(uid, name), reasonInfo{reason, message})
}

// Update updates the reason cache with the SyncPodResult. Only SyncResult with
// StartContainer action will change the cache.
//获取启动容器动作的同步结果,如果表明同步出错,则添加对应信息到原因缓存中
func (c *ReasonCache) Update(uid types.UID, result kubecontainer.PodSyncResult) {
	for _, r := range result.SyncResults {
		if r.Action != kubecontainer.StartContainer {
			continue
		}
		//获得同步结果相关的对象
		name := r.Target.(string)
		if r.Error != nil {
			c.add(uid, name, r.Error, r.Message)
		} else {
			c.Remove(uid, name)
		}
	}
}

// Remove removes error reason from the cache
//移除指定的对象的失败原因缓存
func (c *ReasonCache) Remove(uid types.UID, name string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.cache.Remove(c.composeKey(uid, name))
}

// Get gets error reason from the cache. The return values are error reason, error message and
// whether an error reason is found in the cache. If no error reason is found, empty string will
// be returned for error reason and error message.
//获取指定的容器失败原因缓存
func (c *ReasonCache) Get(uid types.UID, name string) (error, string, bool) {
	c.lock.Lock()
	defer c.lock.Unlock()
	value, ok := c.cache.Get(c.composeKey(uid, name))
	if !ok {
		return nil, "", ok
	}
	info := value.(reasonInfo)
	return info.reason, info.message, ok
}
