package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// sysProcAttr puts the child in its own process group (so the whole group can
// be signalled) and asks the kernel to SIGKILL it when its parent dies.
//
// Pdeathsig gotcha: the signal fires when the *thread* that forked the child
// exits, not the process. Go may retire OS threads (LockOSThread goroutines
// that return). All spawns therefore go through spawner, a goroutine locked
// to one OS thread that never exits.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
}

type spawnReq struct {
	cmd  *exec.Cmd
	done chan error
}

// spawner serialises cmd.Start on a dedicated, never-exiting OS thread.
type spawner struct{ reqs chan spawnReq }

func newSpawner() *spawner {
	s := &spawner{reqs: make(chan spawnReq)}
	go func() {
		runtime.LockOSThread() // never unlocked: the thread lives as long as the process
		for r := range s.reqs {
			r.done <- r.cmd.Start()
		}
	}()
	return s
}

func (s *spawner) start(cmd *exec.Cmd) error {
	done := make(chan error, 1)
	s.reqs <- spawnReq{cmd: cmd, done: done}
	return <-done
}

// killGroup signals the whole process group. ESRCH (group gone) is not an
// error.
func killGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 1 {
		return fmt.Errorf("refusing to signal pgid %d", pgid)
	}
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// Limits are applied in the child, between fork and exec, by the exec helper
// (the node binary re-executed with ExecHelperArg). os/exec has no rlimit or
// no_new_privs hook, and prlimit(2) after Start leaves a window in which the
// tool runs unconstrained.
type Limits struct {
	AddressSpace uint64 // RLIMIT_AS in bytes (0 = unchanged); §8.4 default 512 MiB
	OpenFiles    uint64 // RLIMIT_NOFILE (0 = unchanged)
	CPUSeconds   uint64 // RLIMIT_CPU (0 = unchanged)
	Core         *uint64
	Nice         int  // setpriority; batch decoders use 10 (§8.3 rule 6)
	NoNewPrivs   bool // PR_SET_NO_NEW_PRIVS
}

func (l Limits) zero() bool {
	return l.AddressSpace == 0 && l.OpenFiles == 0 && l.CPUSeconds == 0 && l.Core == nil && l.Nice == 0 && !l.NoNewPrivs
}

// ExecHelperArg is argv[1] of the helper mode.
const ExecHelperArg = "__exec-helper"

func helperArgv(helper string, l Limits, path string, argv []string) []string {
	out := []string{helper, ExecHelperArg}
	if l.AddressSpace > 0 {
		out = append(out, "as="+strconv.FormatUint(l.AddressSpace, 10))
	}
	if l.OpenFiles > 0 {
		out = append(out, "nofile="+strconv.FormatUint(l.OpenFiles, 10))
	}
	if l.CPUSeconds > 0 {
		out = append(out, "cpu="+strconv.FormatUint(l.CPUSeconds, 10))
	}
	if l.Core != nil {
		out = append(out, "core="+strconv.FormatUint(*l.Core, 10))
	}
	if l.Nice != 0 {
		out = append(out, "nice="+strconv.Itoa(l.Nice))
	}
	if l.NoNewPrivs {
		out = append(out, "nnp=1")
	}
	out = append(out, "--", path)
	return append(out, argv...)
}

// MaybeRunExecHelper must be the first call in main (and TestMain). In helper
// mode it applies the limits and execve()s the tool; it never returns. Exec
// failures exit 127 (ENOENT) or 126 (other), like a shell would, so the
// supervisor classifies them the same way as a direct spawn.
func MaybeRunExecHelper() {
	if len(os.Args) < 2 || os.Args[1] != ExecHelperArg {
		return
	}
	// prctl(PR_SET_NO_NEW_PRIVS) and setpriority(PRIO_PROCESS, 0) apply to
	// the calling thread on Linux: every call, and the execve that inherits
	// them, must run on the same OS thread. The helper never returns, so
	// the thread is never unlocked.
	runtime.LockOSThread()
	nnp := false
	args := os.Args[2:]
	i := 0
	for ; i < len(args) && args[i] != "--"; i++ {
		k, v, _ := strings.Cut(args[i], "=")
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			helperFail(126, "bad limit "+args[i])
		}
		switch k {
		case "as":
			setrlimit(syscall.RLIMIT_AS, uint64(n))
		case "nofile":
			setrlimit(syscall.RLIMIT_NOFILE, uint64(n))
		case "cpu":
			setrlimit(syscall.RLIMIT_CPU, uint64(n))
		case "core":
			setrlimit(syscall.RLIMIT_CORE, uint64(n))
		case "nice":
			if err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, int(n)); err != nil {
				helperFail(126, "setpriority: "+err.Error())
			}
		case "nnp":
			if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); e != 0 {
				helperFail(126, "prctl no_new_privs: "+e.Error())
			}
			nnp = true
		default:
			helperFail(126, "unknown limit "+k)
		}
	}
	if i+1 >= len(args) {
		helperFail(126, "missing tool path")
	}
	// Never exec a tool without the restriction it was asked for.
	if nnp {
		if v, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prGetNoNewPrivs, 0, 0); e != 0 || v != 1 {
			helperFail(126, "no_new_privs not in effect")
		}
	}
	path, argv := args[i+1], args[i+2:]
	err := syscall.Exec(path, argv, os.Environ())
	if errors.Is(err, syscall.ENOENT) {
		helperFail(127, "exec: "+err.Error())
	}
	helperFail(126, "exec: "+err.Error())
}

// prctl options (linux/prctl.h).
const (
	prSetNoNewPrivs = 38
	prGetNoNewPrivs = 39
)

func setrlimit(res int, v uint64) {
	if err := syscall.Setrlimit(res, &syscall.Rlimit{Cur: v, Max: v}); err != nil {
		helperFail(126, fmt.Sprintf("setrlimit %d: %v", res, err))
	}
}

func helperFail(code int, msg string) {
	fmt.Fprintln(os.Stderr, "exec-helper: "+msg)
	os.Exit(code)
}
