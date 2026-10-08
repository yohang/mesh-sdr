// Command fakeconnector imitates an owrx_connector (rtl_connector,
// rtl_tcp_connector) for tests and the dev stack, without hardware: same
// argv, CF32 IQ on the loopback IQ port, key:value lines on the control
// port. It synthesises an NFM carrier modulated by a 1 kHz tone at
// centre + rate/8, a steady carrier at centre − rate/5 and noise; both
// carriers stay at their absolute frequencies when the centre is retuned.
//
// The device argument (-d) selects a misbehaviour: "crash" exits after
// 300 ms, "stall" stops streaming after 500 ms, "noiq" never opens its IQ
// port; "zvei" modulates the NFM carrier with the ZVEI1 sequence 12345
// every 2 s instead of the 1 kHz tone (decoder tests); "text" replaces
// the NFM carrier with the text modes of the native decoders around a USB
// dial at the same frequency (SITOR-B at +600 Hz, BPSK31 at +1000 Hz, CW
// at +1500 Hz, RTTY-170 at +2200 Hz, each message repeated); "ft8"
// replaces it by an FT8 signal "CQ K1ABC FN42" 1500 Hz above it (USB), in
// every 15 s UTC slot; anything else streams.
//
// It is not part of the product images.
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp/dsptest"
)

func main() {
	device := flag.String("d", "0", "device")
	port := flag.Int("p", 4590, "IQ port")
	ctlPort := flag.Int("c", 0, "control port")
	freq := flag.Float64("f", 145_000_000, "centre frequency")
	rate := flag.Int("s", 2_400_000, "sample rate")
	flag.String("g", "auto", "gain")
	flag.Float64("P", 0, "ppm")
	flag.Bool("i", false, "iq swap")
	flag.Bool("b", false, "bias-tee")
	flag.Int("e", 0, "direct sampling input")
	version := flag.Bool("v", false, "print the version")
	flag.BoolVar(version, "version", false, "print the version")
	flag.Parse()

	if *version {
		fmt.Println("fakeconnector version 0.6.5-fake")
		os.Exit(1) // like owrx_connector
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-sig
		os.Exit(0)
	}()

	var center atomic.Int64
	center.Store(int64(*freq))

	if *ctlPort > 0 {
		go control(*ctlPort, &center)
	}

	switch *device {
	case "crash":
		time.Sleep(300 * time.Millisecond)
		fmt.Fprintln(os.Stderr, "device lost")
		os.Exit(1)
	case "noiq":
		select {}
	}

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to bind the IQ port:", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "fakeconnector streaming at", *rate)

	s := &synth{
		rate: float64(*rate), carrier: float64(*freq) + float64(*rate)/8, tone: float64(*freq) - float64(*rate)/5,
		zvei: *device == "zvei", ft8: *device == "ft8",
	}
	if *device == "text" {
		s.text = textSignals()
	}

	s.rng = rand.New(rand.NewPCG(1, 2))
	stallAfter := time.Duration(0)

	if *device == "stall" {
		stallAfter = 500 * time.Millisecond
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			os.Exit(1)
		}

		s.stream(conn, &center, stallAfter)
	}
}

func control(port int, center *atomic.Int64) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to bind the control port:", err)

		return
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}

			if k == "center_freq" {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					center.Store(int64(f))
					fmt.Fprintln(os.Stderr, "center_freq set to", v)
				}
			}
		}

		_ = conn.Close()
	}
}

type synth struct {
	rate          float64
	carrier, tone float64
	n             float64
	phaseA        float64
	phaseB        float64
	phaseM        float64
	zvei          bool
	ft8           bool
	// text are the text mode signals at textRate (device "text").
	text [][]complex64
	rng  *rand.Rand
}

// textRate is the rate the text mode signals are synthesised at.
const textRate = 12000

// textSignals synthesises the text mode signals of the "text" device.
func textSignals() [][]complex64 {
	return [][]complex64{
		dsptest.SITORB("ZCZC EA01 DEV NAVAREA TEST MESSAGE NNNN\n", 170, textRate, 600),
		dsptest.PSK("CQ CQ CQ de F4DEV F4DEV pse k\n", 31.25, textRate, 1000),
		dsptest.CW("CQ CQ DE F4DEV F4DEV K", 18, textRate, 1500),
		dsptest.RTTY("RYRYRY CQ CQ DE F4DEV F4DEV K\r\n", 45.45, 170, false, textRate, 2200),
	}
}

// textAt returns the sum of the text signals at time t, linearly
// interpolated, each one repeated.
func (s *synth) textAt(t float64) complex128 {
	x := t * textRate
	i := int(x)
	f := x - float64(i)

	var v complex128

	for _, sig := range s.text {
		a, b := sig[i%len(sig)], sig[(i+1)%len(sig)]
		v += complex128(a)*complex(1-f, 0) + complex128(b)*complex(f, 0)
	}

	return v
}

// ft8Symbols are the 79 channel symbols of "CQ K1ABC FN42" (ft8sim, WSJT-X
// 2.7): 8-FSK, 6.25 Hz apart, 0.16 s each, from 0.5 s into the slot.
const ft8Symbols = "3140652000000001005476704606021533433140652736011047517007334745455133543140652"

// ft8Tone returns the FT8 tone (Hz above the carrier) at the Unix time
// sec, and false between transmissions.
func ft8Tone(sec float64) (float64, bool) {
	i := int((math.Mod(sec, 15) - 0.5) / 0.16)
	if math.Mod(sec, 15) < 0.5 || i >= len(ft8Symbols) {
		return 0, false
	}

	return 1500 + 6.25*float64(ft8Symbols[i]-'0'), true
}

// zveiTones are the ZVEI1 tones of the digits 1 to 5, 70 ms each.
var zveiTones = []float64{1060, 1160, 1270, 1400, 1530}

// modulation returns the audio tone at t (0: silence).
func (s *synth) modulation(t float64) float64 {
	if s.text != nil {
		return 0
	}

	if !s.zvei {
		return 1000
	}

	i := int(math.Mod(t, 2) / 0.07)
	if i >= len(zveiTones) {
		return 0
	}

	return zveiTones[i]
}

func (s *synth) stream(conn net.Conn, center *atomic.Int64, stallAfter time.Duration) {
	defer func() { _ = conn.Close() }()

	const block = 8192

	buf := make([]byte, block*8)
	period := time.Duration(float64(block) / s.rate * float64(time.Second))
	started := time.Now()
	next := started

	for {
		if stallAfter > 0 && time.Since(started) > stallAfter {
			time.Sleep(time.Hour)
		}

		c := float64(center.Load())
		at := float64(next.UnixNano()) / 1e9

		for i := range block {
			t := s.n / s.rate
			s.n++

			// NFM: 2.5 kHz deviation by the tone.
			if f := s.modulation(t); f > 0 {
				s.phaseM += 2 * math.Pi * f / s.rate
			}

			inst, amp := s.carrier-c+2500*math.Sin(s.phaseM), 0.3
			if s.ft8 {
				// The sample time follows the pacing of the blocks.
				f, on := ft8Tone(at + float64(i)/s.rate)
				inst = s.carrier - c + f

				if !on {
					amp = 0
				}
			}

			s.phaseA += 2 * math.Pi * inst / s.rate
			s.phaseB += 2 * math.Pi * (s.tone - c) / s.rate

			re := amp*math.Cos(s.phaseA) + 0.05*math.Cos(s.phaseB) + 0.003*s.rng.NormFloat64()
			im := amp*math.Sin(s.phaseA) + 0.05*math.Sin(s.phaseB) + 0.003*s.rng.NormFloat64()

			if s.text != nil {
				// The text signals around the carrier frequency, no carrier.
				v := s.textAt(t) * complex(0.2*math.Cos(s.phaseA)/0.5, 0.2*math.Sin(s.phaseA)/0.5)
				re = real(v) + 0.05*math.Cos(s.phaseB) + 0.003*s.rng.NormFloat64()
				im = imag(v) + 0.05*math.Sin(s.phaseB) + 0.003*s.rng.NormFloat64()
			}

			binary.LittleEndian.PutUint32(buf[8*i:], math.Float32bits(float32(re)))
			binary.LittleEndian.PutUint32(buf[8*i+4:], math.Float32bits(float32(im)))
		}

		s.phaseA = math.Mod(s.phaseA, 2*math.Pi)
		s.phaseB = math.Mod(s.phaseB, 2*math.Pi)
		s.phaseM = math.Mod(s.phaseM, 2*math.Pi)

		if _, err := conn.Write(buf); err != nil {
			return
		}

		next = next.Add(period)
		time.Sleep(time.Until(next))
	}
}
