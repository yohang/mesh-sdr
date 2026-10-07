// Command fakedecoder imitates a streaming or batch decoder: it reads frames
// on stdin, prints one decode line per frame on stdout and writes stderr lines
// on demand. Built by the supervisor tests.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	frame := flag.Int("frame", 16, "bytes per frame")
	stderrLines := flag.String("stderr", "", "lines to print on stderr at start, separated by |")
	flood := flag.Int("flood", 0, "unknown stderr lines to print at start")
	longLine := flag.Int("long-line", 0, "print one stderr line of this many bytes")
	statusEvery := flag.Duration("status-every", 0, "print a status line on stdout periodically")
	silentAfter := flag.Duration("silent-after", 0, "stop all stdout after this duration (keep running)")
	exitAfter := flag.Duration("exit-after", 0, "exit after this duration")
	exitCode := flag.Int("exit-code", 1, "exit code for -exit-after or EOF")
	hang := flag.Bool("hang", false, "do not read stdin, never exit")
	dumpLimits := flag.Bool("dump-limits", false, "print /proc/self/limits and status on stderr")
	flag.Parse()

	out := bufio.NewWriter(os.Stdout)
	var silent atomic.Bool
	if *silentAfter > 0 {
		time.AfterFunc(*silentAfter, func() { silent.Store(true) })
	}
	if *stderrLines != "" {
		for _, l := range strings.Split(*stderrLines, "|") {
			fmt.Fprintln(os.Stderr, l)
		}
	}
	for i := range *flood {
		fmt.Fprintf(os.Stderr, "noise line %d\n", i)
	}
	if *longLine > 0 {
		fmt.Fprintln(os.Stderr, strings.Repeat("x", *longLine))
		fmt.Fprintln(os.Stderr, "after long line")
	}
	if *dumpLimits {
		for _, f := range []string{"/proc/self/limits", "/proc/self/status"} {
			b, _ := os.ReadFile(f)
			for _, l := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(l, "Max address space") || strings.HasPrefix(l, "Max open files") || strings.HasPrefix(l, "NoNewPrivs") {
					fmt.Fprintln(os.Stderr, "limit "+strings.Join(strings.Fields(l), " "))
				}
			}
		}
		prio, _ := syscall.Getpriority(syscall.PRIO_PROCESS, 0)
		fmt.Fprintf(os.Stderr, "limit nice %d\n", 20-prio) // raw kernel value is 20-nice
	}
	if *exitAfter > 0 {
		time.AfterFunc(*exitAfter, func() { os.Exit(*exitCode) })
	}
	if *statusEvery > 0 {
		go func() {
			for range time.Tick(*statusEvery) {
				if !silent.Load() {
					fmt.Println("STATUS ok") // unbuffered stdout, separate from out
				}
			}
		}()
	}
	if *hang {
		for {
			time.Sleep(time.Hour)
		}
	}
	buf := make([]byte, *frame)
	n := 0
	for {
		if _, err := io.ReadFull(os.Stdin, buf); err != nil {
			out.Flush()
			if *statusEvery > 0 {
				for {
					time.Sleep(time.Hour)
				} // periodic-output tool: stays alive on EOF
			}
			os.Exit(*exitCode)
		}
		n++
		if !silent.Load() {
			fmt.Fprintf(out, "DECODE n=%d first=%d\n", n, buf[0])
			out.Flush()
		}
	}
}
