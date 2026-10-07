// Command fakeconnector imitates an owrx_connector (rtl_connector,
// rtl_tcp_connector) for tests and the dev stack, without hardware: same
// argv, CF32 IQ on the loopback IQ port, key:value lines on the control
// port. It synthesises an NFM carrier modulated by a 1 kHz tone at
// centre + rate/8, a steady carrier at centre − rate/5 and noise; both
// carriers stay at their absolute frequencies when the centre is retuned.
//
// The device argument (-d) selects a misbehaviour: "crash" exits after
// 300 ms, "stall" stops streaming after 500 ms, "noiq" never opens its IQ
// port; anything else streams.
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

	s := &synth{rate: float64(*rate), carrier: float64(*freq) + float64(*rate)/8, tone: float64(*freq) - float64(*rate)/5}
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
	rng           *rand.Rand
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

		for i := range block {
			t := s.n / s.rate
			s.n++

			// NFM: 2.5 kHz deviation by a 1 kHz tone.
			inst := s.carrier - c + 2500*math.Sin(2*math.Pi*1000*t)
			s.phaseA += 2 * math.Pi * inst / s.rate
			s.phaseB += 2 * math.Pi * (s.tone - c) / s.rate

			re := 0.3*math.Cos(s.phaseA) + 0.05*math.Cos(s.phaseB) + 0.003*s.rng.NormFloat64()
			im := 0.3*math.Sin(s.phaseA) + 0.05*math.Sin(s.phaseB) + 0.003*s.rng.NormFloat64()

			binary.LittleEndian.PutUint32(buf[8*i:], math.Float32bits(float32(re)))
			binary.LittleEndian.PutUint32(buf[8*i+4:], math.Float32bits(float32(im)))
		}

		s.phaseA = math.Mod(s.phaseA, 2*math.Pi)
		s.phaseB = math.Mod(s.phaseB, 2*math.Pi)

		if _, err := conn.Write(buf); err != nil {
			return
		}

		next = next.Add(period)
		time.Sleep(time.Until(next))
	}
}
