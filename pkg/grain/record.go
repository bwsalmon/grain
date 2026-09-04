package grain

import (
	"encoding/json"
	"time"
)

// Source says who produced a trajectory record. It exists because the
// container's stdout has more than one writer: kontur already routes the
// guest's serial console there (internal/hypervisor/args.go's "--serial
// tty", in its own words "so it shows up under kubectl logs"), and the
// shim writes the agent's trajectory to the same stream.
//
// Tagging is not optional and cannot be replaced by writing one of them
// to stderr: `kubectl logs` merges stdout and stderr, so separating by
// file descriptor works under docker and nowhere else.
type Source string

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
	// the shim's vocabulary is small and this package's, the agent's is
	// whichever framework produced it. Empty where Data is the whole
	// content, as it is for a console line.
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
