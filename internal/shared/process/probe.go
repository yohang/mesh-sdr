package process

import (
	"bufio"
	"context"
	"io"
	"time"
)

// ProbeTimeout bounds a capability probe (§8.4 "Capability probing" rule 2).
const ProbeTimeout = 5 * time.Second

// Probe runs a version probe of a tool (§8.4 capability probing): a batch
// instance of spec (ID, Path, Args, ToolDirs) within ProbeTimeout, where any
// exit code completes it. Every line it prints, on stdout or stderr, goes to
// line, which judges the output. s nil (no runtime dir): ErrNoRuntimeDir.
func Probe(ctx context.Context, s *Supervisor, spec Spec, line func(string)) error {
	if s == nil {
		return ErrNoRuntimeDir
	}

	spec.Kind, spec.Mode, spec.Probe = "probe", Batch, true
	spec.Timeouts = Timeouts{Job: ProbeTimeout, Stop: time.Second}
	spec.Stdout = func(_ context.Context, r io.Reader) error {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line(sc.Text())
		}

		return sc.Err()
	}
	spec.OnLine = func(l Line) { line(l.Text) }

	in, err := s.NewInstance(spec)
	if err != nil {
		return err
	}

	return in.Run(ctx)
}
