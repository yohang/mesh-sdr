// Command fakeconnector imitates an SDR connector: it streams bytes on stdout
// and misbehaves on demand. Built by the supervisor tests.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

func main() {
	chunk := flag.Int("chunk", 4096, "bytes per write")
	interval := flag.Duration("interval", 0, "delay between writes")
	readyDelay := flag.Duration("ready-delay", 0, "delay before the first byte")
	hang := flag.Bool("hang", false, "never write anything")
	exitAfter := flag.Duration("exit-after", 0, "exit after this duration")
	exitCode := flag.Int("exit-code", 1, "exit code for -exit-after")
	crashAfter := flag.Duration("crash-after", 0, "die by SIGABRT after this duration")
	ignoreTerm := flag.Bool("ignore-term", false, "ignore SIGTERM")
	spawnChild := flag.Bool("spawn-child", false, "spawn a grandchild that ignores SIGTERM")
	report := flag.Bool("report", false, "report total bytes written on stderr after each write")
	dumpEnv := flag.Bool("dump-env", false, "print env, cwd and limits on stderr")
	stderrLines := flag.String("stderr", "", "lines to print on stderr at start, separated by |")
	flag.Parse()

	if *ignoreTerm {
		signal.Ignore(syscall.SIGTERM)
	}
	if *stderrLines != "" {
		for _, l := range strings.Split(*stderrLines, "|") {
			fmt.Fprintln(os.Stderr, l)
		}
	}
	if *dumpEnv {
		for _, kv := range os.Environ() {
			fmt.Fprintln(os.Stderr, "env "+kv)
		}
		wd, _ := os.Getwd()
		fmt.Fprintln(os.Stderr, "cwd "+wd)
		fmt.Fprintf(os.Stderr, "pgid %d pid %d\n", syscall.Getpgrp(), os.Getpid())
	}
	if *spawnChild {
		c := exec.Command(os.Args[0], "-hang", "-ignore-term")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr // grandchild holds the pipes too
		if err := c.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "spawn:", err)
			os.Exit(3)
		}
		fmt.Fprintf(os.Stderr, "child %d\n", c.Process.Pid)
	}
	if *exitAfter > 0 {
		time.AfterFunc(*exitAfter, func() { os.Exit(*exitCode) })
	}
	if *crashAfter > 0 {
		time.AfterFunc(*crashAfter, func() {
			debug.SetTraceback("crash") // runtime aborts with a real SIGABRT
			panic("fake crash")
		})
	}
	if *hang {
		for {
			time.Sleep(time.Hour)
		}
	}
	time.Sleep(*readyDelay)
	buf := make([]byte, *chunk)
	for i := range buf {
		buf[i] = byte(i)
	}
	total := 0
	for {
		n, err := os.Stdout.Write(buf)
		total += n
		if err != nil {
			os.Exit(0) // EPIPE: consumer gone
		}
		if *report {
			fmt.Fprintf(os.Stderr, "written %d\n", total)
		}
		if *interval > 0 {
			time.Sleep(*interval)
		}
	}
}
