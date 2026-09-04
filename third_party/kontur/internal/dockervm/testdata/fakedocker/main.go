// Command fakedocker stands in for the docker CLI in tests of the
// dockervm package: it records every invocation it's given (one line per
// call, to the file named by FAKEDOCKER_LOG) so tests can assert on the
// exact sequence and arguments of "docker" commands dockervm issues,
// without needing a real docker daemon.
//
//   - FAKEDOCKER_MISSING names a comma-separated set of container names
//     that "stop"/"rm"/"inspect" should fail against with the same "No
//     such container"/"No such object" messages the real docker CLI uses,
//     so dockervm's not-found handling can be exercised too.
//   - FAKEDOCKER_FAIL_CONTAINS, if set, fails any call whose argv (joined
//     with spaces) contains it, so callers' error handling can be
//     exercised without needing a specific real failure mode.
//   - FAKEDOCKER_RUNNING sets what `inspect -f {{.State.Running}}`
//     prints (default "true").
//   - FAKEDOCKER_EXEC_EXIT sets the status an "exec" call exits with,
//     standing in for the guest command's own (default "0").
//   - FAKEDOCKER_PROBE_FAIL makes the readiness probe ("kontur ready",
//     see dockervm.readyProbeArgs) fail that many times before it
//     succeeds, which is how a guest that is still booting looks; -1
//     fails every one, for a guest that never comes up. It counts the
//     "exec" lines already in FAKEDOCKER_LOG, so it needs one to be set.
//     The probe is answered separately from FAKEDOCKER_EXEC_EXIT so a
//     test can have a guest that is reachable and a command that exits
//     non-zero.
//   - FAKEDOCKER_NO_READY_MODE makes the probe fail the way an image
//     predating "kontur ready" does, with cmd/kontur's own "unknown
//     mode" line.
//
// An "exec" call copies its stdin to stdout, so tests can assert that a
// session's stdin actually reaches the guest command.
package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func main() {
	args := os.Args[1:]

	logPath := os.Getenv("FAKEDOCKER_LOG")
	if logPath != "" {
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
		if missing(args[len(args)-1]) {
			fmt.Fprintf(os.Stderr, "Error response from daemon: No such container: %s\n", args[len(args)-1])
			os.Exit(1)
		}
	case "inspect":
		name := args[len(args)-1]
		if missing(name) {
			fmt.Fprintf(os.Stderr, "Error response from daemon: No such object: %s\n", name)
			os.Exit(1)
		}
		if len(args) >= 3 && args[1] == "-f" && args[2] == "{{.State.Running}}" {
			fmt.Println(envOr("FAKEDOCKER_RUNNING", "true"))
			os.Exit(0)
		}
	case "exec":
		io.Copy(os.Stdout, os.Stdin)
		if isReadinessProbe(args) {
			if os.Getenv("FAKEDOCKER_NO_READY_MODE") != "" {
				fmt.Fprintln(os.Stderr, `kontur: unknown mode "ready" (want "run", "netshim", "exec", "cp", "resize", "ready" or "sleep")`)
				os.Exit(1)
			}
			n, _ := strconv.Atoi(os.Getenv("FAKEDOCKER_PROBE_FAIL"))
			if n < 0 || (n > 0 && execCalls(logPath) <= n) {
				fmt.Fprintln(os.Stderr, "fakedocker: guest not reachable yet")
				os.Exit(1)
			}
			os.Exit(0)
		}
		code, _ := strconv.Atoi(os.Getenv("FAKEDOCKER_EXEC_EXIT"))
		os.Exit(code)
	}

	fmt.Println("fakedocker0123456789")
	os.Exit(0)
}

// isReadinessProbe reports whether this "exec" is dockervm's readiness
// probe rather than a guest command a caller asked for: the probe is the
// only thing that runs kontur's "ready" mode.
func isReadinessProbe(args []string) bool {
	for i, a := range args {
		if a == "kontur" && i+1 < len(args) && args[i+1] == "ready" {
			return true
		}
	}
	return false
}

// missing reports whether name is one of FAKEDOCKER_MISSING's containers.
func missing(name string) bool {
	for _, m := range strings.Split(os.Getenv("FAKEDOCKER_MISSING"), ",") {
		if m != "" && m == name {
			return true
		}
	}
	return false
}

// execCalls counts the "exec" invocations recorded in the log so far,
// this one included.
func execCalls(logPath string) int {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "exec\x1f") {
			n++
		}
	}
	return n
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
