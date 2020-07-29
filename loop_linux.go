// Copyright 2019 Andy Pan. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build linux

package gnet

import "github.com/panjf2000/gnet/internal/netpoll"

// 用来处理读写的回调事件
func (el *eventloop) handleEvent(fd int, ev uint32) error {
	// 如果建立的连接，也就是连接存在
	if c, ok := el.connections[fd]; ok {
		switch c.outboundBuffer.IsEmpty() {
		// Don't change the ordering of processing EPOLLOUT | EPOLLRDHUP / EPOLLIN unless you're 100%
		// sure what you're doing!
		// Re-ordering can easily introduce bugs and bad side-effects, as I found out painfully in the past.
		case false:
			// 首先处理写事件
			if ev&netpoll.OutEvents != 0 {
				return el.loopWrite(c)
			}
			return nil
		case true:
			if ev&netpoll.InEvents != 0 {
				return el.loopRead(c)
			}
			return nil
		}
	}
	return el.loopAccept(fd)
}
