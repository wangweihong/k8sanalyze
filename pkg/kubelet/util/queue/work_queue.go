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

package queue

import (
	"sync"
	"time"

	"k8s.io/kubernetes/pkg/types"
	"k8s.io/kubernetes/pkg/util/clock"
)

// WorkQueue allows queuing items with a timestamp. An item is
// considered ready to process if the timestamp has expired.
type WorkQueue interface {
	// GetWork dequeues and returns all ready items.
	GetWork() []types.UID //所有出栈时间早于当前时间的都出栈
	// Enqueue inserts a new item or overwrites an existing item.
	Enqueue(item types.UID, delay time.Duration) // deploy
}

type basicWorkQueue struct {
	clock clock.Clock
	lock  sync.Mutex
	queue map[types.UID]time.Time //记录每个ID的出栈时间(入栈时间+deplay时间)
}

var _ WorkQueue = &basicWorkQueue{}

func NewBasicWorkQueue(clock clock.Clock) WorkQueue {
	queue := make(map[types.UID]time.Time)
	return &basicWorkQueue{queue: queue, clock: clock}
}

//返回所有入栈时间早于当前时间的对象ID
func (q *basicWorkQueue) GetWork() []types.UID {
	q.lock.Lock()
	defer q.lock.Unlock()
	now := q.clock.Now() //当前时间
	var items []types.UID
	for k, v := range q.queue {
		if v.Before(now) {
			items = append(items, k)
			delete(q.queue, k)
		}
	}
	return items
}

//记录出栈时间
func (q *basicWorkQueue) Enqueue(item types.UID, delay time.Duration) {
	q.lock.Lock()
	defer q.lock.Unlock()
	q.queue[item] = q.clock.Now().Add(delay)
}
