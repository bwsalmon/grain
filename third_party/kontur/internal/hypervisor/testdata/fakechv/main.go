// Command fakechv stands in for the cloud-hypervisor binary in tests of
// the Runner's process, shutdown and suspend handling. It understands
// just enough of the real CLI and API to exercise Runner: it parses
// --api-socket from argv, serves the same vm.power-button / vmm.shutdown
// / vm.pause / vm.resume / vm.snapshot endpoints, and its behaviour is
// tuned through environment variables rather than flags so tests can
// drive it without inventing a parallel CLI.
//
//   - FAKECHV_SERVE_API=0   don't start the API socket at all
//   - FAKECHV_EXIT_ON_POWER_BUTTON=0   ack vm.power-button but keep running
//   - FAKECHV_EXIT_ON_VMM_SHUTDOWN=0   ack vmm.shutdown but keep running
//   - FAKECHV_IGNORE_SIGTERM=1   don't exit on SIGTERM either
package main

import (
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	var socketPath string
	for i, a := range os.Args {
		if a == "--api-socket" && i+1 < len(os.Args) {
			socketPath = strings.TrimPrefix(os.Args[i+1], "path=")
		}
	}

	if envBool("FAKECHV_SERVE_API", true) && socketPath != "" {
		l, err := net.Listen("unix", socketPath)
		if err != nil {
			os.Exit(2)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/vmm.ping", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		mux.HandleFunc("/api/v1/vm.power-button", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
			if envBool("FAKECHV_EXIT_ON_POWER_BUTTON", true) {
				go delayedExit(0)
			}
		})
		mux.HandleFunc("/api/v1/vmm.shutdown", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
			if envBool("FAKECHV_EXIT_ON_VMM_SHUTDOWN", true) {
				go delayedExit(0)
			}
		})
		mux.HandleFunc("/api/v1/vm.pause", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("/api/v1/vm.resume", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("/api/v1/vm.snapshot", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		go http.Serve(l, mux)
	}

	if !envBool("FAKECHV_IGNORE_SIGTERM", false) {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM)
		go func() {
			<-sigCh
			os.Exit(3)
		}()
	}

	select {}
}

func delayedExit(code int) {
	time.Sleep(10 * time.Millisecond)
	os.Exit(code)
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
