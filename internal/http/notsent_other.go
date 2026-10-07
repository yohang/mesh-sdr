//go:build !linux

package http

func setNotSentLowat(uintptr, int) {}
