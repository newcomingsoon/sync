// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package semaphore provides a weighted semaphore implementation.
package semaphore // import "golang.org/x/sync/semaphore"

import (
	"container/list"
	"context"
	"sync"
)

type waiter struct {
	// 等待需要获取的token数
	n     int64
	// waiter内部只写的channel
	// 通过关闭该channel来通知，获取锁的协程退出锁等待的阻塞状态
	ready chan<- struct{} // Closed when semaphore acquired.
}

// NewWeighted creates a new weighted semaphore with the given
// maximum combined weight for concurrent access.
// 带权重n的信号量
func NewWeighted(n int64) *Weighted {
	w := &Weighted{size: n}
	return w
}

// Weighted provides a way to bound concurrent access to a resource.
// The callers can request access with a given weight.
type Weighted struct {
	// 总的数量
	size    int64
	// 当前被占有的数量
	cur     int64
	mu      sync.Mutex
	waiters list.List
}

// Acquire acquires the semaphore with a weight of n, blocking until resources
// are available or ctx is done. On success, returns nil. On failure, returns
// ctx.Err() and leaves the semaphore unchanged.
//
// If ctx is already done, Acquire may still succeed without blocking.
func (s *Weighted) Acquire(ctx context.Context, n int64) error {
	s.mu.Lock()
	if s.size-s.cur >= n && s.waiters.Len() == 0 {
		s.cur += n
		s.mu.Unlock()
		return nil
	}

	if n > s.size {
		// Don't make other Acquire calls block on one that's doomed to fail.
		s.mu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	}

	ready := make(chan struct{})
	w := waiter{n: n, ready: ready}
	elem := s.waiters.PushBack(w)
	// 进入阻塞前一定要释放掉锁
	s.mu.Unlock()
  // 阻塞：等待足够的tokens
	select {
	// 单独处理context被主动取消或超时的情况
	// 可以理解为，提供一种主动放弃获取锁的机制
	case <-ctx.Done():
		err := ctx.Err()
		//  以下操作需要加速
		s.mu.Lock()
		select {
		// ready 被closed
		// 在主动放弃获取锁前，已经获取到了足够的token
		case <-ready:
			// Acquired the semaphore after we were canceled.  Rather than trying to
			// fix up the queue, just pretend we didn't notice the cancelation.
			// 返回获取锁成功，不理会是主动放弃获取锁的逻辑
			// 不用调整waiters队列，因为在 notifyWaiters 获取到锁的时候，同时会删除该等待者以及同时关闭ready channel
			err = nil
		default:
			// 主动放弃获取锁的对应处理
			isFront := s.waiters.Front() == elem
			s.waiters.Remove(elem)
			// If we're at the front and there're extra tokens left, notify other waiters.
			// 判断是否需要通知其他等待者
			if isFront && s.size > s.cur {
				s.notifyWaiters()
			}
		}
		s.mu.Unlock()
		return err
  // 等待该waiter被通知（有信号量被释放），然后关闭ready channel，阻塞退出
	// 获取锁成功
	case <-ready:
		return nil
	}
}

// TryAcquire acquires the semaphore with a weight of n without blocking.
// On success, returns true. On failure, returns false and leaves the semaphore unchanged.
func (s *Weighted) TryAcquire(n int64) bool {
	s.mu.Lock()
	success := s.size-s.cur >= n && s.waiters.Len() == 0
	if success {
		s.cur += n
	}
	s.mu.Unlock()
	return success
}

// Release releases the semaphore with a weight of n.
// 释放信号量
// 修改数据，通知等待者
func (s *Weighted) Release(n int64) {
	s.mu.Lock()
	s.cur -= n
	if s.cur < 0 {
		s.mu.Unlock()
		panic("semaphore: released more than held")
	}
	s.notifyWaiters()
	s.mu.Unlock()
}

// notifyWaiters 只可内部调用
// 操作waiters都是需要加锁
// 此处没有加锁的动作，因为都是在 Release 和 Acquire 中被调用的
// 被调用的代码片断已经加过锁了
func (s *Weighted) notifyWaiters() {
	for {
		next := s.waiters.Front()
		if next == nil {
			break // No more waiters blocked.
		}

		w := next.Value.(waiter)
		if s.size-s.cur < w.n {
			// Not enough tokens for the next waiter.  We could keep going (to try to
			// find a waiter with a smaller request), but under load that could cause
			// starvation for large requests; instead, we leave all remaining waiters
			// blocked.
			//
			// Consider a semaphore used as a read-write lock, with N tokens, N
			// readers, and one writer.  Each reader can Acquire(1) to obtain a read
			// lock.  The writer can Acquire(N) to obtain a write lock, excluding all
			// of the readers.  If we allow the readers to jump ahead in the queue,
			// the writer will starve — there is always one token available for every
			// reader.
			// 言外之意： 直接退出，所有waiters都阻塞住（waiters必须满足获取锁的顺序是：先进先出）
			// 如果不是直接退出，通过找队列等待者中需要更少的token的那个时，那么需要获取最多token的那个等待者可能处于饥饿状态
			// 一直获取不到锁
			// 考虑场景： 使信号量来实现读写锁
			// 读：通过 Acquire(1) 来获取锁，由于读与写互斥，那么此时写通过：Acquire(N) 来获取锁
			// 读高频时，队列中一直存在读操作等待者，如果此时需要写，那么写操作需要获取的token数量最大，
			// 即使写操作排在队列的第一，由于队列等待者中存在需要更少token的读操作等待者，那么写操作将长时间无法获取到锁（饥饿状态）
			// 直到没有任何读操作，这样是不合理的
			break
		}

		s.cur += w.n
		s.waiters.Remove(next)
		close(w.ready)
	}
}
