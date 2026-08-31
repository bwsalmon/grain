package orchestrator

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestWaitForPortHealthySucceedsWhenPortAlreadyListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitForPortHealthy(ctx, host, port); err != nil {
		t.Fatalf("waitForPortHealthy = %v, want nil", err)
	}
}

// TestWaitForPortHealthyRetriesUntilPortStartsListening covers the actual
// bwsalmon/agents#553 race: a slot becomes visible to Health() (ensure()
// has marked it created) before whatever the guest's SSH port needs --
// docker's own veth/ARP setup, under BackendDocker -- has finished, so the
// first dial or two see "connection refused"/"no route to host" even
// though the guest is genuinely on its way up. waitForPortHealthy is
// expected to ride that out rather than surface it as slotHealth's Error.
func TestWaitForPortHealthyRetriesUntilPortStartsListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	ln.Close()

	go func() {
		time.Sleep(200 * time.Millisecond)
		ln2, err := net.Listen("tcp", net.JoinHostPort(host, portStr))
		if err != nil {
			return
		}
		defer ln2.Close()
		time.Sleep(2 * time.Second)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
	defer cancel()
	if err := waitForPortHealthy(ctx, host, port); err != nil {
		t.Fatalf("waitForPortHealthy = %v, want nil once the port starts listening", err)
	}
}

func TestWaitForPortHealthyGivesUpWhenCtxExpires(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := waitForPortHealthy(ctx, host, port); err == nil {
		t.Fatal("waitForPortHealthy succeeded, want an error once ctx expires with nothing ever listening")
	}
}

func TestParseProcStatsReadsLoadAverageAndMemory(t *testing.T) {
	output := "0.10 0.05 0.01 1/234 5678\n" +
		"MemTotal:        1024000 kB\n" +
		"MemFree:          200000 kB\n" +
		"MemAvailable:     400000 kB\n"

	loadAverage, usedMB, totalMB := parseProcStats(output)

	if loadAverage != "0.10 0.05 0.01" {
		t.Errorf("loadAverage = %q, want %q", loadAverage, "0.10 0.05 0.01")
	}
	if totalMB != 1000 {
		t.Errorf("totalMB = %d, want 1000", totalMB)
	}
	// usedMB is MemTotal minus MemAvailable, not minus MemFree -- see
	// parseProcStats' own doc comment on why.
	if usedMB != 609 {
		t.Errorf("usedMB = %d, want 609", usedMB)
	}
}

func TestParseProcStatsToleratesMissingMeminfo(t *testing.T) {
	loadAverage, usedMB, totalMB := parseProcStats("0.50 0.40 0.30 2/10 99\n")

	if loadAverage != "0.50 0.40 0.30" {
		t.Errorf("loadAverage = %q, want %q", loadAverage, "0.50 0.40 0.30")
	}
	if usedMB != 0 || totalMB != 0 {
		t.Errorf("usedMB/totalMB = %d/%d, want 0/0 with no MemTotal line", usedMB, totalMB)
	}
}
