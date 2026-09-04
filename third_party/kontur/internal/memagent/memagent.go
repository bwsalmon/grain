// Package memagent implements the host-side half of automatic memory
// hotplug: a small listener that lets a guest-side agent (see
// deploy/guest-image's kontur-mem-agent, installed in both guest image
// variants) ask this VM's own already-running "kontur run" process to
// grow its memory when the guest sees pressure, via the same
// cloud-hypervisor API hypervisor.APIClient.Resize (and so "kontur
// resize") already uses -- just triggered automatically, from inside the
// guest, rather than by an operator's kubectl exec.
//
// The protocol is deliberately minimal: the guest opens a plain TCP
// connection to Server's listen address (reachable directly, since the
// guest and this container are joined by netshim's control link -- see
// the top-level README's "Container networking") and writes one line,
// "PRESSURE <value>\n", where <value> is whatever pressure metric the
// guest used to decide to signal at all (currently /proc/pressure/memory's
// "some avg10", but Server itself never interprets it -- it's carried
// through only for logging). Any successfully read line grows the guest
// by Config.StepMB, capped at Config.MaxMB and rate-limited by
// Config.Cooldown; there is nothing here for the guest to negotiate a
// specific target size, on the theory that "grow a bit" repeated as
// pressure persists is simple enough to start with. See Config's own
// doc comment for why this stops well short of guest-driven shrinking or
// an authenticated protocol.
package memagent

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// Resizer is the subset of hypervisor.APIClient's methods Server depends
// on -- just enough to keep this package's tests (see memagent_test.go)
// from needing a real cloud-hypervisor API socket.
type Resizer interface {
	Resize(ctx context.Context, desiredRAMBytes uint64) error
}

// Config holds Server's tunables. See internal/config's CHV_MEM_AGENT*
// variables, which populate this in production.
type Config struct {
	// Addr is the address Server listens on, e.g. "169.254.100.1:30090"
	// -- must be reachable from the guest, and must match whatever
	// address/port the guest-side agent baked into the disk image was
	// told (or defaults) to signal.
	Addr string

	// StartMB is the guest's memory size at boot (config.Config's
	// MemoryMB), i.e. Server's own idea of the guest's current size
	// before any resize it performs itself. Server does not re-query
	// cloud-hypervisor for the guest's actual current size (via
	// vm.info) before each resize, so a resize performed some other way
	// while Server is also running (e.g. a concurrent "kontur resize")
	// would leave Server's internal bookkeeping stale -- acceptable for
	// a first pass (see this package's doc comment), worth revisiting
	// if that turns out to matter in practice.
	StartMB int

	// MaxMB is the ceiling no resize Server performs may exceed
	// (config.Config's MemoryMaxMB).
	MaxMB int

	// StepMB is how much a single pressure signal grows the guest by,
	// before clamping to MaxMB.
	StepMB int

	// Cooldown is the minimum time between two resizes Server performs.
	// A resize is asynchronous from the guest's point of view (see
	// hypervisor.APIClient.Resize's own doc comment), so without this a
	// guest still under pressure right after one grow -- because the
	// grow hasn't actually landed yet, not because it wasn't enough --
	// would just trigger another one immediately.
	Cooldown time.Duration
}

// Server accepts pressure signals from a guest-side agent and turns them
// into resize calls against Resizer. See this package's doc comment for
// the wire protocol.
type Server struct {
	cfg Config
	api Resizer

	mu         sync.Mutex
	currentMB  int
	lastResize time.Time
}

// New returns a Server ready for Serve. It performs no I/O itself.
func New(cfg Config, api Resizer) *Server {
	return &Server{cfg: cfg, api: api, currentMB: cfg.StartMB}
}

// Serve listens on cfg.Addr and handles pressure signals until ctx is
// done, at which point it closes the listener and returns nil. Blocks
// until then; run it in its own goroutine.
func (s *Server) Serve(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("memagent: listening on %s: %w", s.cfg.Addr, err)
	}

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("memagent: accept on %s: %w", s.cfg.Addr, err)
			}
		}
		go s.handle(ctx, conn)
	}
}

// handle reads a single "PRESSURE <value>" line from conn and, if one
// arrives, requests growth. One connection is used per signal rather
// than a persistent one, so a guest with nothing to say need not hold
// anything open.
func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		if err != nil {
			log.Printf("memagent: reading signal from %s: %v", conn.RemoteAddr(), err)
		}
		return
	}

	reading := strings.TrimSpace(strings.TrimPrefix(line, "PRESSURE"))
	s.requestGrowth(ctx, reading)
}

// requestGrowth grows the guest by cfg.StepMB (capped at cfg.MaxMB),
// unless it's already at the ceiling or still within cfg.Cooldown of the
// last grow. reading is only used for logging -- see this package's doc
// comment for why Server itself doesn't interpret it.
func (s *Server) requestGrowth(ctx context.Context, reading string) {
	s.mu.Lock()
	now := time.Now()
	if since := now.Sub(s.lastResize); !s.lastResize.IsZero() && since < s.cfg.Cooldown {
		s.mu.Unlock()
		log.Printf("memagent: pressure signal (%s) ignored: %s left in cooldown", reading, s.cfg.Cooldown-since)
		return
	}
	if s.currentMB >= s.cfg.MaxMB {
		s.mu.Unlock()
		log.Printf("memagent: pressure signal (%s) ignored: already at ceiling of %d MiB", reading, s.cfg.MaxMB)
		return
	}
	target := s.currentMB + s.cfg.StepMB
	if target > s.cfg.MaxMB {
		target = s.cfg.MaxMB
	}
	s.mu.Unlock()

	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.api.Resize(rctx, uint64(target)*1024*1024); err != nil {
		log.Printf("memagent: resizing to %d MiB after pressure signal (%s): %v", target, reading, err)
		return
	}

	s.mu.Lock()
	s.currentMB = target
	s.lastResize = now
	s.mu.Unlock()

	log.Printf("memagent: grew guest memory to %d MiB after pressure signal (%s)", target, reading)
}
