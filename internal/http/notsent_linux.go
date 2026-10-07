package http

import "golang.org/x/sys/unix"

func setNotSentLowat(fd uintptr, n int) {
	_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, n)
}
