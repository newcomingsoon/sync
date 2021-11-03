// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package singleflight provides a duplicate function call suppression
// mechanism.
package singleflight // import "golang.org/x/sync/singleflight"

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
)

// errGoexit indicates the runtime.Goexit was called in
// the user given function.
var errGoexit = errors.New("runtime.Goexit was called")

// A panicError is an arbitrary value recovered from a panic
// with the stack trace during the execution of given function.
type panicError struct {
	value interface{}
	stack []byte
}

// Error implements error interface.
func (p *panicError) Error() string {
	return fmt.Sprintf("%v\n\n%s", p.value, p.stack)
}

func newPanicError(v interface{}) error {
	stack := debug.Stack()

	// The first line of the stack trace is of the form "goroutine N [status]:"
	// but by the time the panic reaches Do the goroutine may no longer exist
	// and its status will have changed. Trim out the misleading line.
	if line := bytes.IndexByte(stack[:], '\n'); line >= 0 {
		stack = stack[line+1:]
	}
	return &panicError{value: v, stack: stack}
}

// call is an in-flight or completed singleflight.Do call
type call struct {
	wg sync.WaitGroup

	// These fields are written once before the WaitGroup is done
	// and are only read after the WaitGroup is done.
	val interface{}
	err error

	// forgotten indicates whether Forget was called with this call's key
	// while the call was still in flight. ，
	forgotten bool

	// These fields are read and written with the singleflight
	// mutex held before the WaitGroup is done, and are read but
	// not written after the WaitGroup is done.
	dups  int
	chans []chan<- Result
}

// Group represents a class of work and forms a namespace in
// which units of work can be executed with duplicate suppression.
type Group struct {
	mu sync.Mutex       // protects m
	m  map[string]*call // lazily initialized
}

// Result holds the results of Do, so they can be passed
// on a channel.
type Result struct {
	Val    interface{}
	Err    error
	Shared bool
}

// Do executes and returns the results of the given function, making
// sure that only one execution is in-flight for a given key at a
// time. If a duplicate comes in, the duplicate caller waits for the
// original to complete and receives the same results.
// The return value shared indicates whether v was given to multiple callers.
func (g *Group) Do(key string, fn func() (interface{}, error)) (v interface{}, err error, shared bool) {
	// 直接加互斥锁，多个请求进来如果锁没有释放，直接阻塞住
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		// 存在key，dups++
		c.dups++
		// 如果存在，提前释放锁，可以让更多并发请求进来，等待第一次key的结果返回
		g.mu.Unlock()
		// 等待先前请求的完成,结果存储在call对象中
		// 如果之前的fn执行已经结束了， 不会阻塞。只有第一次fn进入还没执行完时才会被阻塞
		// fn执行完后，此时forgotten=false， key在map中被删除。
		// 但是，已经进来的请求，还是直接返回之前fn得到的call对象保存的结果，注意：此时key已经在map中被删除了，但是call对象是没有被回收
		// 注意： map中内存回收，是不会直接清理之前已经分配的call对象的
		// 在fn第一次执行完成后，再通过map判断是不存在的，会重新执行对应的fn函数定义，得到新的call对象，新的结果存储在其中。
		c.wg.Wait()
		// 判断先前请求的错误类型
		if e, ok := c.err.(*panicError); ok {
			panic(e)
		} else if c.err == errGoexit {
			runtime.Goexit()
		}
		// 返回请求已经成功call对象存储的值，错误类型， 是否共享先前的请求结果
		return c.val, c.err, true
	}
	// 刚开始不存在该key的时候
	// 每个不同的key，对应不同的call对象存储其请求的结果返回。
	c := new(call)
	c.wg.Add(1)
	// 将第一次的请求先缓存到map中，后续请求等待结果
	g.m[key] = c
	// 释放锁，尽可能减少锁的时间，执行fn的过程不受锁控制（锁提前释放了）
	g.mu.Unlock()
  // 同步去执行fn，获取第一次请求key的结果存入call中
	g.doCall(c, key, fn)
	return c.val, c.err, c.dups > 0
}

// DoChan is like Do but returns a channel that will receive the
// results when they are ready.
//
// The returned channel will not be closed.
func (g *Group) DoChan(key string, fn func() (interface{}, error)) <-chan Result {
	ch := make(chan Result, 1)
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		c.dups++
		// 获取之前请求的call对象，然后将本次请求的返回channel加入chans队列
		// 之前请求的doCall协程一直在后台循环向chans队列的channel发送第一次fn函数执行的结果
		// 注意：调用DoChan之前最好先调用Forget(key string) 使其将key一直缓存在map中
		// 不然，多次关于同一个key的DoChan调用都会产生一个不会终止的后端goroutine来处理请求的返回结果
		// 最后结果就是，相同请求的key的结果得不到复用
		c.chans = append(c.chans, ch)
		g.mu.Unlock()
		return ch
	}
	c := &call{chans: []chan<- Result{ch}}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()
  // 异步执行请求调用， 通过channel来接收最后的结果
	go g.doCall(c, key, fn)
	// 返回存储结果的channel， 只读
	return ch
}

// doCall handles the single call for a key.
func (g *Group) doCall(c *call, key string, fn func() (interface{}, error)) {
	// 正常执行完fn后的返回标记字段
	normalReturn := false
	// 执行过程中出现异常错误的返回标记字段
	recovered := false

	// use double-defer to distinguish panic from runtime.Goexit,
	// more details see https://golang.org/cl/134395
	defer func() {
		// the given function invoked runtime.Goexit
		// 执行fn函数，直接触发runtime.Goexit， recover是无法捕获的
		// 所以通过 recovered = false字段和，normalReturn = false来区分触发的异常是不是
		// runtime.Goexit
		if !normalReturn && !recovered {
			c.err = errGoexit
		}

		c.wg.Done()
		g.mu.Lock()
		defer g.mu.Unlock()
    // 默认false，直接就会删除对应的key
		// 这样做的目的是： 保证在当前fn函数执行完成之前的所有并发请求相同key的结果（这个时候锁已经释放了）和当前fn请求的结果相同
		// 只有当Forget(key string)函数被调用的时候并且当前key已经存在map中时：
		// c.forgotten = true 才生效
		// 之后key一直存在map中，除非Forget(key string)函数被重新调用
		if !c.forgotten {
			delete(g.m, key)
		}

		if e, ok := c.err.(*panicError); ok {
			// In order to prevent the waiting channels from being blocked forever,
			// needs to ensure that this panic cannot be recovered.
			// 如果是通过channel来等待结果的， 那么为了不永久的阻塞掉这些channel，
			// 需要确保这个panic不能被recover掉
			if len(c.chans) > 0 {
				// 启动另一个协程panic
				go panic(e)
				// 阻塞：保持该goroutine不退出
				// 那么该协程的调用栈也能出现在panic产生后显示出来
				select {} // Keep this goroutine around so that it will appear in the crash dump.
			} else {
				// 直接panic
				panic(e)
			}
		} else if c.err == errGoexit {
			// Already in the process of goexit, no need to call again
		} else {
			// Normal return
			// 通过channel的形式获取请求的返回填入ch中
			// 如果不是DoChan的形式（c.chans 为nil），那么直接返回
			for _, ch := range c.chans {
				// 所有后续等待相同的key的结果，不管自定义fn做了什么，都是返回之前请求得到的结果
				ch <- Result{c.val, c.err, c.dups > 0}
			}
		}
	}()

	// 同步调用func()
	func() {
		defer func() {
			if !normalReturn {
				// Ideally, we would wait to take a stack trace until we've determined
				// whether this is a panic or a runtime.Goexit.
				//
				// Unfortunately, the only way we can distinguish the two is to see
				// whether the recover stopped the goroutine from terminating, and by
				// the time we know that, the part of the stack trace relevant to the
				// panic has been discarded.
				if r := recover(); r != nil {
					c.err = newPanicError(r)
				}
			}
		}()

		c.val, c.err = fn()
		// 正常执行后的返回
		normalReturn = true
	}()
	// 如果fn中异常runtime.Goexit退出，该部分以下代码段不会被执行
	if !normalReturn {
		recovered = true
	}
}

// Forget tells the singleflight to forget about a key.  Future calls
// to Do for this key will call the function rather than waiting for
// an earlier call to complete.
// 调用该函数后，后续的请求结果都是由之后第一次fn函数执行后的结果决定的
// 后续已存在map中的key只有再调用该函数才会被清除
func (g *Group) Forget(key string) {
	g.mu.Lock()
	if c, ok := g.m[key]; ok {
		c.forgotten = true
	}
	// 如果map为nil或者key不存在，delete相当于未操作，无负影响
	// 同事删除已存在的key缓存
	delete(g.m, key)
	g.mu.Unlock()
}
