package connector

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
)

// ErrNoPort means node.ipc_port_range is exhausted.
var ErrNoPort = errors.New("no free port in node.ipc_port_range")

// Ports allocates the loopback ports of the connectors from
// node.ipc_port_range (§8.2 rule 3): a port is reserved while its run
// lasts, and every run takes fresh ones (round robin), so a connector that
// failed to bind gets new ports on its next attempt.
type Ports struct {
	mu     sync.Mutex
	lo, hi int
	next   int
	used   map[int]bool
}

// NewPorts returns the pool [lo, hi].
func NewPorts(lo, hi int) (*Ports, error) {
	if lo < 1024 || hi > 65535 || hi < lo+1 {
		return nil, fmt.Errorf("invalid port range %d-%d", lo, hi)
	}

	return &Ports{lo: lo, hi: hi, next: lo, used: map[int]bool{}}, nil
}

// Take reserves n ports that are free on 127.0.0.1.
func (p *Ports) Take(n int) ([]int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]int, 0, n)
	size := p.hi - p.lo + 1

	for tries := 0; tries < size && len(out) < n; tries++ {
		port := p.next

		p.next++
		if p.next > p.hi {
			p.next = p.lo
		}

		if p.used[port] || !free(port) {
			continue
		}

		p.used[port] = true
		out = append(out, port)
	}

	if len(out) < n {
		for _, port := range out {
			delete(p.used, port)
		}

		return nil, ErrNoPort
	}

	return out, nil
}

// Release returns ports to the pool.
func (p *Ports) Release(ports []int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, port := range ports {
		delete(p.used, port)
	}
}

func free(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}

	_ = ln.Close()

	return true
}
