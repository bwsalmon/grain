// ui.go implements `grain ui`, formerly its own cmd/ui binary before
// bwsalmon/agents#313 combined every mode into one: grain's standalone
// task UI, serving pkg/ui's JSON API and static frontend on localhost and
// opening it in the system's default browser, with no install step
// beyond building this one binary. See v2/README.md's ui/ section and
// pkg/ui's own package doc comment for why it is shaped this way -- a
// local web server rather than an Electron/Tauri/native app, so the same
// binary that runs on a developer's Mac or Linux box today is exactly the
// API surface a future iOS/Android client would speak to, with nothing
// about the server itself to rebuild.
//
// -server-data-dir is the opt in to bwsalmon/agents#357: when this UI
// runs on the same host as a `grain daemon`, pointing it at that
// daemon's own -data-dir lets the UI set and delete the secrets under
// its secrets/ directory directly on disk -- no RPC to the daemon, since
// pkg/secrets.Store already reads fresh off disk on every call. It is
// write/list only: nothing in pkg/ui ever resolves a secret's value, so
// there is no path through this UI that reads one back.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/sqlite"
	"github.com/bwsalmon/grain/v2/pkg/secrets"
	"github.com/bwsalmon/grain/v2/pkg/ui"
)

func uiServe(args []string) {
	fs := flag.NewFlagSet("grain ui", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8420", "address to serve the UI on")
	dataDir := fs.String("data-dir", "", "root directory of the embedded SQLite store (required, unless -demo) -- a "+
		"daemon, a ui and this CLI may all point at the same one at once; SQLite's own file locking serialises the writes")
	serverDataDir := fs.String("server-data-dir", "",
		"the -data-dir a colocated `grain daemon` was started with, so this UI can set/delete/list its secrets "+
			"(under <server-data-dir>/secrets) without ever reading one back (bwsalmon/agents#357). This is "+
			"unrelated to -data-dir above, which names this UI's own embedded task store, not the server's -- "+
			"leave it unset when this UI does not run on the same host as the server.")
	actor := fs.String("as", "", "principal to attribute tasks created here to (defaults to the OS user)")
	defaultTargetRepo := fs.String("default-target-repo", "",
		"owner/name a task with no repo of its own targets")
	open := fs.Bool("open", true, "open the UI in the system's default browser once it's listening")
	demo := fs.Bool("demo", false,
		"seed a throwaway store with fake tasks instead of opening a real one -- "+
			"for trying out the UI with no orchestrator, sandbox or repo behind it")
	fs.Parse(args)

	if *demo && *dataDir != "" {
		fmt.Fprintln(os.Stderr, "grain ui: -demo seeds its own throwaway store; -data-dir must be unset")
		os.Exit(2)
	}
	if *demo {
		dir, err := os.MkdirTemp("", "grain-ui-demo-")
		if err != nil {
			log.Fatalf("grain ui: creating a throwaway store for -demo: %v", err)
		}
		*dataDir = dir
	}
	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "grain ui: -data-dir is required (unless -demo)")
		os.Exit(2)
	}

	db, err := sqlite.Open(sqlite.DefaultConfig(*dataDir))
	if err != nil {
		log.Fatalf("grain ui: opening the task store: %v", err)
	}
	defer db.Close()

	store := model.New(db)
	if err := store.Init(context.Background()); err != nil {
		log.Fatalf("grain ui: applying the schema: %v", err)
	}

	cfg := ui.Config{
		Actor:        ui.DefaultActor(actorID(*actor)),
		Capabilities: ui.DefaultCapabilities(),
	}
	if *serverDataDir != "" {
		cfg.Secrets = secrets.New(filepath.Join(*serverDataDir, "secrets"))
	}
	if *defaultTargetRepo != "" {
		repo, err := model.ParseRepo(*defaultTargetRepo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "grain ui: -default-target-repo: %v\n", err)
			os.Exit(2)
		}
		cfg.DefaultTarget = &repo
	}
	if *demo {
		if cfg.DefaultTarget == nil {
			repo := model.RepoRef{Owner: "acme", Name: "widgets"}
			cfg.DefaultTarget = &repo
		}
		if err := seedDemo(context.Background(), store, cfg); err != nil {
			log.Fatalf("grain ui: seeding -demo data: %v", err)
		}
		log.Printf("grain ui: -demo mode -- serving fake tasks from a throwaway store at %s, nothing here is real", *dataDir)
	}
	srv := ui.NewServer(cfg, store)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("grain ui: listening on %s: %v", *addr, err)
	}
	url := "http://" + ln.Addr().String()
	log.Printf("grain ui: serving the task store on %s as %s", url, cfg.Actor.ID)
	if *open {
		openBrowser(url)
	}
	log.Fatal(http.Serve(ln, srv))
}

// openBrowser best-effort launches url in the system's default browser --
// failing to do so (headless box, unknown OS) is never fatal, since the
// server is up and the URL is printed regardless.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("grain ui: opening browser: %v", err)
	}
}
