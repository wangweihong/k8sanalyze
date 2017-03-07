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

package container

import (
	"sync"
	"time"

	"k8s.io/kubernetes/pkg/types"
)

// Cache stores the PodStatus for the pods. It represents *all* the visible
// pods/containers in the container runtime. All cache entries are at least as
// new or newer than the global timestamp (set by UpdateTime()), while
// individual entries may be slightly newer than the global timestamp. If a pod
// has no states known by the runtime, Cache returns an empty PodStatus object
// with ID populated.
//
// Cache provides two methods to retrive the PodStatus: the non-blocking Get()
// and the blocking GetNewerThan() method. The component responsible for
// populating the cache is expected to call Delete() to explicitly free the
// cache entries.
//保存所有pod的状态?
type Cache interface {
	Get(types.UID) (*PodStatus, error)
	Set(types.UID, *PodStatus, error, time.Time)
	// GetNewerThan is a blocking call that only returns the status
	// when it is newer than the given time.
	GetNewerThan(types.UID, time.Time) (*PodStatus, error)
	Delete(types.UID)
	UpdateTime(time.Time)
}

//这个data维护一个pod的更新
type data struct {
	// Status of the pod.
	status *PodStatus
	// Error got when trying to inspect the pod.
	err error
	// Time when the data was last modified.
	modified time.Time //更新时间
}

//订阅记录
type subRecord struct {
	time time.Time  //时间戳.当pod在订阅时间有更新,或者整个cahe全局timestamp新于订阅时间,则发送pod状态给ch
	ch   chan *data //传递pod状态的通道
}

// cache implements Cache.
type cache struct {
	// Lock which guards all internal data structures.
	lock sync.RWMutex
	// Map that stores the pod statuses.
	pods map[types.UID]*data
	// A global timestamp represents how fresh the cached data is. All
	// cache content is at the least newer than this timestamp. Note that the
	// timestamp is nil after initialization, and will only become non-nil when
	// it is ready to serve the cached statuses.
	timestamp *time.Time //缓存的全局时间戳.当这个时间戳更新时,将会通知所有pod中订阅时间早于该时间戳的订阅者其订阅pod的当前状态
	// Map that stores the subscriber records.
	subscribers map[types.UID][]*subRecord //这个订阅者记录的作用是什么?
	//在尝试获取订阅时间之后指定types.UID pod的状态,如果在订阅时间之后没有更新状态(指定pod)或者缓存的时间戳早于订阅时间(全部pod),则创建一个以该pod id为key的订阅器.所有试图获取pod状态的协程(不同协程有不同的订阅时间)均在此等待
	//当指定的pod更新了状态或者缓存的timestamp更新,则将pod状态发送给订阅时间早于更新时间或缓存timestamp
}

// NewCache creates a pod cache.
func NewCache() Cache {
	return &cache{pods: map[types.UID]*data{}, subscribers: map[types.UID][]*subRecord{}}
}

// Get returns the PodStatus for the pod; callers are expected not to
// modify the objects returned.
//获取pod的状态
func (c *cache) Get(id types.UID) (*PodStatus, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	d := c.get(id)
	return d.status, d.err
}

// 尝试获取指定pod,指定订阅时间之后的状态,如果该订阅时间的状态,则在订阅列表中等待
func (c *cache) GetNewerThan(id types.UID, minTime time.Time) (*PodStatus, error) {
	ch := c.subscribe(id, minTime)
	d := <-ch
	return d.status, d.err
}

// Set sets the PodStatus for the pod.
//更新指定Pod的状态.遍历该pod的订阅列表,如果有订阅者的订阅时间早于更新时间timestamp,则发送pod当前状态给订阅者,否则继续等待
func (c *cache) Set(id types.UID, status *PodStatus, err error, timestamp time.Time) {
	c.lock.Lock()
	defer c.lock.Unlock()
	defer c.notify(id, timestamp)
	c.pods[id] = &data{status: status, err: err, modified: timestamp}
}

// Delete removes the entry of the pod.
//将pod移除
func (c *cache) Delete(id types.UID) {
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.pods, id)
}

//  UpdateTime modifies the global timestamp of the cache and notify
//  subscribers if needed.
// 缓存的全局时间戳更新.遍历所有pod的订阅者,如果订阅时间早于timestamp,则将pdd状态发送给它,否则继续等待
func (c *cache) UpdateTime(timestamp time.Time) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.timestamp = &timestamp
	// Notify all the subscribers if the condition is met.
	for id := range c.subscribers {
		c.notify(id, *c.timestamp)
	}
}

func makeDefaultData(id types.UID) *data {
	return &data{status: &PodStatus{ID: id}, err: nil}
}

//获取指定pod的状态,如果pod已经不在缓存中,返回一个默认空pod状态
func (c *cache) get(id types.UID) *data {
	d, ok := c.pods[id]
	if !ok {
		// Cache should store *all* pod/container information known by the
		// container runtime. A cache miss indicates that there are no states
		// regarding the pod last time we queried the container runtime.
		// What this *really* means is that there are no visible pod/containers
		// associated with this pod. Simply return an default (mostly empty)
		// PodStatus to reflect this.
		return makeDefaultData(id)
	}
	return d
}

// getIfNewerThan returns the data it is newer than the given time.
// Otherwise, it returns nil. The caller should acquire the lock.
//获取指定订阅时间后pod[id]的最新状态
func (c *cache) getIfNewerThan(id types.UID, minTime time.Time) *data {
	d, ok := c.pods[id]
	//确认是否缓存中的时间戳比较新
	globalTimestampIsNewer := (c.timestamp != nil && c.timestamp.After(minTime))
	if !ok && globalTimestampIsNewer {
		// Status is not cached, but the global timestamp is newer than
		// minTime, return the default status.
		return makeDefaultData(id)
	}
	//如果pod的修改时间比参数时间新或者缓存中的全局时间更新
	if ok && (d.modified.After(minTime) || globalTimestampIsNewer) {
		// Status is cached, return status if either of the following is true.
		//   * status was modified after minTime
		//   * the global timestamp of the cache is newer than minTime.
		return d
	}
	// The pod status is not ready.
	return nil
}

// notify sends notifications for pod with the given id, if the requirements
// are met. Note that the caller should acquire the lock.
//获取指定pod的订阅列表,将当前pod的状态发送给订阅时间早于timeStamp的订阅者,订阅时间晚于timestamp则继续等待
func (c *cache) notify(id types.UID, timestamp time.Time) {
	list, ok := c.subscribers[id]
	if !ok {
		// No one to notify.
		return
	}
	newList := []*subRecord{}
	for i, r := range list {
		if timestamp.Before(r.time) {
			// Doesn't meet the time requirement; keep the record.
			newList = append(newList, list[i])
			continue
		}
		r.ch <- c.get(id)
		close(r.ch)
	}
	if len(newList) == 0 {
		delete(c.subscribers, id)
	} else {
		c.subscribers[id] = newList
	}
}

//添加订阅者到缓存中,这个是给谁订阅的?
// 尝试获取指定pod,指定订阅时间之后的状态,如果该订阅时间的状态,则在订阅列表中等待
func (c *cache) subscribe(id types.UID, timestamp time.Time) chan *data {
	ch := make(chan *data, 1)
	c.lock.Lock()
	defer c.lock.Unlock()
	//获取指定时间timestamp的pod[id]的状态
	d := c.getIfNewerThan(id, timestamp)
	if d != nil {
		// If the cache entry is ready, send the data and return immediately.
		ch <- d
		return ch
	}
	// Add the subscription record.
	//如果指定pod没有新于timestamp的状态,则,创建一个pod的订阅器
	c.subscribers[id] = append(c.subscribers[id], &subRecord{time: timestamp, ch: ch})
	return ch
}
