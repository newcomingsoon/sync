// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package errgroup provides synchronization, error propagation, and Context
// cancelation for groups of goroutines working on subtasks of a common task.
package errgroup

import (
	"context"
	"sync"
)

// A Group is a collection of goroutines working on subtasks that are part of
// the same overall task.
//
// A zero Group is valid and does not cancel on error.
type Group struct {
	cancel func()
	// 等待其他协程运行结束
	wg sync.WaitGroup
	// 保证只记录第一次报错的error信息
	errOnce sync.Once
	// 存储第一次报错的error信息
	err     error
}

// WithContext returns a new Group and an associated Context derived from ctx.
//
// The derived Context is canceled the first time a function passed to Go
// returns a non-nil error or the first time Wait returns, whichever occurs
// first.
// 注意： 这里返回的context是一个可以取消的context
// 对errgroup来说，传入的context就是root context，传context.Background()即可
// 如果是可取消的context，那么传入的context取消了会直接影响f的执行
func WithContext(ctx context.Context) (*Group, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	return &Group{cancel: cancel}, ctx
}

// Wait blocks until all function calls from the Go method have returned, then
// returns the first non-nil error (if any) from them.
func (g *Group) Wait() error {
	g.wg.Wait()
	if g.cancel != nil {
		g.cancel()
	}
	return g.err
}

// Go calls the given function in a new goroutine.
//
// The first call to return a non-nil error cancels the group; its error will be
// returned by Wait.
func (g *Group) Go(f func() error) {
	g.wg.Add(1)

	go func() {
		defer g.wg.Done()
		// 如果某个f出现了错误，其他f还是依旧会执行的
		if err := f(); err != nil {
			g.errOnce.Do(func() {
				// wait调用会返回该err
				g.err = err
				if g.cancel != nil {
					g.cancel()
				}
			})
		}
	}()
}
