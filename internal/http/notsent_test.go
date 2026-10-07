package http

import (
	"context"
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNotSentLowat(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			defer func() { _ = c.Close() }()
		}
	}()

	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	NotSentLowat(16<<10)(context.Background(), c)

	raw, _ := c.(*net.TCPConn).SyscallConn()

	var got int

	_ = raw.Control(func(fd uintptr) { got, _ = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT) })

	if got != 16<<10 {
		t.Fatalf("TCP_NOTSENT_LOWAT = %d", got)
	}
}
