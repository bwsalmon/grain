package netshim

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// qdiscAddAttempts bounds how many times addIngressQdisc retries on
// EBUSY. Adding an ingress qdisc races with anything else touching the
// same device's qdisc tree (including the kernel finishing a previous
// delete), and the kernel reports that race as EBUSY rather than
// blocking, so a bounded retry is the documented way to handle it.
const qdiscAddAttempts = 5

// splice cross-connects two links so that every frame arriving on either
// one is transmitted out the other: a wire, not a switch. It is what puts
// the guest directly onto the container's own network segment without a
// bridge in between.
//
// Each direction is an ingress qdisc plus a match-everything filter whose
// action is mirred/egress-redirect. "Ingress" selects frames *arriving*
// on the device; "egress redirect" hands them to the other device's
// transmit path. Redirect (as opposed to mirror) reuses the same skb
// rather than cloning it, so nothing is copied and the frame never leaves
// the kernel.
//
// Because there is no forwarding database involved, both ends may carry
// the same MAC address -- which is the entire point: it lets the guest
// boot with the address and MAC the container runtime assigned to the
// veth, so nothing upstream sees a new endpoint appear. A bridge cannot
// do this, since one MAC on two of its ports would flap its FDB.
//
// splice is idempotent: each direction's ingress qdisc is deleted before
// being re-added, which takes that qdisc's filters with it, so a rerun
// (the same way Kubernetes may retry a failed init container) reinstalls
// the same end state rather than accumulating duplicate filters.
func splice(a, b netlink.Link) error {
	for _, dir := range [][2]netlink.Link{{a, b}, {b, a}} {
		from, to := dir[0], dir[1]
		if err := addIngressQdisc(from); err != nil {
			return err
		}
		if err := addRedirectFilter(from, to); err != nil {
			return err
		}
	}
	return nil
}

// addIngressQdisc (re)creates link's ingress qdisc, first removing any
// existing one so the caller starts from a known-empty filter list.
func addIngressQdisc(link netlink.Link) error {
	name := link.Attrs().Name
	qdisc := &netlink.Ingress{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: link.Attrs().Index,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_INGRESS,
		},
	}

	if err := netlink.QdiscDel(qdisc); err != nil && !isNoSuchQdisc(err) {
		return fmt.Errorf("removing existing ingress qdisc on %s: %w", name, err)
	}

	var err error
	for i := 0; i < qdiscAddAttempts; i++ {
		if err = netlink.QdiscAdd(qdisc); err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) {
			break
		}
	}
	return fmt.Errorf("adding ingress qdisc on %s: %w", name, err)
}

// addRedirectFilter installs the filter that does the actual splicing:
// every frame (ETH_P_ALL, matched unconditionally) arriving on from is
// stolen and transmitted out to.
//
// The classifier is u32 with no selectors, which matches everything. The
// "matchall" classifier expresses that more directly, but it needs
// CONFIG_NET_CLS_MATCHALL, which plenty of minimal kernels leave out --
// and the failure is a bare ENOENT from the filter add, with nothing to
// say the classifier is the missing piece. u32 is effectively always
// present, and is what kata-containers uses for the same job.
//
// The qdisc's parent is HANDLE_INGRESS, but a filter attached to that
// qdisc names it by handle -- ffff:0 -- instead. Passing HANDLE_INGRESS
// here would silently fail to attach.
func addRedirectFilter(from, to netlink.Link) error {
	filter := &netlink.U32{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: from.Attrs().Index,
			Parent:    netlink.MakeHandle(0xffff, 0),
			Priority:  1,
			Protocol:  unix.ETH_P_ALL,
		},
		Actions: []netlink.Action{netlink.NewMirredAction(to.Attrs().Index)},
	}
	if err := netlink.FilterAdd(filter); err != nil {
		return fmt.Errorf("redirecting %s to %s: %w",
			from.Attrs().Name, to.Attrs().Name, err)
	}
	return nil
}

// isNoSuchQdisc reports whether err is the kernel saying there was no
// ingress qdisc to delete, which for an idempotent setup is success.
// Kernels differ on which errno they use for it, so both are accepted; a
// delete that failed for some other reason still surfaces loudly, as the
// add that follows it then fails with EEXIST.
func isNoSuchQdisc(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.EINVAL)
}
