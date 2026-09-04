// Command kontur-mem-agent is a minimal guest-side daemon: it polls
// /proc/pressure/memory for this guest's own memory pressure and, when it
// crosses a threshold, signals the host's internal/memagent listener --
// the "kontur run" container this guest belongs to, reachable over
// netshim's control link, see the top-level README's "Container
// networking" -- to grow this VM's memory via cloud-hypervisor's
// virtio-mem hotplug device. This is the guest-side half of the flow an
// operator's "kontur resize" drives manually, but automatic and observed
// from inside the guest, where memory pressure is actually visible.
//
// Deliberately not folded into the multi-mode "kontur" binary (see
// cmd/kontur): that one lives in the outer scratch image, never the
// guest disk image itself (see the top-level Dockerfile's
// guest-rootfs-* stages), and pulling in its netshim/hypervisor/guestexec
// machinery just for this loop would be pointless weight on a guest that
// only ever needs to read one /proc file and open one plain TCP
// connection. Written in Go rather than a shell script so it needs no
// runtime dependency beyond a kernel with CONFIG_PSI -- neither guest
// rootfs variant otherwise carries nc/curl/gawk (see
// deploy/guest-image/README.md), and needing one of those only for this
// would be the one thing that differed between the Debian and Alpine
// builds of this daemon.
package main

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	pressurePath = "/proc/pressure/memory"
	routePath    = "/proc/net/route"

	defaultInterval  = 10 * time.Second
	defaultThreshold = 10.0
	defaultPort      = 30090
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("kontur-mem-agent: ")

	interval := envDuration("KONTUR_MEM_AGENT_INTERVAL", defaultInterval)
	threshold := envFloat("KONTUR_MEM_AGENT_THRESHOLD", defaultThreshold)
	port := envInt("KONTUR_MEM_AGENT_PORT", defaultPort)
	host := os.Getenv("KONTUR_MEM_AGENT_HOST")

	// A kernel built without CONFIG_PSI has no /proc/pressure at all,
	// and never grows one later -- the kernel cloud-hypervisor publishes
	// for its own CI, which is the one kontur's reference guests boot,
	// is such a kernel. Polling it anyway means the same ENOENT in the
	// guest's journal every interval for the life of the VM, saying
	// nothing about why. Say it once, plainly, and stop: there is
	// nothing for this daemon to do on this kernel, and the host's own
	// "kontur resize" is unaffected.
	if _, err := os.Stat(pressurePath); errors.Is(err, fs.ErrNotExist) {
		log.Printf("no %s: this guest's kernel is built without CONFIG_PSI, so it cannot report memory pressure and this agent has nothing to poll -- the host can still resize this VM with \"kontur resize\"", pressurePath)
		return
	}

	log.Printf("polling %s every %s, signalling port %d when \"some avg10\" >= %.2f", pressurePath, interval, port, threshold)

	for {
		avg10, err := readPressure(pressurePath)
		switch {
		case err != nil:
			log.Printf("reading %s: %v", pressurePath, err)
		case avg10 >= threshold:
			signalPressure(host, port, avg10)
		}
		time.Sleep(interval)
	}
}

// readPressure returns the "some avg10" field from a PSI file in the
// format /proc/pressure/memory publishes, e.g.:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=0
//
// "some" (at least one task stalled on memory) is used rather than
// "full" (every task stalled) since it reacts to pressure earlier -- by
// the time "full" has been nonzero for long, the guest is already
// thrashing.
func readPressure(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, field := range fields[1:] {
			k, v, ok := strings.Cut(field, "=")
			if ok && k == "avg10" {
				return strconv.ParseFloat(v, 64)
			}
		}
		return 0, fmt.Errorf("%q line has no avg10 field", "some")
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("no %q line found", "some")
}

// signalPressure asks the host to grow this guest's memory, by dialing
// its internal/memagent listener on port and writing a single
// "PRESSURE <value>\n" line. Errors (including no listener there at all,
// e.g. because the host has CHV_MEM_AGENT unset) are logged and
// otherwise ignored: this is just the next poll's problem too.
//
// An empty host falls back to this guest's default route's gateway (see
// defaultGateway), which is wrong here: the guest is spliced onto the
// container network, so its default route leads out there rather than to
// the host side of this VM. The guest's control link is configured with
// an explicit host instead -- see deploy/guest-image's
// kontur-control-net.
func signalPressure(host string, port int, avg10 float64) {
	if host == "" {
		gw, err := defaultGateway(routePath)
		if err != nil {
			log.Printf("finding default gateway: %v", err)
			return
		}
		host = gw.String()
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		log.Printf("signalling %s: %v", addr, err)
		return
	}
	defer conn.Close()

	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(conn, "PRESSURE %.2f\n", avg10); err != nil {
		log.Printf("signalling %s: %v", addr, err)
		return
	}
	log.Printf("signalled memory pressure (some avg10=%.2f) to %s", avg10, addr)
}

// defaultGateway returns this guest's default route's gateway address --
// the same address CHV_CMDLINE's "ip=guest::gateway:..." boot parameter
// already configured on this guest. It is only a fallback for a
// KONTUR_MEM_AGENT_HOST nothing supplied: a netshim-managed guest is
// spliced onto the container network, so this gateway is that network's,
// not the "kontur run" container beside it (see the top-level README's
// "Container networking"). Parsed from /proc/net/route instead of shelling out to
// "ip route" so this binary needs nothing beyond a kernel with /proc --
// neither guest rootfs variant otherwise needs iproute2 as of this
// writing (only the host side does), so pulling it in just for this
// would be its own new dependency on the Alpine side.
func defaultGateway(path string) (net.IP, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Scan() // header line: "Iface Destination Gateway Flags ..."
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		// Destination "00000000" is the default route, regardless of the
		// byte order the kernel prints these fields in (see below) --
		// all-zero bytes read the same either way.
		if fields[1] != "00000000" {
			continue
		}
		gw, err := hexLEToIPv4(fields[2])
		if err != nil {
			return nil, fmt.Errorf("parsing gateway field %q: %w", fields[2], err)
		}
		return gw, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("no default route found in %s", path)
}

// hexLEToIPv4 decodes one of /proc/net/route's address fields: 4 bytes,
// hex-encoded, in little-endian order (i.e. the reverse of the byte
// order a dotted-quad prints in -- the kernel formats these as a raw
// %08X of the host-byte-order 32-bit address, and this guest only ever
// runs on little-endian architectures).
func hexLEToIPv4(s string) (net.IP, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != 4 {
		return nil, fmt.Errorf("want 4 bytes, got %d", len(b))
	}
	return net.IPv4(b[3], b[2], b[1], b[0]), nil
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("%s: invalid duration %q, using default %s", key, v, def)
		return def
	}
	return d
}

func envFloat(key string, def float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Printf("%s: invalid number %q, using default %.2f", key, v, def)
		return def
	}
	return f
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("%s: invalid integer %q, using default %d", key, v, def)
		return def
	}
	return n
}
