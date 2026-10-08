package decoder

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// The packet decoder (DEC-031): direwolf demodulates AX.25 1200 Bd from FM
// audio at 48 kHz read on its stdin and hands every frame to the session
// over KISS on a pseudo-terminal (-p) that it names on its stdout: no
// network port. Its configuration lives in the session workdir. No iGate:
// APRS-IS is the hub's (M4).

// direwolfConfig is the config file of a session, in its workdir (the
// tool's working directory).
const direwolfConfig = "direwolf.conf"

// maxKISSFrame bounds a frame (AX.25 frames are at most ~330 bytes).
const maxKISSFrame = 2048

// direwolfConf is the configuration: one 1200 Bd channel on stdin, no
// audio output, no KISS TCP port (KISSPORT 0, the default 8001 included),
// no AGW port, no MYCALL, no iGate.
const direwolfConf = "ACHANNELS 1\nADEVICE stdin null\nCHANNEL 0\nMODEM 1200\nKISSPORT 0\nAGWPORT 0\n"

// direwolfArgs runs direwolf on raw s16le at 48 kHz from stdin (ADEVICE
// in the config) with its KISS pseudo-terminal, colours off, without the
// decoded APRS description (the node parses the frames). The heard lines
// with the audio level stay (§8.7: no -q h).
func direwolfArgs(sessionConfig) []string {
	return []string{"-c", direwolfConfig, "-r", "48000", "-t", "0", "-q", "d", "-p"}
}

// direwolfRules classify the stderr of direwolf (most of its output is on
// stdout).
var direwolfRules = []process.Rule{
	{Pattern: regexp.MustCompile(`(?i)(invalid|unrecognized|unknown) (option|command|parameter)`), Class: process.ClassFatalConfig},
	{Pattern: regexp.MustCompile(`(?i)(could not|can't|cannot|unable to) (open|read)`), Class: process.ClassInputError},
	{Pattern: regexp.MustCompile(`.`), Class: process.ClassInfo},
}

// kissPTY is the line where direwolf names its pseudo-terminal.
var kissPTY = regexp.MustCompile(`^Virtual KISS TNC is available on (/dev/pts/[0-9]{1,6})\s*$`)

// writeDirewolfConfig writes the config into the workdir (0600, exclusive
// create, no symlink followed, §8.4 rule 4).
func writeDirewolfConfig(workdir string) error {
	f, err := process.CreateFile(workdir, direwolfConfig, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", direwolfConfig, err)
	}

	if _, err := f.WriteString(direwolfConf); err != nil {
		_ = f.Close()

		return fmt.Errorf("write %s: %w", direwolfConfig, err)
	}

	return f.Close()
}

// kissRun prepares each run of direwolf: its config and a fresh signal
// for its KISS pseudo-terminal.
func (s *session) kissRun(args []string) func() (process.Run, error) {
	return func() (process.Run, error) {
		s.mu.Lock()
		s.kiss = &kissRun{up: make(chan struct{}), failed: make(chan struct{})}
		s.mu.Unlock()

		return process.Run{Args: args}, nil
	}
}

// kissRun is the KISS link of one run of direwolf: up is closed once its
// pseudo-terminal is read, failed when it cannot be: the run's stdin is
// then closed, direwolf exits and the restart policy applies.
type kissRun struct {
	up, failed chan struct{}
}

// currentKISS returns the KISS link of the current run.
func (s *session) currentKISS() *kissRun {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.kiss
}

// waitKISS waits until the KISS link of the run is read; an error ends
// the run's input.
func (s *session) waitKISS(ctx context.Context, k *kissRun) error {
	select {
	case <-k.up:
		return nil
	case <-k.failed:
		return errors.New("the KISS pseudo-terminal of direwolf could not be read")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// direwolfStdout reads the stdout of a run: once direwolf names its
// pseudo-terminal, the KISS frames are read from it until the run ends.
func (s *session) direwolfStdout(ctx context.Context, rd io.Reader, parse frameParser) error {
	k := s.currentKISS()
	opened := false

	process.ScanLines(rd, func(text string, _ bool) {
		m := kissPTY.FindStringSubmatch(text)
		if m == nil || opened {
			return
		}

		opened = true

		go func() {
			if err := s.readKISS(ctx, m[1], k.up, parse); err != nil && ctx.Err() == nil {
				s.log.Warn("direwolf KISS pseudo-terminal not read: the run ends", slog.Any("error", err))

				select {
				case <-k.up:
				default:
					close(k.failed)
				}
			}
		}()
	})

	return nil
}

// readKISS opens the pseudo-terminal in raw mode and parses every data
// frame until the run ends.
func (s *session) readKISS(ctx context.Context, path string, up chan struct{}, parse frameParser) error {
	// Non-blocking: the file is then read through the poller, and closing
	// it ends a pending read.
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}

	if err := rawTTY(fd); err != nil {
		_ = unix.Close(fd)

		return err
	}

	f := os.NewFile(uintptr(fd), path)
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })

	defer stop()
	defer func() { _ = f.Close() }()

	close(up)

	return readKISSFrames(bufio.NewReader(f), func(frame []byte) {
		for _, rec := range parse(frame, s.r.o.Now()) {
			s.decode(rec)
		}
	})
}

// rawTTY puts a terminal in raw mode (cfmakeraw): KISS bytes pass
// unchanged.
func rawTTY(fd int) error {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return fmt.Errorf("pseudo-terminal attributes: %w", err)
	}

	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		return fmt.Errorf("pseudo-terminal raw mode: %w", err)
	}

	return nil
}

// KISS framing (KA9Q/K3MC): FEND delimits frames, FESC escapes FEND and
// FESC inside them.
const (
	kissFEND  = 0xC0
	kissFESC  = 0xDB
	kissTFEND = 0xDC
	kissTFESC = 0xDD
)

// readKISSFrames reads KISS frames until r ends and calls fn with the AX.25
// frame of every data frame of port 0 (command byte 0x00). Oversized
// frames are dropped. The end of the pseudo-terminal (EIO once direwolf
// exits) and a closed file end it without error.
func readKISSFrames(r io.ByteReader, fn func(frame []byte)) error {
	var (
		buf     []byte
		escaped bool
		over    bool
	)

	for {
		b, err := r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EIO) {
				return nil
			}

			return err
		}

		switch {
		case b == kissFEND:
			if !over && len(buf) > 1 && buf[0] == 0x00 {
				fn(buf[1:])
			}

			buf, escaped, over = buf[:0], false, false

			continue
		case escaped:
			escaped = false

			switch b {
			case kissTFEND:
				b = kissFEND
			case kissTFESC:
				b = kissFESC
			}
		case b == kissFESC:
			escaped = true

			continue
		}

		if len(buf) >= maxKISSFrame {
			over = true

			continue
		}

		buf = append(buf, b)
	}
}

// AX25Frame is an AX.25 UI frame: addresses and information field.
type AX25Frame struct {
	Destination string
	Source      string
	// Path are the digipeaters, "*" marking those that repeated the frame.
	Path []string
	Info []byte
}

// parseAX25 decodes the address field (7 bytes per callsign, the last one
// with its extension bit set) and the information field of a UI frame
// (control 0x03, PID 0xF0).
func parseAX25(f []byte) (AX25Frame, error) {
	var out AX25Frame

	n := 0

	for i := 0; i+7 <= len(f) && n <= 10; i += 7 {
		call, err := ax25Call(f[i : i+7])
		if err != nil {
			return AX25Frame{}, err
		}

		switch n {
		case 0:
			out.Destination = call
		case 1:
			out.Source = call
		default:
			if f[i+6]&0x80 != 0 {
				call += "*"
			}

			out.Path = append(out.Path, call)
		}

		n++

		if f[i+6]&0x01 != 0 {
			rest := f[i+7:]
			if n < 2 || len(rest) < 2 || rest[0] != 0x03 || rest[1] != 0xF0 {
				return AX25Frame{}, errors.New("ax25: not a UI frame")
			}

			out.Info = rest[2:]

			return out, nil
		}
	}

	return AX25Frame{}, errors.New("ax25: truncated address field")
}

// ax25Call decodes one address: six shifted ASCII characters and the SSID.
func ax25Call(b []byte) (string, error) {
	var sb strings.Builder

	for _, c := range b[:6] {
		ch := c >> 1
		if ch == ' ' {
			continue
		}

		if (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') {
			return "", errors.New("ax25: invalid callsign character")
		}

		sb.WriteByte(ch)
	}

	if sb.Len() == 0 {
		return "", errors.New("ax25: empty callsign")
	}

	if ssid := (b[6] >> 1) & 0x0F; ssid > 0 {
		sb.WriteString("-" + strconv.Itoa(int(ssid)))
	}

	return sb.String(), nil
}
