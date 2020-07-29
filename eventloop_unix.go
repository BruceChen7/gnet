// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build linux darwin netbsd freebsd openbsd dragonfly

package gnet

import (
	"net"
	"time"

	"github.com/panjf2000/gnet/internal/netpoll"
	"golang.org/x/sys/unix"
)

type eventloop struct {
	idx               int                     // loop index in the server loops list
	svr               *server                 // server in loop
	codec             ICodec                  // codec for TCP
	packet            []byte                  // read packet buffer
	poller            *netpoll.Poller         // epoll or kqueue
	connCount         int32                   // number of active connections in event-loop
	connections       map[int]*conn           // loop connections fd -> conn
	eventHandler      EventHandler            // user eventHandler
	calibrateCallback func(*eventloop, int32) // callback func for re-adjusting connCount
}

func (el *eventloop) closeAllConns() {
	// Close loops and all outstanding connections
	for _, c := range el.connections {
		_ = el.loopCloseConn(c, nil)
	}
}

func (el *eventloop) loopRun() {
	defer func() {
		el.closeAllConns()
		if el.idx == 0 && el.svr.opts.Ticker {
			close(el.svr.ticktock)
		}
		el.svr.signalShutdown()
	}()

	// 如果是主loop，并且设置了Ticker
	if el.idx == 0 && el.svr.opts.Ticker {
		// 添加一个goroutine做一些定时任务
		go el.loopTicker()
	}

	el.svr.logger.Printf("event-loop:%d exits with error: %v\n", el.idx, el.poller.Polling(el.handleEvent))
}

func (el *eventloop) loopAccept(fd int) error {
	if fd == el.svr.ln.fd {
		if el.svr.ln.pconn != nil {
			return el.loopReadUDP(fd)
		}
		nfd, sa, err := unix.Accept(fd)
		if err != nil {
			if err == unix.EAGAIN {
				return nil
			}
			return err
		}
		if err = unix.SetNonblock(nfd, true); err != nil {
			return err
		}
		c := newTCPConn(nfd, el, sa)
		if err = el.poller.AddRead(c.fd); err == nil {
			el.connections[c.fd] = c
			el.calibrateCallback(el, 1)
			return el.loopOpen(c)
		}
		return err
	}
	return nil
}

func (el *eventloop) loopOpen(c *conn) error {
	c.opened = true
	c.localAddr = el.svr.ln.lnaddr
	c.remoteAddr = netpoll.SockaddrToTCPOrUnixAddr(c.sa)
	out, action := el.eventHandler.OnOpened(c)
	if el.svr.opts.TCPKeepAlive > 0 {
		if _, ok := el.svr.ln.ln.(*net.TCPListener); ok {
			_ = netpoll.SetKeepAlive(c.fd, int(el.svr.opts.TCPKeepAlive/time.Second))
		}
	}
	if out != nil {
		c.open(out)
	}

	if !c.outboundBuffer.IsEmpty() {
		_ = el.poller.AddWrite(c.fd)
	}

	return el.handleAction(c, action)
}

func (el *eventloop) loopRead(c *conn) error {
	n, err := unix.Read(c.fd, el.packet)
	if n == 0 || err != nil {
		if err == unix.EAGAIN {
			return nil
		}
		return el.loopCloseConn(c, err)
	}
	c.buffer = el.packet[:n]
	// 用来解码
	for inFrame, _ := c.read(); inFrame != nil; inFrame, _ = c.read() {
		// 调用用户层的回调
		out, action := el.eventHandler.React(inFrame, c)
		if out != nil {
			// 写frame
			outFrame, _ := el.codec.Encode(c, out)
			el.eventHandler.PreWrite()
			// 写到连接的buffer
			c.write(outFrame)
		}
		switch action {
		case None:
		case Close:
			return el.loopCloseConn(c, nil)
		case Shutdown:
			return errServerShutdown
		}
		if !c.opened {
			return nil
		}
	}
	// 没有达到一条application message，写到 connection buffer中
	_, _ = c.inboundBuffer.Write(c.buffer)

	return nil
}

func (el *eventloop) loopWrite(c *conn) error {
	// 调用用户层的回调
	el.eventHandler.PreWrite()

	head, tail := c.outboundBuffer.LazyReadAll()
	// 向fd写head中的数据
	n, err := unix.Write(c.fd, head)
	if err != nil {
		// 写到eagain，那么下次进行继续写
		if err == unix.EAGAIN {
			return nil
		}
		// 写失败，直接关闭
		return el.loopCloseConn(c, err)
	}
	// 移动buffer的读指针
	c.outboundBuffer.Shift(n)
	// 都写完了，
	if len(head) == n && tail != nil {
		n, err = unix.Write(c.fd, tail)
		if err != nil {
			if err == unix.EAGAIN {
				return nil
			}
			return el.loopCloseConn(c, err)
		}
		c.outboundBuffer.Shift(n)
	}
	// 写buffer都写完了
	if c.outboundBuffer.IsEmpty() {
		// 调整成read模式
		_ = el.poller.ModRead(c.fd)
	}
	return nil
}

func (el *eventloop) loopCloseConn(c *conn, err error) error {
	// 等待写buffer中的写完
	if !c.outboundBuffer.IsEmpty() && err == nil {
		_ = el.loopWrite(c)
	}
	err0, err1 := el.poller.Delete(c.fd), unix.Close(c.fd)
	if err0 == nil && err1 == nil {
		// 释放map中的键
		delete(el.connections, c.fd)
		el.calibrateCallback(el, -1)
		switch el.eventHandler.OnClosed(c, err) {
		case Shutdown:
			return errServerShutdown
		}
		// 释放tcp资源
		c.releaseTCP()
	} else {
		if err0 != nil {
			el.svr.logger.Printf("failed to delete fd:%d from poller, error:%v\n", c.fd, err0)
		}
		if err1 != nil {
			el.svr.logger.Printf("failed to close fd:%d, error:%v\n", c.fd, err1)
		}
	}
	return nil
}

func (el *eventloop) loopWake(c *conn) error {
	//if co, ok := el.connections[c.fd]; !ok || co != c {
	//	return nil // ignore stale wakes.
	//}
	out, action := el.eventHandler.React(nil, c)
	if out != nil {
		frame, _ := el.codec.Encode(c, out)
		c.write(frame)
	}
	return el.handleAction(c, action)
}

// 做一些定时任务
func (el *eventloop) loopTicker() {
	var (
		delay time.Duration
		open  bool
		err   error
	)
	for {
		//
		err = el.poller.Trigger(func() (err error) {
			// 用户自己设置回调返回的delay和action
			delay, action := el.eventHandler.Tick()
			el.svr.ticktock <- delay
			switch action {
			case None:
			case Shutdown:
				err = errServerShutdown
			}
			return
		})
		if err != nil {
			el.svr.logger.Printf("failed to awake poller with error:%v, stopping ticker\n", err)
			break
		}
		// 再一次睡眠
		if delay, open = <-el.svr.ticktock; open {
			time.Sleep(delay)
		} else {
			break
		}
	}
}

func (el *eventloop) handleAction(c *conn, action Action) error {
	switch action {
	case None:
		return nil
	case Close:
		return el.loopCloseConn(c, nil)
	case Shutdown:
		return errServerShutdown
	default:
		return nil
	}
}

func (el *eventloop) loopReadUDP(fd int) error {
	n, sa, err := unix.Recvfrom(fd, el.packet, 0)
	if err != nil || n == 0 {
		if err != nil && err != unix.EAGAIN {
			el.svr.logger.Printf("failed to read UDP packet from fd:%d, error:%v\n", fd, err)
		}
		return nil
	}
	c := newUDPConn(fd, el, sa)
	out, action := el.eventHandler.React(el.packet[:n], c)
	if out != nil {
		el.eventHandler.PreWrite()
		_ = c.sendTo(out)
	}
	switch action {
	case Shutdown:
		return errServerShutdown
	}
	c.releaseUDP()
	return nil
}
