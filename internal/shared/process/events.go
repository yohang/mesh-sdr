package process

import (
	"errors"
	"time"
)

// State is the supervisor-level lifecycle state of one instance. The device
// manager maps it to device.state (§8.2); the decoder supervisor maps it, with
// Diag, to diagnostics (§9.3).
type State string

const (
	StateStarting    State = "starting"
	StateRunning     State = "running"     // readiness seen (first stdout byte or Touch)
	StateRetryWait   State = "retry_wait"  // waiting for the back-off delay
	StateCrashLoop   State = "crash_loop"  // crash-loop threshold hit, slow retry
	StateStopping    State = "stopping"    // SIGTERM sent to the process group
	StateStopped     State = "stopped"     // ctx cancelled, or batch job completed
	StateFailed      State = "failed"      // max attempts exhausted
	StateUnavailable State = "unavailable" // tool missing, not executable or misconfigured: no restart
	StateErrored     State = "errored"     // input_format error: no restart until config changes
	StateTimedOut    State = "timed_out"   // batch job deadline
)

// Diag is the diagnostics state suggested by the supervisor (TECHNICAL_SPEC
// §8.4 classification table, §9.3). Empty when the event carries none. The
// evaluator owns the session state; this is an input to it.
type Diag string

const (
	DiagNone         Diag = ""
	DiagUnavailable  Diag = "UNAVAILABLE"
	DiagDecoderError Diag = "DECODER_ERROR"
	DiagTimeout      Diag = "TIMEOUT"
)

// ExitClass classifies how a process ended (§8.4 "Exit codes").
type ExitClass string

const (
	ExitNone       ExitClass = ""
	ExitNormal     ExitClass = "normal"     // 0
	ExitError      ExitClass = "error"      // other non-zero
	ExitCrash      ExitClass = "crash"      // signal not sent by the supervisor
	ExitNotFound   ExitClass = "not_found"  // ENOENT at exec or 127
	ExitPermission ExitClass = "permission" // EACCES at exec or 126
	ExitStopped    ExitClass = "stopped"    // killed by the supervisor
)

// ExitInfo is attached to events that follow a process exit.
type ExitInfo struct {
	Class  ExitClass
	Code   int    // -1 when killed by a signal or never started
	Signal string // signal name when killed by a signal
}

// Event is emitted on every transition and diagnostics-relevant occurrence.
// Sinks MUST NOT block: the node event buffer is bounded (ADR 0008).
type Event struct {
	Time     time.Time
	Instance string
	Kind     string
	PID      int
	State    State
	Reason   string
	Diag     Diag
	Attempt  int           // restart attempt (1-based) for retry_wait
	Delay    time.Duration // back-off delay for retry_wait / crash_loop
	Reprobe  bool          // ENOENT / 127: re-run the capability probe (§8.4)
	Exit     *ExitInfo
}

// Sink receives events. It is called synchronously from the supervisor
// goroutine and must return quickly.
type Sink func(Event)

// Terminal errors returned by Instance.Run.
var (
	ErrUnavailable  = errors.New("tool unavailable")
	ErrDecoderError = errors.New("decoder error")
	ErrFailed       = errors.New("max start attempts exhausted")
	ErrJobTimeout   = errors.New("job deadline exceeded")
	ErrInvalidSpec  = errors.New("invalid spec")
	ErrWorkdir      = errors.New("workdir")
	// ErrNoRuntimeDir: without node.runtime_dir there is no supervisor.
	ErrNoRuntimeDir = errors.New("node.runtime_dir is not set: no tool can run")
)
