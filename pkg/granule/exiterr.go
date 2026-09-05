package granule

import (
	"errors"
	"os/exec"
)

// asExitError is errors.As with the concrete type named once, so the
// two callers that need to tell "the command said no" from "the command
// could not be run" agree on how that question is asked.
func asExitError(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}
