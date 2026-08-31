package netshim

import (
	"bytes"
	"fmt"
	"os"
	"testing"
	"time"
	"unsafe"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// testFrame is an Ethernet frame carrying a locally-assigned experimental
// ethertype (0x88b5), chosen so the kernel's own protocol handlers ignore
// it and the only thing that can move it between the two devices is the
// splice itself.
func testFrame(payload string) []byte {
	frame := []byte{
		0x02, 0x00, 0x00, 0x00, 0x00, 0x02, // dst mac
		0x02, 0x00, 0x00, 0x00, 0x00, 0x01, // src mac
		0x88, 0xb5, // ethertype
	}
	frame = append(frame, []byte(payload)...)
	// Pad to the 60-byte Ethernet minimum so nothing along the way pads
	// it for us and changes the bytes we compare against.
	for len(frame) < 60 {
		frame = append(frame, 0)
	}
	return frame
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// openTapFD attaches to an already-created persistent tap, returning a
// file descriptor for it. Opening it is also what gives the tap a
// carrier: until some process holds it open, frames redirected into it
// are dropped, which is exactly the coupling the flat-mode setup has to
// cloud-hypervisor being up.
func openTapFD(t *testing.T, name string) int {
	t.Helper()
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open /dev/net/tun: %v", err)
	}
	t.Cleanup(func() { unix.Close(fd) })

	var req struct {
		name  [unix.IFNAMSIZ]byte
		flags uint16
		_     [22]byte
	}
	copy(req.name[:], name)
	req.flags = unix.IFF_TAP | unix.IFF_NO_PI
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd),
		uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&req))); errno != 0 {
		t.Fatalf("TUNSETIFF %s: %v", name, errno)
	}
	return fd
}

// openPacketSocket returns a raw AF_PACKET socket bound to the named
// interface, for injecting and capturing frames on it directly.
func openPacketSocket(t *testing.T, index int) int {
	t.Helper()
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		t.Fatalf("AF_PACKET socket: %v", err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_ALL),
		Ifindex:  index,
	}); err != nil {
		t.Fatalf("bind AF_PACKET to index %d: %v", index, err)
	}
	return fd
}

// readFrameContaining polls fd until it sees a frame containing want, or
// the deadline passes. Both sources are noisy -- an AF_PACKET socket sees
// the frames the test itself sent out that interface, and a live
// namespace carries unrelated background traffic -- so it matches on the
// payload rather than taking the first frame to arrive.
func readFrameContaining(fd int, want []byte, timeout time.Duration) bool {
	buf := make([]byte, 2048)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n, err := unix.Read(fd, buf)
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if err != nil {
			return false
		}
		if n > 0 && bytes.Contains(buf[:n], want) {
			return true
		}
	}
	return false
}

// TestSplice_MovesFramesBothWays is the end-to-end check that the splice
// actually carries traffic, rather than only that the netlink objects
// were accepted: a frame put on one end of a veth pair comes out of the
// tap, and a frame written into the tap comes out of the veth.
//
// This is the whole mechanism flat mode rests on, so it is worth testing
// against the real kernel rather than a fake.
func TestSplice_MovesFramesBothWays(t *testing.T) {
	requireRoot(t)

	suffix := os.Getpid() % 10000
	vethName := fmt.Sprintf("sv-%d", suffix)
	peerName := fmt.Sprintf("sp-%d", suffix)
	tapName := fmt.Sprintf("st-%d", suffix)

	t.Cleanup(func() {
		for _, n := range []string{vethName, tapName} {
			if link, err := netlink.LinkByName(n); err == nil {
				netlink.LinkDel(link)
			}
		}
	})

	// The veth pair stands in for the container's own interface: the
	// peer is "the network", and vethName is what the guest takes over.
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: vethName},
		PeerName:  peerName,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("creating veth pair: %v", err)
	}
	vethLink, err := netlink.LinkByName(vethName)
	if err != nil {
		t.Fatalf("looking up %s: %v", vethName, err)
	}
	peerLink, err := netlink.LinkByName(peerName)
	if err != nil {
		t.Fatalf("looking up %s: %v", peerName, err)
	}

	tapLink, err := ensureTapMTU(tapName, vethLink.Attrs().MTU)
	if err != nil {
		t.Fatalf("ensureTapMTU: %v", err)
	}
	for _, l := range []netlink.Link{vethLink, peerLink} {
		if err := netlink.LinkSetUp(l); err != nil {
			t.Fatalf("bringing up %s: %v", l.Attrs().Name, err)
		}
	}

	if err := splice(vethLink, tapLink); err != nil {
		t.Fatalf("splice: %v", err)
	}

	tapFD := openTapFD(t, tapName)
	peerFD := openPacketSocket(t, peerLink.Attrs().Index)

	// Peer -> tap: a frame sent into the network side should be stolen
	// off the veth's ingress and handed to the tap.
	inbound := testFrame("splice-inbound")
	if _, err := unix.Write(peerFD, inbound); err != nil {
		t.Fatalf("writing to peer: %v", err)
	}
	if !readFrameContaining(tapFD, []byte("splice-inbound"), 2*time.Second) {
		t.Errorf("frame sent on %s never arrived on tap %s", peerName, tapName)
	}

	// Tap -> peer: a frame written by the VMM should be stolen off the
	// tap's ingress and transmitted out the veth.
	outbound := testFrame("splice-outbound")
	if _, err := unix.Write(tapFD, outbound); err != nil {
		t.Fatalf("writing to tap: %v", err)
	}
	if !readFrameContaining(peerFD, []byte("splice-outbound"), 2*time.Second) {
		t.Errorf("frame written to tap %s never arrived on %s", tapName, peerName)
	}
}

// TestSplice_Idempotent confirms a rerun leaves one filter per direction
// rather than stacking a second copy, the same convergence Setup and
// SetupFlat both promise a retried init container.
func TestSplice_Idempotent(t *testing.T) {
	requireRoot(t)

	suffix := os.Getpid() % 10000
	aName := fmt.Sprintf("sia-%d", suffix)
	bName := fmt.Sprintf("sib-%d", suffix)

	t.Cleanup(func() {
		for _, n := range []string{aName, bName} {
			if link, err := netlink.LinkByName(n); err == nil {
				netlink.LinkDel(link)
			}
		}
	})

	for _, n := range []string{aName, bName} {
		if _, err := ensureTapMTU(n, 1500); err != nil {
			t.Fatalf("ensureTapMTU %s: %v", n, err)
		}
	}
	a, err := netlink.LinkByName(aName)
	if err != nil {
		t.Fatalf("looking up %s: %v", aName, err)
	}
	b, err := netlink.LinkByName(bName)
	if err != nil {
		t.Fatalf("looking up %s: %v", bName, err)
	}

	for i := 0; i < 2; i++ {
		if err := splice(a, b); err != nil {
			t.Fatalf("splice (run %d): %v", i+1, err)
		}
	}

	for _, l := range []netlink.Link{a, b} {
		filters, err := netlink.FilterList(l, netlink.MakeHandle(0xffff, 0))
		if err != nil {
			t.Fatalf("listing filters on %s: %v", l.Attrs().Name, err)
		}
		if len(filters) != 1 {
			t.Errorf("%s has %d filters after two splices, want 1", l.Attrs().Name, len(filters))
		}
	}
}
