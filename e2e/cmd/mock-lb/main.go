// Command mock-lb is a tiny health-checked TCP load balancer for the FCoS-VM
// e2e tier. It stands in for the two Octavia load balancers a real Magnum
// cluster provisions in front of the control plane:
//
//	api_lb   — TCP :6443 across all master kube-apiservers
//	etcd_lb  — TCP :2379 across all master etcd members
//
// The reconciler reaches these through heat-params (KUBE_API_*_ADDRESS and
// ETCD_LB_VIP), so a faithful multi-master e2e needs a VIP that load-balances
// across the masters — exactly what a joining master's etcd client and a
// worker's kubeconfig expect. mock-lb is L4 only (plain TCP forward); it never
// terminates TLS, so the masters' real certificates flow through untouched.
//
// It is intentionally dependency-free (stdlib only) and mirrors the other e2e
// Go helpers (mock-magnum, mock-heat): the harness cross-builds it static and
// runs it under systemd-run on master-0.
//
// Usage:
//
//	mock-lb -name api  -listen 192.168.77.8:6443 -backends 192.168.77.10:6443,192.168.77.11:6443
//	mock-lb -name etcd -listen 192.168.77.9:2379 -backends 192.168.77.10:2379,192.168.77.11:2379
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type backend struct {
	addr    string
	healthy atomic.Bool
}

type pool struct {
	name     string
	backends []*backend
	rr       atomic.Uint64
	verbose  bool
}

// pick returns the next healthy backend in round-robin order, or nil if none
// are currently healthy.
func (p *pool) pick() *backend {
	n := len(p.backends)
	start := int(p.rr.Add(1))
	for i := range n {
		b := p.backends[(start+i)%n]
		if b.healthy.Load() {
			return b
		}
	}
	return nil
}

// healthLoop probes every backend on an interval with a plain TCP dial. A port
// that accepts a connection is treated as healthy — enough to route to a master
// whose apiserver/etcd is up, and to drop one that is mid-restart (the case the
// CA-rotation barrier exercises). Transitions are logged so CI shows the LB
// following the rolling control plane.
func (p *pool) healthLoop(interval, dialTimeout time.Duration) {
	for {
		for _, b := range p.backends {
			conn, err := net.DialTimeout("tcp", b.addr, dialTimeout)
			up := err == nil
			if conn != nil {
				_ = conn.Close()
			}
			if was := b.healthy.Swap(up); was != up {
				state := "DOWN"
				if up {
					state = "UP"
				}
				log.Printf("[%s] backend %s -> %s", p.name, b.addr, state)
			}
		}
		time.Sleep(interval)
	}
}

func (p *pool) serve(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			log.Printf("[%s] accept: %v", p.name, err)
			continue
		}
		go p.handle(c)
	}
}

func (p *pool) handle(client net.Conn) {
	defer client.Close()
	b := p.pick()
	if b == nil {
		log.Printf("[%s] no healthy backend for %s — dropping", p.name, client.RemoteAddr())
		return
	}
	upstream, err := net.DialTimeout("tcp", b.addr, 5*time.Second)
	if err != nil {
		log.Printf("[%s] dial backend %s: %v", p.name, b.addr, err)
		return
	}
	defer upstream.Close()
	if p.verbose {
		log.Printf("[%s] %s -> %s", p.name, client.RemoteAddr(), b.addr)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		// Half-close so the peer sees EOF and the other copy can finish.
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}
	go cp(upstream, client)
	go cp(client, upstream)
	wg.Wait()
}

func main() {
	var (
		name         = flag.String("name", "lb", "label for log lines (api|etcd)")
		listen       = flag.String("listen", "", "VIP listen address host:port (required)")
		backends     = flag.String("backends", "", "comma-separated backend host:port list (required)")
		healthEvery  = flag.Duration("health-interval", 2*time.Second, "backend health probe interval")
		healthDialTO = flag.Duration("health-timeout", time.Second, "backend health probe dial timeout")
		verbose      = flag.Bool("v", false, "log every proxied connection")
	)
	flag.Parse()

	if *listen == "" || *backends == "" {
		log.Fatal("mock-lb: -listen and -backends are required")
	}

	p := &pool{name: *name, verbose: *verbose}
	for a := range strings.SplitSeq(*backends, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		p.backends = append(p.backends, &backend{addr: a})
	}
	if len(p.backends) == 0 {
		log.Fatal("mock-lb: no backends parsed")
	}

	l, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("mock-lb: listen %s: %v", *listen, err)
	}
	log.Printf("[%s] listening on %s -> %v (health every %s)", *name, *listen, backendAddrs(p), *healthEvery)

	go p.healthLoop(*healthEvery, *healthDialTO)
	p.serve(l)
}

func backendAddrs(p *pool) []string {
	out := make([]string, len(p.backends))
	for i, b := range p.backends {
		out[i] = b.addr
	}
	return out
}
