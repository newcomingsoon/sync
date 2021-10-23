// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package singleflight

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 测试正常Do执行
func TestDo(t *testing.T) {
	var g Group
	v, err, _ := g.Do("key", func() (interface{}, error) {
		return "bar", nil
	})
	if got, want := fmt.Sprintf("%v (%T)", v, v), "bar (string)"; got != want {
		t.Errorf("Do = %v; want %v", got, want)
	}
	if err != nil {
		t.Errorf("Do error = %v", err)
	}
}
// 测试Do执行出现err
func TestDoErr(t *testing.T) {
	var g Group
	someErr := errors.New("Some error")
	v, err, _ := g.Do("key", func() (interface{}, error) {
		return nil, someErr
	})
	if err != someErr {
		t.Errorf("Do error = %v; want someErr %v", err, someErr)
	}
	if v != nil {
		t.Errorf("unexpected non-nil value %#v", v)
	}
}

// 测试同一个key并发调用下，在fn执行相对比较慢的情况下
// key命中缓存的情况
func TestDoDupSuppress(t *testing.T) {
	var g Group
	var wg1, wg2 sync.WaitGroup
	c := make(chan string, 1)
	var calls int32
	fn := func() (interface{}, error) {
		// 第一次执行该fn的时候，条件为真
		if atomic.AddInt32(&calls, 1) == 1 {
			// First invocation.
			wg1.Done()
		}
		v := <-c
		// 在第一个fn等待一段时间执行结束后，key的缓存会被删除
		// 通过将之前调用fn的值，继续写入c中
		// 之后调用fn的函数就不会阻塞在上一步等待c中结果了
		c <- v // pump; make available for any future calls
		// 如果fn很快退出，其他相同key的goroutines 判断key会发现不在map中
		// fn函数执行完成后，在map中的key会被删除
		time.Sleep(10 * time.Millisecond) // let more goroutines enter Do

		return v, nil
	}

	const n = 10
	wg1.Add(1)
	for i := 0; i < n; i++ {
		wg1.Add(1)
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			wg1.Done()
			v, err, _ := g.Do("key", fn)
			if err != nil {
				t.Errorf("Do error: %v", err)
				return
			}
			if s, _ := v.(string); s != "bar" {
				t.Errorf("Do = %T %v; want %q", v, v, "bar")
			}
		}()
	}
	wg1.Wait()
	// At least one goroutine is in fn now and all of them have at
	// least reached the line before the Do.
	// 此操作之前所有的Do关于该key操作都被阻塞住
	c <- "bar"
	// 等待其他fn退出
	wg2.Wait()
	// 用来判断缓存命中的次数，缓存至少命中一次，分析：
	// fn函数至少执行一次，至多执行n-1次
	// fn函数第一次执行的时候，还没结束时（需要等待一段时间才结束），很多相同key的请求已经通过了key是否map中的检查
	// 那么这些请求返回值就会复用第一次执行fn函数得到的结果，fn函数就不会执行，calls的数值就不会累加。
	// 所以 calls最终的值必须是小于n，大于0的
	if got := atomic.LoadInt32(&calls); got <= 0 || got >= n {
		t.Errorf("number of calls = %d; want over 0 and less than %d", got, n)
	}
}

// Test that singleflight behaves correctly after Forget called.
// See https://github.com/golang/go/issues/31420
// 测试Forget函数的使用
// 主要用于分析，在Forget调用之后，多次调用同一个key的DoChan后的结果都是以之后第一次写入key到map中的那个请求返回的结果为主
func TestForget(t *testing.T) {
	var g Group

	var (
		firstStarted  = make(chan struct{})
		unblockFirst  = make(chan struct{})
		firstFinished = make(chan struct{})
	)

	go func() {
		g.Do("key", func() (i interface{}, e error) {
			close(firstStarted)
			<-unblockFirst
			close(firstFinished)
			return
		})
	}()
	<-firstStarted
	// 此时key在map中会被删除，之后第一个关于key的缓存会被一直留在缓存map中
	// 直到再次调用Forget("key")，才会再次清除key缓存
	g.Forget("key")

	unblockSecond := make(chan struct{})
	secondResult := g.DoChan("key", func() (i interface{}, e error) {
		<-unblockSecond
		return 2, nil
	})

	// 此时让第一个fn函数执行结束
	close(unblockFirst)
	<-firstFinished
	// 此时的fn函数的返回结果被忽略，应为key被forgotten了
	thirdResult := g.DoChan("key", func() (i interface{}, e error) {
		return 3, nil
	})

	// 第二个fn请求执行结束，map中缓存的是该fn返回的结果
	close(unblockSecond)
	<-secondResult
	r := <-thirdResult
	// 之所Val = 2，首先是因为调用了Forget("key")，之后key请求缓存的结果不会被淘汰了
	// 此时key已经存。thirdResult返回的结果与secondResult返回的结果一样
	if r.Val != 2 {
		t.Errorf("We should receive result produced by second call, expected: 2, got %d", r.Val)
	}
}

// 测试DoChan
func TestDoChan(t *testing.T) {
	var g Group
	ch := g.DoChan("key", func() (interface{}, error) {
		return "bar", nil
	})

	res := <-ch
	v := res.Val
	err := res.Err
	if got, want := fmt.Sprintf("%v (%T)", v, v), "bar (string)"; got != want {
		t.Errorf("Do = %v; want %v", got, want)
	}
	if err != nil {
		t.Errorf("Do error = %v", err)
	}
}

// Test singleflight behaves correctly after Do panic.
// See https://github.com/golang/go/issues/41133
// 测试Do发生panic时
// 比较同一个key的多次调用是否都是生成预期的panic
func TestPanicDo(t *testing.T) {
	var g Group
	fn := func() (interface{}, error) {
		panic("invalid memory address or nil pointer dereference")
	}

	const n = 5
	waited := int32(n)
	panicCount := int32(0)
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer func() {
				if err := recover(); err != nil {
					t.Logf("Got panic: %v\n%s", err, debug.Stack())
					atomic.AddInt32(&panicCount, 1)
				}

				if atomic.AddInt32(&waited, -1) == 0 {
					close(done)
				}
			}()

			g.Do("key", fn)
		}()
	}

	select {
	case <-done:
		if panicCount != n {
			t.Errorf("Expect %d panic, but got %d", n, panicCount)
		}
	case <-time.After(time.Second):
		t.Fatalf("Do hangs")
	}
}
// 测试Do Goexit的情况
// Goexit 不是panic
func TestGoexitDo(t *testing.T) {
	var g Group
	fn := func() (interface{}, error) {
		 // 区别于panic， 该函数只是使得该goroutine正常退出
		runtime.Goexit()
		return nil, nil
	}

	const n = 5
	waited := int32(n)
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			var err error
			defer func() {
				if err != nil {
					t.Errorf("Error should be nil, but got: %v", err)
				}
				if atomic.AddInt32(&waited, -1) == 0 {
					close(done)
				}
			}()
			_, err, _ = g.Do("key", fn)
		}()
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("Do hangs")
	}
}
// 测试DoChan下panic的
func TestPanicDoChan(t *testing.T) {
	if runtime.GOOS == "js" {
		t.Skipf("js does not support exec")
	}

	if os.Getenv("TEST_PANIC_DOCHAN") != "" {
		defer func() {
			recover()
		}()

		g := new(Group)
		ch := g.DoChan("", func() (interface{}, error) {
			panic("Panicking in DoChan")
		})
		// 阻塞住了
		<-ch
		t.Fatalf("DoChan unexpectedly returned")
	}

	t.Parallel()

	cmd := exec.Command(os.Args[0], "-test.run="+t.Name(), "-test.v")
	cmd.Env = append(os.Environ(), "TEST_PANIC_DOCHAN=1")
	out := new(bytes.Buffer)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	err := cmd.Wait()
	t.Logf("%s:\n%s", strings.Join(cmd.Args, " "), out)
	if err == nil {
		t.Errorf("Test subprocess passed; want a crash due to panic in DoChan")
	}
	if bytes.Contains(out.Bytes(), []byte("DoChan unexpectedly")) {
		t.Errorf("Test subprocess failed with an unexpected failure mode.")
	}
	if !bytes.Contains(out.Bytes(), []byte("Panicking in DoChan")) {
		t.Errorf("Test subprocess failed, but the crash isn't caused by panicking in DoChan")
	}
}

// 测试DoChan下panic，调用结果是否是共享之前调用Do的panic
func TestPanicDoSharedByDoChan(t *testing.T) {
	if runtime.GOOS == "js" {
		t.Skipf("js does not support exec")
	}

	if os.Getenv("TEST_PANIC_DOCHAN") != "" {
		blocked := make(chan struct{})
		unblock := make(chan struct{})

		g := new(Group)
		go func() {
			defer func() {
				recover()
			}()
			g.Do("", func() (interface{}, error) {
				close(blocked)
				<-unblock
				panic("Panicking in Do")
			})
		}()

		<-blocked
		ch := g.DoChan("", func() (interface{}, error) {
			panic("DoChan unexpectedly executed callback")
		})
		close(unblock)
		// 由于第一个fn同样被panic了，所以阻塞住
		<-ch
		t.Fatalf("DoChan unexpectedly returned")
	}

	t.Parallel()

	cmd := exec.Command(os.Args[0], "-test.run="+t.Name(), "-test.v")
	cmd.Env = append(os.Environ(), "TEST_PANIC_DOCHAN=1")
	out := new(bytes.Buffer)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	err := cmd.Wait()
	t.Logf("%s:\n%s", strings.Join(cmd.Args, " "), out)
	if err == nil {
		t.Errorf("Test subprocess passed; want a crash due to panic in Do shared by DoChan")
	}
	if bytes.Contains(out.Bytes(), []byte("DoChan unexpectedly")) {
		t.Errorf("Test subprocess failed with an unexpected failure mode.")
	}
	if !bytes.Contains(out.Bytes(), []byte("Panicking in Do")) {
		t.Errorf("Test subprocess failed, but the crash isn't caused by panicking in Do")
	}
}
