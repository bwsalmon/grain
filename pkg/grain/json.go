package grain

import (
	"encoding/json"
	"fmt"
	"time"
)

// Contract is the version of the wire format this build speaks: the
// subcommand set of the in-container `grain` CLI, the JSON documents that
// cross it, and the trajectory records on the container's stdout
// (docs/grain-cli.md).
//
// It is stamped on every document in both directions, which is the whole
// of version negotiation. The shim ships in the sandbox image and the
// controller ships in the daemon binary, so the two can genuinely differ:
// a deployment upgrades one and not the other, or pins an image. A
// receiver that does not recognise a document's contract must refuse it
// and say so, naming both versions -- never interpret it on a best
// effort, which is how a mismatch turns into a run that does the wrong
// thing quietly instead of one that fails legibly.
//
// It supersedes SpecVersion, which said the same thing about one document
// in one direction. There is one contract because the two halves are
// released together in the same binary; a shim that can read a Spec it
// cannot write a Status for would be a state with no use.
const Contract = 1

// Duration is a time.Duration that crosses the wire as a string --
// "2h0m0s" rather than 7200000000000.
//
// Go's own marshalling gives nanoseconds as an integer, which is correct
// and unreadable. These documents are read by people during incidents:
// `grain status` piped to a terminal, a spec recovered from a container
// that will not start. A field whose value has to be divided by a billion
// before it means anything is a field that gets misread, and the cost of
// the wrapper is one type nobody has to think about again.
type Duration time.Duration

// MarshalJSON writes the duration in time.Duration's own notation.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts that notation, and a bare 0 so that a document
// written by hand can leave a duration out without spelling one.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n int64
		if json.Unmarshal(b, &n) == nil && n == 0 {
			*d = 0
			return nil
		}
		return fmt.Errorf("grain: a duration must be a string like \"2h0m0s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("grain: parsing duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// String renders the duration the same way the wire does.
func (d Duration) String() string { return time.Duration(d).String() }
