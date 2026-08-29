// Command fakedocker stands in for the docker CLI in tests of the
// dockervm package: it records every invocation it's given (one line per
// call, to the file named by FAKEDOCKER_LOG) so tests can assert on the
// exact sequence and arguments of "docker" commands dockervm issues,
// without needing a real docker daemon.
//
//   - FAKEDOCKER_MISSING names a comma-separated set of container names
//     that "stop"/"rm" should fail against with the same "No such
//     container" message the real docker CLI uses, so dockervm's
//     not-found handling can be exercised too.
//   - FAKEDOCKER_FAIL_CONTAINS, if set, fails any call whose argv (joined
//     with spaces) contains it, so callers' error handling can be
//     exercised without needing a specific real failure mode.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	args := os.Args[1:]

	if logPath := os.Getenv("FAKEDOCKER_LOG"); logPath != "" {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintln(f, strings.Join(args, "\x1f"))
		f.Close()
	}

	if len(args) == 0 {
		os.Exit(0)
	}

	if fail := os.Getenv("FAKEDOCKER_FAIL_CONTAINS"); fail != "" && strings.Contains(strings.Join(args, " "), fail) {
		fmt.Fprintf(os.Stderr, "fakedocker: forced failure (matched %q)\n", fail)
		os.Exit(1)
	}

	switch args[0] {
	case "stop", "rm":
		name := args[len(args)-1]
		for _, missing := range strings.Split(os.Getenv("FAKEDOCKER_MISSING"), ",") {
			if missing != "" && missing == name {
				fmt.Fprintf(os.Stderr, "Error response from daemon: No such container: %s\n", name)
				os.Exit(1)
			}
		}
	}

	fmt.Println("fakedocker0123456789")
	os.Exit(0)
}
