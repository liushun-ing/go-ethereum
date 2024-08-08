// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package event

import (
	"errors"
	"reflect"
	"sync"
)

var errBadChannel = errors.New("event: Subscribe argument does not have sendable channel type")

// Feed implements one-to-many subscriptions where the carrier of events is a channel.
// Values sent to a Feed are delivered to all subscribed channels simultaneously.
//
// Feeds can only be used with a single type. The type is determined by the first Send or
// Subscribe operation. Subsequent calls to these methods panic if the type does not
// match.
//
// The zero value is ready to use.
type Feed struct {
	once      sync.Once        // ensures that init only runs once
	sendLock  chan struct{}    // sendLock has a one-element buffer and is empty when held.It protects sendCases.
	removeSub chan interface{} // interrupts Send
	sendCases caseList         // the active set of select cases used by Send

	// The inbox holds newly subscribed channels until they are added to sendCases.
	mu    sync.Mutex
	inbox caseList // 感觉像个待处理，是为了减少sendCases的同步问题应该是，
	etype reflect.Type
}

// This is the index of the first actual subscription channel in sendCases.
// sendCases[0] is a SelectRecv case for the removeSub channel.
const firstSubSendCase = 1

type feedTypeError struct {
	got, want reflect.Type
	op        string
}

func (e feedTypeError) Error() string {
	return "event: wrong type in " + e.op + " got " + e.got.String() + ", want " + e.want.String()
}

func (f *Feed) init(etype reflect.Type) {
	f.etype = etype
	f.removeSub = make(chan interface{})
	f.sendLock = make(chan struct{}, 1)
	// 用 channel 来模拟了一个锁，一开始就加入一个消息，谁读到了消息，就拿到了锁
	f.sendLock <- struct{}{}
	// 使用 removeSub 初始化了 sendCases
	f.sendCases = caseList{{Chan: reflect.ValueOf(f.removeSub), Dir: reflect.SelectRecv}}
}

// Subscribe adds a channel to the feed. Future sends will be delivered on the channel
// until the subscription is canceled. All channels added must have the same element type.
//
// The channel should have ample buffer space to avoid blocking other subscribers.
// Slow subscribers are not dropped.
// 注意传过来的是一个通道，是自己构建的通道，而之前的是由订阅机制构建一个channel返回
// 返回的是一个 feedSub 对象，包含 channel 和 Feed 对西那个
func (f *Feed) Subscribe(channel interface{}) Subscription {
	chanval := reflect.ValueOf(channel)
	chantyp := chanval.Type()
	if chantyp.Kind() != reflect.Chan || chantyp.ChanDir()&reflect.SendDir == 0 {
		panic(errBadChannel)
	}
	// 构建一个feedSub对象
	sub := &feedSub{feed: f, channel: chanval, err: make(chan error, 1)}

	// 初始化
	f.once.Do(func() { f.init(chantyp.Elem()) })
	if f.etype != chantyp.Elem() {
		panic(feedTypeError{op: "Subscribe", got: chantyp, want: reflect.ChanOf(reflect.SendDir, f.etype)})
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// Add the select case to the inbox.
	// The next Send will add it to f.sendCases.
	// 构架一个只能发送的case
	cas := reflect.SelectCase{Dir: reflect.SelectSend, Chan: chanval}
	// 加入到inbox中
	f.inbox = append(f.inbox, cas)
	return sub
}

// 删除订阅对象
func (f *Feed) remove(sub *feedSub) {
	// Delete from inbox first, which covers channels
	// that have not been added to f.sendCases yet.
	ch := sub.channel.Interface()
	f.mu.Lock()
	index := f.inbox.find(ch)
	if index != -1 {
		f.inbox = f.inbox.delete(index)
		f.mu.Unlock()
		return
	}
	f.mu.Unlock()

	select {
	case f.removeSub <- ch:
		// Send will remove the channel from f.sendCases.
	case <-f.sendLock: // 拿到了sendLock，说明删除也要拿到锁
		// No Send is in progress, delete the channel now that we have the send lock.
		f.sendCases = f.sendCases.delete(f.sendCases.find(ch))
		f.sendLock <- struct{}{}
	}
}

// Send delivers to all subscribed channels simultaneously.
// It returns the number of subscribers that the value was sent to.
func (f *Feed) Send(value interface{}) (nsent int) {
	rvalue := reflect.ValueOf(value)

	f.once.Do(func() { f.init(rvalue.Type()) })
	if f.etype != rvalue.Type() {
		panic(feedTypeError{op: "Send", got: rvalue.Type(), want: f.etype})
	}

	<-f.sendLock

	// Add new cases from the inbox after taking the send lock.
	f.mu.Lock()
	f.sendCases = append(f.sendCases, f.inbox...) // 合并待处理信息
	f.inbox = nil
	f.mu.Unlock()

	// Set the sent value on all channels.
	// 这里应该只是填充值
	for i := firstSubSendCase; i < len(f.sendCases); i++ {
		f.sendCases[i].Send = rvalue
	}

	// Send until all channels except removeSub have been chosen. 'cases' tracks a prefix
	// of sendCases. When a send succeeds, the corresponding case moves to the end of
	// 'cases' and it shrinks by one element.
	cases := f.sendCases
	for {
		// Fast path: try sending without blocking before adding to the select set.
		// This should usually succeed if subscribers are fast enough and have free
		// buffer space.
		// 这里才是尝试群发
		for i := firstSubSendCase; i < len(cases); i++ {
			if cases[i].Chan.TrySend(rvalue) {
				nsent++
				cases = cases.deactivate(i)
				i--
			}
		}
		if len(cases) == firstSubSendCase {
			break
		}
		// Select on all the receivers, waiting for them to unblock.
		// 这个 select 会阻塞，直到有一个case可用
		chosen, recv, _ := reflect.Select(cases)
		if chosen == 0 /* <-f.removeSub */ { // 这是有人朝这个管道写入了一个channel，也就是执行了remove函数，所以才需要删除一个
			index := f.sendCases.find(recv.Interface())
			f.sendCases = f.sendCases.delete(index)
			if index >= 0 && index < len(cases) {
				// Shrink 'cases' too because the removed case was still active.
				cases = f.sendCases[:len(cases)-1]
			}
		} else {
			cases = cases.deactivate(chosen)
			nsent++
		}
	}

	// Forget about the sent value and hand off the send lock.
	for i := firstSubSendCase; i < len(f.sendCases); i++ {
		f.sendCases[i].Send = reflect.Value{}
	}
	f.sendLock <- struct{}{}
	return nsent
}

type feedSub struct {
	feed    *Feed
	channel reflect.Value
	errOnce sync.Once
	err     chan error
}

func (sub *feedSub) Unsubscribe() {
	sub.errOnce.Do(func() {
		sub.feed.remove(sub)
		close(sub.err)
	})
}

func (sub *feedSub) Err() <-chan error {
	return sub.err
}

type caseList []reflect.SelectCase

// find returns the index of a case containing the given channel.
func (cs caseList) find(channel interface{}) int {
	for i, cas := range cs {
		if cas.Chan.Interface() == channel {
			return i
		}
	}
	return -1
}

// delete removes the given case from cs.
func (cs caseList) delete(index int) caseList {
	return append(cs[:index], cs[index+1:]...)
}

// deactivate moves the case at index into the non-accessible portion of the cs slice.
// 将 index 与最后一个元素交换，并将 slice 大小减 1
func (cs caseList) deactivate(index int) caseList {
	last := len(cs) - 1
	cs[index], cs[last] = cs[last], cs[index]
	return cs[:last]
}

// func (cs caseList) String() string {
//     s := "["
//     for i, cas := range cs {
//             if i != 0 {
//                     s += ", "
//             }
//             switch cas.Dir {
//             case reflect.SelectSend:
//                     s += fmt.Sprintf("%v<-", cas.Chan.Interface())
//             case reflect.SelectRecv:
//                     s += fmt.Sprintf("<-%v", cas.Chan.Interface())
//             }
//     }
//     return s + "]"
// }
