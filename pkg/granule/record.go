package granule

import (
	"encoding/json"
	"time"
)

// Source says who produced a record. Three things share one stream by
// design -- the shim's own narration, the agent's output, and the guest's
// serial console -- so a tag is how they are told apart.
//
// The shim wraps all three rather than letting any of them through raw.
// kontur routes the guest console to its own stdio
// (internal/hypervisor/args.go's "--serial tty", "so it shows up under
// kubectl logs"), and as the shim's child that output is the shim's to
// capture and re-emit as records. Wrapping is what makes a console line
// addressable: a run killed by the provisioning budget can quote the last
// few in its detail, which raw interleaved text could not support.
//
// Which is why **stdout carries records and nothing else**, and anything
// the shim wants to say to a human goes to stderr. Without that rule a
// stray line -- a library's warning, a panic trace -- is indistinguishable
// from a damaged record, and "skip what does not parse" stops being a rule
// about damage and becomes one about mixed content.
//
// File descriptors cannot do this job even where the discipline holds.
// Docker tags each entry with its stream and its API can return them
// separately, but Kubernetes' pod log API strips it: the CRI log file
// carries the stream per line and the API returns only the message. So
// separating by fd works under one backend and not the other, and the tag
// is in the record instead.
type Source string

// Record kinds the shim itself emits. Both are this package's vocabulary;
// an agent's are its framework's.
const (
	// KindStatus carries a whole Status, and is what lets a controller
	// learn a grain's state without an exec at all: it already tails this
	// stream for the trajectory, so in the steady state reading a grain
	// costs nothing extra.
	//
	// A full snapshot rather than a delta, so this stays level-triggered
	// -- the property Reconcile rests on -- and absence stays meaningful:
	// the container's own state comes from the pod listing, so "running
	// but nothing recent here" is a distinguishable, and more
	// informative, state than an exec that hangs.
	//
	// Emitted on change plus a slow heartbeat, never on a fast fixed
	// interval. The kubelet rotates container logs at 10 MB across 5
	// files by default, and status records would otherwise eat the budget
	// the trajectory needs.
	KindStatus = "status"
	// KindPhase is a transition on its own, for a reader following the
	// stream rather than sampling it.
	KindPhase = "phase"
)

const (
	// SrcShim is the shim's own narration: phase transitions, rebuilds,
	// what it is doing while there is no agent yet.
	SrcShim Source = "shim"
	// SrcAgent is the agent CLI's own output, mirrored verbatim.
	SrcAgent Source = "agent"
	// SrcConsole is the guest's serial console, forwarded.
	//
	// Sharing the stream with it is what makes a failed boot legible: a
	// run killed by Policy.ProvisionBudget can quote the last console
	// lines in its detail rather than reporting only that time ran out.
	// Under the arrangement this replaces, that output went to the
	// container's stdout with nothing reading it.
	SrcConsole Source = "console"
)

// Record is one line of a grain's trajectory, as written to the
// container's stdout and read back with the runtime's own log stream.
//
// One JSON object per line, and never a multi-line value: a log stream is
// line-oriented, a reader may join it at any point, and rotation can take
// the first half of anything that spans lines. A record that cannot be
// parsed is skipped rather than fatal, for the same reason -- the tail of
// a rotated file routinely begins mid-line.
type Record struct {
	// Version is the wire format, on every line rather than once at the
	// top: a reader may start anywhere in the stream, so there is no
	// "top" to have read.
	Version string `json:"version"`
	// Seq is monotonic across a grain's whole life, and is the cursor
	// Status.Seq reports and Transcript takes. It is what makes a
	// sequence rather than a byte offset the right cursor: `docker logs`
	// and `kubectl logs` are addressed by time and line, so a byte offset
	// into the stream is not something either can seek to.
	Seq int64     `json:"seq"`
	T   time.Time `json:"t"`
	Src Source    `json:"src"`
	// Kind is the record's own type within Src, and is Src's to define:
	// the shim's vocabulary is small and this package's (KindStatus,
	// KindPhase), the agent's is whichever framework produced it. Empty
	// where Data is the whole content, as it is for a console line.
	Kind string          `json:"kind,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Labels a sandbox image carries, saying what it can do. They replace a
// `grain version` subcommand, and are better than one for the reason that
// matters: a controller wants these *before* it creates a grain -- to
// refuse a task naming a framework this image lacks, or an image speaking
// a wire it does not -- and asking a grain requires a grain to exist.
// Read once per image with `docker inspect`, not once per run.
//
// The wire version is also on every document (Version), which is what a
// controller checks when it is already talking to a grain. These are for
// deciding whether to start one.
const (
	// LabelWireVersions are the wire formats this image speaks, comma
	// separated. A list rather than one value because an image may speak
	// several through an upgrade, and a controller should take one they
	// share rather than refuse an image that could have served it.
	LabelWireVersions = "grain.wire-versions"
	// LabelFrameworks are the agent profiles this image carries -- the
	// names Spec.Framework.Name may take.
	LabelFrameworks = "grain.frameworks"
)
