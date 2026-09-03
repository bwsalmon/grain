// Command fakedocker stands in for the docker CLI in tests of the
// guestbuild package: it records every invocation (one line per call, to
// the file named by FAKEDOCKER_LOG) and answers the two `docker inspect`
// queries guestbuild makes, so the whole boot/provision/commit sequence
// can be asserted without a docker daemon, a KVM host or a real VM.
//
//   - FAKEDOCKER_RUNNING sets what `inspect -f {{.State.Running}}`
//     prints (default "true").
//   - FAKEDOCKER_EXIT sets what `inspect -f {{.State.ExitCode}}` prints
//     (default "0"); "137" is how a guest that had to be SIGKILLed looks.
//   - FAKEDOCKER_FAIL_CONTAINS fails any call whose argv (joined with
//     spaces) contains it, so guestbuild's error paths can be exercised.
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
		// Newlines inside an argument -- the scrub script is one --
		// would otherwise split a single call across log lines.
		fmt.Fprintln(f, strings.ReplaceAll(strings.Join(args, "\x1f"), "\n", "\x1e"))
		f.Close()
	}

	if want := os.Getenv("FAKEDOCKER_FAIL_CONTAINS"); want != "" && strings.Contains(strings.Join(args, " "), want) {
		fmt.Fprintf(os.Stderr, "fakedocker: failing on request: %s\n", want)
		os.Exit(1)
	}

	if len(args) >= 3 && args[0] == "inspect" && args[1] == "-f" {
		switch args[2] {
		case "{{.State.Running}}":
			fmt.Println(envOr("FAKEDOCKER_RUNNING", "true"))
		case "{{.State.ExitCode}}":
			fmt.Println(envOr("FAKEDOCKER_EXIT", "0"))
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
