package granule_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/grain/pkg/granule"
)

// fakeGuest is a sandbox that never boots. Every test in this package
// uses one, which is the point of Guest being an interface: the
// lifecycle is decided by granule and should be assertable without KVM.
type fakeGuest struct {
	mu sync.Mutex

	readyAfter int // attempts before Ready succeeds
	attempts   int

	unpacked [][]byte
	execs    [][]string
	exitCode int
	execOut  string
	execErr  error

	activity string
}

func (g *fakeGuest) Ready(context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.attempts++
	if g.attempts > g.readyAfter {
		return nil
	}
	return errors.New("guest not up")
}

func (g *fakeGuest) Exec(_ context.Context, cmd []string, stdout, stderr io.Writer) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.execs = append(g.execs, cmd)
	if g.execErr != nil {
		return 0, g.execErr
	}
	if g.execOut != "" {
		fmt.Fprint(stdout, g.execOut)
	}
	return g.exitCode, nil
}

func (g *fakeGuest) Unpack(_ context.Context, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.unpacked = append(g.unpacked, b)
	return nil
}

func (g *fakeGuest) ReadFile(_ context.Context, path string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if path == "/run/grain/activity" && g.activity != "" {
		return []byte(g.activity), nil
	}
	return nil, nil
}

func (g *fakeGuest) ranSetup() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.execs {
		if len(c) > 0 && strings.Contains(c[0], "setup") {
			return true
		}
	}
	return false
}

type fakeVMM struct {
	started    bool
	shutdown   bool
	startErr   error
	console    io.Writer
	consoleMsg string
}

func (v *fakeVMM) Start(_ context.Context, console io.Writer) error {
	if v.startErr != nil {
		return v.startErr
	}
	v.started, v.console = true, console
	if v.consoleMsg != "" {
		fmt.Fprint(console, v.consoleMsg)
	}
	return nil
}

func (v *fakeVMM) Shutdown(context.Context) error { v.shutdown = true; return nil }
func (v *fakeVMM) Wait() error                    { return nil }

// clock is a fixed time that advances only when a test says so, so
// waitReady's budget can be exhausted without a test sleeping for it.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var _ granule.Guest = (*fakeGuest)(nil)
var _ granule.VMM = (*fakeVMM)(nil)
